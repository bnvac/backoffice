package app

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// --- stock reservations -----------------------------------------------------

func TestBuildingProjectReservesStockFromOtherProjects(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 3})

	weather, err := app.store.CreateProject("Weather station", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	doorbell, err := app.store.CreateProject("Doorbell", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	for _, p := range []int64{weather, doorbell} {
		if err := app.store.AddProjectPart(p, &ids[0], "", 2, ""); err != nil {
			t.Fatalf("AddProjectPart: %v", err)
		}
	}

	// Planning holds nothing: both projects still see all three.
	got, err := app.store.GetProject(doorbell)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if n := got.Parts[0].Free(); n != 3 {
		t.Errorf("free while both are planning = %d, want 3", n)
	}
	if len(got.Shortfall()) != 0 {
		t.Errorf("shortfall while both are planning = %d lines, want 0", len(got.Shortfall()))
	}

	// Once the weather station is building, the doorbell can only have one.
	if err := app.store.UpdateProject(weather, "Weather station", "", "building", nil); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err = app.store.GetProject(doorbell)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	part := got.Parts[0]
	if part.Free() != 1 || part.Short() != 1 {
		t.Errorf("free/short with a rival building = %d/%d, want 1/1", part.Free(), part.Short())
	}
	if !part.Contested() {
		t.Error("the line should read as contested, not as simply unaffordable")
	}
	if !strings.Contains(part.RivalNote(), "Weather station") {
		t.Errorf("rival note = %q, want it to name the other project", part.RivalNote())
	}

	// The item itself agrees, and does not double-count the shelf.
	it, err := app.store.GetItem(ids[0])
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if it.Committed() != 2 || it.Available() != 1 {
		t.Errorf("committed/available = %d/%d, want 2/1", it.Committed(), it.Available())
	}
	if it.Quantity != 3 {
		t.Errorf("reserving changed the shelf count to %d; it should still be 3", it.Quantity)
	}
}

func TestConsumeAndReturnProjectMoveStockExactlyOnce(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", Quantity: 3},
		Item{Name: "BME280", Quantity: 1},
	)
	id, _ := app.store.CreateProject("Weather station", "")
	app.store.AddProjectPart(id, &ids[0], "", 2, "")
	app.store.AddProjectPart(id, &ids[1], "", 2, "") // more than is on the shelf

	taken, short, err := app.store.ConsumeProject(id)
	if err != nil {
		t.Fatalf("ConsumeProject: %v", err)
	}
	if taken != 3 || short != 1 {
		t.Errorf("taken/short = %d/%d, want 3/1", taken, short)
	}
	for i, want := range []int{1, 0} {
		it, _ := app.store.GetItem(ids[i])
		if it.Quantity != want {
			t.Errorf("item %d quantity after building = %d, want %d", i, it.Quantity, want)
		}
	}

	// Running it again must not deduct a second time.
	if _, _, err := app.store.ConsumeProject(id); err == nil {
		t.Fatal("consuming a built project twice should be refused")
	}
	it, _ := app.store.GetItem(ids[0])
	if it.Quantity != 1 {
		t.Errorf("a refused second consume changed stock to %d", it.Quantity)
	}

	// A consumed project holds nothing: its parts are gone, not reserved.
	if it.Committed() != 0 {
		t.Errorf("a built project still claims %d pieces", it.Committed())
	}

	back, err := app.store.ReturnProject(id)
	if err != nil {
		t.Fatalf("ReturnProject: %v", err)
	}
	if back != 3 {
		t.Errorf("returned %d pieces, want 3", back)
	}
	for i, want := range []int{3, 1} {
		it, _ := app.store.GetItem(ids[i])
		if it.Quantity != want {
			t.Errorf("item %d quantity after returning = %d, want %d — a return must put back what was taken, not what the BOM says", i, it.Quantity, want)
		}
	}
}

func TestLowStockUsesPerItemThreshold(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "Raspberry Pi 5", Quantity: 2, LowStock: 1}, // plenty
		Item{Name: "0805 100nF", Quantity: 40, LowStock: 100},  // nearly out
		Item{Name: "ESP32 devkit", Quantity: 2, LowStock: 2},   // right on the line
		Item{Name: "Odd screw", Quantity: 0, LowStock: 0},      // out entirely
	)
	low, err := app.store.LowStock(0)
	if err != nil {
		t.Fatalf("LowStock: %v", err)
	}
	var names []string
	for _, it := range low {
		names = append(names, it.Name)
	}
	got := strings.Join(names, ", ")
	if strings.Contains(got, "Raspberry Pi") {
		t.Errorf("low stock = %q, want the Pi excluded: two of them is over its threshold of one", got)
	}
	for _, want := range []string{"0805 100nF", "ESP32 devkit", "Odd screw"} {
		if !strings.Contains(got, want) {
			t.Errorf("low stock = %q, want %q in it", got, want)
		}
	}
	// The 40-in-stock capacitor is in more trouble than the screw at zero,
	// because its threshold says so.
	if names[0] != "0805 100nF" {
		t.Errorf("scarcest first = %q, want the capacitor 60 under its threshold", names[0])
	}
}

