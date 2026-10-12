package app

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Interfaces is the controlled vocabulary of what a part speaks and needs.
// It is deliberately a fixed list rather than free tags: a project can only
// reason about compatibility if everyone spells I2C the same way.
var Interfaces = []struct{ Name, Icon, Group string }{
	{"I2C", "🔗", "Bus"},
	{"SPI", "🔗", "Bus"},
	{"UART", "🔗", "Bus"},
	{"1-Wire", "🧵", "Bus"},
	{"CAN", "🚌", "Bus"},
	{"I2S", "🎵", "Bus"},
	{"PWM", "〰", "Signal"},
	{"ADC", "📈", "Signal"},
	{"DAC", "📉", "Signal"},
	{"GPIO", "📌", "Signal"},
	{"USB-C", "🔌", "Power / host"},
	{"Micro-USB", "🔌", "Power / host"},
	{"USB-A", "🔌", "Power / host"},
	{"Barrel jack", "🔌", "Power / host"},
	{"JST-PH", "🔋", "Power / host"},
	{"Qwiic / STEMMA QT", "🔗", "Bus"},
	{"3V3 logic", "⚡", "Level"},
	{"5V logic", "⚡", "Level"},
	{"12V", "⚡", "Level"},
	{"Mains", "⚠", "Level"},
	{"WiFi", "📶", "Wireless"},
	{"Bluetooth / BLE", "🔵", "Wireless"},
	{"LoRa", "🛰", "Wireless"},
	{"Zigbee", "🐝", "Wireless"},
	{"Ethernet", "🌐", "Wireless"},
	{"HDMI", "🖵", "Display"},
	{"MIPI / DSI", "🖵", "Display"},
	{"microSD", "💾", "Storage"},
}

var interfaceIcons = func() map[string]string {
	m := make(map[string]string, len(Interfaces))
	for _, i := range Interfaces {
		m[i.Name] = i.Icon
	}
	return m
}()

// IconForInterface is used wherever an interface is rendered.
func IconForInterface(name string) string { return interfaceIcons[name] }

// InterfaceGroups returns the vocabulary grouped for the edit form's checkboxes.
func InterfaceGroups() []InterfaceGroup {
	var order []string
	byGroup := map[string][]string{}
	for _, i := range Interfaces {
		if _, seen := byGroup[i.Group]; !seen {
			order = append(order, i.Group)
		}
		byGroup[i.Group] = append(byGroup[i.Group], i.Name)
	}
	out := make([]InterfaceGroup, 0, len(order))
	for _, g := range order {
		out = append(out, InterfaceGroup{Name: g, Members: byGroup[g]})
	}
	return out
}

type InterfaceGroup struct {
	Name    string
	Members []string
}

// validInterface keeps typos and injected values out of the vocabulary.
func validInterface(name string) bool {
	_, ok := interfaceIcons[name]
	return ok
}

// Spec is one line of a part's specification table.
type Spec struct {
	Name  string
	Value string
}

// Reference is a datasheet, pinout diagram or manual. It can be a link, a
// stored image, or both -- a pinout is only useful if you can look at it.
type Reference struct {
	ID       int64
	ItemID   int64
	Kind     string
	Title    string
	URL      string
	Filename string
	Position int
}

func (r Reference) IsImage() bool { return r.Filename != "" }

// RefKinds are the reference types offered in the UI.
var RefKinds = []string{"datasheet", "pinout", "manual", "guide", "schematic", "reference"}

// --- interfaces -------------------------------------------------------------

func (s *Store) attachInterfaces(items []Item, byID map[int64]int) error {
	return s.eachInChunks(itemIDs(byID), func(in string) string {
		return `SELECT item_id, name FROM item_interfaces
			WHERE item_id IN (` + in + `) ORDER BY name`
	}, func(rows *sql.Rows) error {
		var itemID int64
		var name string
		if err := rows.Scan(&itemID, &name); err != nil {
			return err
		}
		items[byID[itemID]].Interfaces = append(items[byID[itemID]].Interfaces, name)
		return nil
	})
}

func setInterfacesTx(tx *sql.Tx, itemID int64, names []string) error {
	if _, err := tx.Exec(`DELETE FROM item_interfaces WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for _, n := range names {
		if !validInterface(n) {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO item_interfaces (item_id, name) VALUES (?,?)`, itemID, n); err != nil {
			return err
		}
	}
	return nil
}

