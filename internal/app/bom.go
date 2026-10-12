package app

import (
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A bill of materials is how a schematic tells you what to buy. KiCad, EasyEDA,
// Altium and every spreadsheet anyone has ever kept all export CSV, but none of
// them agree on the column names, so the header is matched by meaning rather
// than by position and unknown columns are ignored.

// BOMLine is one row of an imported bill of materials, plus what it was matched
// against in the inventory.
type BOMLine struct {
	Reference  string // designators: "R1 R2 R3"
	Name       string
	PartNumber string
	Value      string
	Footprint  string
	Quantity   int
	Note       string

	Match *Item  // the inventory item this line resolved to, if any
	Why   string // how it matched, so a wrong guess is visible
}

// Label is what to call this line in the parts list.
//
// The specific fields win over the descriptive ones. A KiCad row carries both
// "4k7" and "Resistor", and "4k7" is the one you can order; the description
// ends up in the note, where it is still there but not in the way.
func (l BOMLine) Label() string {
	switch {
	case l.PartNumber != "":
		return l.PartNumber
	case l.Value != "":
		return l.Value
	case l.Name != "":
		return l.Name
	}
	return l.Reference
}

// Describe is everything about the line that the label left out.
func (l BOMLine) Describe() string {
	var parts []string
	for _, s := range []string{l.Name, l.Footprint, l.Note} {
		if s != "" && s != l.Label() {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// bomAliases maps the column names real tools emit onto the fields above.
// Everything is compared lowercased with punctuation stripped.
var bomAliases = map[string]string{
	"ref": "reference", "refs": "reference", "reference": "reference",
	"references": "reference", "designator": "reference", "designators": "reference",
	"refdes": "reference", "designation": "reference",

	"qty": "quantity", "qnty": "quantity", "quantity": "quantity",
	"count": "quantity", "amount": "quantity", "quantitypercb": "quantity",

	"value": "value", "val": "value", "comment": "value",

	"footprint": "footprint", "package": "footprint", "pattern": "footprint",
	"pcbfootprint": "footprint",

	"mpn": "part_number", "partnumber": "part_number", "part": "part_number",
	"manufacturerpartnumber": "part_number", "mfrpartno": "part_number",
	"mfgpartnumber": "part_number", "manufacturerpart": "part_number",
	"lcscpartnumber": "part_number", "supplierpartnumber": "part_number",

	"description": "name", "name": "name", "partname": "name", "item": "name",
	"title": "name", "component": "name",

	"note": "note", "notes": "note", "remark": "note", "manufacturer": "note",
	"mfr": "note", "supplier": "note",
}

// normaliseHeader reduces a column name to the key bomAliases is written in.
func normaliseHeader(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ParseBOM reads a pasted or uploaded bill of materials.
//
// It copes with the preamble KiCad writes above the header, with semicolon and
// tab separated files, and with a headerless two-column "part, qty" list --
// which is what people actually paste when they are in a hurry.
func ParseBOM(raw string) ([]BOMLine, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if raw == "" {
		return nil, fmt.Errorf("paste a bill of materials first")
	}

	records, err := readDelimited(raw)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("nothing readable in that bill of materials")
	}

	// Find the header: the first row where at least two columns are recognised.
	// Anything above it is the exporter's preamble.
	headerAt, columns := -1, map[int]string{}
	for i, rec := range records {
		found := map[int]string{}
		for j, cell := range rec {
			if field, ok := bomAliases[normaliseHeader(cell)]; ok {
				found[j] = field
			}
		}
		if len(found) >= 2 {
			headerAt, columns = i, found
			break
		}
	}

	var lines []BOMLine
	if headerAt < 0 {
		// No header at all: treat it as "name, quantity" and say so if that
		// turns out to be wrong.
		for _, rec := range records {
			line := BOMLine{Name: strings.TrimSpace(rec[0]), Quantity: 1}
			if len(rec) > 1 {
				if n, err := strconv.Atoi(strings.TrimSpace(rec[1])); err == nil && n > 0 {
					line.Quantity = n
				} else {
					line.Note = strings.TrimSpace(rec[1])
				}
			}
			if line.Name != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			return nil, fmt.Errorf("could not find a header row, and the first column was empty")
		}
		return lines, nil
	}

	for _, rec := range records[headerAt+1:] {
		var line BOMLine
		for j, cell := range rec {
			cell = strings.TrimSpace(cell)
			if cell == "" {
				continue
			}
			switch columns[j] {
			case "reference":
				line.Reference = cell
			case "quantity":
				if n, err := strconv.Atoi(strings.Fields(cell)[0]); err == nil {
					line.Quantity = n
				}
			case "value":
				line.Value = cell
			case "footprint":
				line.Footprint = cell
			case "part_number":
				line.PartNumber = cell
			case "name":
				line.Name = cell
			case "note":
				line.Note = appendNote(line.Note, cell)
			}
		}
		if line.Quantity <= 0 {
			// KiCad's grouped export sometimes omits the count and relies on the
			// designator list; a line with no count at all is one piece.
			line.Quantity = len(designators(line.Reference))
			if line.Quantity == 0 {
				line.Quantity = 1
			}
		}
		if line.Label() == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("found the header but no parts under it")
	}
	return lines, nil
}

// readDelimited parses with whichever separator the file actually uses. CSV is
// tried first because a comma inside a quoted description would otherwise make
// a semicolon file look comma-separated.
func readDelimited(raw string) ([][]string, error) {
	best, bestScore := [][]string(nil), -1
	for _, sep := range []rune{',', ';', '\t', '|'} {
		r := csv.NewReader(strings.NewReader(raw))
		r.Comma = sep
		r.FieldsPerRecord = -1 // the preamble has a different width to the body
		r.LazyQuotes = true
		r.TrimLeadingSpace = true
		recs, err := r.ReadAll()
		if err != nil || len(recs) == 0 {
			continue
		}
		// The right separator is the one that actually splits the rows.
		score := 0
		for _, rec := range recs {
			score += len(rec)
		}
		if score > bestScore {
			best, bestScore = recs, score
		}
	}
	if best == nil {
		return nil, fmt.Errorf("that does not look like CSV — export the BOM as CSV and paste it again")
	}
	// Drop rows that are entirely empty, which trailing newlines leave behind.
	var out [][]string
	for _, rec := range best {
		for _, cell := range rec {
			if strings.TrimSpace(cell) != "" {
				out = append(out, rec)
				break
			}
		}
	}
	return out, nil
}

func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// designators splits "R1, R2 R3" into individual reference designators.
func designators(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\t'
	})
	var out []string
	for _, d := range f {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// MatchBOM resolves each line against the inventory. Matching is deliberately
// conservative and always says how it matched, because a bill of materials
// silently pointed at the wrong part is worse than one that is honestly blank.
func MatchBOM(lines []BOMLine, items []Item) []BOMLine {
	byMPN := map[string]*Item{}
	byName := map[string]*Item{}
	byValue := map[string]*Item{}
	for i := range items {
		it := &items[i]
		if mpn := strings.ToLower(strings.TrimSpace(it.PartNumber)); mpn != "" {
			if _, taken := byMPN[mpn]; !taken {
				byMPN[mpn] = it
			}
		}
		if name := strings.ToLower(strings.TrimSpace(it.Name)); name != "" {
			if _, taken := byName[name]; !taken {
				byName[name] = it
			}
		}
		if v := normaliseValue(it.Value); v != "" {
			if _, taken := byValue[v]; !taken {
				byValue[v] = it
			}
		}
	}

	// A schematic's value column often holds the manufacturer part number --
	// KiCad puts ESP32-WROOM-32 there, not in a separate MPN column -- so the
	// value is tried against part numbers as well as against values.
	for i := range lines {
		l := &lines[i]
		mpn, name, value := strings.ToLower(l.PartNumber), strings.ToLower(l.Name), strings.ToLower(l.Value)
		switch {
		case mpn != "" && byMPN[mpn] != nil:
			l.Match, l.Why = byMPN[mpn], "part number"
		case value != "" && byMPN[value] != nil:
			l.Match, l.Why = byMPN[value], "part number"
		case name != "" && byMPN[name] != nil:
			l.Match, l.Why = byMPN[name], "part number"
		case name != "" && byName[name] != nil:
			l.Match, l.Why = byName[name], "name"
		case value != "" && byName[value] != nil:
			l.Match, l.Why = byName[value], "name"
		case l.Value != "" && byValue[normaliseValue(l.Value)] != nil:
			l.Match, l.Why = byValue[normaliseValue(l.Value)], "value"
		}
	}
	return lines
}

// normaliseValue makes "4.7k", "4k7" and "4700" comparable, so a BOM that
// spells a resistor differently to your inventory still lines up.
func normaliseValue(s string) string {
	v, unit, ok := ParseComponentValue(s)
	if !ok {
		return ""
	}
	// A value with no unit at all is a resistance by convention -- a BOM that
	// says 4700 and an inventory that says 4k7 are the same part.
	if unit == "" {
		unit = "R"
	}
	return fmt.Sprintf("%s:%.6g", unit, v)
}

// bomCSV writes parsed lines back out in the canonical column order, which is
// how the review page hands them to the confirm step without the server having
// to remember anything between the two requests.
func bomCSV(lines []BOMLine) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Write([]string{"reference", "name", "part_number", "value", "footprint", "quantity", "note"})
	for _, l := range lines {
		w.Write([]string{l.Reference, l.Name, l.PartNumber, l.Value, l.Footprint,
			strconv.Itoa(l.Quantity), l.Note})
	}
	w.Flush()
	return b.String()
}

// BOMImport is the result of reading a bill of materials, in the shape the
// confirmation page needs.
type BOMImport struct {
	Lines   []BOMLine
	Matched int
	Unknown int
	Pieces  int
}

func Summarise(lines []BOMLine) BOMImport {
	sum := BOMImport{Lines: lines}
	for _, l := range lines {
		sum.Pieces += l.Quantity
		if l.Match != nil {
			sum.Matched++
		} else {
			sum.Unknown++
		}
	}
	return sum
}

// ImportBOM writes a parsed bill of materials onto a project. Lines that
// matched become references to owned items; the rest are recorded by name, so
// they show up in the shortfall list as things to buy.
func (s *Store) ImportBOM(projectID int64, lines []BOMLine, replace bool) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if replace {
		if _, err := tx.Exec(`DELETE FROM project_parts WHERE project_id = ?`, projectID); err != nil {
			return 0, err
		}
	}
	added := 0
	for _, l := range lines {
		qty := l.Quantity
		if qty < 1 {
			qty = 1
		}
		// The designators and the description are worth keeping: they are how
		// you find the part on the board once it is in front of you.
		note := l.Describe()
		if l.Reference != "" {
			note = appendNote(l.Reference, note)
		}
		var itemID *int64
		name := l.Label()
		if l.Match != nil {
			id := l.Match.ID
			itemID, name = &id, ""
			// A schematic knows the land pattern; the inventory usually does
			// not. Fill it in when it is missing, but never overwrite one
			// somebody chose.
			if l.Footprint != "" && l.Match.Footprint == "" {
				if _, err := tx.Exec(`UPDATE items SET footprint = ? WHERE id = ? AND footprint = ''`,
					l.Footprint, id); err != nil {
					return 0, err
				}
			}
		}
		_, err := tx.Exec(`INSERT INTO project_parts (project_id, item_id, name, quantity, note)
			VALUES (?,?,?,?,?)`, projectID, itemID, name, qty, note)
		if err != nil {
			return 0, err
		}
		added++
	}
	_, err = tx.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, nowRFC3339(), projectID)
	if err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

// ExportBOM renders a project's parts list as CSV, in a shape that reads back
// into this same importer -- and into a spreadsheet or a distributor's bulk
// order form, which is the point.
func (p Project) ExportBOM() [][]string {
	out := [][]string{{
		"reference", "name", "part_number", "value", "quantity",
		"have", "short", "location", "unit_price", "link",
	}}
	parts := append([]ProjectPart(nil), p.Parts...)
	sort.SliceStable(parts, func(i, j int) bool {
		return strings.ToLower(parts[i].Label()) < strings.ToLower(parts[j].Label())
	})
	for _, part := range parts {
		row := []string{
			part.Note, part.Label(), "", "", strconv.Itoa(part.Quantity),
			strconv.Itoa(part.Free()), strconv.Itoa(part.Short()), "", "", "",
		}
		if it := part.Item; it != nil {
			row[2], row[3], row[7] = it.PartNumber, it.Value, it.Location
			if best := it.Best(); best != nil {
				row[8] = strconv.FormatFloat(best.Amount, 'f', 2, 64)
				row[9] = best.URL
			}
			if row[9] == "" {
				row[9] = it.Link
			}
		}
		out = append(out, row)
	}
	return out
}