func TestReservationCountsAgainstLowStock(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 5, LowStock: 2})
	id, _ := app.store.CreateProject("Sensor net", "")
	app.store.AddProjectPart(id, &ids[0], "", 4, "")

	low, _ := app.store.LowStock(0)
	if len(low) != 0 {
		t.Fatalf("five in stock with a threshold of two should not be low, got %d", len(low))
	}
	app.store.UpdateProject(id, "Sensor net", "", "building", nil)

	low, _ = app.store.LowStock(0)
	if len(low) != 1 {
		t.Fatalf("with four of five reserved, one free is under the threshold; got %d low items", len(low))
	}
}

// --- bills of materials -----------------------------------------------------

func TestParseBOMHandlesKiCadExport(t *testing.T) {
	// KiCad writes a preamble above the header and groups by value.
	raw := `Source:,/home/me/proj/proj.kicad_sch
Date:,2026-01-14
Tool:,Eeschema 8.0
Component Count:,12

Ref,Qnty,Value,Cmp name,Footprint,Description
"R1, R2, R3",3,4k7,R,R_0805_2012Metric,Resistor
C1,1,100nF,C,C_0805_2012Metric,Unpolarized capacitor
U1,1,ESP32-WROOM-32,ESP32,Module,Wifi module
`
	lines, err := ParseBOM(raw)
	if err != nil {
		t.Fatalf("ParseBOM: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(lines), lines)
	}
	if lines[0].Quantity != 3 || lines[0].Value != "4k7" {
		t.Errorf("first line = qty %d value %q, want 3 and 4k7", lines[0].Quantity, lines[0].Value)
	}
	if lines[0].Reference != "R1, R2, R3" {
		t.Errorf("designators = %q, want them kept", lines[0].Reference)
	}
	if lines[2].Value != "ESP32-WROOM-32" {
		t.Errorf("third line value = %q", lines[2].Value)
	}
}

func TestParseBOMHandlesSemicolonsAndBareLists(t *testing.T) {
	semi := "Designator;Quantity;Value\nR1;2;10k\nC1;1;1uF\n"
	lines, err := ParseBOM(semi)
	if err != nil {
		t.Fatalf("semicolon BOM: %v", err)
	}
	if len(lines) != 2 || lines[0].Quantity != 2 {
		t.Errorf("semicolon BOM = %+v", lines)
	}

	bare := "ESP32 devkit,2\nBME280,1\n"
	lines, err = ParseBOM(bare)
	if err != nil {
		t.Fatalf("headerless BOM: %v", err)
	}
	if len(lines) != 2 || lines[0].Name != "ESP32 devkit" || lines[0].Quantity != 2 {
		t.Errorf("headerless BOM = %+v", lines)
	}
}

func TestMatchBOMPrefersPartNumberAndSaysWhy(t *testing.T) {
	items := []Item{
		{ID: 1, Name: "ESP32 devkit v1", PartNumber: "ESP32-WROOM-32"},
		{ID: 2, Name: "4k7 resistor", Value: "4.7k"},
		{ID: 3, Name: "BME280 breakout"},
	}
	lines := MatchBOM([]BOMLine{
		{PartNumber: "esp32-wroom-32", Quantity: 1},
		{Value: "4700", Quantity: 3}, // same resistance, written differently
		{Name: "BME280 breakout", Quantity: 1},
		{Name: "Rotary encoder", Quantity: 1},
	}, items)

	if lines[0].Match == nil || lines[0].Match.ID != 1 || lines[0].Why != "part number" {
		t.Errorf("MPN line matched %v via %q", lines[0].Match, lines[0].Why)
	}
	if lines[1].Match == nil || lines[1].Match.ID != 2 || lines[1].Why != "value" {
		t.Errorf("4700 should match the 4.7k resistor by value, got %v via %q", lines[1].Match, lines[1].Why)
	}
	if lines[2].Match == nil || lines[2].Match.ID != 3 {
		t.Errorf("name line matched %v", lines[2].Match)
	}
	if lines[3].Match != nil {
		t.Errorf("an unowned part matched %v; a wrong guess is worse than a blank", lines[3].Match)
	}
}

func TestImportBOMBecomesAParteListWithShortfall(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "ESP32 devkit", PartNumber: "ESP32-WROOM-32", Quantity: 1})
	id, _ := app.store.CreateProject("Sensor node", "")

	lines, err := ParseBOM("mpn,qty,description\nESP32-WROOM-32,2,Wifi module\nBME280,1,Sensor\n")
	if err != nil {
		t.Fatalf("ParseBOM: %v", err)
	}
	items, _ := app.store.ListItems(Query{})
	lines = MatchBOM(lines, items)

	n, err := app.store.ImportBOM(id, lines, false)
	if err != nil {
		t.Fatalf("ImportBOM: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d lines, want 2", n)
	}
	p, _ := app.store.GetProject(id)
	if len(p.Parts) != 2 {
		t.Fatalf("project has %d parts", len(p.Parts))
	}
	short := p.Shortfall()
	if len(short) != 2 {
		t.Fatalf("shortfall = %d lines, want both: one owned but not enough, one not owned at all", len(short))
	}

	// Re-importing with replace must not duplicate the list.
	if _, err := app.store.ImportBOM(id, lines, true); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	p, _ = app.store.GetProject(id)
	if len(p.Parts) != 2 {
		t.Errorf("after a replacing import the list holds %d lines, want 2", len(p.Parts))
	}
}

