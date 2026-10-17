package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMoonraker answers the two calls the app makes, the way a Neptune 4 does,
// and remembers what it was sent.
type fakeMoonraker struct {
	mu           sync.Mutex
	key          string // required key, if any
	state        string
	progress     float64
	duration     float64
	busy         bool    // refuses to start because something is printing
	totalSeconds float64 // lifetime print time it reports

	gotName  string
	gotBody  []byte
	gotPrint string
	gotRoot  string
	chunked  bool // the request came without a Content-Length
}

func (f *fakeMoonraker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.key != "" && r.Header.Get("X-Api-Key") != f.key {
		http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/server/history/totals":
		fmt.Fprintf(w, `{"result":{"job_totals":{"total_jobs":42,"total_time":%g,"total_print_time":%g,
			"total_filament_used":123456.7,"longest_job":9000,"longest_print":8800}}}`,
			f.totalSeconds*1.1, f.totalSeconds)
	case "/printer/objects/query":
		fmt.Fprintf(w, `{"result":{"eventtime":1.0,"status":{
			"print_stats":{"state":%q,"filename":"case.gcode","print_duration":%g,"message":""},
			"virtual_sdcard":{"progress":%g},
			"display_status":{"message":null}}}}`, f.state, f.duration, f.progress)
	case "/server/files/upload":
		// Go decodes chunked bodies for us, which is exactly why a chunked
		// upload passed here while a real embedded server garbled it. Record
		// it so the tests can insist on a Content-Length.
		f.chunked = len(r.TransferEncoding) > 0 || r.ContentLength <= 0
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.gotName = hdr.Filename
		f.gotBody, _ = io.ReadAll(file)
		f.gotPrint = r.FormValue("print")
		f.gotRoot = r.FormValue("root")
		started := f.gotPrint == "true" && !f.busy
		w.WriteHeader(http.StatusCreated)
		// The current shape has no "result" wrapper on this endpoint.
		fmt.Fprintf(w, `{"item":{"path":%q,"root":"gcodes"},"print_started":%t,"action":"create_file"}`,
			hdr.Filename, started)
	default:
		http.NotFound(w, r)
	}
}

func TestMoonrakerStatusWhilePrinting(t *testing.T) {
	srv := httptest.NewServer(&fakeMoonraker{state: "printing", progress: 0.25, duration: 1800})
	defer srv.Close()

	link, ok := linkFor(Printer{LinkKind: "moonraker", LinkURL: srv.URL})
	if !ok {
		t.Fatal("a Moonraker printer with an address should be linkable")
	}
	st, err := link.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "printing" || st.Percent() != 25 || st.File != "case.gcode" {
		t.Fatalf("got %+v", st)
	}
	// 30 min for a quarter: a straight line says 90 min more.
	if st.Left != 90*time.Minute {
		t.Errorf("Left = %v, want 1h30m", st.Left)
	}
	if !strings.Contains(st.Headline(), "1 h 30 min") {
		t.Errorf("Headline() = %q", st.Headline())
	}
}

// Klipper says "standby" when idle. The app should not show a word the user
// has never seen on the printer's screen as a state of its own.
func TestMoonrakerStandbyIsIdle(t *testing.T) {
	srv := httptest.NewServer(&fakeMoonraker{state: "standby"})
	defer srv.Close()
	link, _ := linkFor(Printer{LinkKind: "moonraker", LinkURL: srv.URL})
	st, err := link.Status(context.Background())
	if err != nil || st.State != "idle" || st.Headline() != "Idle" {
		t.Errorf("got %+v, %v", st, err)
	}
}

func TestMoonrakerUploadAndStart(t *testing.T) {
	fake := &fakeMoonraker{state: "standby"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	link, _ := linkFor(Printer{LinkKind: "moonraker", LinkURL: srv.URL})

	body := strings.Repeat("G1 X10 Y10\n", 1000)
	started, err := link.Upload(context.Background(), "pi5_case.gcode", strings.NewReader(body), int64(len(body)), true)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Error("the printer said it started; Upload should say so")
	}
	if fake.gotName != "pi5_case.gcode" || string(fake.gotBody) != body {
		t.Errorf("printer got %q (%d bytes)", fake.gotName, len(fake.gotBody))
	}
	if fake.gotPrint != "true" || fake.gotRoot != "gcodes" {
		t.Errorf("print=%q root=%q", fake.gotPrint, fake.gotRoot)
	}
	if fake.chunked {
		t.Error("the upload was sent chunked; embedded servers read the chunk framing as data")
	}
}

