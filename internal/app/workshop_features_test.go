package app

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- project files ---------------------------------------------------------------

func uploadFiles(t *testing.T, app *App, projectID int64, note string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, body := range files {
		fw, _ := mw.CreateFormFile("files", name)
		io.WriteString(fw, body)
	}
	mw.WriteField("note", note)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/projects/%d/files", projectID), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	return rec
}

func TestSameNameIsTheNextRevision(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("Desk lamp", "")

	uploadFiles(t, app, pid, "first go", map[string]string{"arm.step": "ISO-10303-21; v1", "lamp.kicad_pcb": "(kicad_pcb)"})
	uploadFiles(t, app, pid, "thicker wall", map[string]string{"arm.step": "ISO-10303-21; v2"})
	// The same bytes again is not a new revision.
	rec := uploadFiles(t, app, pid, "", map[string]string{"arm.step": "ISO-10303-21; v2"})
	if loc, _ := url.QueryUnescape(rec.Header().Get("Location")); !strings.Contains(loc, "had not changed") {
		t.Errorf("re-uploading an unchanged file should say so: %s", loc)
	}

	files, _ := app.store.ProjectFiles(pid)
	groups := GroupFiles(files)
	if len(groups) != 2 {
		t.Fatalf("want 2 names, got %+v", groups)
	}
	arm := groups[0] // CAD sorts before PCB
	if arm.Latest.Name != "arm.step" || arm.Latest.Rev != 2 || arm.Latest.Note != "thicker wall" ||
		len(arm.Older) != 1 || arm.Older[0].Rev != 1 {
		t.Errorf("arm = %+v", arm)
	}
	if groups[1].Latest.Kind != "pcb" {
		t.Errorf("a KiCad board is a PCB: %+v", groups[1].Latest)
	}

	// Each revision downloads as what it was.
	for _, rev := range []ProjectFile{arm.Latest, arm.Older[0]} {
		rec := httptest.NewRecorder()
		app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/files/%d", rev.ID), nil))
		want := fmt.Sprintf("ISO-10303-21; v%d", rev.Rev)
		if rec.Body.String() != want {
			t.Errorf("rev %d downloaded %q", rev.Rev, rec.Body.String())
		}
		if !strings.Contains(rec.Header().Get("Content-Disposition"), `attachment; filename="arm.step"`) {
			t.Errorf("disposition = %s", rec.Header().Get("Content-Disposition"))
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Error("an uploaded file must never run as a page of this site")
		}
	}

	// The project page lists them.
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/projects/%d", pid), nil))
	if page := rec.Body.String(); !strings.Contains(page, "arm.step") || !strings.Contains(page, "1 earlier revision") {
		t.Error("the project page should list the files and their history")
	}
}