func TestExportBOMRoundTripsThroughTheImporter(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", PartNumber: "ESP32-WROOM-32", Quantity: 4},
		Item{Name: "BME280 breakout", Quantity: 2},
	)
	id, _ := app.store.CreateProject("Sensor node", "")
	app.store.AddProjectPart(id, &ids[0], "", 1, "U1")
	app.store.AddProjectPart(id, &ids[1], "", 2, "U2")
	app.store.AddProjectPart(id, nil, "Enclosure", 1, "")

	p, _ := app.store.GetProject(id)
	var csv strings.Builder
	for _, row := range p.ExportBOM() {
		csv.WriteString(strings.Join(row, ",") + "\n")
	}

	lines, err := ParseBOM(csv.String())
	if err != nil {
		t.Fatalf("re-reading the export: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("round trip produced %d lines, want 3: %q", len(lines), csv.String())
	}
	items, _ := app.store.ListItems(Query{})
	lines = MatchBOM(lines, items)
	matched := 0
	for _, l := range lines {
		if l.Match != nil {
			matched++
		}
	}
	if matched != 2 {
		t.Errorf("round trip re-matched %d of the 2 owned lines", matched)
	}
}

// --- calculators ------------------------------------------------------------

func TestParseComponentValue(t *testing.T) {
	cases := []struct {
		in    string
		value float64
		unit  string
	}{
		{"4k7", 4700, "R"},
		{"4.7k", 4700, "R"},
		{"4700", 4700, ""},
		{"220R", 220, "R"},
		{"4R7", 4.7, "R"},
		{"10 kΩ", 10000, "R"},
		{"1M", 1e6, "R"},
		{"100nF", 100e-9, "F"},
		{"0.1uF", 100e-9, "F"},
		{"4u7", 4.7e-6, "F"},
		{"10uH", 10e-6, "H"},
	}
	for _, c := range cases {
		v, unit, ok := ParseComponentValue(c.in)
		if !ok {
			t.Errorf("ParseComponentValue(%q) failed", c.in)
			continue
		}
		if math.Abs(v-c.value) > c.value*1e-9 {
			t.Errorf("ParseComponentValue(%q) = %g, want %g", c.in, v, c.value)
		}
		if unit != c.unit {
			t.Errorf("ParseComponentValue(%q) unit = %q, want %q", c.in, unit, c.unit)
		}
	}
	for _, bad := range []string{"", "3.3V", "blue", "ESP32", "5V"} {
		if v, _, ok := ParseComponentValue(bad); ok {
			t.Errorf("ParseComponentValue(%q) = %g, want a refusal — guessing here mislabels parts", bad, v)
		}
	}
}

func TestNearestStandardCrossesDecades(t *testing.T) {
	cases := map[float64]float64{
		330:  330,
		9500: 9100, // 400 below beats 500 above
		9800: 10000,
		4500: 4700, // an exact tie rounds up
		1.02: 1,
	}
	for in, want := range cases {
		if got := NearestStandard(in); math.Abs(got-want) > 0.001 {
			t.Errorf("NearestStandard(%g) = %g, want %g", in, got, want)
		}
	}
}