// InterfaceFacets counts how many items speak each interface, for the filter row.
func (s *Store) InterfaceFacets() ([]Facet, error) {
	rows, err := s.db.Query(`SELECT name, COUNT(*) FROM item_interfaces
		GROUP BY name ORDER BY COUNT(*) DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Facet
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, err
		}
		f.Icon = IconForInterface(f.Value)
		out = append(out, f)
	}
	return out, rows.Err()
}

// --- specs ------------------------------------------------------------------

func (s *Store) attachSpecs(items []Item, byID map[int64]int) error {
	return s.eachInChunks(itemIDs(byID), func(in string) string {
		return `SELECT item_id, name, value FROM specs
			WHERE item_id IN (` + in + `) ORDER BY item_id, position, name`
	}, func(rows *sql.Rows) error {
		var itemID int64
		var sp Spec
		if err := rows.Scan(&itemID, &sp.Name, &sp.Value); err != nil {
			return err
		}
		items[byID[itemID]].Specs = append(items[byID[itemID]].Specs, sp)
		return nil
	})
}

func setSpecsTx(tx *sql.Tx, itemID int64, specs []Spec) error {
	if _, err := tx.Exec(`DELETE FROM specs WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for i, sp := range specs {
		if sp.Name == "" {
			continue
		}
		_, err := tx.Exec(`INSERT OR REPLACE INTO specs (item_id, name, value, position)
			VALUES (?,?,?,?)`, itemID, sp.Name, sp.Value, i)
		if err != nil {
			return err
		}
	}
	return nil
}

// ParseSpecs reads the "name: value" lines the edit form collects.
func ParseSpecs(raw string) []Spec {
	var out []Spec
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			// A line with no colon is still worth keeping as a bare note.
			name, value = line, ""
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, Spec{Name: name, Value: value})
	}
	return out
}

