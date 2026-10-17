package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKlipper is a Neptune 4's Moonraker as far as pairing, history, previews
// and layers go.
type fakeKlipper struct {
	mu    sync.Mutex
	host  string
	jobs  []map[string]any
	thumb []byte
	layer [2]int // current, total
	state string
}

func (f *fakeKlipper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/printer/info":
		fmt.Fprintf(w, `{"result":{"state":"ready","hostname":%q,"software_version":"v0.12.0"}}`, f.host)
	case "/server/history/list":
		since, _ := strconv.ParseFloat(r.URL.Query().Get("since"), 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var out []map[string]any
		for _, j := range f.jobs {
			if j["start_time"].(float64) > since {
				out = append(out, j)
			}
		}
		if r.URL.Query().Get("order") == "desc" {
			for i, k := 0, len(out)-1; i < k; i, k = i+1, k-1 {
				out[i], out[k] = out[k], out[i]
			}
		}
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"count": len(out), "jobs": out}})
	case "/server/files/metadata":
		fmt.Fprint(w, `{"result":{"thumbnails":[
			{"width":32,"height":32,"relative_path":".thumbs/x-32x32.png"},
			{"width":300,"height":300,"relative_path":".thumbs/x-300x300.png"}]}}`)
	case "/server/files/gcodes/.thumbs/x-300x300.png":
		w.Write(f.thumb)
	case "/printer/objects/query":
		fmt.Fprintf(w, `{"result":{"status":{"print_stats":{"state":%q,"filename":"knob.gcode","print_duration":600,
			"info":{"current_layer":%d,"total_layer":%d}},"virtual_sdcard":{"progress":0.4},"display_status":{}}}}`,
			f.state, f.layer[0], f.layer[1])
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeKlipper) add(id, file, status string, start, dur, usedMM, plannedMM, plannedG float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, map[string]any{
		"job_id": id, "filename": file, "status": status, "start_time": start, "end_time": start + dur,
		"print_duration": dur, "filament_used": usedMM, "exists": true,
		"metadata": map[string]any{"filament_total": plannedMM, "filament_weight_total": plannedG, "estimated_time": dur},
	})
}

