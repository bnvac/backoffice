package app

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type filamentData struct {
	Spools    []Spool
	All       int // before filtering, so "nothing matches" reads differently from "nothing yet"
	Summary   FilamentSummary
	Materials []Material
	Families  []familyChip
	MatChips  []matChip
	Colour    string // the colour family being shown, if any
	Material  string
	ShowEmpty bool
	Presets   []struct{ Name, Hex string }
	Printers  []Printer
}

type familyChip struct {
	Key, Swatch string
	Count       int
	On          bool
}

type matChip struct {
	Key, Name string
	Count     int
	On        bool
}

// handleFilament is the shelf of spools, filterable by colour family and by
// material — which together are how anybody actually looks for one.
func (a *App) handleFilament(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	showEmpty := q.Get("empty") == "1"
	all, err := a.store.ListSpools(showEmpty)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	printers, _ := a.store.ListPrinters()

	colour, material := q.Get("colour"), q.Get("material")
	data := filamentData{
		All:       len(all),
		Summary:   SummariseFilament(all),
		Materials: Materials,
		Colour:    colour,
		Material:  material,
		ShowEmpty: showEmpty,
		Presets:   ColourPresets,
		Printers:  printers,
	}

	// Chips show only what is on the shelf, with counts, and each is counted
	// against the other filter so "red" shows how many red spools are PETG
	// once PETG is picked.
	famCount, matCount := map[string]int{}, map[string]int{}
	for _, s := range all {
		if material == "" || s.Material == material {
			famCount[s.Family()]++
		}
		if colour == "" || s.Family() == colour {
			matCount[s.Material]++
		}
		if (colour == "" || s.Family() == colour) && (material == "" || s.Material == material) {
			data.Spools = append(data.Spools, s)
		}
	}
	for _, f := range ColourFamilies {
		if famCount[f.Key] > 0 || f.Key == colour {
			data.Families = append(data.Families, familyChip{f.Key, f.Swatch, famCount[f.Key], f.Key == colour})
		}
	}
	for _, m := range Materials {
		if matCount[m.Key] > 0 || m.Key == material {
			data.MatChips = append(data.MatChips, matChip{m.Key, m.Name, matCount[m.Key], m.Key == material})
		}
	}
	a.render(w, r, "filament.html", "Filament", data)
}

// FilterURL builds the filament page link with one filter changed, the other
// kept, and the same value clearing it — every chip is a toggle.
func (d filamentData) FilterURL(key, value string) string {
	v := url.Values{}
	if d.Colour != "" {
		v.Set("colour", d.Colour)
	}
	if d.Material != "" {
		v.Set("material", d.Material)
	}
	if d.ShowEmpty {
		v.Set("empty", "1")
	}
	if v.Get(key) == value {
		v.Del(key)
	} else {
		v.Set(key, value)
	}
	if len(v) == 0 {
		return "/filament"
	}
	return "/filament?" + v.Encode()
}

func spoolFromForm(r *http.Request) Spool {
	num := func(name string, def float64) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue(name)), 64)
		if err != nil {
			return def
		}
		return v
	}
	// A swatch button and the free colour picker both post a colour; the
	// swatch wins because clicking one is the more deliberate act.
	hex := r.FormValue("swatch")
	if hex == "" {
		hex = r.FormValue("colour_hex")
	}
	name := strings.TrimSpace(r.FormValue("colour_name"))
	if name == "" {
		for _, p := range ColourPresets {
			if p.Hex == normaliseHex(hex) {
				name = p.Name
			}
		}
	}
	return Spool{
		Material:   r.FormValue("material"),
		Brand:      r.FormValue("brand"),
		ColourName: name,
		ColourHex:  hex,
		Diameter:   num("diameter", 1.75),
		NetGrams:   num("net_grams", 1000),
		TareGrams:  num("tare_grams", 0),
		Location:   r.FormValue("location"),
		Notes:      r.FormValue("notes"),
	}
}

func (a *App) handleAddSpool(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	sp := spoolFromForm(r)

	// A box of four of the same is four spools: each is used up on its own.
	count, _ := strconv.Atoi(r.FormValue("count"))
	if count < 1 {
		count = 1
	}
	if count > 20 {
		count = 20
	}
	var lastID int64
	for i := 0; i < count; i++ {
		id, err := a.store.AddSpool(sp)
		if err != nil {
			redirect(w, r, "/filament#add", "", err.Error())
			return
		}
		lastID = id
	}
	a.store.Record(a.actor(r), "added filament", "spool", lastID, sp.Label())
	msg := "Added " + sp.Label()
	if count > 1 {
		msg = fmt.Sprintf("Added %d spools of %s", count, sp.Label())
	}
	redirect(w, r, "/filament", msg, "")
}

func (a *App) handleSpool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sp, err := a.store.GetSpool(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobs, _ := a.store.PrintJobs(JobFilter{Spool: id}, 20)
	printers, _ := a.store.ListPrinters()
	a.render(w, r, "spool.html", sp.Label(), struct {
		Spool     Spool
		Jobs      []PrintJob
		Printers  []Printer
		Materials []Material
		Presets   []struct{ Name, Hex string }
		Edit      bool
	}{sp, jobs, printers, Materials, ColourPresets, r.URL.Query().Get("edit") == "1"})
}

func (a *App) handleUpdateSpool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	sp := spoolFromForm(r)
	sp.ID = id
	if err := a.store.UpdateSpool(sp); err != nil {
		redirect(w, r, fmt.Sprintf("/filament/%d?edit=1", id), "", err.Error())
		return
	}
	redirect(w, r, fmt.Sprintf("/filament/%d", id), "Saved", "")
}

// handleSpoolAction covers the one-button jobs on a spool: weigh it, load it,
// dry it, finish it, delete it. They all come back to wherever they were
// pressed from, which is either the spool's page or the list.
func (a *App) handleSpoolAction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	back := safeNext(r.FormValue("return"))
	if back == "/" {
		back = fmt.Sprintf("/filament/%d", id)
	}
	sp, err := a.store.GetSpool(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch r.PathValue("action") {
	case "weigh":
		gross, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("gross")), 64)
		got, err := a.store.WeighSpool(id, gross)
		if err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		redirect(w, r, back, fmt.Sprintf("%s: %.0f g left", got.Label(), got.Remaining()), "")
	case "load":
		target := optionalID(r.FormValue("printer_id"))
		if err := a.store.LoadSpool(id, target); err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		if target == nil {
			redirect(w, r, back, "Took "+sp.Label()+" off the printer", "")
			return
		}
		p, _ := a.store.GetPrinter(*target)
		redirect(w, r, back, "Loaded "+sp.Label()+" on "+p.Name, "")
	case "dried":
		a.store.MarkSpoolDried(id)
		redirect(w, r, back, sp.Label()+" marked as dried just now", "")
	case "finish":
		a.store.FinishSpool(id)
		redirect(w, r, "/filament", sp.Label()+" is used up — it is kept, so its prints still say what they were made in", "")
	case "delete":
		a.store.DeleteSpool(id)
		redirect(w, r, "/filament", "Deleted "+sp.Label(), "")
	default:
		http.NotFound(w, r)
	}
}