func TestLEDCalculatorAndItsRefusals(t *testing.T) {
	calc := CalculatorByKey("led")
	if calc == nil {
		t.Fatal("no LED calculator")
	}
	res, err := calc.Run(CalcInputs{"vs": "5", "vf": "2", "i": "10"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// (5 - 2) / 0.01 = 300 ohms, nearest E24 is 300.
	if math.Abs(res.Target-300) > 0.001 {
		t.Errorf("target resistor = %g, want 300", res.Target)
	}
	if res.TargetUnit != "R" {
		t.Errorf("target unit = %q", res.TargetUnit)
	}
	if _, err := calc.Run(CalcInputs{"vs": "2", "vf": "3.2", "i": "20"}); err == nil {
		t.Error("an LED that needs more than the supply should be refused, not answered with a negative resistor")
	}
	if _, err := calc.Run(CalcInputs{"vs": "5"}); err == nil {
		t.Error("missing inputs should be refused")
	}
}

func TestOhmsLawFromAnyTwo(t *testing.T) {
	calc := CalculatorByKey("ohm")
	res, err := calc.Run(CalcInputs{"v": "12", "r": "100"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	joined := ""
	for _, row := range res.Rows {
		joined += row.Label + "=" + row.Value + " "
	}
	if !strings.Contains(joined, "0.12 A") {
		t.Errorf("12 V across 100 ohms should read 0.12 A, got %q", joined)
	}
	if !strings.Contains(joined, "1.44 W") {
		t.Errorf("12 V across 100 ohms should read 1.44 W, got %q", joined)
	}
}

func TestMatchStockOnlyOffersTheRightQuantity(t *testing.T) {
	items := []Item{
		{ID: 1, Name: "330R resistor", Value: "330R", Quantity: 50},
		{ID: 2, Name: "100nF cap", Value: "100nF", Quantity: 100},
		{ID: 3, Name: "ESP32", Value: "3.3V logic"},
		{ID: 4, Name: "300R resistor", Value: "300", Quantity: 10},
	}
	got := MatchStock(items, 300, "R", 15, 5)
	if len(got) != 2 {
		t.Fatalf("matched %d resistors near 300R, want the 300 and the 330: %+v", len(got), got)
	}
	if got[0].Item.ID != 4 {
		t.Errorf("closest first = item %d, want the exact 300R", got[0].Item.ID)
	}
	if MatchStock(items, 300, "R", 15, 5)[1].Item.ID != 1 {
		t.Error("330R should be the second match")
	}
	// A capacitance target must not offer resistors.
	for _, m := range MatchStock(items, 100e-9, "F", 20, 5) {
		if m.Item.ID != 2 {
			t.Errorf("a farad target matched %q", m.Item.Name)
		}
	}
}

// --- pin budget -------------------------------------------------------------

func TestPinBudgetSharesBusesAndFlagsConflicts(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{
			Name:       "ESP32 devkit",
			Quantity:   1,
			Interfaces: []string{"I2C", "SPI", "GPIO", "3V3 logic"},
			Specs:      []Spec{{Name: "GPIO", Value: "24 usable"}},
		},
		Item{Name: "BME280", Quantity: 2, Interfaces: []string{"I2C", "3V3 logic"},
			Specs: []Spec{{Name: "I2C address", Value: "0x76"}}},
		Item{Name: "BMP280", Quantity: 1, Interfaces: []string{"I2C", "3V3 logic"},
			Specs: []Spec{{Name: "I2C address", Value: "0x76"}}},
		Item{Name: "SD card slot", Quantity: 1, Interfaces: []string{"SPI", "3V3 logic"}},
		Item{Name: "Relay board", Quantity: 4, Interfaces: []string{"GPIO", "5V logic"}},
	)
	id, _ := app.store.CreateProject("Greenhouse", "")
	for i, qty := range []int{1, 2, 1, 1, 4} {
		app.store.AddProjectPart(id, &ids[i], "", qty, "")
	}
	if err := app.store.UpdateProject(id, "Greenhouse", "", "planning", &ids[0]); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	p, err := app.store.GetProject(id)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	b := BudgetPins(p)

	if b.Capacity != 24 || b.CapacityFrom != "GPIO" {
		t.Errorf("capacity = %d from %q, want 24 from the GPIO spec", b.Capacity, b.CapacityFrom)
	}
	// I2C: 2 shared. SPI: 3 shared + 1 CS. GPIO: 4 relays.
	if b.Total != 10 {
		t.Errorf("total pins = %d, want 10 (2 I2C + 3 SPI + 1 CS + 4 relay GPIO)", b.Total)
	}
	var i2c *BusUse
	for i := range b.Buses {
		if b.Buses[i].Name == "I2C" {
			i2c = &b.Buses[i]
		}
	}
	if i2c == nil || i2c.Pins != 2 || len(i2c.Devices) != 2 {
		t.Errorf("I2C bus = %+v, want two devices sharing two pins", i2c)
	}

	conflicts := strings.Join(b.Conflicts, " | ")
	if !strings.Contains(conflicts, "0x76") {
		t.Errorf("conflicts = %q, want the duplicate I2C address named", conflicts)
	}
	if !strings.Contains(conflicts, "Relay board") || !strings.Contains(conflicts, "level shifter") {
		t.Errorf("conflicts = %q, want the 5 V relay board on a 3.3 V board flagged", conflicts)
	}
}

func TestPinBudgetSaysWhenItDoesNotKnow(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "Mystery board", Quantity: 1, Interfaces: []string{"I2C"}},
		Item{Name: "BME280", Quantity: 1, Interfaces: []string{"I2C"}},
	)
	id, _ := app.store.CreateProject("Thing", "")
	app.store.AddProjectPart(id, &ids[1], "", 1, "")
	app.store.UpdateProject(id, "Thing", "", "planning", &ids[0])

	p, _ := app.store.GetProject(id)
	b := BudgetPins(p)
	if b.Capacity != 0 {
		t.Errorf("capacity = %d, want 0 — the board does not say and inventing one would be worse", b.Capacity)
	}
	if len(b.Notes) == 0 || !strings.Contains(strings.Join(b.Notes, " "), "pin count") {
		t.Errorf("notes = %v, want an explanation of why there is no budget", b.Notes)
	}
	if b.Over() {
		t.Error("an unknown capacity must not read as over budget")
	}
}

// --- build log --------------------------------------------------------------

func TestBuildLogKeepsEntriesNewestFirst(t *testing.T) {
	app := newTestApp(t)
	id, _ := app.store.CreateProject("Weather station", "")

	first, err := app.store.AddLogEntry(id, "Soldered the headers on.", "ben")
	if err != nil {
		t.Fatalf("AddLogEntry: %v", err)
	}
	if _, err := app.store.AddLogEntry(id, "I2C works at 400kHz.\nSecond line.", "ben"); err != nil {
		t.Fatalf("AddLogEntry: %v", err)
	}
	if err := app.store.AddLogPhoto(first, "abcdef0123456789.jpg"); err != nil {
		t.Fatalf("AddLogPhoto: %v", err)
	}

	entries, err := app.store.LogEntries(id)
	if err != nil {
		t.Fatalf("LogEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if !strings.HasPrefix(entries[0].Body, "I2C works") {
		t.Errorf("newest entry = %q, want the most recent first", entries[0].Body)
	}
	if len(entries[0].Lines()) != 2 {
		t.Errorf("a two-line entry rendered as %d lines", len(entries[0].Lines()))
	}
	if len(entries[1].Photos) != 1 {
		t.Errorf("the first entry lost its photo")
	}

	projectID, files, err := app.store.DeleteLogEntry(first)
	if err != nil {
		t.Fatalf("DeleteLogEntry: %v", err)
	}
	if projectID != id || len(files) != 1 {
		t.Errorf("delete reported project %d and %d files, want %d and 1", projectID, len(files), id)
	}
}

// --- accounts and audit -----------------------------------------------------

func TestPasswordHashingRejectsWrongPasswords(t *testing.T) {
	hash, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if strings.Contains(hash, "correct") {
		t.Fatal("the stored hash contains the password")
	}
	if !verifyPassword(hash, "correct horse battery") {
		t.Error("the right password did not verify")
	}
	for _, wrong := range []string{"", "correct horse batter", "Correct horse battery"} {
		if verifyPassword(hash, wrong) {
			t.Errorf("%q verified against a different password", wrong)
		}
	}
	// A hash in a format this build does not recognise must fail closed rather
	// than falling back to a weaker comparison.
	for _, bad := range []string{"plaintext", "md5$x$y", "pbkdf2-sha256$0$$", ""} {
		if verifyPassword(bad, "anything") {
			t.Errorf("unrecognised hash %q accepted a password", bad)
		}
	}
}

func TestAccountsAndAdminProtection(t *testing.T) {
	app := newTestApp(t)

	// The first account is always an administrator, whatever was asked for.
	id, err := app.store.CreateUser("ben", "hunter2hunter2", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, _ := app.store.GetUser(id)
	if !u.Admin {
		t.Error("the first account must be an administrator, or nobody can manage the install")
	}
	if _, err := app.store.CreateUser("ben", "anotherpassword", false); err == nil {
		t.Error("duplicate usernames should be refused")
	}
	if _, err := app.store.CreateUser("sam", "short", false); err == nil {
		t.Error("a short password should be refused")
	}
	if _, err := app.store.CreateUser("bad name!", "longenoughpassword", false); err == nil {
		t.Error("a username with punctuation should be refused")
	}

	if _, ok := app.store.Authenticate("ben", "hunter2hunter2"); !ok {
		t.Error("the right password did not authenticate")
	}
	if _, ok := app.store.Authenticate("ben", "wrong"); ok {
		t.Error("a wrong password authenticated")
	}
	if _, ok := app.store.Authenticate("nobody", "hunter2hunter2"); ok {
		t.Error("an account that does not exist authenticated")
	}

	// The last administrator cannot be removed or demoted.
	if err := app.store.DeleteUser(id); err == nil {
		t.Error("deleting the only administrator should be refused")
	}
	if err := app.store.SetAdmin(id, false); err == nil {
		t.Error("demoting the only administrator should be refused")
	}
	second, err := app.store.CreateUser("sam", "longenoughpassword", true)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := app.store.DeleteUser(id); err != nil {
		t.Errorf("with a second administrator, removing the first should work: %v", err)
	}
	_ = second
}

func TestAuditTrailRecordsAndFilters(t *testing.T) {
	app := newTestApp(t)
	app.store.Record("ben", "added an item", "item", 7, "ESP32")
	app.store.Record("sam", "changed a project", "project", 2, "Weather station")
	app.store.Record("", "did something", "system", 0, "")

	all, err := app.store.AuditTrail(0, "", 0)
	if err != nil {
		t.Fatalf("AuditTrail: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3", len(all))
	}
	items, _ := app.store.AuditTrail(0, "item", 0)
	if len(items) != 1 || items[0].Actor != "ben" {
		t.Errorf("filtered trail = %+v", items)
	}
	if items[0].Link() != "/items/7" {
		t.Errorf("item link = %q, want /items/7", items[0].Link())
	}
	// An unattributed change is still recorded, just as "someone".
	var anon bool
	for _, e := range all {
		if e.Entity == "system" {
			anon = e.Actor == "someone"
		}
	}
	if !anon {
		t.Error("an unattributed change should be recorded as someone, not blank")
	}
}

func TestSignInWithAnAccountAttributesTheChange(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.store.CreateUser("ben", "hunter2hunter2", true); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	auth, err := NewAuth("", filepath.Join(app.cfg.DataDir, "session.key"), app.store)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	app.auth = auth
	if !auth.Enabled() || !auth.Accounts() {
		t.Fatal("adding an account should turn the door on even with no shared password")
	}

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// Anonymous requests are turned away now.
	res, err := client.Get(srv.URL + "/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous GET /items = %d, want a redirect to sign in", res.StatusCode)
	}

	res, err = client.PostForm(srv.URL+"/login", map[string][]string{
		"username": {"ben"}, "password": {"hunter2hunter2"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want a redirect", res.StatusCode)
	}

	res, err = client.PostForm(srv.URL+"/items", map[string][]string{
		"name": {"ESP32 devkit"}, "quantity": {"3"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("creating an item = %d", res.StatusCode)
	}

	trail, _ := app.store.AuditTrail(0, "item", 0)
	if len(trail) != 1 || trail[0].Actor != "ben" {
		t.Errorf("audit trail = %+v, want the change attributed to ben", trail)
	}
	// A wrong password gets nowhere.
	res, _ = client.PostForm(srv.URL+"/login", map[string][]string{
		"username": {"ben"}, "password": {"nope"},
	})
	res.Body.Close()
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("a wrong password redirected to %q, want an error", loc)
	}
}

// --- backup and restore -----------------------------------------------------

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", Quantity: 4, Location: "Drawer 3", LowStock: 5},
		Item{Name: "BME280", Quantity: 2},
	)
	projectID, _ := app.store.CreateProject("Weather station", "notes here")
	app.store.AddProjectPart(projectID, &ids[0], "", 2, "U1")
	app.store.AddLogEntry(projectID, "Soldered the headers.", "ben")
	app.store.SetPrice(ids[0], Price{Source: "Adafruit", Amount: 12.5, Currency: "USD"})
	app.store.Record("ben", "added an item", "item", ids[0], "ESP32 devkit")

	// A real photo file, so the archive has something to carry.
	photoName, err := app.photos.Save(bytes.NewReader(testJPEG(t, 40, 30, 0)), "board.jpg")
	if err != nil {
		t.Fatalf("Save photo: %v", err)
	}
	if err := app.store.AddPhoto(ids[0], photoName); err != nil {
		t.Fatalf("AddPhoto: %v", err)
	}

	var archive bytes.Buffer
	if err := app.WriteBackup(&archive); err != nil {
		t.Fatalf("WriteBackup: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatalf("the backup is not a readable zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if !contains(names, "inventory.db") {
		t.Fatalf("backup holds %v, want the database in it", names)
	}
	if !contains(names, "photos/"+photoName) {
		t.Fatalf("backup holds %v, want the photo in it", names)
	}

	// Wreck the live install, then restore over the top of it.
	if _, err := app.store.DeleteItem(ids[0]); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if err := app.store.DeleteProject(projectID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	seed(t, app.store, Item{Name: "Something added after the backup", Quantity: 1})
	os.Remove(app.photos.Path(photoName))

	rep, err := app.Restore(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep.Photos != 1 {
		t.Errorf("restored %d photos, want 1", rep.Photos)
	}

	items, err := app.store.ListItems(Query{Sort: "name"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("after restore there are %d items, want the 2 from the backup — a restore replaces, it does not merge", len(items))
	}
	var esp *Item
	for i := range items {
		if items[i].Name == "ESP32 devkit" {
			esp = &items[i]
		}
	}
	if esp == nil {
		t.Fatal("the restored inventory lost the ESP32")
	}
	if esp.Quantity != 4 || esp.Location != "Drawer 3" || esp.LowStock != 5 {
		t.Errorf("restored item = qty %d, %q, threshold %d", esp.Quantity, esp.Location, esp.LowStock)
	}
	if len(esp.Photos) != 1 {
		t.Errorf("restored item has %d photos", len(esp.Photos))
	}
	if len(esp.Prices) != 1 {
		t.Errorf("restored item has %d prices", len(esp.Prices))
	}
	if _, err := os.Stat(app.photos.Path(photoName)); err != nil {
		t.Errorf("the photo file was not put back: %v", err)
	}

	projects, _ := app.store.ListProjects()
	if len(projects) != 1 || projects[0].Name != "Weather station" {
		t.Fatalf("restored projects = %+v", projects)
	}
	entries, _ := app.store.LogEntries(projects[0].ID)
	if len(entries) != 1 {
		t.Errorf("the build log did not survive the restore")
	}
	trail, _ := app.store.AuditTrail(0, "", 0)
	if len(trail) == 0 {
		t.Error("the audit trail did not survive the restore")
	}
}

func TestRestoreRefusesRubbish(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.Restore(strings.NewReader("this is not a zip file")); err == nil {
		t.Error("restoring a text file should be refused")
	}

	// A valid zip with no database in it.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("readme.txt")
	w.Write([]byte("hello"))
	zw.Close()
	if _, err := app.Restore(bytes.NewReader(buf.Bytes())); err == nil {
		t.Error("restoring a zip with no database should be refused")
	}

	// Whatever was there must still be there.
	seed(t, app.store, Item{Name: "Still here", Quantity: 1})
	app.Restore(strings.NewReader("nonsense"))
	items, _ := app.store.ListItems(Query{})
	if len(items) != 1 {
		t.Errorf("a refused restore left %d items, want the 1 that was there", len(items))
	}
}

func TestHealthReportsDriftAndSchema(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 1})

	// A row pointing at a file that is not there.
	if err := app.store.AddPhoto(ids[0], "deadbeefdeadbeef.jpg"); err != nil {
		t.Fatalf("AddPhoto: %v", err)
	}
	// A file nothing points at.
	orphan, err := app.photos.Save(bytes.NewReader(testJPEG(t, 40, 30, 0)), "loose.jpg")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	h := app.Health()
	if h.SchemaVersion != len(migrations) || h.SchemaExpected != len(migrations) {
		t.Errorf("schema = %d of %d, want both at %d", h.SchemaVersion, h.SchemaExpected, len(migrations))
	}
	if h.Integrity != "ok" {
		t.Errorf("integrity = %q", h.Integrity)
	}
	if !contains(h.Missing, "deadbeefdeadbeef.jpg") {
		t.Errorf("missing = %v, want the dangling reference reported", h.Missing)
	}
	if !contains(h.Orphans, orphan) {
		t.Errorf("orphans = %v, want the loose file reported", h.Orphans)
	}
	if h.Healthy() {
		t.Error("an install with a dangling photo reference should not report itself healthy")
	}

	if n := app.SweepOrphans(); n != 1 {
		t.Errorf("swept %d files, want 1", n)
	}
	if _, err := os.Stat(app.photos.Path(orphan)); err == nil {
		t.Error("the orphan is still on disk after a sweep")
	}
}

// --- pages render -----------------------------------------------------------

func TestNewPagesRender(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{
		Name: "ESP32 devkit", Quantity: 3, Value: "4k7",
		Interfaces: []string{"I2C", "3V3 logic"},
		Specs:      []Spec{{Name: "GPIO", Value: "24"}},
	})
	projectID, _ := app.store.CreateProject("Weather station", "")
	app.store.AddProjectPart(projectID, &ids[0], "", 2, "")
	app.store.UpdateProject(projectID, "Weather station", "", "building", &ids[0])
	app.store.AddLogEntry(projectID, "Soldered the headers.", "ben")

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	for _, path := range []string{
		"/", "/items", "/projects", "/tools", "/admin", "/activity",
		"/tools?calc=led&vs=5&vf=2&i=10",
		"/tools?calc=rc&r=10k&c=100n",
		"/tools?calc=trace&i=2&rise=10&copper=1&layer=external",
		"/tools?calc=led&vs=2&vf=3.3&i=20", // the refusal path still has to render
		"/items/1",
		"/projects/1",
		"/projects/1/bom.csv",
		"/projects/1/shopping-list.csv",
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readAll(t, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.StatusCode)
			continue
		}
		if strings.Contains(body, "<no value>") || strings.Contains(body, "ERROR:") {
			t.Errorf("GET %s rendered a template error:\n%s", path, firstLines(body, 40))
		}
	}
}

func TestBOMImportFlowShowsBeforeItWrites(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "ESP32 devkit", PartNumber: "ESP32-WROOM-32", Quantity: 1})
	projectID, _ := app.store.CreateProject("Sensor node", "")

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	bom := "Ref,Qnty,Value,MPN\nU1,2,ESP32,ESP32-WROOM-32\nU2,1,BME280,BME280\n"

	// Step one previews and writes nothing.
	res, err := http.PostForm(srv.URL+"/projects/1/bom", map[string][]string{"bom": {bom}})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	body := readAll(t, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d", res.StatusCode)
	}
	if !strings.Contains(body, "matched on part number") {
		t.Errorf("the preview did not say how it matched:\n%s", firstLines(body, 60))
	}
	p, _ := app.store.GetProject(projectID)
	if len(p.Parts) != 0 {
		t.Fatalf("the preview wrote %d parts; it must write nothing", len(p.Parts))
	}

	// Step two commits.
	res, err = http.PostForm(srv.URL+"/projects/1/bom",
		map[string][]string{"bom": {bom}, "confirm": {"1"}})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	res.Body.Close()
	p, _ = app.store.GetProject(projectID)
	if len(p.Parts) != 2 {
		t.Fatalf("after confirming, the project has %d parts, want 2", len(p.Parts))
	}
	if p.Parts[0].Item == nil || p.Parts[0].Item.PartNumber != "ESP32-WROOM-32" {
		t.Errorf("the matched line did not become a reference to the owned item")
	}
	if p.Parts[1].Item != nil {
		t.Errorf("the unmatched line should stay a name, not point at something")
	}
}

func TestBuildAndReturnThroughTheWeb(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 3})
	projectID, _ := app.store.CreateProject("Weather station", "")
	app.store.AddProjectPart(projectID, &ids[0], "", 2, "")

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/projects/1/consume", nil)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	res.Body.Close()
	it, _ := app.store.GetItem(ids[0])
	if it.Quantity != 1 {
		t.Fatalf("after building, stock = %d, want 1", it.Quantity)
	}

	res, err = http.PostForm(srv.URL+"/projects/1/return", nil)
	if err != nil {
		t.Fatalf("return: %v", err)
	}
	res.Body.Close()
	it, _ = app.store.GetItem(ids[0])
	if it.Quantity != 3 {
		t.Errorf("after returning, stock = %d, want 3", it.Quantity)
	}
}