func tinyPNG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{240, 124, 28, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func postForm(t *testing.T, h http.Handler, path string, v url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFindAPrinterOnTheNetwork(t *testing.T) {
	fake := &fakeKlipper{host: "mkspi"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	// Three addresses, one of them the printer.
	found := scanForMoonraker(context.Background(), []string{"127.0.0.2", "127.0.0.1", "127.0.0.3"}, port)
	if len(found) != 1 || found[0].Host != "mkspi" || found[0].Address != srv.URL || found[0].State != "ready" {
		t.Fatalf("found %+v", found)
	}
	// Typed with no scheme.
	if f, ok := checkAddress(context.Background(), u.Host); !ok || f.Host != "mkspi" {
		t.Errorf("checkAddress(%q) = %+v %v", u.Host, f, ok)
	}
	if _, ok := checkAddress(context.Background(), "127.0.0.1:1"); ok {
		t.Error("nothing listens on port 1")
	}
}

func TestHostsInASubnet(t *testing.T) {
	p := localSubnetFrom(t, "192.168.1.77/24")
	hosts := hostsIn(p)
	if len(hosts) != 254 || hosts[0] != "192.168.1.1" || hosts[253] != "192.168.1.254" {
		t.Errorf("hosts = %d, %s … %s", len(hosts), hosts[0], hosts[len(hosts)-1])
	}
}

// Pairing does not reach back into old history: two years of prints should
// not come off today's spools.
func TestPairingStartsCountingFromNow(t *testing.T) {
	app := newTestApp(t)
	fake := &fakeKlipper{host: "mkspi", thumb: tinyPNG(t)}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.add("old1", "benchy.gcode", "completed", float64(time.Now().Add(-48*time.Hour).Unix()), 3600, 4000, 4000, 12)

	rec := postForm(t, app.routes(), "/printing/pair", url.Values{"address": {srv.URL}, "printer": {"preset:Elegoo Neptune 4"}})
	loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(loc, "Paired Elegoo Neptune 4") {
		t.Fatalf("pair: %d %s", rec.Code, loc)
	}
	printers, _ := app.store.ListPrinters()
	if len(printers) != 1 || printers[0].LinkKind != "moonraker" || printers[0].LinkURL != srv.URL || printers[0].HistorySince == 0 {
		t.Fatalf("printer = %+v", printers)
	}
	if printers[0].Bed.X != 225 {
		t.Error("the preset's figures should come with it")
	}
	results, _ := app.syncPrinter(context.Background(), printers[0])
	jobs, _ := app.store.PrintJobs(JobFilter{Printer: printers[0].ID}, 10)
	if len(results) != 0 || len(jobs) != 0 {
		t.Errorf("an old print was imported: %+v", jobs)
	}
}

func TestEveryPrintCountsAndCancelledOnesGiveFilamentBack(t *testing.T) {
	app := newTestApp(t)
	fake := &fakeKlipper{host: "mkspi", thumb: tinyPNG(t)}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	pid, _ := app.store.AddPrinter(Printer{Name: "Neptune 4", LinkKind: "moonraker", LinkURL: srv.URL})
	start := time.Now().Add(-time.Hour)
	app.store.SetHistorySince(pid, float64(start.Add(-time.Minute).Unix()))
	spool, _ := app.store.AddSpool(Spool{Material: "petg", ColourName: "Orange", ColourHex: "#f07c1c", NetGrams: 1000})
	app.store.LoadSpool(spool, &pid)

	// Sent from here: the send took the slicer's 30 g.
	sid := spool
	sent, _ := app.store.AddPrintJob(PrintJob{PrinterID: pid, Name: "lid.gcode", Started: true, SpoolID: &sid,
		Grams: 30, EstSeconds: 5400, SentAt: start.Add(-time.Minute)})
	app.store.UseFilament(spool, 30)

	// The printer says: lid.gcode cancelled a third of the way through; then a
	// phone stand started from the slicer finished; then one still running.
	fake.add("0001", "lid.gcode", "cancelled", float64(start.Unix()), 1200, 3000, 9000, 30)
	fake.add("0002", "phone_stand.gcode", "completed", float64(start.Add(30*time.Minute).Unix()), 2400, 5000, 5000, 15)
	fake.add("0003", "big_box.gcode", "in_progress", float64(start.Add(50*time.Minute).Unix()), 0, 100, 90000, 270)

	p, _ := app.store.GetPrinter(pid)
	results, err := app.syncPrinter(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want the two finished prints, got %d", len(results))
	}

	lid, _ := app.store.GetPrintJob(sent)
	if lid.Outcome != "cancelled" || lid.RemoteID != "0001" || math.Abs(lid.Grams-10) > 0.01 || lid.EstSeconds != 1200 {
		t.Errorf("the sent job should be settled to what the printer did: %+v", lid)
	}
	if lid.CanReturn() {
		t.Error("once the printer has said how it went, there is no 'didn't print' button")
	}
	if results[0].GaveBack < 19.9 || !strings.Contains(results[0].Body(), "20 g back on the spool") {
		t.Errorf("cancelled: %s", results[0].Body())
	}
	if results[0].Thumb == nil || lid.Thumb == "" {
		t.Error("the preview should come with it")
	}

	jobs, _ := app.store.PrintJobs(JobFilter{Printer: pid}, 10)
	var stand *PrintJob
	for i := range jobs {
		if jobs[i].Name == "phone_stand.gcode" {
			stand = &jobs[i]
		}
	}
	if stand == nil || stand.Via != "printer" || stand.Outcome != "completed" || stand.Grams != 15 || stand.SpoolRef() != spool {
		t.Fatalf("a print started from the slicer should be counted against the loaded spool: %+v", stand)
	}

	// 1000 − 30 (sent) + 20 (back) − 15 (the stand) = 975.
	sp, _ := app.store.GetSpool(spool)
	if math.Abs(sp.Remaining()-975) > 0.01 {
		t.Errorf("spool has %.2f g, want 975", sp.Remaining())
	}

	// Reading again changes nothing, and the running one is still to come.
	p, _ = app.store.GetPrinter(pid)
	if again, _ := app.syncPrinter(context.Background(), p); len(again) != 0 {
		t.Errorf("read twice: %d new", len(again))
	}
	if p.HistorySince >= float64(start.Add(50*time.Minute).Unix()) {
		t.Error("the mark must not pass a print still running")
	}

	// When it finishes, it is picked up.
	fake.mu.Lock()
	fake.jobs[2]["status"], fake.jobs[2]["filament_used"], fake.jobs[2]["print_duration"] = "completed", 90000.0, 7200.0
	fake.mu.Unlock()
	p, _ = app.store.GetPrinter(pid)
	late, _ := app.syncPrinter(context.Background(), p)
	if len(late) != 1 || late[0].Job.Name != "big_box.gcode" || math.Abs(late[0].Grams-270) > 0.01 {
		t.Errorf("the long print, once done: %+v", late)
	}
}

// Finished and failed prints reach the phone, picture attached.
func TestNotificationsGoToNtfyWithThePicture(t *testing.T) {
	type got struct {
		method, title, msg, tags, prio, file string
		body                                 []byte
	}
	var mu sync.Mutex
	var sent []got
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q := r.URL.Query()
		mu.Lock()
		sent = append(sent, got{r.Method, q.Get("title"), q.Get("message"), q.Get("tags"), q.Get("priority"), q.Get("filename"), b})
		mu.Unlock()
	}))
	defer ntfy.Close()

	app := newTestApp(t)
	app.store.SetSetting("notify.url", ntfy.URL+"/workshop-7f3k")
	pic := tinyPNG(t)
	ok := SyncResult{Printer: Printer{Name: "Neptune 4"}, Job: PrintJob{Name: "knob.gcode", Outcome: "completed"},
		Seconds: 3900, Grams: 23, Thumb: pic}
	bad := SyncResult{Printer: Printer{Name: "Neptune 4"}, Job: PrintJob{Name: "lid.gcode", Outcome: "failed"}, Seconds: 600}
	app.notifyPrint(context.Background(), ok, "")
	app.notifyPrint(context.Background(), bad, "")

	if len(sent) != 2 {
		t.Fatalf("sent %d", len(sent))
	}
	if s := sent[0]; s.method != http.MethodPut || !bytes.Equal(s.body, pic) || s.title != "Print finished on Neptune 4" ||
		!strings.Contains(s.msg, "knob.gcode · 1 h 5 min · 23 g") || s.file != "print.png" {
		t.Errorf("finished = %+v", s)
	}
	if s := sent[1]; s.method != http.MethodPost || s.prio != "high" || s.tags != "x" || !strings.Contains(string(s.body), "stopped after 10 min") {
		t.Errorf("failed = %+v", s)
	}

	// Switched off for finished prints: nothing goes.
	app.store.SetSetting("notify.finished", "off")
	app.notifyPrint(context.Background(), ok, "")
	if len(sent) != 2 {
		t.Error("a finished print was sent with finished notifications off")
	}
}

