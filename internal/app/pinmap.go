package app

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The pin budget counts. This records which pin went where, which is the only
// way to catch the mistake counting cannot see: the same pin used twice.
//
// It is also the thing you actually want in front of you at the bench -- a
// printable table of what connects to what -- so it doubles as the wiring card.

// PinAssignment is one connection from the controller to something else.
type PinAssignment struct {
	ID        int64
	ProjectID int64
	Pin       string // "GPIO21", "D4", "A0", "3V3"
	ItemID    *int64
	Part      string
	Signal    string // "SDA", "CS", "INT"
	Note      string
	Position  int

	Item *Item
	// Filled in by the conflict pass: other assignments sharing this pin.
	Clashes []string
}

func (p PinAssignment) Where() string {
	if p.Item != nil {
		return p.Item.Name
	}
	return p.Part
}

func (p PinAssignment) ItemRef() int64 {
	if p.ItemID == nil {
		return 0
	}
	return *p.ItemID
}

// normalisePin makes "GPIO21", "gpio 21", "IO21" and "D21" comparable, so a pin
// written two ways is still caught as one pin used twice. What is stored is
// what was typed; this is only used for the comparison.
var pinShapes = []struct {
	pattern *regexp.Regexp
	format  string
}{
	{regexp.MustCompile(`^(?:gpio|io)[\s_-]*(\d+)$`), "GPIO%d"},
	{regexp.MustCompile(`^d(\d+)$`), "D%d"},
	{regexp.MustCompile(`^a(\d+)$`), "A%d"},
	{regexp.MustCompile(`^pin[\s_-]*(\d+)$`), "PIN%d"},
	{regexp.MustCompile(`^p(\d+)$`), "PIN%d"},
}

func normalisePin(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	for _, shape := range pinShapes {
		if m := shape.pattern.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return fmt.Sprintf(shape.format, n)
		}
	}
	// Power and ground can legitimately be shared by everything, so they are
	// normalised but never treated as a clash.
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// sharedPins are the rails every device is meant to be connected to at once.
var sharedPins = map[string]bool{
	"3V3": true, "3.3V": true, "5V": true, "VCC": true, "VIN": true, "VBUS": true,
	"GND": true, "AGND": true, "DGND": true, "0V": true,
	// Bus lines are shared by design too: several devices on SDA is the point.
	"SDA": true, "SCL": true, "SCK": true, "MOSI": true, "MISO": true,
}

