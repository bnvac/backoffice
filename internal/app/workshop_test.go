package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bnvac/backoffice/internal/media"
)

// --- orders and receiving ---------------------------------------------------

func TestReceivingAnOrderPutsStockOnTheShelfOnce(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 1})

	orderID, err := app.store.CreateOrder(Order{
		Source: "LCSC", Status: "ordered", Currency: "USD",
		PlacedAt: time.Now().AddDate(0, 0, -19),
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := app.store.AddOrderLine(orderID, OrderLine{ItemID: &ids[0], Quantity: 5, UnitPrice: 3.20}); err != nil {
		t.Fatalf("AddOrderLine: %v", err)
	}
	if err := app.store.AddOrderLine(orderID, OrderLine{Name: "Enclosure", Quantity: 1, UnitPrice: 8}); err != nil {
		t.Fatalf("AddOrderLine: %v", err)
	}

	n, err := app.store.ReceiveOrder(orderID)
	if err != nil {
		t.Fatalf("ReceiveOrder: %v", err)
	}
	if n != 6 {
		t.Errorf("received %d pieces, want 6", n)
	}
	it, _ := app.store.GetItem(ids[0])
	if it.Quantity != 6 {
		t.Fatalf("stock after receiving = %d, want 1 + 5", it.Quantity)
	}

	// What you actually paid becomes the recorded price, and how long it
	// actually took becomes the recorded lead time.
	if len(it.Prices) != 1 {
		t.Fatalf("receiving recorded %d prices, want 1", len(it.Prices))
	}
	if it.Prices[0].Source != "LCSC" || math.Abs(it.Prices[0].Amount-3.20) > 0.001 {
		t.Errorf("recorded price = %+v", it.Prices[0])
	}
	if it.Prices[0].LeadDays != 19 {
		t.Errorf("recorded lead time = %d days, want the 19 it really took", it.Prices[0].LeadDays)
	}

	// Receiving twice must not double the stock.
	if _, err := app.store.ReceiveOrder(orderID); err == nil {
		t.Error("receiving an arrived order twice should be refused")
	}
	it, _ = app.store.GetItem(ids[0])
	if it.Quantity != 6 {
		t.Errorf("a refused second receive changed stock to %d", it.Quantity)
	}

	back, err := app.store.UnreceiveOrder(orderID)
	if err != nil {
		t.Fatalf("UnreceiveOrder: %v", err)
	}
	if back != 6 {
		t.Errorf("un-received %d, want 6", back)
	}
	it, _ = app.store.GetItem(ids[0])
	if it.Quantity != 1 {
		t.Errorf("stock after un-receiving = %d, want the 1 that was there before", it.Quantity)
	}
}

func TestOrderFromShortfallGroupsByShop(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", Quantity: 0},
		Item{Name: "BME280", Quantity: 0},
		Item{Name: "0805 4k7", Quantity: 0},
	)
	app.store.SetPrice(ids[0], Price{Source: "Adafruit", Amount: 12})
	app.store.SetPrice(ids[1], Price{Source: "Adafruit", Amount: 9})
	app.store.SetPrice(ids[2], Price{Source: "LCSC", Amount: 0.01})

	projectID, _ := app.store.CreateProject("Weather station", "")
	for i, qty := range []int{1, 2, 100} {
		app.store.AddProjectPart(projectID, &ids[i], "", qty, "")
	}
	app.store.AddProjectPart(projectID, nil, "Enclosure", 1, "")

	orderIDs, err := app.store.OrderFromShortfall(projectID)
	if err != nil {
		t.Fatalf("OrderFromShortfall: %v", err)
	}
	// Adafruit, LCSC, and one bucket for the unpriced enclosure.
	if len(orderIDs) != 3 {
		t.Fatalf("drafted %d orders, want one per shop plus one for the unpriced line", len(orderIDs))
	}

	bySource := map[string]Order{}
	for _, id := range orderIDs {
		o, err := app.store.GetOrder(id)
		if err != nil {
			t.Fatalf("GetOrder: %v", err)
		}
		bySource[o.Source] = o
	}
	ada, ok := bySource["Adafruit"]
	if !ok {
		t.Fatalf("no Adafruit order among %v", bySource)
	}
	if len(ada.Lines) != 2 {
		t.Errorf("Adafruit order has %d lines, want the two parts it sells", len(ada.Lines))
	}
	if ada.Total() != 12+2*9 {
		t.Errorf("Adafruit total = %v, want the shortfall quantities priced", ada.Total())
	}
	lcsc := bySource["LCSC"]
	if len(lcsc.Lines) != 1 || lcsc.Lines[0].Quantity != 100 {
		t.Errorf("LCSC order = %+v, want 100 resistors", lcsc.Lines)
	}
	// Nothing has actually been bought, so nothing is on the shelf yet.
	it, _ := app.store.GetItem(ids[0])
	if it.Quantity != 0 {
		t.Errorf("drafting an order changed stock to %d", it.Quantity)
	}
}