func TestDiscordGetsAMultipartPost(t *testing.T) {
	var ctype string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctype = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	// sendNote decides by the address, so point a Discord-shaped one at the fake.
	if !isDiscordWebhook("https://discord.com/api/webhooks/123/abc") || isDiscordWebhook("https://ntfy.sh/api/webhooks/x") {
		t.Fatal("Discord detection")
	}
	orig := notifyClient
	notifyClient = &http.Client{Transport: rewriteTo(srv.URL)}
	defer func() { notifyClient = orig }()
	if err := sendNote(context.Background(), "https://discord.com/api/webhooks/123/abc",
		Note{Title: "Print finished", Body: "knob", Image: tinyPNG(t)}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ctype, "multipart/form-data") || !bytes.Contains(body, []byte(`"content":"**Print finished**\nknob"`)) ||
		!bytes.Contains(body, []byte("print.png")) {
		t.Errorf("discord got %s: %.300s", ctype, body)
	}
}

type rewriteTo string

func (u rewriteTo) RoundTrip(r *http.Request) (*http.Response, error) {
	t, _ := url.Parse(string(u))
	r.URL.Scheme, r.URL.Host = t.Scheme, t.Host
	return http.DefaultTransport.RoundTrip(r)
}

func TestNotifyAddressIsCheckedAndNeverShownWhole(t *testing.T) {
	for _, bad := range []string{"ntfy.sh/x", "https://ntfy.sh/", "ftp://x/y"} {
		if _, err := checkNotifyURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	n := NotifySettings{URL: "https://ntfy.sh/zq-secret-topic-9d1c"}
	if n.Masked() != "https://ntfy.sh/…9d1c" || strings.Contains(n.Masked(), "zq-secret") {
		t.Errorf("masked = %s", n.Masked())
	}
	app := newTestApp(t)
	app.store.SetSetting("notify.url", n.URL)
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/printing", nil))
	if strings.Contains(rec.Body.String(), "zq-secret-topic") {
		t.Error("the printing page shows the whole notification address")
	}
}

// A damp dry box pings once, not every minute, and again only after it has
// been fixed and gone damp again.
func TestSensorAlarmIsSentOnce(t *testing.T) {
	var n int
	var mu sync.Mutex
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); n++; mu.Unlock() }))
	defer ntfy.Close()
	app := newTestApp(t)
	app.store.SetSetting("notify.url", ntfy.URL+"/t")
	id, _, _ := app.store.AddSensor(Sensor{Name: "Dry box", Kind: "drybox", MaxHumidity: 20})
	read := func(h float64) {
		app.store.AddReading(id, nil, &h, time.Now())
		s, _ := app.store.GetSensor(id)
		app.sensorAlarm(context.Background(), s)
	}
	read(25)
	read(26)
	read(27)
	if n != 1 {
		t.Errorf("sent %d, want 1", n)
	}
	read(12) // fresh desiccant
	read(24)
	if n != 2 {
		t.Errorf("sent %d after it went damp again, want 2", n)
	}
}

