package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- pairing ---------------------------------------------------------------------

type pairData struct {
	Scanned  bool
	Found    []Found
	Checked  string // an address someone typed that did not answer
	Printers []Printer
	Presets  []Preset
	Next     string
	Subnets  []string
}

func (a *App) pairPage(w http.ResponseWriter, r *http.Request, d pairData) {
	d.Printers, _ = a.store.ListPrinters()
	d.Presets = PrinterPresets
	d.Next = safeNextOrEmpty(r.FormValue("next"))
	for _, p := range localSubnets() {
		d.Subnets = append(d.Subnets, p.String())
	}
	// Mark what is already paired, so the list does not offer it twice.
	for i, f := range d.Found {
		for _, p := range d.Printers {
			if base, err := normaliseLinkURL(p.LinkURL); err == nil && strings.EqualFold(base, f.Address) {
				d.Found[i].PairedTo = p.Name
			}
		}
	}
	a.render(w, r, "pair.html", "Find your printer", d)
}

func safeNextOrEmpty(next string) string {
	if n := safeNext(next); n != "/" {
		return n
	}
	return ""
}

func (a *App) handlePair(w http.ResponseWriter, r *http.Request) {
	a.pairPage(w, r, pairData{})
}

// handlePairScan looks for Klipper printers on this machine's own networks.
func (a *App) handlePairScan(w http.ResponseWriter, r *http.Request) {
	parseForm(r)
	var hosts []string
	for _, p := range localSubnets() {
		hosts = append(hosts, hostsIn(p)...)
	}
	found := scanForMoonraker(r.Context(), hosts, moonrakerPort)
	a.pairPage(w, r, pairData{Scanned: true, Found: found})
}

// handlePairCheck tries one typed address — the one on the printer's screen
// under Settings → Network.
func (a *App) handlePairCheck(w http.ResponseWriter, r *http.Request) {
	parseForm(r)
	addr := strings.TrimSpace(r.FormValue("address"))
	if f, ok := checkAddress(r.Context(), addr); ok {
		a.pairPage(w, r, pairData{Scanned: true, Found: []Found{f}})
		return
	}
	a.pairPage(w, r, pairData{Scanned: true, Checked: addr})
}

// handlePairConfirm connects a found printer to one recorded here, or records
// a new one from a preset. From then on its history is read every minute.
func (a *App) handlePairConfirm(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	addr, err := normaliseLinkURL(r.FormValue("address"))
	if err != nil {
		redirect(w, r, "/printing/pair", "", "that address did not read right — scan again")
		return
	}
	// Only an address that really answers as Moonraker is paired, so a
	// mistyped one is caught here rather than on the first print.
	f, ok := probeMoonraker(r.Context(), addr)
	if !ok {
		redirect(w, r, "/printing/pair", "", "nothing answered at "+addr+" just now — is the printer on?")
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if f.State == "needs its API key" && key == "" {
		redirect(w, r, "/printing/pair", "", "that printer wants its API key — paste it in and pair again")
		return
	}

	var id int64
	target := r.FormValue("printer")
	switch {
	case strings.HasPrefix(target, "preset:"):
		preset, ok := PresetByName(strings.TrimPrefix(target, "preset:"))
		if !ok {
			redirect(w, r, "/printing/pair", "", "pick which printer it is")
			return
		}
		p := preset
		p.LinkKind, p.LinkURL, p.LinkKey = "moonraker", addr, key
		if id, err = a.store.AddPrinter(p); err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		a.store.Record(a.actor(r), "added a printer", "printer", id, p.Name)
	default:
		pid := optionalID(target)
		if pid == nil {
			redirect(w, r, "/printing/pair", "", "pick which printer it is")
			return
		}
		id = *pid
		if err := a.store.SetPrinterLink(id, "moonraker", addr, key); err != nil {
			redirect(w, r, "/printing/pair", "", err.Error())
			return
		}
	}
	p, err := a.store.GetPrinter(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.store.Record(a.actor(r), "paired a printer", "printer", id, addr)
	// The first read sets where its history starts: now, or the start of the
	// print already running.
	a.syncMu.Lock()
	a.syncPrinter(r.Context(), p)
	a.syncMu.Unlock()

	msg := fmt.Sprintf("Paired %s at %s — every print from now on is counted, however it is started", p.Name, addr)
	if next := safeNextOrEmpty(r.FormValue("next")); next != "" {
		redirect(w, r, next, msg, "")
		return
	}
	redirect(w, r, fmt.Sprintf("/printing/printers/%d", id), msg, "")
}

func (s *Store) SetPrinterLink(id int64, kind, url, key string) error {
	res, err := s.db.Exec(`UPDATE printers SET link_kind = ?, link_url = ?,
		link_key = CASE WHEN ? = '' THEN link_key ELSE ? END, history_since = 0 WHERE id = ?`,
		kind, url, key, key, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("that printer is not here any more")
	}
	return nil
}

// --- reading the history --------------------------------------------------------------

// handleSyncPrinter reads one printer's history now, rather than waiting for
// the minute to come round.
func (a *App) handleSyncPrinter(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetPrinter(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	dest := fmt.Sprintf("/printing/printers/%d", id)
	a.syncMu.Lock()
	results, err := a.syncPrinter(r.Context(), p)
	a.syncMu.Unlock()
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	for _, res := range results {
		a.notifyPrint(r.Context(), res, a.linkTo(r, dest))
	}
	switch len(results) {
	case 0:
		redirect(w, r, dest, "Up to date — nothing new has finished", "")
	case 1:
		redirect(w, r, dest, results[0].Headline()+": "+results[0].Body(), "")
	default:
		redirect(w, r, dest, fmt.Sprintf("%d prints read from %s's history", len(results), p.Name), "")
	}
}

func (a *App) linkTo(r *http.Request, p string) string {
	base := a.cfg.BaseURL
	if base == "" && r != nil {
		base = a.BaseURL(r)
	}
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + p
}

// watch reads every connected printer's history once a minute, and tells
// your phone about anything that finished. It is what makes prints started
// from the slicer count, and it runs whether or not a page is open.
func (a *App) watch(ctx context.Context) {
	first := time.NewTimer(15 * time.Second)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-tick.C:
		}
		a.watchOnce(ctx)
	}
}

func (a *App) watchOnce(ctx context.Context) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	printers, err := a.store.ListPrinters()
	if err != nil {
		return
	}
	for _, p := range printers {
		if p.LinkKind != "moonraker" || p.LinkURL == "" {
			continue
		}
		results, err := a.syncPrinter(ctx, p)
		if err != nil {
			continue // switched off; the status card already says so
		}
		for _, res := range results {
			log.Printf("printer: %s — %s", res.Headline(), res.Body())
			a.notifyPrint(ctx, res, a.linkTo(nil, fmt.Sprintf("/printing/printers/%d", p.ID)))
		}
	}
	a.checkSensorAlarms(ctx)
}