// The exact bytes on the wire, read by a parser that knows nothing about
// chunked encoding — the way a minimal embedded server reads them.
func TestSizedUploadIsExactlyItsLength(t *testing.T) {
	body := "G28\nG1 Z0.2\n"
	rd, length, ctype, err := sizedUpload("file", "x.gcode", strings.NewReader(body),
		int64(len(body)), map[string]string{"print": "true", "root": "gcodes"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rd)
	if int64(len(raw)) != length {
		t.Fatalf("declared %d bytes, produced %d", length, len(raw))
	}
	// And it is a well-formed form when parsed with only the length to go on.
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	req.Header.Set("Content-Type", ctype)
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	f, _, _ := req.FormFile("file")
	got, _ := io.ReadAll(f)
	if string(got) != body || req.FormValue("print") != "true" || req.FormValue("root") != "gcodes" {
		t.Errorf("parsed back as file=%q print=%q root=%q", got, req.FormValue("print"), req.FormValue("root"))
	}
}

// Asked to start, file arrives, printer busy: Upload must not claim a print
// is running.
func TestMoonrakerBusyDoesNotClaimStarted(t *testing.T) {
	srv := httptest.NewServer(&fakeMoonraker{state: "printing", busy: true})
	defer srv.Close()
	link, _ := linkFor(Printer{LinkKind: "moonraker", LinkURL: srv.URL})
	started, err := link.Upload(context.Background(), "x.gcode", strings.NewReader("G28\n"), 4, true)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Error("a busy printer did not start anything")
	}
}

func TestMoonrakerWrongKeyIsExplained(t *testing.T) {
	srv := httptest.NewServer(&fakeMoonraker{key: "right", state: "standby"})
	defer srv.Close()
	link, _ := linkFor(Printer{LinkKind: "moonraker", LinkURL: srv.URL, LinkKey: "wrong"})
	st, err := link.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("want an error about the key, got %v", err)
	}
	if !st.Offline() {
		t.Errorf("an unreadable printer should read as offline, got %q", st.State)
	}

	link, _ = linkFor(Printer{LinkKind: "moonraker", LinkURL: srv.URL, LinkKey: "right"})
	if _, err := link.Status(context.Background()); err != nil {
		t.Errorf("right key refused: %v", err)
	}
}

func TestSwitchedOffPrinterIsOfflineNotAnError(t *testing.T) {
	srv := httptest.NewServer(&fakeMoonraker{})
	addr := srv.URL
	srv.Close() // nothing listening now

	link, _ := linkFor(Printer{LinkKind: "moonraker", LinkURL: addr})
	st, err := link.Status(context.Background())
	if !st.Offline() || st.Headline() != "Not answering" {
		t.Errorf("got %+v", st)
	}
	if err == nil || !strings.Contains(err.Error(), "check the address") {
		t.Errorf("want an actionable message, got %v", err)
	}
}

// The other protocol at the right address is a common mistake.
func TestWrongProtocolIsExplained(t *testing.T) {
	srv := httptest.NewServer(&fakeMoonraker{state: "standby"})
	defer srv.Close()
	link, _ := linkFor(Printer{LinkKind: "octoprint", LinkURL: srv.URL})
	_, err := link.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "other kind") {
		t.Errorf("want a hint about the protocol, got %v", err)
	}
}