func TestAnUploadedPageCannotRunHere(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("x", "")
	uploadFiles(t, app, pid, "", map[string]string{"evil.html": "<script>alert(1)</script>", `C:\Users\ben\notes.txt`: "hi"})
	files, _ := app.store.ProjectFiles(pid)
	for _, f := range files {
		if strings.Contains(f.Name, `\`) {
			t.Errorf("a Windows path should be cut to its file name: %q", f.Name)
		}
		rec := httptest.NewRecorder()
		app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/files/%d", f.ID), nil))
		if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "html") {
			t.Errorf("%s served as %s", f.Name, ct)
		}
	}
}

func TestKindOf(t *testing.T) {
	for name, want := range map[string]string{
		"arm.STEP": "cad", "body.f3d": "cad", "case.stl": "mesh", "plate.3mf": "mesh",
		"case_PETG.gcode": "gcode", "board.kicad_pcb": "pcb", "board-F_Cu.gtl": "pcb",
		"logger.uf2": "firmware", "main.py": "code", "datasheet.pdf": "doc", "photo.jpg": "image",
		"gerbers.zip": "archive", "mystery": "other", "photo.gif": "image",
	} {
		if got := KindOf(name); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// G-code kept on a project goes to the printer from the project, and what it
// used counts towards the build.
func TestPrintFromAProjectCountsTowardsItsCost(t *testing.T) {
	app := newTestApp(t)
	fake := &fakeMoonraker{state: "standby"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	printer, _ := app.store.AddPrinter(Printer{Name: "Neptune 4", LinkKind: "moonraker", LinkURL: srv.URL})
	spool, _ := app.store.AddSpool(Spool{Material: "petg", ColourName: "Orange", ColourHex: "#f07c1c", NetGrams: 1000, Price: 20})
	app.store.LoadSpool(spool, &printer)

	ids := seed(t, app.store, Item{Name: "ESP32", Quantity: 3}, Item{Name: "BME280", Quantity: 1})
	app.store.SetPrice(ids[0], Price{Source: "Pi Hut", Amount: 6, Currency: "GBP"})
	pid, _ := app.store.CreateProject("Weather station", "")
	app.store.AddProjectPart(pid, &ids[0], "", 2, "")
	app.store.AddProjectPart(pid, &ids[1], "", 1, "") // no price known

	gcode := `; HEADER_BLOCK_START
; generated by OrcaSlicer 2.1.1
; total filament weight [g] : 50.00
; model printing time: 1h 0m 0s; total estimated time: 2h 0m 0s
; HEADER_BLOCK_END
; filament_type = PETG
`
	uploadFiles(t, app, pid, "", map[string]string{"case.gcode": gcode})
	files, _ := app.store.ProjectFiles(pid)
	if len(files) != 1 || !files[0].Printable() {
		t.Fatalf("files = %+v", files)
	}

	form := url.Values{"printer_id": {fmt.Sprint(printer)}, "start": {"on"}}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/files/%d/print", files[0].ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	loc, _ := url.QueryUnescape(rec.Header().Get("Location"))
	if !strings.HasPrefix(loc, fmt.Sprintf("/projects/%d", pid)) || !strings.Contains(loc, "printing") {
		t.Errorf("sending from a project should come back to it: %s", loc)
	}
	if string(fake.gotBody) != gcode || fake.gotPrint != "true" {
		t.Errorf("the printer got %d bytes, print=%q", len(fake.gotBody), fake.gotPrint)
	}

	jobs, _ := app.store.PrintJobs(JobFilter{Project: pid}, 10)
	if len(jobs) != 1 || jobs[0].ProjectID == nil || *jobs[0].ProjectID != pid {
		t.Fatalf("the job should be tagged with the project: %+v", jobs)
	}

	p, _ := app.store.GetProject(pid)
	sp, _ := app.store.GetSpool(spool)
	c := CostOf(p, jobs, map[int64]Spool{spool: sp})
	if len(c.Parts) != 1 || c.Parts[0].Amount != 12 || c.Parts[0].Currency != "GBP" {
		t.Errorf("parts = %+v", c.Parts)
	}
	if len(c.Unpriced) != 1 || c.Unpriced[0] != "BME280" {
		t.Errorf("the unpriced part should be named, not counted as free: %v", c.Unpriced)
	}
	if math.Abs(c.FilamentCost-1.0) > 1e-9 || c.Grams != 50 || c.PrintSeconds != 7200 {
		t.Errorf("filament = %+v", c)
	}
	if c.TotalLabel() != "£13.00" {
		t.Errorf("total = %s", c.TotalLabel())
	}

	// A print marked as not printed stops counting.
	app.store.ReturnJobFilament(jobs[0].ID)
	jobs, _ = app.store.PrintJobs(JobFilter{Project: pid}, 10)
	if c := CostOf(p, jobs, map[int64]Spool{spool: sp}); c.Grams != 0 || c.FilamentCost != 0 {
		t.Errorf("a failed print still counted: %+v", c)
	}
}

func TestCostNeverAddsCurrenciesTogether(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "A", Quantity: 1}, Item{Name: "B", Quantity: 1})
	app.store.SetPrice(ids[0], Price{Source: "x", Amount: 5, Currency: "GBP"})
	app.store.SetPrice(ids[1], Price{Source: "y", Amount: 7, Currency: "USD"})
	pid, _ := app.store.CreateProject("p", "")
	app.store.AddProjectPart(pid, &ids[0], "", 1, "")
	app.store.AddProjectPart(pid, &ids[1], "", 1, "")
	p, _ := app.store.GetProject(pid)
	c := CostOf(p, nil, nil)
	if len(c.Parts) != 2 || c.HasTotal() {
		t.Errorf("two currencies should show as two amounts and no single total: %+v", c)
	}
}

// Firmware images and project files are the only copy of something made by
// hand, so a backup has to carry them, and a restore bring them back.
func TestBackupKeepsFirmwareAndProjectFiles(t *testing.T) {
	app := newTestApp(t)
	pid, _ := app.store.CreateProject("Lamp", "")
	uploadFiles(t, app, pid, "", map[string]string{"arm.step": "ISO-10303-21; arm"})
	blob, sum, size, err := app.firmware.Save(strings.NewReader("UF2 image bytes"), ".uf2")
	if err != nil {
		t.Fatal(err)
	}
	app.store.AddFlash(Flash{Firmware: "lamp", File: "lamp.uf2", Blob: blob, SHA256: sum, Size: size})

	var archive bytes.Buffer
	if err := app.WriteBackup(&archive); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	joined := strings.Join(names, " ")
	if !strings.Contains(joined, "firmware/"+blob) || !strings.Contains(joined, "files/") {
		t.Fatalf("backup holds %v", names)
	}

	fresh := newTestApp(t)
	rep, err := fresh.Restore(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 2 {
		t.Errorf("restored %d files, want 2", rep.Files)
	}
	f, err := fresh.firmware.Open(blob)
	if err != nil {
		t.Fatal("the firmware image did not come back")
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "UF2 image bytes" {
		t.Errorf("firmware = %q", got)
	}
	files, _ := fresh.store.ProjectFiles(pid)
	rec := httptest.NewRecorder()
	fresh.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/files/%d", files[0].ID), nil))
	if rec.Body.String() != "ISO-10303-21; arm" {
		t.Errorf("project file after restore = %q", rec.Body.String())
	}
}

// --- sensors ---------------------------------------------------------------------

func postReading(t *testing.T, h http.Handler, auth, ctype, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sensors/reading", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestSensorPostsWithItsTokenEvenWhenSignInIsOn(t *testing.T) {
	app, srv, cookie := signedInApp(t, "off")
	id, token, err := app.store.AddSensor(Sensor{Name: "Dry box 1", Kind: "drybox", MaxHumidity: 20})
	if err != nil {
		t.Fatal(err)
	}
	h := app.routes()

	if code, _ := postReading(t, h, "", "application/json", `{"humidity": 12}`); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := postReading(t, h, "bos_wrong", "application/json", `{"humidity": 12}`); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	code, body := postReading(t, h, token, "application/json", `{"temperature": 21.4, "humidity": 24}`)
	if code != http.StatusOK || !strings.Contains(body, "desiccant") {
		t.Errorf("a damp dry box should be told so: %d %s", code, body)
	}
	s, _ := app.store.GetSensor(id)
	if s.LastHum == nil || *s.LastHum != 24 || s.LastTemp == nil || *s.LastTemp != 21.4 || !s.Alarm() {
		t.Errorf("sensor = %+v", s)
	}

	// The token is not stored as itself.
	var stored string
	app.store.db.QueryRow(`SELECT token_hash FROM sensors WHERE id = ?`, id).Scan(&stored)
	if stored == token || strings.Contains(stored, token) {
		t.Error("the token should only be kept as a hash")
	}

	// The dashboard says what is wrong, to someone signed in.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.AddCookie(cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), "Dry box 1 is at 24% humidity") {
		t.Error("the dashboard should show the alert")
	}
	// But the sensor pages themselves still need sign-in.
	res, _ = http.Get(srv.URL + "/sensors")
	body2, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(body2), "Dry box 1") {
		t.Error("the sensor list should need sign-in")
	}
}

func TestSensorReadingShapes(t *testing.T) {
	app := newTestApp(t)
	id, token, _ := app.store.AddSensor(Sensor{Name: "Wall", Kind: "room"})
	h := app.routes()
	step := func() {
		app.store.db.Exec(`UPDATE sensors SET last_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), id)
	}

	// A form post with short names and the token in the body, the way a
	// minimal Arduino sketch does it.
	code, body := postReading(t, h, "", "application/x-www-form-urlencoded", "token="+token+"&t=19.5&rh=55")
	if code != http.StatusOK {
		t.Fatalf("form post: %d %s", code, body)
	}
	s, _ := app.store.GetSensor(id)
	if *s.LastTemp != 19.5 || *s.LastHum != 55 {
		t.Errorf("sensor = %+v", s)
	}

	// Humidity only keeps the last temperature.
	step()
	postReading(t, h, token, "", `{"humidity":"50"}`)
	s, _ = app.store.GetSensor(id)
	if *s.LastTemp != 19.5 || *s.LastHum != 50 {
		t.Errorf("a humidity-only reading should keep the temperature: %+v", s)
	}

	// A disconnected DHT22 says -999; that is not a reading.
	step()
	if code, _ := postReading(t, h, token, "", `{"temperature": -999}`); code != http.StatusBadRequest {
		t.Errorf("-999 °C accepted: %d", code)
	}
	// Too fast is refused politely.
	step()
	postReading(t, h, token, "", `{"temperature": 20}`)
	if code, _ := postReading(t, h, token, "", `{"temperature": 20}`); code != http.StatusTooManyRequests {
		t.Errorf("a second reading straight away: %d", code)
	}
}