// SpecText renders specs back into the textarea format.
func (i Item) SpecText() string {
	var b strings.Builder
	for _, sp := range i.Specs {
		b.WriteString(sp.Name)
		if sp.Value != "" {
			b.WriteString(": ")
			b.WriteString(sp.Value)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// --- references -------------------------------------------------------------

func (s *Store) attachRefs(items []Item, byID map[int64]int) error {
	return s.eachInChunks(itemIDs(byID), func(in string) string {
		return `SELECT id, item_id, kind, title, url, filename, position
			FROM refs WHERE item_id IN (` + in + `) ORDER BY item_id, position, id`
	}, func(rows *sql.Rows) error {
		var r Reference
		if err := rows.Scan(&r.ID, &r.ItemID, &r.Kind, &r.Title, &r.URL, &r.Filename, &r.Position); err != nil {
			return err
		}
		items[byID[r.ItemID]].Refs = append(items[byID[r.ItemID]].Refs, r)
		return nil
	})
}

func (s *Store) AddReference(r Reference) (int64, error) {
	var next int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(position)+1, 0) FROM refs WHERE item_id = ?`, r.ItemID).Scan(&next)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO refs (item_id, kind, title, url, filename, position)
		VALUES (?,?,?,?,?,?)`, r.ItemID, r.Kind, r.Title, r.URL, r.Filename, next)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteReference removes a reference and reports the stored file, if any, so
// the caller can unlink it.
func (s *Store) DeleteReference(id int64) (itemID int64, filename string, err error) {
	err = s.db.QueryRow(`SELECT item_id, filename FROM refs WHERE id = ?`, id).Scan(&itemID, &filename)
	if err != nil {
		return 0, "", err
	}
	_, err = s.db.Exec(`DELETE FROM refs WHERE id = ?`, id)
	return itemID, filename, err
}

// --- price history ----------------------------------------------------------

// PricePoint is one observation of a price over time.
type PricePoint struct {
	Source   string
	Amount   float64
	Currency string
	SeenAt   time.Time
}

func (p PricePoint) Display() string { return formatMoney(p.Amount, p.Currency) }

// PriceHistory returns every observation for an item, oldest first.
func (s *Store) PriceHistory(itemID int64) ([]PricePoint, error) {
	rows, err := s.db.Query(`SELECT source, amount, currency, seen_at FROM price_history
		WHERE item_id = ? ORDER BY seen_at, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PricePoint
	for rows.Next() {
		var p PricePoint
		var seen string
		if err := rows.Scan(&p.Source, &p.Amount, &p.Currency, &seen); err != nil {
			return nil, err
		}
		p.SeenAt, _ = time.Parse(time.RFC3339, seen)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PriceChange describes how a source's price has moved since it was first seen.
type PriceChange struct {
	Source string
	First  float64
	Latest float64
	Points int
}

func (c PriceChange) Direction() string {
	switch {
	case c.Latest > c.First:
		return "up"
	case c.Latest < c.First:
		return "down"
	}
	return "flat"
}

// PriceChanges summarises the history per source, skipping sources only ever
// seen once -- a single observation is not a trend.
func PriceChanges(points []PricePoint) []PriceChange {
	first := map[string]PricePoint{}
	last := map[string]PricePoint{}
	count := map[string]int{}
	var order []string

	for _, p := range points {
		if _, seen := first[p.Source]; !seen {
			first[p.Source] = p
			order = append(order, p.Source)
		}
		last[p.Source] = p
		count[p.Source]++
	}

	var out []PriceChange
	for _, src := range order {
		if count[src] < 2 || first[src].Amount == last[src].Amount {
			continue
		}
		out = append(out, PriceChange{
			Source: src, First: first[src].Amount,
			Latest: last[src].Amount, Points: count[src],
		})
	}
	return out
}

// --- projects ---------------------------------------------------------------

// ProjectStatuses are the states a build moves through. "building" is the one
// with teeth: it reserves the parts on the bill of materials against every
// other project.
var ProjectStatuses = []string{"planning", "building", "done", "shelved"}

// Project is a build you are planning, with the parts it calls for.
type Project struct {
	ID           int64
	Name         string
	Notes        string
	Status       string
	ControllerID *int64 // the board everything else hangs off
	ConsumedAt   time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time

	Parts      []ProjectPart
	Controller *Item
}

// Consumed means the parts have actually been taken off the shelf.
func (p Project) Consumed() bool { return !p.ConsumedAt.IsZero() }

// Building means this project is holding its parts against everything else.
func (p Project) Building() bool { return p.Status == "building" && !p.Consumed() }

// ControllerRef exists because html/template cannot dereference a *int64.
func (p Project) ControllerRef() int64 {
	if p.ControllerID == nil {
		return 0
	}
	return *p.ControllerID
}

// ProjectPart is one line of a project's bill of materials. It either points at
// something on the shelf or names something not owned yet.
type ProjectPart struct {
	ID           int64
	ItemID       *int64
	SubProjectID *int64 // a sub-assembly: another project used as a part
	Name         string
	Quantity     int
	Note         string

	// Filled in when the line points at a real item.
	Item *Item
	// Filled in when the line points at another project. Buildable is how many
	// complete copies of it the shelf could supply right now.
	Sub       *Project
	Buildable int
	// How many of that item other projects being built have claimed. This is
	// what makes two projects that each look satisfied stop looking satisfied
	// when between them they need more than you own.
	Claimed int
	Rival   []Commitment
}

// Label is what to call this line: the item's name when it is owned, otherwise
// whatever was typed.
// IsAssembly reports whether this line is another project rather than a part.
func (p ProjectPart) IsAssembly() bool { return p.SubProjectID != nil }

func (p ProjectPart) SubRef() int64 {
	if p.SubProjectID == nil {
		return 0
	}
	return *p.SubProjectID
}

func (p ProjectPart) Label() string {
	switch {
	case p.Item != nil:
		return p.Item.Name
	case p.Sub != nil:
		return p.Sub.Name
	}
	return p.Name
}

// Have is how many are on the shelf. A line for a part you do not own has none
// by definition; a sub-assembly has as many as its own parts could make.
func (p ProjectPart) Have() int {
	switch {
	case p.Item != nil:
		return p.Item.Quantity
	case p.SubProjectID != nil:
		return p.Buildable
	}
	return 0
}

// Free is how many this project can actually get its hands on: what is on the
// shelf, less what other projects being built have already claimed. A
// sub-assembly's buildable count is already measured against free stock, so
// there is nothing further to subtract.
func (p ProjectPart) Free() int {
	if p.SubProjectID != nil {
		return p.Buildable
	}
	if n := p.Have() - p.Claimed; n > 0 {
		return n
	}
	return 0
}

func (p ProjectPart) Short() int {
	if n := p.Quantity - p.Free(); n > 0 {
		return n
	}
	return 0
}

func (p ProjectPart) Enough() bool { return p.Short() == 0 }

// Contested means this line would be covered if another project were not
// holding the same parts -- a different problem from simply not owning enough,
// and one you fix by finishing a build rather than by ordering.
func (p ProjectPart) Contested() bool { return p.Claimed > 0 && p.Quantity <= p.Have() && !p.Enough() }

// RivalNote names who else is holding the part.
func (p ProjectPart) RivalNote() string {
	if len(p.Rival) == 0 {
		return ""
	}
	names := make([]string, 0, len(p.Rival))
	for _, c := range p.Rival {
		names = append(names, fmt.Sprintf("%s (%d)", c.Project, c.Quantity))
	}
	return "also claimed by " + strings.Join(names, ", ")
}

// Shortfall is every line the shelf cannot currently cover -- the answer to
// "what else do I need to buy".
func (p Project) Shortfall() []ProjectPart {
	var out []ProjectPart
	for _, part := range p.Parts {
		if !part.Enough() {
			out = append(out, part)
		}
	}
	return out
}

func (p Project) Ready() bool { return len(p.Shortfall()) == 0 && len(p.Parts) > 0 }

// EstimatedCost totals the cheapest known price of everything still missing.
func (p Project) EstimatedCost() float64 {
	var total float64
	for _, part := range p.Shortfall() {
		if part.Item == nil {
			continue
		}
		if best := part.Item.Best(); best != nil {
			total += best.Amount * float64(part.Short())
		}
	}
	return total
}

// Interfaces is the set of interfaces the project's owned parts use, which is
// what drives the "you may also need" suggestions.
func (p Project) Interfaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range p.Parts {
		if part.Item == nil {
			continue
		}
		for _, i := range part.Item.Interfaces {
			if !seen[i] {
				seen[i] = true
				out = append(out, i)
			}
		}
	}
	sort.Strings(out)
	return out
}

const projectCols = `id, name, notes, status, controller_id, consumed_at, created_at, updated_at`

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach parts so the list can show progress without N queries.
	for i := range out {
		parts, err := s.projectParts(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Parts = parts
	}
	return out, nil
}

func scanProject(sc interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var consumed, created, updated string
	err := sc.Scan(&p.ID, &p.Name, &p.Notes, &p.Status, &p.ControllerID,
		&consumed, &created, &updated)
	if err != nil {
		return p, err
	}
	p.ConsumedAt, _ = time.Parse(time.RFC3339, consumed)
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return p, nil
}

func (s *Store) GetProject(id int64) (Project, error) {
	row := s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if err != nil {
		return p, err
	}
	if p.Parts, err = s.projectParts(id); err != nil {
		return p, err
	}
	if p.ControllerID != nil {
		// The controller is often on the bill of materials too, so reuse the
		// copy already loaded rather than querying again.
		for i := range p.Parts {
			if p.Parts[i].Item != nil && p.Parts[i].Item.ID == *p.ControllerID {
				p.Controller = p.Parts[i].Item
			}
		}
		if p.Controller == nil {
			it, err := s.GetItem(*p.ControllerID)
			if err == nil {
				p.Controller = &it
			} else if !errors.Is(err, sql.ErrNoRows) {
				return p, err
			}
			// A controller that has been deleted just leaves the budget empty.
		}
	}
	return p, nil
}

// projectParts loads a bill of materials, resolving each line that points at
// something owned into the full item so stock and price are available.
func (s *Store) projectParts(projectID int64) ([]ProjectPart, error) {
	rows, err := s.db.Query(`SELECT id, item_id, sub_project_id, name, quantity, note
		FROM project_parts WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var parts []ProjectPart
	var itemIDs, subIDs []int64
	for rows.Next() {
		var p ProjectPart
		if err := rows.Scan(&p.ID, &p.ItemID, &p.SubProjectID, &p.Name, &p.Quantity, &p.Note); err != nil {
			return nil, err
		}
		if p.ItemID != nil {
			itemIDs = append(itemIDs, *p.ItemID)
		}
		if p.SubProjectID != nil {
			subIDs = append(subIDs, *p.SubProjectID)
		}
		parts = append(parts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One load of the inventory serves both the item lines and the buildable
	// count on every sub-assembly line. It used to be loaded twice over, and a
	// third time by the page itself.
	if len(itemIDs) == 0 && len(subIDs) == 0 {
		return parts, nil
	}
	items, err := s.ListItems(Query{})
	if err != nil {
		return nil, err
	}
	if len(subIDs) > 0 {
		if err := s.attachSubAssemblies(parts, items); err != nil {
			return nil, err
		}
	}
	if len(itemIDs) == 0 {
		return parts, nil
	}
	byID := map[int64]*Item{}
	index := map[int64]int{}
	for i := range items {
		byID[items[i].ID] = &items[i]
		index[items[i].ID] = i
	}
	// The grid does not need specifications, but a project does: the pin budget
	// reads its capacity and its I2C addresses out of them.
	if err := s.attachSpecs(items, index); err != nil {
		return nil, err
	}
	for i := range parts {
		if parts[i].ItemID == nil {
			continue
		}
		it := byID[*parts[i].ItemID]
		parts[i].Item = it
		if it == nil {
			continue
		}
		// Only *other* projects contend for the part: a project is not competing
		// with itself for the parts it has listed.
		for _, c := range it.Claims {
			if c.ProjectID != projectID {
				parts[i].Claimed += c.Quantity
				parts[i].Rival = append(parts[i].Rival, c)
			}
		}
	}
	return parts, nil
}

func (s *Store) CreateProject(name, notes string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`INSERT INTO projects (name, notes, status, created_at, updated_at)
		VALUES (?,?,?,?,?)`, name, notes, "planning", now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateProject saves the settings form. Moving a consumed project back to a
// status other than done would leave its parts deducted with nothing saying so,
// so consumed projects keep their status until the parts are returned.
func (s *Store) UpdateProject(id int64, name, notes, status string, controllerID *int64) error {
	var consumed string
	if err := s.db.QueryRow(`SELECT consumed_at FROM projects WHERE id = ?`, id).Scan(&consumed); err != nil {
		return err
	}
	if consumed != "" {
		status = "done"
	}
	_, err := s.db.Exec(`UPDATE projects SET name=?, notes=?, status=?, controller_id=?, updated_at=? WHERE id=?`,
		name, notes, status, controllerID, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *Store) DeleteProject(id int64) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}

func (s *Store) AddProjectPart(projectID int64, itemID *int64, name string, qty int, note string) error {
	if qty < 1 {
		qty = 1
	}
	_, err := s.db.Exec(`INSERT INTO project_parts (project_id, item_id, name, quantity, note)
		VALUES (?,?,?,?,?)`, projectID, itemID, name, qty, note)
	if err != nil {
		return err
	}
	return s.touchProject(projectID)
}

func (s *Store) DeleteProjectPart(partID int64) (int64, error) {
	var projectID int64
	if err := s.db.QueryRow(`SELECT project_id FROM project_parts WHERE id = ?`, partID).Scan(&projectID); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(`DELETE FROM project_parts WHERE id = ?`, partID); err != nil {
		return projectID, err
	}
	return projectID, s.touchProject(projectID)
}

func (s *Store) touchProject(id int64) error {
	_, err := s.db.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// SuggestForProject proposes parts already on the shelf that speak one of the
// interfaces this project uses and are not in its bill of materials yet. It is
// a nudge from your own inventory -- "you have a level shifter that fits this"
// -- not a shopping recommendation.
func (s *Store) SuggestForProject(p Project, limit int) ([]Item, error) {
	want := p.Interfaces()
	if len(want) == 0 {
		return nil, nil
	}
	wanted := map[string]bool{}
	for _, i := range want {
		wanted[i] = true
	}
	inBOM := map[int64]bool{}
	for _, part := range p.Parts {
		if part.ItemID != nil {
			inBOM[*part.ItemID] = true
		}
	}

	items, err := s.ListItems(Query{})
	if err != nil {
		return nil, err
	}

	type scored struct {
		item    Item
		matches int
	}
	var hits []scored
	for _, it := range items {
		if inBOM[it.ID] || it.Quantity == 0 {
			continue
		}
		matches := 0
		for _, i := range it.Interfaces {
			if wanted[i] {
				matches++
			}
		}
		if matches > 0 {
			hits = append(hits, scored{it, matches})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].matches > hits[j].matches })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Item, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.item)
	}
	return out, nil
}

// attachSubAssemblies fills in each sub-assembly line: what the sub-project is
// called, and how many complete copies of it the shelf could supply right now.
func (s *Store) attachSubAssemblies(parts []ProjectPart, items []Item) error {
	boms, err := s.loadBOMs()
	if err != nil {
		return err
	}
	free := make(map[int64]int, len(items))
	for _, it := range items {
		free[it.ID] = it.Available()
	}

	names := map[int64]string{}
	rows, err := s.db.Query(`SELECT id, name FROM projects`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range parts {
		if parts[i].SubProjectID == nil {
			continue
		}
		id := *parts[i].SubProjectID
		parts[i].Sub = &Project{ID: id, Name: names[id]}
		parts[i].Buildable = buildable(boms, id, free)
	}
	return nil
}