func TestIncomingStockIsCountedSeparately(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 0})

	draft, _ := app.store.CreateOrder(Order{Source: "LCSC", Status: "draft"})
	app.store.AddOrderLine(draft, OrderLine{ItemID: &ids[0], Quantity: 3})
	placed, _ := app.store.CreateOrder(Order{Source: "Adafruit", Status: "ordered"})
	app.store.AddOrderLine(placed, OrderLine{ItemID: &ids[0], Quantity: 2})

	incoming, err := app.store.Incoming()
	if err != nil {
		t.Fatalf("Incoming: %v", err)
	}
	if incoming[ids[0]] != 2 {
		t.Errorf("incoming = %d, want only the 2 actually ordered — a draft is not on its way",
			incoming[ids[0]])
	}
}

func TestLeadTimesMeasureWhatShopsActuallyDid(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "0805 4k7", Quantity: 0})
	app.store.SetPrice(ids[0], Price{Source: "LCSC", Amount: 0.01, LeadDays: 12})

	for _, days := range []int{18, 20} {
		id, _ := app.store.CreateOrder(Order{
			Source: "LCSC", Status: "ordered", PlacedAt: time.Now().AddDate(0, 0, -days),
		})
		app.store.AddOrderLine(id, OrderLine{ItemID: &ids[0], Quantity: 10})
		if _, err := app.store.ReceiveOrder(id); err != nil {
			t.Fatalf("ReceiveOrder: %v", err)
		}
	}

	leads, err := app.store.LeadTimes()
	if err != nil {
		t.Fatalf("LeadTimes: %v", err)
	}
	if len(leads) != 1 {
		t.Fatalf("got %d shops, want 1", len(leads))
	}
	l := leads[0]
	if l.Orders != 2 || l.MeanDays != 19 || l.MinDays != 18 || l.MaxDays != 20 {
		t.Errorf("lead times = %+v, want 2 orders averaging 19 days", l)
	}
	if !strings.Contains(l.Drift(), "slower") {
		t.Errorf("drift = %q, want it to say LCSC is slower than the 12 days quoted", l.Drift())
	}
}

// --- scanning ---------------------------------------------------------------

func TestScanResolvesEveryKindOfLabel(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", PartNumber: "ESP32-WROOM-32", Quantity: 3},
		Item{Name: "Mystery module", Quantity: 1},
	)

	cases := []struct {
		code string
		want int64
		why  string
	}{
		{fmt.Sprintf("http://box.local:8080/items/%d", ids[0]), ids[0], "a QR code from this app"},
		{fmt.Sprintf("BO-%d", ids[1]), ids[1], "the barcode fallback for a part with no MPN"},
		{"ESP32-WROOM-32", ids[0], "a manufacturer's own barcode"},
		{"esp32-wroom-32", ids[0], "the same, in the wrong case"},
		{"Mystery module", ids[1], "a unique name"},
	}
	for _, c := range cases {
		it, problem, ok := app.resolveCode(c.code)
		if !ok {
			t.Errorf("%s (%q) did not resolve: %s", c.why, c.code, problem)
			continue
		}
		if it.ID != c.want {
			t.Errorf("%s (%q) resolved to item %d, want %d", c.why, c.code, it.ID, c.want)
		}
	}

	for _, bad := range []string{"", "BO-9999", "NOT-A-PART", "http://box.local/items/999"} {
		if it, problem, ok := app.resolveCode(bad); ok {
			t.Errorf("%q resolved to %q; it should have failed", bad, it.Name)
		} else if problem == "" {
			t.Errorf("%q failed without saying why", bad)
		}
	}
}

