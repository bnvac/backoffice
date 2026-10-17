package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// One printer's own page: what it is doing, what is loaded, how many hours it
// has done, what is due, and the spares kept for it. The printing page is the
// overview; this is where you go to look after one machine.

type printerPage struct {
	Card    printerCard
	Hours   Hours
	Tasks   []TaskState
	Due     int
	History []MaintLog
	Presets []MaintTask
	Spares  []Item
	Items   []Item
	Spools  []Spool
	Jobs    []PrintJob
}

func (a *App) handlePrinter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetPrinter(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	cards := askPrinters(ctx, []Printer{p})
	a.keepCounters(cards)
	if cards[0].Reported > 0 {
		p.ReportedSeconds = cards[0].Reported
	}

	hours, err := a.store.PrinterHours(p)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tasks, err := a.store.ListMaint(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	data := printerPage{Card: cards[0], Hours: hours, Presets: MaintPresets}
	now := time.Now()
	for _, t := range tasks {
		st := t.State(hours.Total, now)
		if st.Due {
			data.Due++
		}
		data.Tasks = append(data.Tasks, st)
	}
	data.History, _ = a.store.MaintHistory(id, 8)
	data.Spares, _ = a.store.Spares(id)
	data.Items, _ = a.store.ListItems(Query{Sort: "name"})
	data.Spools, _ = a.store.ListSpools(false)
	data.Jobs, _ = a.store.PrintJobs(JobFilter{Printer: id}, 10)
	if sp, ok := a.store.LoadedSpool(id); ok {
		data.Card.Spool = &sp
	}
	a.render(w, r, "printer.html", p.Name, data)
}

// hoursFor is the printer's running total right now, for stamping a task.
func (a *App) hoursFor(printerID int64) (float64, error) {
	p, err := a.store.GetPrinter(printerID)
	if err != nil {
		return 0, err
	}
	h, err := a.store.PrinterHours(p)
	return h.Total, err
}

func (a *App) handleAddMaint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	back := fmt.Sprintf("/printing/printers/%d#maintenance", id)
	hours, err := a.hoursFor(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// A preset button posts its index; the form posts its fields.
	var t MaintTask
	if i, err := strconv.Atoi(r.FormValue("preset")); err == nil && i >= 0 && i < len(MaintPresets) {
		t = MaintPresets[i]
	} else {
		every, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("every_hours")), 64)
		days, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("every_days")))
		t = MaintTask{Task: r.FormValue("task"), EveryHours: every, EveryDays: days, Notes: r.FormValue("notes")}
	}
	t.PrinterID = id
	if _, err := a.store.AddMaint(t, hours); err != nil {
		redirect(w, r, back, "", err.Error())
		return
	}
	redirect(w, r, back, "Added “"+strings.TrimSpace(t.Task)+"” — the clock starts now", "")
}

func (a *App) handleDoneMaint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	printerID, err := a.store.MaintPrinter(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	hours, err := a.hoursFor(printerID)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	if _, err = a.store.DoneMaint(id, hours, r.FormValue("note")); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "did maintenance", "printer", printerID, "")
	redirect(w, r, fmt.Sprintf("/printing/printers/%d#maintenance", printerID), "Done — the clock starts again", "")
}

func (a *App) handleDeleteMaint(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	printerID, err := a.store.DeleteMaint(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	redirect(w, r, fmt.Sprintf("/printing/printers/%d#maintenance", printerID), "Removed that job", "")
}

func (a *App) handleBaseHours(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	h, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("base_hours")), 64)
	if err != nil || h < 0 {
		redirect(w, r, fmt.Sprintf("/printing/printers/%d#maintenance", id), "", "enter the hours as a number")
		return
	}
	a.store.SetBaseHours(id, h)
	redirect(w, r, fmt.Sprintf("/printing/printers/%d#maintenance", id), "Saved", "")
}

func (a *App) handleSpare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	item := optionalID(r.FormValue("item_id"))
	back := fmt.Sprintf("/printing/printers/%d#spares", id)
	if item == nil {
		redirect(w, r, back, "", "pick a part from the shelf")
		return
	}
	if r.FormValue("remove") != "" {
		a.store.RemoveSpare(id, *item)
		redirect(w, r, back, "", "")
		return
	}
	if err := a.store.AddSpare(id, *item); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, back, "", "")
}