func TestUploadedBackupRestoresThroughTheWeb(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 4})

	var archive bytes.Buffer
	if err := app.WriteBackup(&archive); err != nil {
		t.Fatalf("WriteBackup: %v", err)
	}
	seed(t, app.store, Item{Name: "Added after the backup", Quantity: 1})

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	part, _ := mw.CreateFormFile("backup", "backup.zip")
	part.Write(archive.Bytes())
	mw.WriteField("confirm", "replace")
	mw.Close()

	res, err := http.Post(srv.URL+"/admin/restore", mw.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	res.Body.Close()
	// http.Post follows the redirect, so the outcome is in the final URL.
	if final := res.Request.URL.String(); strings.Contains(final, "error=") {
		t.Fatalf("restore failed: %s", final)
	}
	items, _ := app.store.ListItems(Query{})
	if len(items) != 1 || items[0].Name != "ESP32 devkit" {
		t.Errorf("after restoring, the inventory is %+v", items)
	}
}

func TestRestoreRefusedWithoutTheConfirmation(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "Keep me", Quantity: 1})

	var archive bytes.Buffer
	app.WriteBackup(&archive)

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	body := new(bytes.Buffer)
	mw := multipart.NewWriter(body)
	part, _ := mw.CreateFormFile("backup", "backup.zip")
	part.Write(archive.Bytes())
	mw.Close() // no confirm field

	res, err := http.Post(srv.URL+"/admin/restore", mw.FormDataContentType(), body)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	res.Body.Close()
	if final := res.Request.URL.String(); !strings.Contains(final, "error=") {
		t.Errorf("a restore without the typed confirmation went ahead (landed on %q)", final)
	}
	items, _ := app.store.ListItems(Query{})
	if len(items) != 1 || items[0].Name != "Keep me" {
		t.Errorf("a refused restore still changed the inventory: %+v", items)
	}
}

