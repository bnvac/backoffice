package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The printing page. Two halves: the machines you own, recorded properly, and
// what that means you can actually load into them. The second half is the
// point — "can I print this in nylon" is a question nobody can answer from a
// bed size alone.

type printingData struct {
	Printers []printerCard
	Rows     []materialRow
	Presets  []Preset
	Nozzles  []struct{ Key, Label string }
	Links    []struct{ Key, Label, Hint string }
	Edit     *Printer
	Caps     Capabilities
	Jobs     []PrintJob
	Items    []Item
	Limit    string
	Spools   []Spool
}

// printerCard is a printer with what it said when asked just now.
type printerCard struct {
	Printer
	Status    PrintStatus
	StatusErr string
	Asked     bool
	Reported  float64 // the printer's lifetime print seconds, when it keeps count
	Spool     *Spool  // what is loaded on it
	Due       int     // maintenance jobs due
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

	// This page is about the printers, so it asks them before rendering — but a
	// switched-off printer should cost a couple of seconds, not four.
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	cards := askPrinters(ctx, printers)
	a.keepCounters(cards)
	due := a.store.MaintDue(printers)
	spools, _ := a.store.ListSpools(false)
	for i := range cards {
		cards[i].Due = due[cards[i].ID]
		for _, sp := range spools {
			if sp.PrinterRef() == cards[i].ID {
				sp := sp
				cards[i].Spool = &sp
			}
		}
	}
	data := printingData{
		Printers: cards,
		Spools:   spools,
		Presets:  PrinterPresets,
		Nozzles:  NozzleKinds,
		Links:    LinkKinds,
		Caps:     SummariseWorkshop(tools, printers),
		Limit:    a.gcode.Limit(),
	}
	if data.Jobs, err = a.store.RecentPrintJobs(0, 12); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	if data.Items, err = a.store.ListItems(Query{Sort: "name"}); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
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
	link := r.FormValue("link_kind")
	if link != "moonraker" && link != "octoprint" {
		link = ""
	}
	return Printer{
		LinkKind:    link,
		LinkURL:     strings.TrimSpace(r.FormValue("link_url")),
		LinkKey:     strings.TrimSpace(r.FormValue("link_key")),
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
		if p.LinkKind == "" {
			p.LinkKind = preset.LinkKind
		}
	}

	if p.Name == "" {
		redirect(w, r, "/printing", "", "give the printer a name")
		return
	}
	id, err := a.store.AddPrinter(p)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "added a printer", "printer", id, p.Name)

	// A preset that knows how to talk to the machine still needs its address,
	// and there is no way to guess one, so go straight to the form for it.
	if p.LinkKind != "" && p.LinkURL == "" {
		redirect(w, r, fmt.Sprintf("/printing?edit=%d#edit", id),
			"Added "+p.Name+" — give it the printer's address to connect it", "")
		return
	}
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

	// The key is never sent back to the browser, so a blank box means "keep
	// what is saved", and clearing it is a deliberate tick.
	if old, err := a.store.GetPrinter(id); err == nil && p.LinkKey == "" && r.FormValue("clear_key") == "" {
		p.LinkKey = old.LinkKey
	}
	if p.LinkKind != "" && p.LinkURL != "" {
		if _, err := normaliseLinkURL(p.LinkURL); err != nil {
			redirect(w, r, fmt.Sprintf("/printing?edit=%d#edit", id), "", err.Error())
			return
		}
	}
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

// askPrinters gets every connected printer's status at once. Each has the
// status client's short timeout, and they run side by side, so a printer that
// is switched off costs the page a few seconds once rather than per printer.
func askPrinters(ctx context.Context, printers []Printer) []printerCard {
	cards := make([]printerCard, len(printers))
	var wg sync.WaitGroup
	for i, p := range printers {
		cards[i].Printer = p
		link, ok := linkFor(p)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(c *printerCard, link PrinterLink) {
			defer wg.Done()
			st, err := link.Status(ctx)
			c.Status, c.Asked = st, true
			if err != nil {
				c.StatusErr = err.Error()
				return
			}
			// Only worth asking for the counter from a printer that answered.
			if pc, ok := link.(PrintCounter); ok {
				if secs, err := pc.PrintSeconds(ctx); err == nil && secs > 0 {
					c.Reported = secs
				}
			}
		}(&cards[i], link)
	}
	wg.Wait()
	return cards
}