func TestScanLookupAnswersJSONAndRedirects(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "ESP32 devkit", PartNumber: "ESP32-WROOM-32", Quantity: 3})

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/lookup?code=ESP32-WROOM-32", nil)
	req.Header.Set("Accept", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	body := readAll(t, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("lookup = %d: %s", res.StatusCode, body)
	}
	for _, want := range []string{`"name":"ESP32 devkit"`, `"quantity":3`, fmt.Sprintf(`"url":"/items/%d"`, ids[0])} {
		if !strings.Contains(body, want) {
			t.Errorf("lookup JSON = %s, want %s in it", body, want)
		}
	}

	// A code that matches nothing is a 404 with an explanation, not a 200.
	req, _ = http.NewRequest("GET", srv.URL+"/lookup?code=NOPE", nil)
	req.Header.Set("Accept", "application/json")
	res, _ = http.DefaultClient.Do(req)
	body = readAll(t, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound || !strings.Contains(body, "error") {
		t.Errorf("unknown code = %d %s, want a 404 with a reason", res.StatusCode, body)
	}

	// Without the JSON header it redirects, so a typed code in a plain form works.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, _ = client.Get(srv.URL + "/lookup?code=ESP32-WROOM-32")
	res.Body.Close()
	if loc := res.Header.Get("Location"); loc != fmt.Sprintf("/items/%d", ids[0]) {
		t.Errorf("plain lookup redirected to %q", loc)
	}
}

// --- pin map ----------------------------------------------------------------

func TestPinMapCatchesADoubleBookedPin(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "BME280", Quantity: 1},
		Item{Name: "SD card slot", Quantity: 1},
	)
	projectID, _ := app.store.CreateProject("Greenhouse", "")

	for _, p := range []PinAssignment{
		{ProjectID: projectID, Pin: "GPIO21", ItemID: &ids[0], Signal: "SDA"},
		{ProjectID: projectID, Pin: "gpio 21", ItemID: &ids[1], Signal: "CS"},
		{ProjectID: projectID, Pin: "3V3", ItemID: &ids[0], Signal: "VCC"},
		{ProjectID: projectID, Pin: "3V3", ItemID: &ids[1], Signal: "VCC"},
	} {
		if err := app.store.AddPinAssignment(p); err != nil {
			t.Fatalf("AddPinAssignment: %v", err)
		}
	}

	pins, err := app.store.PinAssignments(projectID)
	if err != nil {
		t.Fatalf("PinAssignments: %v", err)
	}
	conflicts := PinConflicts(pins)
	if len(conflicts) != 1 {
		t.Fatalf("got %d conflicts, want exactly one: %v", len(conflicts), conflicts)
	}
	if !strings.Contains(conflicts[0], "GPIO21") && !strings.Contains(conflicts[0], "gpio 21") {
		t.Errorf("conflict = %q, want it to name the pin", conflicts[0])
	}
	if !strings.Contains(conflicts[0], "BME280") || !strings.Contains(conflicts[0], "SD card slot") {
		t.Errorf("conflict = %q, want both things fighting over it named", conflicts[0])
	}

	// A rail shared by everything is not a conflict, which is the whole reason
	// the exemption exists.
	if n := AssignedPins(pins); n != 1 {
		t.Errorf("assigned pins = %d, want 1: 3V3 is a rail, not a pin budget line", n)
	}
}

func TestPinNormalisationTreatsTheSamePinAsTheSamePin(t *testing.T) {
	same := [][]string{
		{"GPIO21", "gpio 21", "gpio-21", "IO21", "io_21"},
		{"D4", "d4"},
		{"A0", "a0"},
		{"pin 7", "PIN7", "p7"},
	}
	for _, group := range same {
		first := normalisePin(group[0])
		for _, spelling := range group[1:] {
			if got := normalisePin(spelling); got != first {
				t.Errorf("normalisePin(%q) = %q, want %q so it matches %q", spelling, got, first, group[0])
			}
		}
	}
	// Different pins must stay different.
	if normalisePin("GPIO2") == normalisePin("GPIO21") {
		t.Error("GPIO2 and GPIO21 collapsed to the same pin")
	}
	if normalisePin("D4") == normalisePin("A4") {
		t.Error("D4 and A4 collapsed to the same pin")
	}
}

func TestSuggestPinsProposesBusLines(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", Quantity: 1, Interfaces: []string{"I2C", "SPI"}},
		Item{Name: "BME280", Quantity: 1, Interfaces: []string{"I2C"}},
	)
	projectID, _ := app.store.CreateProject("Greenhouse", "")
	app.store.AddProjectPart(projectID, &ids[0], "", 1, "")
	app.store.AddProjectPart(projectID, &ids[1], "", 1, "")
	app.store.UpdateProject(projectID, "Greenhouse", "", "planning", &ids[0])

	p, _ := app.store.GetProject(projectID)
	ideas := SuggestPins(p, nil)

	var signals []string
	for _, i := range ideas {
		if i.Part != "BME280" {
			t.Errorf("suggested a connection for %q; the controller does not wire to itself", i.Part)
		}
		signals = append(signals, i.Signal)
	}
	if got := strings.Join(signals, ","); got != "SDA,SCL" {
		t.Errorf("suggested %q, want SDA,SCL for the one I2C sensor", got)
	}

	// Anything already wired is not suggested again.
	app.store.AddPinAssignment(PinAssignment{
		ProjectID: projectID, Pin: "GPIO21", ItemID: &ids[1], Part: "BME280", Signal: "SDA",
	})
	existing, _ := app.store.PinAssignments(projectID)
	if ideas := SuggestPins(p, existing); len(ideas) != 1 || ideas[0].Signal != "SCL" {
		t.Errorf("after wiring SDA, suggestions = %+v, want just SCL", ideas)
	}
}