func TestPreviewComesOutOfTheGcode(t *testing.T) {
	pic := tinyPNG(t)
	b64 := base64.StdEncoding.EncodeToString(pic)
	var g strings.Builder
	g.WriteString("; generated by OrcaSlicer\n;\n; THUMBNAIL_BLOCK_START\n")
	fmt.Fprintf(&g, "; thumbnail begin 300x300 %d\n", len(b64))
	for i := 0; i < len(b64); i += 78 {
		end := min(i+78, len(b64))
		g.WriteString("; " + b64[i:end] + "\n")
	}
	g.WriteString("; thumbnail end\n; THUMBNAIL_BLOCK_END\nG28\n")
	if got := GcodeThumbnail(strings.NewReader(g.String())); !bytes.Equal(got, pic) {
		t.Errorf("got %d bytes, want the %d-byte PNG", len(got), len(pic))
	}
	if GcodeThumbnail(strings.NewReader("G28\nG1 X0\n")) != nil {
		t.Error("no preview in the file, none out")
	}
}

func TestLayerAndLivePreview(t *testing.T) {
	fake := &fakeKlipper{host: "mkspi", thumb: tinyPNG(t), state: "printing", layer: [2]int{84, 210}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	link, _ := linkFor(Printer{LinkKind: "moonraker", LinkURL: srv.URL})
	st, err := link.Status(context.Background())
	if err != nil || st.LayerLabel() != "layer 84 of 210" {
		t.Errorf("status = %+v %v", st, err)
	}

	app := newTestApp(t)
	pid, _ := app.store.AddPrinter(Printer{Name: "N4", LinkKind: "moonraker", LinkURL: srv.URL})
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/printing/status", nil))
	if !strings.Contains(rec.Body.String(), `"layer":"layer 84 of 210"`) || !strings.Contains(rec.Body.String(), "live.png?file=knob.gcode") {
		t.Errorf("status json = %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/printing/printers/%d/live.png?file=knob.gcode", pid), nil))
	if rec.Header().Get("Content-Type") != "image/png" || !isPNG(rec.Body.Bytes()) {
		t.Errorf("live preview: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func localSubnetFrom(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return p.Masked()
}