// checkSensorAlarms sends each sensor's alarm once, when it starts, and
// clears it when the readings come back in range.
func (a *App) checkSensorAlarms(ctx context.Context) {
	sensors, err := a.store.ListSensors()
	if err != nil {
		return
	}
	for _, s := range sensors {
		a.sensorAlarm(ctx, s)
	}
}

func (a *App) sensorAlarm(ctx context.Context, s Sensor) {
	alerts := s.Alerts()
	switch {
	case len(alerts) > 0 && !s.Alerted:
		a.store.SetSensorAlerted(s.ID, true)
		if a.store.NotifySettings().Sensors {
			a.notify(ctx, Note{Title: s.Name, Body: strings.Join(alerts, "; "), Tag: "warning",
				Link: a.linkTo(nil, fmt.Sprintf("/sensors/%d", s.ID))})
		}
	case len(alerts) == 0 && s.Alerted:
		a.store.SetSensorAlerted(s.ID, false)
	}
}

func (s *Store) SetSensorAlerted(id int64, on bool) error {
	_, err := s.db.Exec(`UPDATE sensors SET alerted = ? WHERE id = ?`, on, id)
	return err
}

// --- previews ----------------------------------------------------------------------

// liveThumbs keeps the preview of what each printer is printing, so the
// dashboard asking every few seconds does not ask the printer every time.
type liveThumbs struct {
	mu sync.Mutex
	m  map[int64]liveThumb
}

type liveThumb struct {
	file string
	png  []byte
	at   time.Time
}

func (l *liveThumbs) get(id int64, file string) ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.m[id]
	if !ok || t.file != file || (t.png == nil && time.Since(t.at) > time.Minute) {
		return nil, false
	}
	return t.png, true
}

func (l *liveThumbs) put(id int64, file string, png []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[int64]liveThumb{}
	}
	l.m[id] = liveThumb{file, png, time.Now()}
}

// handleLiveThumb is the preview of the file a printer is on now.
func (a *App) handleLiveThumb(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetPrinter(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file := r.URL.Query().Get("file")
	png, cached := a.live.get(id, file)
	if !cached {
		if link, ok := linkFor(p); ok {
			if hr, ok := link.(HistoryReader); ok && file != "" {
				png, _ = hr.Thumbnail(r.Context(), file)
			}
		}
		a.live.put(id, file, png)
	}
	servePNG(w, png)
}

// handleJobThumb is the preview kept with a print in the history.
func (a *App) handleJobThumb(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	j, err := a.store.GetPrintJob(id)
	if err != nil || j.Thumb == "" {
		http.NotFound(w, r)
		return
	}
	servePNG(w, a.readThumb(j.Thumb))
}

func servePNG(w http.ResponseWriter, png []byte) {
	if !isPNG(png) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(png)
}

// --- notification settings -------------------------------------------------------------

func (a *App) handleSaveNotify(w http.ResponseWriter, r *http.Request) {
	dest := safeNextOrEmpty(r.FormValue("next"))
	if dest == "" {
		dest = "/printing#notify"
	}
	if !a.canAdminister(r) {
		redirect(w, r, dest, "", "only an administrator can change where notifications go")
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	if raw := strings.TrimSpace(r.FormValue("url")); raw != "" {
		u, err := checkNotifyURL(raw)
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		a.store.SetSetting("notify.url", u)
	}
	for _, k := range []string{"finished", "failed", "sensors"} {
		v := "off"
		if r.FormValue(k) != "" {
			v = "on"
		}
		a.store.SetSetting("notify."+k, v)
	}
	if r.FormValue("test") != "" {
		if err := a.notify(r.Context(), Note{Title: "Backoffice can reach you",
			Body: "This is where finished prints and sensor alarms will turn up.", Tag: "tada"}); err != nil {
			redirect(w, r, dest, "", "Saved, but the test did not arrive: "+err.Error())
			return
		}
		redirect(w, r, dest, "Saved — check your phone for the test", "")
		return
	}
	redirect(w, r, dest, "Saved", "")
}

func (a *App) handleClearNotify(w http.ResponseWriter, r *http.Request) {
	if !a.canAdminister(r) {
		redirect(w, r, "/printing#notify", "", "only an administrator can change where notifications go")
		return
	}
	a.store.SetSetting("notify.url", "")
	a.store.SetSetting("notify.last_error", "")
	redirect(w, r, "/printing#notify", "Notifications are off", "")
}