func TestNewSensorTokenRetiresTheOldOne(t *testing.T) {
	app := newTestApp(t)
	id, old, _ := app.store.AddSensor(Sensor{Name: "Enclosure", Kind: "enclosure"})
	fresh, err := app.store.NewSensorToken(id)
	if err != nil || fresh == old {
		t.Fatal(err)
	}
	h := app.routes()
	if code, _ := postReading(t, h, old, "", `{"temperature": 30}`); code != http.StatusUnauthorized {
		t.Errorf("old token still works: %d", code)
	}
	if code, _ := postReading(t, h, fresh, "", `{"temperature": 30}`); code != http.StatusOK {
		t.Errorf("new token refused: %d", code)
	}
}

func TestQuietSensorSaysSoInsteadOfTrustingItsLastReading(t *testing.T) {
	hum := 80.0
	s := Sensor{Name: "Dry box", Kind: "drybox", MaxHumidity: 20, LastHum: &hum, LastAt: time.Now().Add(-2 * time.Hour)}
	alerts := s.Alerts()
	if len(alerts) != 1 || !strings.Contains(alerts[0], "has not reported") {
		t.Errorf("alerts = %v", alerts)
	}
}

func TestSensorChartAndDownsample(t *testing.T) {
	var rs []Reading
	start := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 1000; i++ {
		v, h := 20+float64(i%10), 30+float64(i%5)
		rs = append(rs, Reading{At: start.Add(time.Duration(i) * time.Minute), Temp: &v, Hum: &h})
	}
	ds := downsample(rs, 100)
	if len(ds) != 100 || *ds[0].Temp < 20 || *ds[0].Temp > 29 {
		t.Errorf("downsampled to %d, first %v", len(ds), *ds[0].Temp)
	}
	svg := SensorChart(ds, "hum", 33, 640, 150, true)
	if !strings.HasPrefix(svg, "<svg") || strings.Contains(svg, "NaN") || !strings.Contains(svg, `class="limit"`) {
		t.Errorf("chart = %.200s", svg)
	}
	if !strings.Contains(SensorChart(nil, "hum", 0, 10, 10, false), "Not enough") {
		t.Error("an empty chart should say so")
	}
}

