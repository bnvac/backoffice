package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- firmware ---------------------------------------------------------------------

func TestFlashHints(t *testing.T) {
	cases := []struct {
		file, board, wantTool, wantIn string
	}{
		{"logger.uf2", "Raspberry Pi Pico 2 W", "drag and drop", ""},
		{"logger.merged.bin", "ESP32-S3 DevKitC", "esptool", "--chip esp32s3"},
		{"node.bin", "ESP32-WROOM-32", "esptool", "--chip esp32 "},
		{"blink.bin", "ESP32-C3 SuperMini", "esptool", "--chip esp32c3"},
		{"blink.hex", "Arduino Uno R3", "avrdude", "-p m328p"},
		{"blink.hex", "Arduino Mega 2560", "avrdude", "-p m2560"},
		{"keys.hex", "SparkFun Pro Micro", "avrdude", "-p m32u4"},
		{"fw.elf", "STM32 Blue Pill", "probe-rs", ""},
	}
	for _, c := range cases {
		h, ok := HintFor(c.file, c.board)
		if !ok {
			t.Errorf("%s on %s: no hint", c.file, c.board)
			continue
		}
		if h.Tool != c.wantTool {
			t.Errorf("%s on %s: tool %q, want %q", c.file, c.board, h.Tool, c.wantTool)
		}
		if c.wantIn != "" && !strings.Contains(h.Command, c.wantIn) {
			t.Errorf("%s on %s: command %q should contain %q", c.file, c.board, h.Command, c.wantIn)
		}
		if h.Note == "" {
			t.Errorf("%s: every hint should say where it is guessing", c.file)
		}
	}
	if _, ok := HintFor("notes.txt", "ESP32"); ok {
		t.Error("a text file is not firmware")
	}
}

// The hint for a .bin has to mention the offset trap, because flashing an
// app-only image at 0x0 bricks the boot until it is fixed.
func TestBinHintWarnsAboutTheOffset(t *testing.T) {
	h, _ := HintFor("app.bin", "ESP32")
	if !strings.Contains(h.Note, "0x10000") {
		t.Errorf("the .bin hint should mention the app offset: %q", h.Note)
	}
}

func TestCurrentIsTheNewestPerBoard(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32-WROOM-32", Quantity: 6})
	board := ids[0]
	day := func(d int) time.Time { return time.Date(2026, 3, d, 12, 0, 0, 0, time.UTC) }

	app.store.AddFlash(Flash{ItemID: &board, Unit: "greenhouse", Firmware: "logger", Version: "v1", FlashedAt: day(1)})
	app.store.AddFlash(Flash{ItemID: &board, Unit: "Greenhouse", Firmware: "logger", Version: "v2", FlashedAt: day(9)})
	app.store.AddFlash(Flash{ItemID: &board, Unit: "desk", Firmware: "ESPHome", FlashedAt: day(5)})
	app.store.AddFlash(Flash{ItemID: &board, Firmware: "blink", FlashedAt: day(2)})

	list, err := app.store.ListFlashes(board, 0)
	if err != nil {
		t.Fatal(err)
	}
	current := map[string]bool{}
	for _, f := range list {
		current[f.Firmware+"/"+f.Version] = f.Current
	}
	if !current["logger/v2"] || current["logger/v1"] {
		t.Errorf("the newer logger should be current and the older replaced: %v", current)
	}
	if !current["ESPHome/"] {
		t.Error("a different unit is its own board")
	}
	if !current["blink/"] {
		t.Error("an unnamed unit cannot be shown to be replaced, so it stays current")
	}
	if list[0].Version != "v2" {
		t.Errorf("newest first, got %q first", list[0].Firmware+list[0].Version)
	}
}