// handlePrinterStatus is the same thing as JSON, for the dashboard's live tile
// and the printing page's refresh. Asked for from script only, so the HTML
// pages never wait on a printer that is switched off.
func (a *App) handlePrinterStatus(w http.ResponseWriter, r *http.Request) {
	printers, err := a.store.ListPrinters()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	type out struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		State    string `json:"state"`
		Headline string `json:"headline"`
		Percent  int    `json:"percent"`
		File     string `json:"file"`
		Error    string `json:"error,omitempty"`
	}
	list := []out{}
	for _, c := range askPrinters(r.Context(), printers) {
		if !c.Asked {
			continue
		}
		list = append(list, out{
			ID: c.ID, Name: c.Name, State: c.Status.State, Headline: c.Status.Headline(),
			Percent: c.Status.Percent(), File: c.Status.File, Error: c.StatusErr,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(list)
}

// sendWindow is how long one send may take end to end. The server's write
// timeout is two minutes, which a large file going over a printer's Wi-Fi can
// outrun, so a send extends its own deadline rather than every request
// getting a longer one.
const sendWindow = 20 * time.Minute

func extendDeadline(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(sendWindow)
	// Not every ResponseWriter supports this (the test recorder does not), and
	// a send that cannot extend its deadline should still be tried.
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

// keepCounters stores any lifetime counters a printer just reported, so the
// maintenance list works on a day the printer is switched off.
func (a *App) keepCounters(cards []printerCard) {
	for _, c := range cards {
		if c.Reported > 0 && c.Reported != c.ReportedSeconds {
			a.store.RecordReportedSeconds(c.ID, c.Reported)
		}
	}
}

// gcodeMeta reads what the slicer wrote into a kept file.
func (a *App) gcodeMeta(blob string) GcodeMeta {
	f, err := a.gcode.Open(blob)
	if err != nil {
		return GcodeMeta{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return GcodeMeta{}
	}
	return ReadGcodeMeta(f, info.Size())
}

// handleSendGcode takes a sliced file, keeps it, and sends it to the printer.
// It is kept first, so a send that fails still leaves the file here to resend
// once the printer is back.
func (a *App) handleSendGcode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	extendDeadline(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxGcode+(1<<20))
	if err := parseForm(r); err != nil {
		redirect(w, r, "/printing", "", "that file is bigger than "+a.gcode.Limit())
		return
	}

	p, err := a.store.GetPrinter(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, header, err := r.FormFile("gcode")
	if err != nil {
		redirect(w, r, "/printing", "", "choose a G-code file to send")
		return
	}
	defer file.Close()
	if !IsGcode(header.Filename) {
		redirect(w, r, "/printing", "", notGcodeHint(header.Filename))
		return
	}

	name := GcodeName(header.Filename)
	blob, sum, size, err := a.gcode.Save(file, path.Ext(name))
	if errors.Is(err, errTooBig) {
		redirect(w, r, "/printing", "", "that file is bigger than "+a.gcode.Limit())
		return
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	job := PrintJob{PrinterID: id, ItemID: optionalID(r.FormValue("item_id")),
		Name: name, Blob: blob, SHA256: sum, Size: size}
	a.sendJob(w, r, p, job, r.FormValue("start") != "", r.FormValue("spool_id"))
}

// handleResendJob sends a kept file again, to the same printer or another.
func (a *App) handleResendJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	extendDeadline(w)
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	old, err := a.store.GetPrintJob(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	printerID := old.PrinterID
	if to := optionalID(r.FormValue("printer_id")); to != nil {
		printerID = *to
	}
	p, err := a.store.GetPrinter(printerID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job := PrintJob{PrinterID: printerID, ItemID: old.ItemID, Name: old.Name, Blob: old.Blob,
		SHA256: old.SHA256, Size: old.Size}
	a.sendJob(w, r, p, job, r.FormValue("start") != "", r.FormValue("spool_id"))
}

// chooseSpool resolves the form's choice: blank is "work it out", "none" is
// "do not track this one", and a number is that spool.
func (a *App) chooseSpool(choice string, printerID int64, meta GcodeMeta) (*Spool, string) {
	switch choice {
	case "none":
		return nil, ""
	case "":
		spools, err := a.store.ListSpools(false)
		if err != nil {
			return nil, ""
		}
		if m := PickSpool(spools, printerID, meta); m.Found {
			return &m.Spool, m.Reason
		}
		return nil, ""
	}
	id := optionalID(choice)
	if id == nil {
		return nil, ""
	}
	sp, err := a.store.GetSpool(*id)
	if err != nil {
		return nil, ""
	}
	return &sp, "chosen"
}

// sendJob does the upload, takes the filament it will use off the spool, and
// records how it went either way. Warnings — not enough left, sliced for a
// different plastic — do not stop the send: the person at the printer may know
// better than the numbers, and they are told rather than overruled.
func (a *App) sendJob(w http.ResponseWriter, r *http.Request, p Printer, job PrintJob, start bool, spoolChoice string) {
	meta := a.gcodeMeta(job.Blob)
	job.EstSeconds = meta.Seconds

	spool, _ := a.chooseSpool(spoolChoice, p.ID, meta)
	var warnings []string
	if spool != nil {
		job.SpoolID = &spool.ID
		job.Grams = meta.GramsFor(spool.Info().Density())
		if want := MaterialFromSlicer(meta.Material); want != "" && want != spool.Material {
			wantName := want
			if m, ok := MaterialByKey(want); ok {
				wantName = m.Name
			}
			warnings = append(warnings, fmt.Sprintf("it was sliced for %s, but %s is %s",
				wantName, spool.Label(), spool.Info().Name))
		}
		if job.Grams > spool.Remaining() {
			warnings = append(warnings, fmt.Sprintf("it needs about %.0f g and %s has %.0f g left",
				job.Grams, spool.Label(), spool.Remaining()))
		}
		if note := spool.DryNote(); note != "" {
			warnings = append(warnings, spool.Label()+": "+note)
		}
		// The spool the grams come off is not necessarily the one in the
		// extruder. If something else is loaded, the print is about to run
		// through the wrong filament — worth saying loudest when it starts now.
		if loaded, ok := a.store.LoadedSpool(p.ID); ok && loaded.ID != spool.ID {
			warnings = append(warnings, fmt.Sprintf("%s is what is loaded on %s, not %s — swap it before the print starts",
				loaded.Label(), p.Name, spool.Label()))
		}
	}

	link, ok := linkFor(p)
	if !ok {
		job.Error = "not connected — add the printer's address first"
		a.store.AddPrintJob(job)
		redirect(w, r, fmt.Sprintf("/printing?edit=%d#edit", p.ID), "",
			p.Name+" is not connected yet, so the file is kept here but was not sent")
		return
	}

	f, err := a.gcode.Open(job.Blob)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(r.Context(), sendWindow)
	defer cancel()
	started, err := link.Upload(ctx, job.Name, f, job.Size, start)
	job.Started = started
	if err != nil {
		job.Error = err.Error()
	}
	if _, rerr := a.store.AddPrintJob(job); rerr != nil {
		a.fail(w, rerr, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "sent a print", "printer", p.ID, job.Name)

	if err != nil {
		redirect(w, r, "/printing", "", "Could not send "+job.Name+": "+err.Error())
		return
	}

	// The file arrived, so the filament is spoken for. If it never prints,
	// the job list has a button to put it back.
	used := ""
	if spool != nil && job.Grams > 0 {
		a.store.UseFilament(spool.ID, job.Grams)
		left := spool.Remaining() - job.Grams
		used = fmt.Sprintf(" · %.0f g from %s, %.0f g left", job.Grams, spool.Label(), math.Max(0, left))
	}
	var msg string
	switch {
	case started:
		msg = "Sent " + job.Name + " to " + p.Name + " — printing"
	case start:
		// Asked to start, file arrived, printer did not say it started. Usually
		// it is already busy; say so rather than claim a print is running.
		msg = "Sent " + job.Name + " to " + p.Name +
			", but it did not start — it may already be printing. Start it from the printer."
	default:
		msg = "Sent " + job.Name + " to " + p.Name + " — start it when the bed is ready"
	}
	warn := ""
	if len(warnings) > 0 {
		warn = "Heads up: " + strings.Join(warnings, "; ") + "."
	}
	redirect(w, r, "/printing", msg+used, warn)
}

// handleReturnFilament is "that one didn't print": the grams go back on the
// spool and the print stops counting towards maintenance hours.
func (a *App) handleReturnFilament(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	j, err := a.store.ReturnJobFilament(id)
	if err != nil {
		redirect(w, r, "/printing", "", err.Error())
		return
	}
	msg := "Marked " + j.Name + " as not printed"
	if j.GivesBack() {
		msg += fmt.Sprintf(" — %.0f g back on the spool", j.Grams)
	}
	redirect(w, r, "/printing", msg, "")
}

// handleJobFile downloads a kept G-code file, under the name it was sliced as.
func (a *App) handleJobFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	job, err := a.store.GetPrintJob(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.serveBlob(w, r, a.gcode, job.Blob, job.Name, "text/x.gcode")
}

// serveBlob sends a stored file as a download. The name in the header is the
// original one, quoted; the bytes come from the hash-named copy.
func (a *App) serveBlob(w http.ResponseWriter, r *http.Request, b *Blobs, blob, name, ctype string) {
	p, ok := b.Path(blob)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(name)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, p)
}