func TestOctoPrintStatusAndUpload(t *testing.T) {
	var gotPrint, gotSelect, gotKey string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		switch r.URL.Path {
		case "/api/job":
			fmt.Fprint(w, `{"job":{"file":{"name":"knob.gcode"}},
				"progress":{"completion":61.5,"printTime":600,"printTimeLeft":420},
				"state":"Printing from SD"}`)
		case "/api/files/local":
			r.ParseMultipartForm(1 << 20)
			f, _, _ := r.FormFile("file")
			gotBody, _ = io.ReadAll(f)
			gotPrint, gotSelect = r.FormValue("print"), r.FormValue("select")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	link, _ := linkFor(Printer{LinkKind: "octoprint", LinkURL: srv.URL, LinkKey: "k"})
	st, err := link.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// OctoPrint's own estimate is used rather than a straight line.
	if st.State != "printing" || st.Percent() != 61 || st.Left != 7*time.Minute {
		t.Errorf("got %+v", st)
	}
	if gotKey != "k" {
		t.Errorf("key header = %q", gotKey)
	}

	started, err := link.Upload(context.Background(), "knob.gcode", strings.NewReader("G28\n"), 4, true)
	if err != nil || !started {
		t.Fatalf("upload: %v started=%v", err, started)
	}
	if gotPrint != "true" || gotSelect != "true" || string(gotBody) != "G28\n" {
		t.Errorf("print=%q select=%q body=%q", gotPrint, gotSelect, gotBody)
	}
}

// OctoPrint answers even when it has lost the serial connection to the
// printer. That is not "idle".
func TestOctoPrintDisconnectedFromPrinter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"job":{"file":{}},"progress":{},"state":"Offline"}`)
	}))
	defer srv.Close()
	link, _ := linkFor(Printer{LinkKind: "octoprint", LinkURL: srv.URL})
	st, _ := link.Status(context.Background())
	if !st.Offline() || !strings.Contains(st.Message, "not connected") {
		t.Errorf("got %+v", st)
	}
}

func TestNormaliseLinkURL(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.50":                 "http://192.168.1.50",
		"192.168.1.50:7125":            "http://192.168.1.50:7125",
		"http://neptune.local/":        "http://neptune.local",
		" https://octopi.local:5000/ ": "https://octopi.local:5000",
		"http://[fe80::1]:7125":        "http://[fe80::1]:7125",
	} {
		got, err := normaliseLinkURL(in)
		if err != nil || got != want {
			t.Errorf("normaliseLinkURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "ftp://printer", "http://"} {
		if _, err := normaliseLinkURL(bad); err == nil {
			t.Errorf("normaliseLinkURL(%q) should fail", bad)
		}
	}
}

func TestGcodeNames(t *testing.T) {
	for in, want := range map[string]string{
		"Pi 5 case_PETG_0.2mm.gcode":     "Pi_5_case_PETG_0.2mm.gcode",
		`C:\Users\me\Desktop\knob.gcode`: "knob.gcode",
		"../../etc/passwd":               "passwd.gcode",
		"bracket.gco":                    "bracket.gco",
		"???":                            "print.gcode",
	} {
		if got := GcodeName(in); got != want {
			t.Errorf("GcodeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The refusal for an STL has to say what to do instead, not just "no".
func TestNotGcodeHintSaysToSlice(t *testing.T) {
	for _, name := range []string{"case.stl", "case.3mf", "CASE.STL"} {
		if IsGcode(name) {
			t.Errorf("%s is not G-code", name)
		}
		if !strings.Contains(notGcodeHint(name), "slice") {
			t.Errorf("hint for %s should say to slice it: %q", name, notGcodeHint(name))
		}
	}
	if !strings.Contains(notGcodeHint("x.bgcode"), "plain G-code") {
		t.Error("binary G-code should get its own explanation")
	}
}

func TestEstimateWaitsUntilItMeansSomething(t *testing.T) {
	if estimateLeft(time.Minute, 1) != 0 {
		t.Error("1% in is too early for a straight line")
	}
	if estimateLeft(time.Hour, 100) != 0 {
		t.Error("finished means nothing left")
	}
	if got := estimateLeft(10*time.Minute, 50); got != 10*time.Minute {
		t.Errorf("half way after 10 min = %v, want 10m", got)
	}
}

// --- the send handler, end to end ------------------------------------------------

func multipartGcode(t *testing.T, fields map[string]string, name, body string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("gcode", name)
	io.WriteString(fw, body)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestSendGcodeToANeptune(t *testing.T) {
	app := newTestApp(t)
	fake := &fakeMoonraker{state: "standby"}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	preset, _ := PresetByName("Elegoo Neptune 4")
	preset.LinkURL = srv.URL
	pid, err := app.store.AddPrinter(preset)
	if err != nil {
		t.Fatal(err)
	}
	ids := seed(t, app.store, Item{Name: "Raspberry Pi 5", Quantity: 1})

	body := "; sliced for the Neptune 4\nG28\nG1 Z0.2\n"
	buf, ctype := multipartGcode(t, map[string]string{
		"start": "on", "item_id": fmt.Sprint(ids[0]),
	}, "Pi 5 case.gcode", body)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/printing/printers/%d/send", pid), buf)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "printing") || strings.Contains(loc, "error") {
		t.Errorf("redirect = %s", loc)
	}
	if string(fake.gotBody) != body || fake.gotName != "Pi_5_case.gcode" {
		t.Errorf("printer got %q as %q", fake.gotBody, fake.gotName)
	}
	if fake.chunked {
		t.Error("the send went chunked")
	}

	jobs, _ := app.store.RecentPrintJobs(0, 10)
	if len(jobs) != 1 {
		t.Fatalf("want one job recorded, got %d", len(jobs))
	}
	j := jobs[0]
	if !j.OK() || !j.Started || j.ItemName != "Raspberry Pi 5" || j.Printer != "Elegoo Neptune 4" {
		t.Errorf("job = %+v", j)
	}

	// The file is kept, named by its hash, and is byte-for-byte what was sent.
	f, err := app.gcode.Open(j.Blob)
	if err != nil {
		t.Fatalf("kept file: %v", err)
	}
	kept, _ := io.ReadAll(f)
	f.Close()
	if string(kept) != body {
		t.Error("kept file differs from what was sent")
	}

	// And it shows on the part's page.
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/items/%d", ids[0]), nil))
	if !strings.Contains(rec.Body.String(), "Printed for this") {
		t.Error("the part page should list what was printed for it")
	}
}

func TestSendingAnSTLIsRefusedWithAReason(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.AddPrinter(Printer{Name: "N4", LinkKind: "moonraker", LinkURL: "http://127.0.0.1:1"})

	buf, ctype := multipartGcode(t, nil, "case.stl", "solid case\nendsolid\n")
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/printing/printers/%d/send", pid), buf)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)

	loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.Contains(loc, "slice it first") {
		t.Errorf("redirect should explain slicing, got %s", loc)
	}
	if jobs, _ := app.store.RecentPrintJobs(0, 10); len(jobs) != 0 {
		t.Error("a refused file should not be recorded as a job")
	}
}

// A send to a printer that is off still keeps the file, and says so.
func TestFailedSendKeepsTheFileForLater(t *testing.T) {
	app := newTestApp(t)
	srv := httptest.NewServer(&fakeMoonraker{})
	addr := srv.URL
	srv.Close()
	pid, _ := app.store.AddPrinter(Printer{Name: "N4", LinkKind: "moonraker", LinkURL: addr})

	buf, ctype := multipartGcode(t, nil, "knob.gcode", "G28\n")
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/printing/printers/%d/send", pid), buf)
	req.Header.Set("Content-Type", ctype)
	app.routes().ServeHTTP(httptest.NewRecorder(), req)

	jobs, _ := app.store.RecentPrintJobs(0, 10)
	if len(jobs) != 1 || jobs[0].OK() {
		t.Fatalf("want one failed job, got %+v", jobs)
	}
	if _, err := app.gcode.Open(jobs[0].Blob); err != nil {
		t.Errorf("the file should be kept for a resend: %v", err)
	}

	// The printer comes back; send it again.
	fake := &fakeMoonraker{state: "standby"}
	srv2 := httptest.NewServer(fake)
	defer srv2.Close()
	p, _ := app.store.GetPrinter(pid)
	p.LinkURL = srv2.URL
	app.store.UpdatePrinter(p)

	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/printing/jobs/%d/resend", jobs[0].ID), nil)
	app.routes().ServeHTTP(httptest.NewRecorder(), req)
	if string(fake.gotBody) != "G28\n" {
		t.Errorf("resend delivered %q", fake.gotBody)
	}
	if jobs, _ = app.store.RecentPrintJobs(0, 10); len(jobs) != 2 || !jobs[0].OK() {
		t.Errorf("resend should be recorded as its own successful job: %+v", jobs)
	}
}

// The saved key is never sent back to the browser, and a blank box keeps it.
func TestPrinterKeyIsKeptAndNeverRendered(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.AddPrinter(Printer{Name: "Octo", LinkKind: "octoprint",
		LinkURL: "http://octopi.local", LinkKey: "SECRET-KEY-123"})

	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/printing?edit=%d", pid), nil))
	if strings.Contains(rec.Body.String(), "SECRET-KEY-123") {
		t.Fatal("the API key was rendered into the page")
	}
	if !strings.Contains(rec.Body.String(), "saved — leave blank to keep") {
		t.Error("the form should say a key is saved")
	}

	form := url.Values{"name": {"Octo"}, "link_kind": {"octoprint"},
		"link_url": {"http://octopi.local"}, "link_key": {""}}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/printing/printers/%d", pid),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.routes().ServeHTTP(httptest.NewRecorder(), req)
	if p, _ := app.store.GetPrinter(pid); p.LinkKey != "SECRET-KEY-123" {
		t.Errorf("a blank key box should keep the saved key, got %q", p.LinkKey)
	}

	form.Set("clear_key", "on")
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/printing/printers/%d", pid),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.routes().ServeHTTP(httptest.NewRecorder(), req)
	if p, _ := app.store.GetPrinter(pid); p.LinkKey != "" {
		t.Error("ticking forget should clear the key")
	}
}

func TestNeptunePresetGoesStraightToTheAddress(t *testing.T) {
	app := newTestApp(t)
	form := url.Values{"preset": {"Elegoo Neptune 4"}}
	req := httptest.NewRequest(http.MethodPost, "/printing/printers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "edit=") {
		t.Errorf("a preset that needs an address should open the form for it, went to %s", loc)
	}
	list, _ := app.store.ListPrinters()
	if len(list) != 1 || list[0].LinkKind != "moonraker" || list[0].MaxHotend != 300 {
		t.Errorf("preset not applied: %+v", list)
	}
}

func TestStatusEndpointSkipsUnconnectedPrinters(t *testing.T) {
	app := newTestApp(t)
	srv := httptest.NewServer(&fakeMoonraker{state: "printing", progress: 0.5, duration: 600})
	defer srv.Close()
	app.store.AddPrinter(Printer{Name: "Neptune", LinkKind: "moonraker", LinkURL: srv.URL})
	app.store.AddPrinter(Printer{Name: "Not on the network"})

	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/printing/status", nil))
	var got []struct {
		Name, State string
		Percent     int
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Neptune" || got[0].State != "printing" || got[0].Percent != 50 {
		t.Errorf("got %+v", got)
	}
}

// --- blobs ----------------------------------------------------------------------

func TestBlobsAreContentAddressed(t *testing.T) {
	b, _ := NewBlobs(t.TempDir(), 1<<10)
	n1, sum1, size, err := b.Save(strings.NewReader("G28\n"), ".gcode")
	if err != nil {
		t.Fatal(err)
	}
	n2, sum2, _, _ := b.Save(strings.NewReader("G28\n"), ".gcode")
	if n1 != n2 || sum1 != sum2 || size != 4 {
		t.Errorf("same bytes should be one file: %s %s", n1, n2)
	}
	if !strings.HasPrefix(n1, sum1) || !strings.HasSuffix(n1, ".gcode") {
		t.Errorf("name = %s", n1)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(b.dir)
	if len(entries) != 1 {
		t.Errorf("want one file in the folder, got %d", len(entries))
	}
}

func TestBlobsRefuseTooBigAndEmpty(t *testing.T) {
	b, _ := NewBlobs(t.TempDir(), 10)
	if _, _, _, err := b.Save(strings.NewReader("01234567890"), ""); err != errTooBig {
		t.Errorf("11 bytes into a 10-byte store: %v", err)
	}
	if _, _, _, err := b.Save(strings.NewReader(""), ""); err == nil {
		t.Error("an empty file should be refused")
	}
	entries, _ := os.ReadDir(b.dir)
	if len(entries) != 0 {
		t.Errorf("a refused upload left %d files behind", len(entries))
	}
}

func TestBlobNamesCannotEscapeTheFolder(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(filepath.Dir(dir), "secret"), []byte("x"), 0o644)
	b, _ := NewBlobs(dir, 1<<10)
	for _, name := range []string{"../secret", "/etc/passwd", "secret",
		strings.Repeat("a", 64) + "/../../secret", strings.Repeat("g", 64)} {
		if _, err := b.Open(name); err == nil {
			t.Errorf("Open(%q) should be refused", name)
		}
	}
}

func TestHeadlineReadsNaturallyAtTheEnds(t *testing.T) {
	for _, c := range []struct {
		left time.Duration
		want string
	}{
		{20 * time.Second, "Printing, 99%, nearly done"},
		{72 * time.Minute, "Printing, 99%, about 1 h 12 min left"},
		{0, "Printing, 99%"}, // too early to estimate
	} {
		got := PrintStatus{State: "printing", Progress: 99.4, Left: c.left}.Headline()
		if got != c.want {
			t.Errorf("Left=%v: %q, want %q", c.left, got, c.want)
		}
	}
}