// --- sub-assemblies ---------------------------------------------------------

func TestSubAssemblyRollsUpIntoTheParent(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "LM7805", Quantity: 5},
		Item{Name: "100uF cap", Quantity: 3},
		Item{Name: "ESP32 devkit", Quantity: 1},
	)

	// A power module: one regulator and two capacitors each.
	psu, _ := app.store.CreateProject("5V supply module", "")
	app.store.AddProjectPart(psu, &ids[0], "", 1, "")
	app.store.AddProjectPart(psu, &ids[1], "", 2, "")

	// A thing that needs two of them.
	rig, _ := app.store.CreateProject("Test rig", "")
	app.store.AddProjectPart(rig, &ids[2], "", 1, "")
	if err := app.store.AddSubAssembly(rig, psu, 2, ""); err != nil {
		t.Fatalf("AddSubAssembly: %v", err)
	}

	needs, err := app.store.Requirements(rig)
	if err != nil {
		t.Fatalf("Requirements: %v", err)
	}
	if needs[ids[0]] != 2 {
		t.Errorf("regulators needed = %d, want 2 (one per module, two modules)", needs[ids[0]])
	}
	if needs[ids[1]] != 4 {
		t.Errorf("capacitors needed = %d, want 4 (two per module, two modules)", needs[ids[1]])
	}
	if needs[ids[2]] != 1 {
		t.Errorf("devkits needed = %d, want 1", needs[ids[2]])
	}

	// Only three capacitors on the shelf, so only one module is buildable.
	p, err := app.store.GetProject(rig)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	var sub *ProjectPart
	for i := range p.Parts {
		if p.Parts[i].IsAssembly() {
			sub = &p.Parts[i]
		}
	}
	if sub == nil {
		t.Fatal("the sub-assembly line vanished")
	}
	if sub.Label() != "5V supply module" {
		t.Errorf("sub-assembly label = %q", sub.Label())
	}
	if sub.Buildable != 1 {
		t.Errorf("buildable = %d, want 1: three capacitors only makes one module of two", sub.Buildable)
	}
	if sub.Short() != 1 {
		t.Errorf("short = %d, want 1 of the two modules", sub.Short())
	}
}

func TestSubAssemblyReservesAndConsumesThroughTheTree(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "LM7805", Quantity: 5},
		Item{Name: "100uF cap", Quantity: 10},
	)
	psu, _ := app.store.CreateProject("5V supply module", "")
	app.store.AddProjectPart(psu, &ids[0], "", 1, "")
	app.store.AddProjectPart(psu, &ids[1], "", 2, "")

	rig, _ := app.store.CreateProject("Test rig", "")
	app.store.AddSubAssembly(rig, psu, 2, "")

	// Building the parent reserves what the sub-assembly needs, even though the
	// parent's own parts list never mentions those items.
	app.store.UpdateProject(rig, "Test rig", "", "building", nil)
	it, _ := app.store.GetItem(ids[1])
	if it.Committed() != 4 {
		t.Fatalf("capacitors committed = %d, want the 4 the sub-assembly reaches", it.Committed())
	}
	if it.Available() != 6 {
		t.Errorf("capacitors available = %d, want 10 - 4", it.Available())
	}

	// And consuming the parent takes them.
	taken, short, err := app.store.ConsumeProject(rig)
	if err != nil {
		t.Fatalf("ConsumeProject: %v", err)
	}
	if taken != 6 || short != 0 {
		t.Errorf("taken/short = %d/%d, want 6/0 (2 regulators, 4 capacitors)", taken, short)
	}
	it, _ = app.store.GetItem(ids[1])
	if it.Quantity != 6 {
		t.Errorf("capacitors after building = %d, want 6", it.Quantity)
	}
	reg, _ := app.store.GetItem(ids[0])
	if reg.Quantity != 3 {
		t.Errorf("regulators after building = %d, want 3", reg.Quantity)
	}
}

func TestSubAssemblyRefusesToContainItself(t *testing.T) {
	app := newTestApp(t)
	a, _ := app.store.CreateProject("A", "")
	b, _ := app.store.CreateProject("B", "")

	if err := app.store.AddSubAssembly(a, a, 1, ""); err == nil {
		t.Error("a project containing itself should be refused")
	}
	if err := app.store.AddSubAssembly(a, b, 1, ""); err != nil {
		t.Fatalf("A containing B: %v", err)
	}
	if err := app.store.AddSubAssembly(b, a, 1, ""); err == nil {
		t.Error("B containing A after A contains B closes a loop and should be refused")
	}
	// The loop being refused means requirements still terminate.
	if _, err := app.store.Requirements(a); err != nil {
		t.Errorf("Requirements: %v", err)
	}
}

