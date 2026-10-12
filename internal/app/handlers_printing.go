package app

import (
	"net/http"
	"strconv"
	"strings"
)

// The printing page. Two halves: the machines you own, recorded properly, and
// what that means you can actually load into them. The second half is the
// point — "can I print this in nylon" is a question nobody can answer from a
// bed size alone.

type printingData struct {
	Printers []Printer
	Rows     []materialRow
	Presets  []Preset
	Nozzles  []struct{ Key, Label string }
	Edit     *Printer
	Caps     Capabilities
}

// materialRow is one filament across every machine you own.
type materialRow struct {
	Material Material
	Best     Check // the machine most likely to run it
	Runs     bool  // something you own takes it as it stands
	Fix      Check // the shortest route to running it, when nothing does
	HasFix   bool
}

func (a *App) handlePrinting(w http.ResponseWriter, r *http.Request) {
	printers, err := a.store.ListPrinters()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tools, err := a.store.ListTools()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	data := printingData{
		Printers: printers,
		Presets:  PrinterPresets,
		Nozzles:  NozzleKinds,
		Caps:     SummariseWorkshop(tools, printers),
	}

	// Only worth working out once there is a machine to work it out against.
	if len(printers) > 0 {
		for _, m := range Materials {
			row := materialRow{Material: m}
			if best, ok := BestFor(printers, m); ok {
				row.Best, row.Runs = best, true
			} else if fix, ok := CheapestFix(printers, m); ok {
				row.Fix, row.HasFix = fix, true
				row.Best = fix
			}
			data.Rows = append(data.Rows, row)
		}
	}

	if id := optionalID(r.URL.Query().Get("edit")); id != nil {
		if p, err := a.store.GetPrinter(*id); err == nil {
			data.Edit = &p
		}
	}

	a.render(w, r, "printing.html", "Printing", data)
}

// printerFromForm reads the specification. A blank field means "not said" and
// is stored as zero, which the checker treats as unknown rather than as a
// machine that cannot reach any temperature at all.
func printerFromForm(r *http.Request) Printer {
	num := func(name string) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue(name)), 64)
		if v < 0 {
			return 0
		}
		return v
	}
	whole := func(name string) int {
		v, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
		if v < 0 {
			return 0
		}
		return v
	}
	kind := r.FormValue("nozzle_kind")
	if _, ok := nozzleKindLabels[kind]; !ok {
		kind = "brass"
	}
	return Printer{
		Name:        strings.TrimSpace(r.FormValue("name")),
		Bed:         Volume{X: num("bed_x"), Y: num("bed_y"), Z: num("bed_z")},
		Nozzle:      num("nozzle"),
		NozzleKind:  kind,
		MaxHotend:   whole("max_hotend"),
		MaxBed:      whole("max_bed"),
		Enclosed:    r.FormValue("enclosed") != "",
		DirectDrive: r.FormValue("direct_drive") != "",
		Dryer:       r.FormValue("dryer") != "",
		Notes:       strings.TrimSpace(r.FormValue("notes")),
	}
}

func (a *App) handleAddPrinter(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	p := printerFromForm(r)

	// Picking a preset fills the blanks without overwriting anything typed, so
	// you can choose "Ender 3 V2" and still say your nozzle is 0.6 hardened.
	if preset, ok := PresetByName(r.FormValue("preset")); ok {
		if p.Name == "" {
			p.Name = preset.Name
		}
		if !p.Bed.Known() {
			p.Bed = preset.Bed
		}
		if p.Nozzle == 0 {
			p.Nozzle, p.NozzleKind = preset.Nozzle, preset.NozzleKind
		}
		if p.MaxHotend == 0 {
			p.MaxHotend = preset.MaxHotend
		}
		if p.MaxBed == 0 {
			p.MaxBed = preset.MaxBed
		}
		if r.FormValue("edited") == "" {
			p.Enclosed, p.DirectDrive = preset.Enclosed, preset.DirectDrive
		}
	}

	if p.Name == "" {
		redirect(w, r, "/printing", "", "give the printer a name")
		return
	}
	if _, err := a.store.AddPrinter(p); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "added a printer", "printer", 0, p.Name)
	redirect(w, r, "/printing", "Added "+p.Name, "")
}

func (a *App) handleUpdatePrinter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	p := printerFromForm(r)
	p.ID = id
	if err := a.store.UpdatePrinter(p); err != nil {
		redirect(w, r, "/printing", "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "changed a printer", "printer", id, p.Name)
	redirect(w, r, "/printing", "Saved "+p.Name, "")
}

func (a *App) handleDeletePrinter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := a.store.GetPrinter(id)
	if err := a.store.DeletePrinter(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "removed a printer", "printer", id, p.Name)
	redirect(w, r, "/printing", "Removed "+p.Name, "")
}

// handleMaterial is the long answer for one filament: every machine, what each
// one says, and what to buy. Reached from the grid.
func (a *App) handleMaterial(w http.ResponseWriter, r *http.Request) {
	m, ok := MaterialByKey(r.PathValue("key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	printers, err := a.store.ListPrinters()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	checks := make([]Check, 0, len(printers))
	for _, p := range printers {
		checks = append(checks, CanPrint(p, m))
	}
	a.render(w, r, "material.html", m.Name, struct {
		Material Material
		Checks   []Check
		Printers int
	}{m, checks, len(printers)})
}