// The sensors page and a sensor's page render, and the token is on the page
// only right after it is made.
func TestSensorPagesShowTheTokenOnce(t *testing.T) {
	app := newTestApp(t)
	h := app.routes()
	form := url.Values{"name": {"Dry box 1"}, "kind": {"drybox"}}
	req := httptest.NewRequest(http.MethodPost, "/sensors", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	page := rec.Body.String()
	start := strings.Index(page, "bos_")
	if rec.Code != http.StatusOK || start < 0 {
		t.Fatalf("the new token should be on the page: %d", rec.Code)
	}
	token := page[start : start+4+48]
	if code, _ := postReading(t, h, token, "", `{"humidity": 15}`); code != http.StatusOK {
		t.Errorf("the shown token does not work: %d", code)
	}
	sensors, _ := app.store.ListSensors()
	if len(sensors) != 1 || sensors[0].MaxHumidity != 20 {
		t.Errorf("a dry box should start with a 20%% limit: %+v", sensors)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/sensors/%d", sensors[0].ID), nil))
	if strings.Contains(rec.Body.String(), token) {
		t.Error("the token must not be shown again")
	}
	if !strings.Contains(rec.Body.String(), "/api/sensors/reading") {
		t.Error("the setup instructions should name the endpoint")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sensors", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "15%") {
		t.Errorf("sensors page: %d", rec.Code)
	}
}