// --- footprints -------------------------------------------------------------

const sample0805 = `(module R_0805_2012Metric (layer F.Cu) (tedit 5F68FEEE)
  (descr "Resistor SMD 0805 (2012 Metric)")
  (tags resistor)
  (attr smd)
  (fp_text reference REF** (at 0 -1.65) (layer F.SilkS))
  (fp_line (start -1 0.625) (end -1 -0.625) (layer F.Fab) (width 0.1))
  (fp_line (start -1 -0.625) (end 1 -0.625) (layer F.Fab) (width 0.1))
  (fp_line (start 1 -0.625) (end 1 0.625) (layer F.Fab) (width 0.1))
  (fp_line (start 1 0.625) (end -1 0.625) (layer F.Fab) (width 0.1))
  (fp_line (start -1.68 -0.95) (end 1.68 -0.95) (layer F.CrtYd) (width 0.05))
  (fp_line (start 1.68 -0.95) (end 1.68 0.95) (layer F.CrtYd) (width 0.05))
  (fp_line (start 1.68 0.95) (end -1.68 0.95) (layer F.CrtYd) (width 0.05))
  (fp_line (start -1.68 0.95) (end -1.68 -0.95) (layer F.CrtYd) (width 0.05))
  (pad 1 smd roundrect (at -0.9125 0) (size 1.025 1.4) (layers F.Cu F.Mask F.Paste) (roundrect_rratio 0.243902))
  (pad 2 smd roundrect (at 0.9125 0) (size 1.025 1.4) (layers F.Cu F.Mask F.Paste) (roundrect_rratio 0.243902))
)`

const sampleDIP = `(module DIP-8_W7.62mm (layer F.Cu)
  (descr "8-lead DIP package")
  (attr through_hole)
  (fp_arc (start 3.81 -1.33) (end 2.81 -1.33) (angle -180) (layer F.SilkS) (width 0.12))
  (fp_line (start -1.55 -2.33) (end 9.17 -2.33) (layer F.CrtYd) (width 0.05))
  (fp_line (start 9.17 -2.33) (end 9.17 9.95) (layer F.CrtYd) (width 0.05))
  (fp_line (start 9.17 9.95) (end -1.55 9.95) (layer F.CrtYd) (width 0.05))
  (fp_line (start -1.55 9.95) (end -1.55 -2.33) (layer F.CrtYd) (width 0.05))
  (pad 1 thru_hole rect (at 0 0) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
  (pad 2 thru_hole oval (at 0 2.54) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
  (pad 3 thru_hole oval (at 0 5.08) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
  (pad 4 thru_hole oval (at 0 7.62) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
  (pad 5 thru_hole oval (at 7.62 7.62) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
  (pad 6 thru_hole oval (at 7.62 5.08) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
  (pad 7 thru_hole oval (at 7.62 2.54) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
  (pad 8 thru_hole oval (at 7.62 0) (size 1.6 1.6) (drill 0.8) (layers *.Cu *.Mask))
)`

func TestParseFootprintReadsPadsAndOutlines(t *testing.T) {
	fp, err := media.ParseFootprint(sample0805)
	if err != nil {
		t.Fatalf("ParseFootprint: %v", err)
	}
	if fp.Name != "R_0805_2012Metric" {
		t.Errorf("name = %q", fp.Name)
	}
	if !fp.SMD || fp.Mounting() != "surface mount" {
		t.Errorf("mounting = %q, want surface mount", fp.Mounting())
	}
	if len(fp.Pads) != 2 {
		t.Fatalf("got %d pads, want 2", len(fp.Pads))
	}
	if fp.Pads[0].Number != "1" || math.Abs(fp.Pads[0].X+0.9125) > 0.0001 {
		t.Errorf("pad 1 = %+v", fp.Pads[0])
	}
	if math.Abs(fp.Pads[0].W-1.025) > 0.0001 || math.Abs(fp.Pads[0].H-1.4) > 0.0001 {
		t.Errorf("pad size = %v x %v, want 1.025 x 1.4", fp.Pads[0].W, fp.Pads[0].H)
	}

	// The courtyard is what the part actually claims on the board, so that is
	// the figure quoted rather than the pads alone.
	b := fp.Extent()
	if math.Abs(b.Width()-3.36) > 0.001 || math.Abs(b.Height()-1.9) > 0.001 {
		t.Errorf("extent = %.3f x %.3f mm, want the 3.36 x 1.9 courtyard", b.Width(), b.Height())
	}
	if got := fp.Size(); got != "3.36 × 1.90 mm" {
		t.Errorf("Size() = %q", got)
	}
}