func TestRecordFirmwareWithItsImage(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "Raspberry Pi Pico 2 W", Quantity: 4})
	image := bytes.Repeat([]byte{0x55, 0xAA}, 4096)
	sum := sha256.Sum256(image)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{
		"item_id": fmt.Sprint(ids[0]), "unit": "greenhouse", "firmware": "greenhouse-logger",
		"version": "3f9a2c1", "flashed_at": "2026-10-03", "return": fmt.Sprintf("/items/%d", ids[0]),
	} {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("image", "logger.uf2")
	fw.Write(image)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/firmware", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}

	list, _ := app.store.ListFlashes(ids[0], 0)
	if len(list) != 1 {
		t.Fatalf("want one record, got %d", len(list))
	}
	f := list[0]
	if f.SHA256 != hex.EncodeToString(sum[:]) || f.Size != int64(len(image)) || f.File != "logger.uf2" {
		t.Errorf("record = %+v", f)
	}
	if f.FlashedDate() != "3 Oct 2026" {
		t.Errorf("flashed date = %q", f.FlashedDate())
	}

	// The record page shows the full checksum and the UF2 instructions.
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/firmware/%d", f.ID), nil))
	page := rec.Body.String()
	if !strings.Contains(page, f.SHA256) || !strings.Contains(page, "BOOTSEL") {
		t.Error("the record page should show the checksum and how to flash it")
	}

	// And the download is byte-for-byte the image, under its own name.
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/firmware/%d/file", f.ID), nil))
	got, _ := io.ReadAll(rec.Body)
	if !bytes.Equal(got, image) {
		t.Error("downloaded image differs from the uploaded one")
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `"logger.uf2"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}

	// The part page lists it.
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/items/%d", ids[0]), nil))
	if !strings.Contains(rec.Body.String(), "greenhouse-logger") {
		t.Error("the part page should list what is flashed on it")
	}
}

// A resistor's page should not offer to record firmware.
func TestFirmwareFormOnlyOnBoards(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "10k resistor", Category: "Passives", Quantity: 100},
		Item{Name: "ESP32-C3 SuperMini", Category: "Microcontrollers", Quantity: 3})

	get := func(id int64) string {
		rec := httptest.NewRecorder()
		app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/items/%d", id), nil))
		return rec.Body.String()
	}
	if strings.Contains(get(ids[0]), "Firmware on these") {
		t.Error("a resistor has no firmware")
	}
	if !strings.Contains(get(ids[1]), "Firmware on these") {
		t.Error("a microcontroller should offer to record its firmware")
	}
}

func TestFirmwareNeedsAName(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.store.AddFlash(Flash{Firmware: "  "}); err == nil {
		t.Error("a record with no firmware named says nothing")
	}
}

// --- machines ---------------------------------------------------------------------

func TestNormaliseSum(t *testing.T) {
	const h = "9b1e3a7f5c2d4e6f8a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f"
	for in, want := range map[string]string{
		h:                        h,
		strings.ToUpper(h):       h,
		"  " + h + "  ":          h,
		"sha256:" + h:            h,
		h + "  ubuntu-24.04.iso": h, // a line pasted from SHA256SUMS
		h + " *raspios.img.xz":   h,
		"not a hash":             "not a hash", // left as typed so the mistake shows
	} {
		if got := NormaliseSum(in); got != want {
			t.Errorf("NormaliseSum(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMachineChecksumIsValidated(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.store.AddMachine(Machine{Name: "nas", ImageSum: "abc123"}); err == nil {
		t.Error("a short checksum should be refused")
	}
	id, err := app.store.AddMachine(Machine{Name: "nas", Port: 22,
		ImageSum: "SHA256:" + strings.Repeat("AB", 32) + "  debian.iso"})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := app.store.GetMachine(id)
	if m.ImageSum != strings.Repeat("ab", 32) {
		t.Errorf("stored %q", m.ImageSum)
	}
}

func TestMachinesCannotRunOnThemselves(t *testing.T) {
	app := newTestApp(t)
	host, _ := app.store.AddMachine(Machine{Name: "proxmox", Port: 22})
	vm, _ := app.store.AddMachine(Machine{Name: "home-assistant", Port: 22, ParentID: &host})
	inner, _ := app.store.AddMachine(Machine{Name: "addon", Port: 0, ParentID: &vm})

	// proxmox -> runs on addon would close the loop through vm.
	m, _ := app.store.GetMachine(host)
	m.ParentID = &inner
	if err := app.store.UpdateMachine(m); err == nil {
		t.Error("a loop through two machines should be refused")
	}
	m.ParentID = &host
	if err := app.store.UpdateMachine(m); err == nil {
		t.Error("running on itself should be refused")
	}
	m.ParentID = nil
	if err := app.store.UpdateMachine(m); err != nil {
		t.Errorf("clearing the parent should be fine: %v", err)
	}
}

func TestReachability(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	openPort := ln.Addr().(*net.TCPAddr).Port

	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	machines := []Machine{
		{Name: "up", Address: "127.0.0.1", Port: openPort},
		{Name: "down", Address: "127.0.0.1", Port: closedPort},
		{Name: "unchecked", Address: "127.0.0.1", Port: 0},
		{Name: "no address", Port: 22},
	}
	Reachability(context.Background(), machines, 500*time.Millisecond)

	if !machines[0].Up.OK || !strings.HasPrefix(machines[0].Up.Label(), "up") {
		t.Errorf("listening port: %+v", machines[0].Up)
	}
	if machines[1].Up.OK || !machines[1].Up.Checked || machines[1].Up.Label() != "not answering" {
		t.Errorf("closed port: %+v", machines[1].Up)
	}
	if machines[2].Up.Checked || machines[3].Up.Checked {
		t.Error("port 0 and a blank address should not be checked")
	}
}

func TestPowerBudgetDoesNotCountUnknownsAsZero(t *testing.T) {
	b := BudgetPower([]Machine{{Watts: 6}, {Watts: 15}, {}})
	if b.Watts != 21 || b.Known != 2 || b.Unknown != 1 {
		t.Errorf("got %+v", b)
	}
	if got := b.KWhPerYear(); got < 183 || got > 185 {
		t.Errorf("21 W all year = %.1f kWh, want about 184", got)
	}
}

func TestMachinesPageAndThePartLink(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "Raspberry Pi 5", Quantity: 1})
	form := url.Values{"name": {"cupboard-pi"}, "role": {"Home Assistant"},
		"address": {"127.0.0.1"}, "port": {"0"}, "item_id": {fmt.Sprint(ids[0])},
		"services": {"Home Assistant :8123\nMosquitto :1883"}, "watts": {"6"}}
	req := httptest.NewRequest(http.MethodPost, "/machines", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	app.routes().ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/machines", nil))
	page := rec.Body.String()
	for _, want := range []string{"cupboard-pi", "Mosquitto :1883", "6 W"} {
		if !strings.Contains(page, want) {
			t.Errorf("machines page missing %q", want)
		}
	}

	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/items/%d", ids[0]), nil))
	if !strings.Contains(rec.Body.String(), "In service") {
		t.Error("the Pi's own page should say it is in service")
	}
}

func TestBadPortIsRefusedNotZeroed(t *testing.T) {
	app := newTestApp(t)
	form := url.Values{"name": {"x"}, "port": {"ssh"}}
	req := httptest.NewRequest(http.MethodPost, "/machines", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if list, _ := app.store.ListMachines(); len(list) != 0 {
		t.Error("a port of \"ssh\" should be refused rather than stored as 0")
	}
}

// --- redirects --------------------------------------------------------------------

func TestSafeNextRefusesProtocolRelative(t *testing.T) {
	for _, bad := range []string{"//evil.com", `/\evil.com`, "https://evil.com", "evil.com", ""} {
		if got := safeNext(bad); got != "/" {
			t.Errorf("safeNext(%q) = %q, want /", bad, got)
		}
	}
	if got := safeNext("/items/4"); got != "/items/4" {
		t.Errorf("a local path should pass, got %q", got)
	}
}

// The newest record's own page must not call it history. A single fetch has
// no siblings to compare against, which is how it once did.
func TestNewestRecordPageDoesNotSayReplaced(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "Pico 2 W", Category: "Microcontrollers", Quantity: 2})
	board := ids[0]
	old, _ := app.store.AddFlash(Flash{ItemID: &board, Unit: "greenhouse", Firmware: "logger",
		Version: "v1", FlashedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)})
	newest, _ := app.store.AddFlash(Flash{ItemID: &board, Unit: "greenhouse", Firmware: "logger",
		Version: "v2", FlashedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)})

	page := func(id int64) string {
		rec := httptest.NewRecorder()
		app.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/firmware/%d", id), nil))
		return rec.Body.String()
	}
	if strings.Contains(page(newest), "newer record exists") {
		t.Error("the newest record is described as replaced")
	}
	if !strings.Contains(page(old), "newer record exists") {
		t.Error("the older record should say it has been replaced")
	}
}