// --- helpers ----------------------------------------------------------------

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestUpgradeFromAnOlderSchema walks a database up one migration at a time,
// writes rows at an older version, and then lets the runner finish. Migrations
// are the one thing that cannot be tested by only ever creating fresh
// databases: the bugs live in what happens to data that is already there.
func TestUpgradeFromAnOlderSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer raw.Close()

	// Stop at the schema as it was before this batch of features.
	const stopAt = 9
	if err := migrateTo(raw, stopAt); err != nil {
		t.Fatalf("migrate to %d: %v", stopAt, err)
	}
	now := "2026-01-01T00:00:00Z"
	_, err = raw.Exec(`INSERT INTO items (name, category, quantity, location, part_number,
		value, link, notes, created_at, updated_at)
		VALUES ('ESP32 devkit','board',4,'Drawer 3','ESP32-WROOM-32','','','',?,?)`, now, now)
	if err != nil {
		t.Fatalf("insert at the old schema: %v", err)
	}
	_, err = raw.Exec(`INSERT INTO projects (name, notes, status, created_at, updated_at)
		VALUES ('Weather station','','building',?,?)`, now, now)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	raw.Close()

	// openDB runs the rest.
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer db.Close()

	var version int
	db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != len(migrations) {
		t.Fatalf("after upgrading, schema is %d, want %d", version, len(migrations))
	}

	store := &Store{db: db}
	items, err := store.ListItems(Query{})
	if err != nil {
		t.Fatalf("ListItems after upgrade: %v", err)
	}
	if len(items) != 1 || items[0].Name != "ESP32 devkit" || items[0].Quantity != 4 {
		t.Fatalf("the row written at the old schema did not survive: %+v", items)
	}
	if items[0].LowStock != defaultLowStock {
		t.Errorf("upgraded item threshold = %d, want the default %d rather than zero",
			items[0].LowStock, defaultLowStock)
	}
	projects, err := store.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects after upgrade: %v", err)
	}
	if len(projects) != 1 || projects[0].Consumed() {
		t.Errorf("upgraded project = %+v, want one that has not been consumed", projects)
	}
	if _, err := store.CountUsers(); err != nil {
		t.Errorf("the accounts table was not created: %v", err)
	}
}

// migrateTo runs the first n migrations, mirroring the runner exactly.
func migrateTo(db *sql.DB, n int) error {
	for i := 0; i < n; i++ {
		m := migrations[i]
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if strings.TrimSpace(m.sql) != "" {
			if _, err := tx.Exec(m.sql); err != nil {
				tx.Rollback()
				return err
			}
		}
		if m.fn != nil {
			if err := m.fn(tx); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.Exec("PRAGMA user_version = " + strconv.Itoa(i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