func (s *Store) PinAssignments(projectID int64) ([]PinAssignment, error) {
	rows, err := s.db.Query(`SELECT id, project_id, pin, item_id, part, signal, note, position
		FROM pin_assignments WHERE project_id = ? ORDER BY position, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PinAssignment
	var needItems bool
	for rows.Next() {
		var p PinAssignment
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.Pin, &p.ItemID, &p.Part,
			&p.Signal, &p.Note, &p.Position); err != nil {
			return nil, err
		}
		needItems = needItems || p.ItemID != nil
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if needItems {
		items, err := s.ListItems(Query{})
		if err != nil {
			return nil, err
		}
		byID := map[int64]*Item{}
		for i := range items {
			byID[items[i].ID] = &items[i]
		}
		for i := range out {
			if out[i].ItemID != nil {
				out[i].Item = byID[*out[i].ItemID]
			}
		}
	}
	sortPins(out)
	markPinClashes(out)
	return out, nil
}

// sortPins puts the map in the order you would read it off a board: numbered
// pins in numerical order, named pins alphabetically after them.
func sortPins(pins []PinAssignment) {
	number := func(p PinAssignment) (string, int, bool) {
		norm := normalisePin(p.Pin)
		for i := len(norm) - 1; i >= 0; i-- {
			if norm[i] < '0' || norm[i] > '9' {
				if i == len(norm)-1 {
					return norm, 0, false
				}
				n, _ := strconv.Atoi(norm[i+1:])
				return norm[:i+1], n, true
			}
		}
		n, _ := strconv.Atoi(norm)
		return "", n, true
	}
	sort.SliceStable(pins, func(i, j int) bool {
		pa, na, oka := number(pins[i])
		pb, nb, okb := number(pins[j])
		if oka != okb {
			return oka // numbered pins first
		}
		if pa != pb {
			return pa < pb
		}
		return na < nb
	})
}

// markPinClashes flags a pin doing two jobs at once.
//
// Sharing is only legitimate when everything on the pin is doing the same job:
// three sensors on SDA is a bus, and everything on 3V3 is a rail. The same pin
// carrying SDA for one part and CS for another is the mistake this exists to
// catch, so the test is on the signals rather than on the count of devices.
func markPinClashes(pins []PinAssignment) {
	byPin := map[string][]int{}
	for i, p := range pins {
		norm := normalisePin(p.Pin)
		if norm == "" || sharedPins[norm] {
			continue // a power or ground rail is shared by definition
		}
		byPin[norm] = append(byPin[norm], i)
	}
	for _, idxs := range byPin {
		if len(idxs) < 2 {
			continue
		}
		if sharedJob(pins, idxs) {
			continue
		}
		for _, i := range idxs {
			for _, j := range idxs {
				if i == j {
					continue
				}
				label := pins[j].Where()
				if pins[j].Signal != "" {
					label += " (" + pins[j].Signal + ")"
				}
				pins[i].Clashes = append(pins[i].Clashes, label)
			}
		}
	}
}

// sharedJob reports whether every assignment on a pin is the same bus line,
// which is the one case where two things on one pin is correct.
func sharedJob(pins []PinAssignment, idxs []int) bool {
	signals := map[string]bool{}
	for _, i := range idxs {
		signals[strings.ToUpper(strings.TrimSpace(pins[i].Signal))] = true
	}
	if len(signals) != 1 {
		return false // two different jobs on one pin is never right
	}
	for sig := range signals {
		return sharedPins[sig]
	}
	return false
}

// PinConflicts is the list of clashes, phrased once per pin rather than once
// per assignment, for the warnings panel.
func PinConflicts(pins []PinAssignment) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pins {
		norm := normalisePin(p.Pin)
		if len(p.Clashes) == 0 || seen[norm] {
			continue
		}
		seen[norm] = true
		label := p.Where()
		if p.Signal != "" {
			label += " (" + p.Signal + ")"
		}
		out = append(out, fmt.Sprintf("%s is wired to %s and %s — only one of them can have it",
			p.Pin, label, strings.Join(dedupe(p.Clashes), " and ")))
	}
	sort.Strings(out)
	return out
}

// AssignedPins is how many distinct pins the map uses, which is the figure to
// compare against the controller's capacity.
func AssignedPins(pins []PinAssignment) int {
	seen := map[string]bool{}
	for _, p := range pins {
		if norm := normalisePin(p.Pin); norm != "" && !sharedPins[norm] {
			seen[norm] = true
		}
	}
	return len(seen)
}

func (s *Store) AddPinAssignment(p PinAssignment) error {
	p.Pin = strings.TrimSpace(p.Pin)
	if p.Pin == "" {
		return fmt.Errorf("say which pin")
	}
	if p.ItemID == nil && strings.TrimSpace(p.Part) == "" && strings.TrimSpace(p.Signal) == "" {
		return fmt.Errorf("say what is on that pin")
	}
	var next int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(position)+1, 0) FROM pin_assignments WHERE project_id = ?`,
		p.ProjectID).Scan(&next)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO pin_assignments (project_id, pin, item_id, part, signal, note, position)
		VALUES (?,?,?,?,?,?,?)`,
		p.ProjectID, p.Pin, p.ItemID, strings.TrimSpace(p.Part),
		strings.TrimSpace(p.Signal), strings.TrimSpace(p.Note), next)
	if err != nil {
		return err
	}
	return s.touchProject(p.ProjectID)
}

func (s *Store) DeletePinAssignment(id int64) (int64, error) {
	var projectID int64
	if err := s.db.QueryRow(`SELECT project_id FROM pin_assignments WHERE id = ?`, id).Scan(&projectID); err != nil {
		return 0, err
	}
	_, err := s.db.Exec(`DELETE FROM pin_assignments WHERE id = ?`, id)
	return projectID, err
}

// SuggestPins proposes the assignments a project's parts imply, so a map can be
// started from what is already known rather than typed from nothing. Only the
// unambiguous ones are offered: a part that speaks I2C needs SDA and SCL, and
// there is no guessing involved.
func SuggestPins(p Project, existing []PinAssignment) []PinAssignment {
	taken := map[string]bool{}
	for _, a := range existing {
		key := strings.ToLower(a.Where() + "/" + a.Signal)
		taken[key] = true
	}

	signals := map[string][]string{
		"I2C":               {"SDA", "SCL"},
		"Qwiic / STEMMA QT": {"SDA", "SCL"},
		"SPI":               {"SCK", "MOSI", "MISO", "CS"},
		"UART":              {"TX", "RX"},
		"1-Wire":            {"DATA"},
		"I2S":               {"BCLK", "LRCLK", "DIN"},
		"CAN":               {"CAN_TX", "CAN_RX"},
	}

	var out []PinAssignment
	for _, part := range p.Parts {
		it := part.Item
		if it == nil || (p.Controller != nil && it.ID == p.Controller.ID) {
			continue
		}
		for _, io := range it.Interfaces {
			for _, sig := range signals[io] {
				if taken[strings.ToLower(it.Name+"/"+sig)] {
					continue
				}
				taken[strings.ToLower(it.Name+"/"+sig)] = true
				id := it.ID
				out = append(out, PinAssignment{
					ProjectID: p.ID, ItemID: &id, Part: it.Name, Signal: sig,
				})
			}
		}
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}