func TestParseFootprintReadsThroughHolePartsAndArcs(t *testing.T) {
	fp, err := media.ParseFootprint(sampleDIP)
	if err != nil {
		t.Fatalf("ParseFootprint: %v", err)
	}
	if len(fp.Pads) != 8 {
		t.Fatalf("got %d pads, want 8", len(fp.Pads))
	}
	if fp.Mounting() != "through hole" {
		t.Errorf("mounting = %q", fp.Mounting())
	}
	if math.Abs(fp.Pads[0].Drill-0.8) > 0.0001 {
		t.Errorf("drill = %v, want 0.8", fp.Pads[0].Drill)
	}
	// Pins are 2.54 mm apart, which is the number that tells you it fits a
	// breadboard.
	if got := fp.Pitch(); !strings.Contains(got, "2.54") || !strings.Contains(got, "breadboard") {
		t.Errorf("Pitch() = %q, want it to name 2.54 mm and say breadboard", got)
	}
	// The silkscreen arc marking pin 1 must survive as a curve, not a chord.
	arcs := 0
	for _, s := range fp.Strokes {
		if s.Layer == "silk" && len(s.Points) > 3 {
			arcs++
		}
	}
	if arcs == 0 {
		t.Error("the pin-1 arc was not drawn as a curve")
	}
}

func TestFootprintSVGIsWellFormedAndToScale(t *testing.T) {
	fp, err := media.ParseFootprint(sample0805)
	if err != nil {
		t.Fatalf("ParseFootprint: %v", err)
	}
	svg := string(fp.SVG())

	if !strings.HasPrefix(svg, "<svg ") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("SVG is not a single element: %.80s…", svg)
	}
	if strings.Count(svg, "<svg") != strings.Count(svg, "</svg>") {
		t.Error("unbalanced svg tags")
	}
	// The viewBox is in millimetres, so the drawing is genuinely to scale
	// rather than fitted to an arbitrary box.
	if !strings.Contains(svg, `viewBox="-2.88 -2.15 5.76 4.3"`) {
		t.Errorf("viewBox is not the courtyard plus margin in mm: %.140s", svg)
	}
	for _, want := range []string{"fp-pad", "fp-courtyard", "fp-fab", "fp-one"} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG has no %s layer", want)
		}
	}
	// Both pad numbers are lettered.
	if strings.Count(svg, `class="fp-num"`) != 2 {
		t.Errorf("want two pad numbers, got %d", strings.Count(svg, `class="fp-num"`))
	}
}

func TestParseFootprintRefusesRubbish(t *testing.T) {
	for _, bad := range []string{
		"", "not an s-expression", "(module", `{"json": true}`,
		"(something_else foo)", "(module Empty (layer F.Cu))",
	} {
		if fp, err := media.ParseFootprint(bad); err == nil {
			t.Errorf("media.ParseFootprint(%q) returned %+v, want a refusal", bad, fp)
		}
	}
}

func TestFootprintCandidatesGuessTheLibrary(t *testing.T) {
	// A fully qualified name is taken at its word.
	got := footprintCandidates("Resistor_SMD:R_0805_2012Metric")
	if len(got) != 1 || got[0] != [2]string{"Resistor_SMD", "R_0805_2012Metric"} {
		t.Errorf("qualified name = %v, want it used as given", got)
	}
	// A bare name gets its library guessed, most likely first.
	got = footprintCandidates("R_0805_2012Metric")
	if len(got) < 2 || got[0][0] != "Resistor_SMD" {
		t.Errorf("bare resistor name = %v, want Resistor_SMD first", got)
	}
	if got = footprintCandidates("DIP-8_W7.62mm"); got[0][0] != "Package_DIP" {
		t.Errorf("DIP name = %v, want Package_DIP first", got)
	}
	if footprintCandidates("") != nil {
		t.Error("an empty name should give no candidates")
	}
	// A name that could escape the path is refused before it reaches the wire.
	for _, bad := range []string{"../../etc/passwd", "a/b", "x%2e%2e"} {
		if safeFootprintPart(bad) {
			t.Errorf("safeFootprintPart(%q) = true", bad)
		}
	}
}

