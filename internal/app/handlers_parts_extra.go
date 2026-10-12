package app

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// --- pin map ----------------------------------------------------------------

func (a *App) handleAddPin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/projects/%d#pins", id)

	// The suggestion buttons post several rows at once; a typed row posts one.
	pins := r.Form["pin"]
	parts := r.Form["part"]
	signals := r.Form["signal"]
	itemIDs := r.Form["item_id"]

	added, failed := 0, ""
	for i, pin := range pins {
		p := PinAssignment{ProjectID: id, Pin: pin, Note: r.FormValue("note")}
		if i < len(parts) {
			p.Part = parts[i]
		}
		if i < len(signals) {
			p.Signal = signals[i]
		}
		if i < len(itemIDs) {
			p.ItemID = optionalID(itemIDs[i])
		}
		if strings.TrimSpace(pin) == "" {
			continue // a suggestion the person left blank
		}
		if err := a.store.AddPinAssignment(p); err != nil {
			failed = err.Error()
			continue
		}
		added++
	}
	if added == 0 {
		redirect(w, r, dest, "", orDefault(failed, "fill in at least one pin"))
		return
	}
	redirect(w, r, dest, fmt.Sprintf("Wired up %s", plural(added, "pin")), failed)
}

func (a *App) handleDeletePin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	projectID, err := a.store.DeletePinAssignment(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/projects/%d#pins", projectID), "Removed", "")
}

// --- sub-assemblies ---------------------------------------------------------

func (a *App) handleAddSubAssembly(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)

	sub := optionalID(r.FormValue("sub_project_id"))
	if sub == nil {
		redirect(w, r, dest, "", "pick a project to use as a sub-assembly")
		return
	}
	qty, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("quantity")))
	if err := a.store.AddSubAssembly(id, *sub, qty, strings.TrimSpace(r.FormValue("note"))); err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "added a sub-assembly", "project", id, "")
	redirect(w, r, dest, "Added as a sub-assembly", "")
}

// --- documentation ----------------------------------------------------------

// handleFindDocs goes looking for the paperwork: datasheets, pinouts, manuals,
// and a photo if the part has none. Pinouts come back as stored images, so they
// are on the page rather than behind a link.
func (a *App) handleFindDocs(w http.ResponseWriter, r *http.Request) {
	it, ok := a.lookup(w, r)
	if !ok {
		return
	}
	dest := fmt.Sprintf("/items/%d", it.ID)

	hunt := a.HuntDocumentation(r.Context(), it)
	summary := hunt.Summary()
	if summary == "" {
		where := "the sources it can reach"
		if len(hunt.Searched) > 0 {
			where = strings.Join(hunt.Searched, ", ")
		}
		note := fmt.Sprintf("nothing new found — looked at %s", where)
		if len(hunt.Notes) > 0 {
			note += ". " + strings.Join(hunt.Notes, "; ")
		}
		if it.Link == "" && it.PartNumber == "" {
			note = "give the item a product link or a part number first — there is nothing to search with"
		}
		redirect(w, r, dest, "", note)
		return
	}
	a.store.Record(a.actor(r), "fetched documentation", "item", it.ID, summary)
	redirect(w, r, dest, summary+" from "+strings.Join(hunt.Searched, ", "),
		strings.Join(hunt.Notes, "; "))
}

// --- footprints -------------------------------------------------------------

// handleSetFootprint records which land pattern a part uses and fetches the
// drawing. The name usually comes straight off a BOM's footprint column.
func (a *App) handleSetFootprint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/items/%d", id)
	name := strings.TrimSpace(r.FormValue("footprint"))

	// An uploaded .kicad_mod covers the parts that are not in the official
	// library -- a module you drew yourself, or a vendor's own file.
	if r.MultipartForm != nil && len(r.MultipartForm.File["file"]) > 0 {
		fh := r.MultipartForm.File["file"][0]
		f, err := fh.Open()
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		body, err := io.ReadAll(io.LimitReader(f, maxFootprintBytes))
		f.Close()
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		if name == "" {
			name = strings.TrimSuffix(fh.Filename, ".kicad_mod")
		}
		fp, err := a.StoreFootprintFile(name, string(body))
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		if err := a.store.SetFootprint(id, name); err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		redirect(w, r, dest, fmt.Sprintf("Footprint %s — %s, %s", fp.Name, fp.Size(), fp.Mounting()), "")
		return
	}

	if name == "" {
		if err := a.store.SetFootprint(id, ""); err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		redirect(w, r, dest, "Footprint cleared", "")
		return
	}

	fp, err := a.FetchFootprint(r.Context(), name)
	if err != nil {
		// The name is still worth keeping: the drawing can be retried later,
		// and a BOM's footprint column is information even when undrawable.
		if serr := a.store.SetFootprint(id, name); serr != nil {
			a.fail(w, serr, http.StatusInternalServerError)
			return
		}
		redirect(w, r, dest, "Footprint name saved", err.Error())
		return
	}
	if err := a.store.SetFootprint(id, name); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "set a footprint", "item", id, name)
	redirect(w, r, dest, fmt.Sprintf("%s — %d pads, %s, %s",
		fp.Name, fp.PadCount(), fp.Size(), fp.Mounting()), "")
}

// handleWiringCSV exports the pin map, which is the one thing from a project
// you want on paper next to the board.
func (a *App) handleWiringCSV(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetProject(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pins, err := a.store.PinAssignments(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-wiring.csv"`, slugify(p.Name)))

	cw := csv.NewWriter(w)
	defer cw.Flush()
	controller := ""
	if p.Controller != nil {
		controller = p.Controller.Name
	}
	// The wire colour is in the sheet as well as the diagram, so a printout and
	// the picture on screen tell you to reach for the same reel.
	cw.Write([]string{"pin", "on", "goes to", "signal", "wire colour", "note", "conflict"})
	for _, pin := range pins {
		cw.Write([]string{pin.Pin, controller, pin.Where(), pin.Signal,
			wireColourName(pin.Pin, pin.Signal), pin.Note, strings.Join(pin.Clashes, "; ")})
	}
}
