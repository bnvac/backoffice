package app

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// --- bill of materials ------------------------------------------------------

type bomData struct {
	Project Project
	Import  BOMImport
	Replace bool
}

// handleImportBOM reads a bill of materials pasted into the box or uploaded as
// a file, matches it against the inventory, and shows what it found before
// writing anything -- a BOM that matched the wrong parts is worth seeing before
// it becomes a parts list.
func (a *App) handleImportBOM(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)

	p, err := a.store.GetProject(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	raw, err := bomText(r)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	lines, err := ParseBOM(raw)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	items, err := a.store.ListItems(Query{})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	lines = MatchBOM(lines, items)
	replace := r.FormValue("replace") != ""

	// Two steps on purpose: the first shows what was understood, the second
	// commits it. "confirm" is what the review page's button sets.
	if r.FormValue("confirm") == "" {
		a.render(w, r, "bom.html", "Import BOM · "+p.Name, bomData{
			Project: p, Import: Summarise(lines), Replace: replace,
		})
		return
	}

	added, err := a.store.ImportBOM(id, lines, replace)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	sum := Summarise(lines)
	a.store.Record(a.actor(r), "imported a BOM", "project", id,
		fmt.Sprintf("%d lines, %d matched", added, sum.Matched))

	msg := fmt.Sprintf("Imported %s — %d matched something you own", plural(added, "line"), sum.Matched)
	var note string
	if sum.Unknown > 0 {
		note = fmt.Sprintf("%s did not match anything in the inventory and are listed as things to buy",
			plural(sum.Unknown, "line"))
	}
	redirect(w, r, dest, msg, note)
}

// bomText takes the pasted text, or the uploaded file when there is one.
func bomText(r *http.Request) (string, error) {
	if r.MultipartForm != nil && len(r.MultipartForm.File["file"]) > 0 {
		fh := r.MultipartForm.File["file"][0]
		f, err := fh.Open()
		if err != nil {
			return "", err
		}
		defer f.Close()
		// A bill of materials is text; anything this size is not one.
		raw, err := io.ReadAll(io.LimitReader(f, 4<<20))
		if err != nil {
			return "", err
		}
		if len(raw) > 0 {
			return string(raw), nil
		}
	}
	text := strings.TrimSpace(r.FormValue("bom"))
	if text == "" {
		return "", fmt.Errorf("paste a bill of materials, or choose a CSV file")
	}
	return text, nil
}

// handleExportBOM writes the parts list back out as CSV, including what is on
// the shelf and what is still short, so it can go straight into a spreadsheet
// or a distributor's bulk order form.
func (a *App) handleExportBOM(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetProject(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-bom.csv"`, slugify(p.Name)))

	cw := csv.NewWriter(w)
	defer cw.Flush()
	for _, row := range p.ExportBOM() {
		if err := cw.Write(row); err != nil {
			return
		}
	}
}

// handleShortfallCSV exports only what still has to be bought, which is the
// list you actually paste into a shopping cart.
func (a *App) handleShortfallCSV(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetProject(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-shopping-list.csv"`, slugify(p.Name)))

	cw := csv.NewWriter(w)
	defer cw.Flush()
	cw.Write([]string{"name", "part_number", "need", "have", "buy", "unit_price", "link"})
	for _, part := range p.Shortfall() {
		row := []string{part.Label(), "", strconv.Itoa(part.Quantity), strconv.Itoa(part.Free()), strconv.Itoa(part.Short()), "", ""}
		if it := part.Item; it != nil {
			row[1] = it.PartNumber
			if best := it.Best(); best != nil {
				row[5] = fmt.Sprintf("%.2f", best.Amount)
				row[6] = best.URL
			}
			if row[6] == "" {
				row[6] = it.Link
			}
		}
		cw.Write(row)
	}
}

// slugify turns a project name into something safe for a filename.
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "project"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