func TestFootprintCachingAndUpload(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "0805 4k7", Quantity: 100})

	fp, err := app.StoreFootprintFile("Resistor_SMD:R_0805_2012Metric", sample0805)
	if err != nil {
		t.Fatalf("StoreFootprintFile: %v", err)
	}
	if fp.PadCount() != 2 {
		t.Errorf("pads = %d", fp.PadCount())
	}
	if err := app.store.SetFootprint(ids[0], "Resistor_SMD:R_0805_2012Metric"); err != nil {
		t.Fatalf("SetFootprint: %v", err)
	}

	// The item page draws from the cache, so it never waits on the network.
	body, source, err := app.store.CachedFootprint("Resistor_SMD:R_0805_2012Metric")
	if err != nil {
		t.Fatalf("CachedFootprint: %v", err)
	}
	if source != "uploaded" || !strings.Contains(body, "R_0805_2012Metric") {
		t.Errorf("cached %q from %q", body[:20], source)
	}

	it, _ := app.store.GetItem(ids[0])
	if it.Footprint != "Resistor_SMD:R_0805_2012Metric" {
		t.Errorf("item footprint = %q", it.Footprint)
	}

	// A fetch with no network still succeeds from the cache.
	offline := newTestApp(t)
	offline.store = app.store
	if _, err := offline.FetchFootprint(context.Background(), "Resistor_SMD:R_0805_2012Metric"); err != nil {
		t.Errorf("a cached footprint should not need the network: %v", err)
	}
}

func TestBOMImportFillsInTheFootprint(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "0805 4k7", Value: "4k7", Quantity: 100},
		Item{Name: "0805 100nF", Value: "100nF", Quantity: 50, Footprint: "Chosen_By_Hand"},
	)
	projectID, _ := app.store.CreateProject("Sensor node", "")

	lines, err := ParseBOM("Ref,Qnty,Value,Footprint\nR1,2,4k7,Resistor_SMD:R_0805_2012Metric\n" +
		"C1,1,100nF,Capacitor_SMD:C_0805_2012Metric\n")
	if err != nil {
		t.Fatalf("ParseBOM: %v", err)
	}
	items, _ := app.store.ListItems(Query{})
	if _, err := app.store.ImportBOM(projectID, MatchBOM(lines, items), false); err != nil {
		t.Fatalf("ImportBOM: %v", err)
	}

	r, _ := app.store.GetItem(ids[0])
	if r.Footprint != "Resistor_SMD:R_0805_2012Metric" {
		t.Errorf("resistor footprint = %q, want the one from the schematic", r.Footprint)
	}
	c, _ := app.store.GetItem(ids[1])
	if c.Footprint != "Chosen_By_Hand" {
		t.Errorf("a footprint someone chose was overwritten with %q", c.Footprint)
	}
}

// --- documentation hunting --------------------------------------------------

func TestScanBodyFindsInlinePinoutImages(t *testing.T) {
	// The pinout on a real shop page is an image in the body, not a link
	// anybody labelled -- which is exactly what used to be missed.
	page := `<html><head><title>ESP32 Feather</title></head><body>
		<img src="/img/logo.png" alt="Shop logo">
		<img src="/img/feather-front.jpg" alt="ESP32 Feather board" width="800" height="600">
		<img src="/img/feather-pinout-v2.png" alt="ESP32 Feather pinout diagram" width="1200" height="900">
		<img src="/img/sch.png" alt="Schematic for the Feather">
		<a href="/files/esp32.pdf">Datasheet</a>
		<a href="/category/feathers">More pinout boards</a>
	</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
	}))
	defer srv.Close()

	f := NewFetcher(true)
	meta, err := f.FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}

	kinds := map[string]string{}
	for _, d := range meta.Documents {
		kinds[d.Kind] = d.URL
	}
	if !strings.Contains(kinds["pinout"], "feather-pinout-v2.png") {
		t.Errorf("pinout = %q, want the inline image found", kinds["pinout"])
	}
	if !strings.Contains(kinds["schematic"], "sch.png") {
		t.Errorf("schematic = %q, want the inline image found", kinds["schematic"])
	}
	if !strings.Contains(kinds["datasheet"], "esp32.pdf") {
		t.Errorf("datasheet = %q", kinds["datasheet"])
	}
	// A category page that merely mentions the word is not a document.
	for _, d := range meta.Documents {
		if strings.Contains(d.URL, "/category/") {
			t.Errorf("a navigation link was kept as a document: %+v", d)
		}
	}
}

func TestHuntDocumentationAttachesWhatItFinds(t *testing.T) {
	app := newTestApp(t)

	// A tiny shop: a product page carrying a pinout image and a datasheet.
	var shop *httptest.Server
	shop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pinout.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(testPNG(t))
		case "/board.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(testJPEG(t, 60, 40, 0))
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><head><meta property="og:image" content="%s/board.jpg"></head><body>
				<img src="%s/pinout.png" alt="ESP32 pinout">
				<a href="%s/esp32.pdf">Datasheet (PDF)</a>
			</body></html>`, shop.URL, shop.URL, shop.URL)
		}
	}))
	defer shop.Close()

	ids := seed(t, app.store, Item{Name: "ESP32 devkit", Link: shop.URL + "/product/1", Quantity: 1})
	it, _ := app.store.GetItem(ids[0])

	hunt := app.HuntDocumentation(context.Background(), it)
	if hunt.Pinouts != 1 {
		t.Errorf("found %d pinouts, want 1: %+v %v", hunt.Pinouts, hunt.Found, hunt.Notes)
	}
	if hunt.Links != 1 {
		t.Errorf("found %d documents, want the datasheet: %+v", hunt.Links, hunt.Found)
	}
	if !hunt.Photo {
		t.Error("an item with no photo should have taken the one from the page")
	}

	it, _ = app.store.GetItem(ids[0])
	if len(it.Pinouts()) != 1 {
		t.Fatalf("the pinout was not stored as an image: %+v", it.Refs)
	}
	if it.Pinouts()[0].Filename == "" {
		t.Error("the pinout was kept as a link; it should have been downloaded so it displays")
	}
	if len(it.Photos) != 1 {
		t.Errorf("photos = %d, want the product image", len(it.Photos))
	}

	// Running it again finds nothing new rather than duplicating everything.
	it, _ = app.store.GetItem(ids[0])
	again := app.HuntDocumentation(context.Background(), it)
	if again.Pinouts != 0 || again.Links != 0 {
		t.Errorf("a second hunt attached %d pinouts and %d documents again", again.Pinouts, again.Links)
	}
}

func TestHuntDocumentationSaysWhyItFoundNothing(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{Name: "Mystery module", Quantity: 1})
	it, _ := app.store.GetItem(ids[0])

	hunt := app.HuntDocumentation(context.Background(), it)
	if hunt.Summary() != "" {
		t.Errorf("an item with no link and no part number found %q", hunt.Summary())
	}

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	res, err := http.PostForm(srv.URL+fmt.Sprintf("/items/%d/docs", ids[0]), nil)
	if err != nil {
		t.Fatalf("docs: %v", err)
	}
	res.Body.Close()
	final := res.Request.URL.String()
	if !strings.Contains(final, "error=") {
		t.Fatalf("landed on %q, want an explanation", final)
	}
	if !strings.Contains(final, "link") && !strings.Contains(final, "part+number") {
		t.Errorf("the explanation %q does not say what to do about it", final)
	}
}

// --- pages render -----------------------------------------------------------

func TestWorkshopPagesRender(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store, Item{
		Name: "ESP32 devkit", Quantity: 3, PartNumber: "ESP32-WROOM-32",
		Interfaces: []string{"I2C"}, Specs: []Spec{{Name: "GPIO", Value: "24"}},
	})
	app.StoreFootprintFile("Resistor_SMD:R_0805_2012Metric", sample0805)
	app.store.SetFootprint(ids[0], "Resistor_SMD:R_0805_2012Metric")

	psu, _ := app.store.CreateProject("5V supply module", "")
	app.store.AddProjectPart(psu, &ids[0], "", 1, "")
	rig, _ := app.store.CreateProject("Test rig", "")
	app.store.AddSubAssembly(rig, psu, 2, "")
	app.store.UpdateProject(rig, "Test rig", "", "planning", &ids[0])
	app.store.AddPinAssignment(PinAssignment{ProjectID: rig, Pin: "GPIO21", ItemID: &ids[0], Signal: "SDA"})

	orderID, _ := app.store.CreateOrder(Order{Source: "LCSC", Status: "ordered", PlacedAt: time.Now()})
	app.store.AddOrderLine(orderID, OrderLine{ItemID: &ids[0], Quantity: 5, UnitPrice: 3})

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	for _, path := range []string{
		"/", "/orders", "/orders/1", "/scan", "/items/1",
		"/projects/1", "/projects/2",
		"/projects/2/wiring.csv", "/projects/2/bom.csv",
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

	// The footprint really is drawn on the item page.
	res, _ := http.Get(srv.URL + "/items/1")
	body := readAll(t, res.Body)
	res.Body.Close()
	if !strings.Contains(body, `class="footprint"`) || !strings.Contains(body, "fp-pad") {
		t.Error("the item page did not draw the footprint")
	}
	if !strings.Contains(body, "3.36 × 1.90 mm") {
		t.Error("the item page did not state the footprint's size")
	}
	if !strings.Contains(body, "5 on order") {
		t.Error("the item page did not say stock is on its way")
	}
}

// testPNG is a real PNG, so the pinout that comes back is stored as an image
// rather than quietly rejected.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 24, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 24; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 10), 40, uint8(y * 12), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}
