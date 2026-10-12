package app

import (
	"strings"
	"testing"
)

// An Ender 3 V2 out of the box: open frame, 0.4 brass, PTFE-lined hotend that
// gives up at 240 °C, bowden, no dryer. It is the machine most of these
// answers have to be useful for.
func ender3() Printer {
	return Printer{
		Name: "Ender 3 V2", Bed: Volume{220, 220, 250},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 240, MaxBed: 100,
	}
}

// An X1C with the hardened nozzle it ships with, plus a dryer.
func x1c() Printer {
	return Printer{
		Name: "Bambu Lab X1C", Bed: Volume{256, 256, 256},
		Nozzle: 0.4, NozzleKind: "hardened", MaxHotend: 300, MaxBed: 110,
		Enclosed: true, DirectDrive: true, Dryer: true,
	}
}

func mustMaterial(t *testing.T, key string) Material {
	t.Helper()
	m, ok := MaterialByKey(key)
	if !ok {
		t.Fatalf("no material %q", key)
	}
	return m
}

// joined is the whole verdict as one string, for asserting that a particular
// reason was given without pinning the order of the list.
func joined(c Check) string {
	parts := append([]string{}, c.Blockers...)
	parts = append(parts, c.Cautions...)
	for _, u := range c.Upgrades {
		parts = append(parts, u.What, u.Why)
	}
	return strings.ToLower(strings.Join(parts, " | "))
}

func TestPLAOnAnything(t *testing.T) {
	c := CanPrint(ender3(), mustMaterial(t, "pla"))
	if !c.OK() {
		t.Fatalf("PLA should just work on a stock Ender 3, got %q: %v", c.Verdict, joined(c))
	}
	if len(c.Upgrades) != 0 {
		t.Errorf("PLA should need nothing bought, got %v", c.Upgrades)
	}
}

// The case the whole page exists for.
func TestNylonCFOnAStockEnder(t *testing.T) {
	c := CanPrint(ender3(), mustMaterial(t, "pa-cf"))
	if !c.Blocked() {
		t.Fatalf("nylon CF must be refused on a stock Ender 3, got %q", c.Verdict)
	}
	all := joined(c)
	for _, want := range []string{
		"0.6",      // the nozzle size
		"hardened", // and that it has to be hardened
		"enclos",   // the chamber
		"dry",      // and the spool
		"260",      // the hotend it cannot reach
	} {
		if !strings.Contains(all, want) {
			t.Errorf("verdict never mentions %q:\n%s", want, all)
		}
	}
	if len(c.Upgrades) < 4 {
		t.Errorf("want an upgrade for each blocker, got %d: %v", len(c.Upgrades), c.Upgrades)
	}
}

// A nozzle that is both too narrow and too soft is still one nozzle. Listing
// it twice is what the naive version of this does.
func TestOneNozzleIsRecommendedOnce(t *testing.T) {
	c := CanPrint(ender3(), mustMaterial(t, "pa-cf"))
	nozzles := 0
	for _, u := range c.Upgrades {
		if strings.Contains(u.What, "nozzle") {
			nozzles++
		}
	}
	if nozzles != 1 {
		t.Fatalf("want one nozzle to buy, got %d: %v", nozzles, c.Upgrades)
	}
	// And the single line has to carry both requirements.
	var what string
	for _, u := range c.Upgrades {
		if strings.Contains(u.What, "nozzle") {
			what = u.What
		}
	}
	if !strings.Contains(what, "0.6") || !strings.Contains(what, "hardened") {
		t.Errorf("the one nozzle should be 0.6 mm and hardened, got %q", what)
	}
}

// Plain nylon is not abrasive, so a brass nozzle survives it. Saying otherwise
// would be the easy mistake, and it would sell somebody a nozzle they do not
// need.
func TestPlainNylonDoesNotDemandAHardenedNozzle(t *testing.T) {
	p := x1c()
	p.Nozzle = 0.6 // so the only thing left to complain about would be wear
	c := CanPrint(p, mustMaterial(t, "pa"))
	if strings.Contains(joined(c), "hardened") {
		t.Errorf("plain nylon should not ask for a hardened nozzle:\n%s", joined(c))
	}
	if !c.OK() {
		t.Errorf("an enclosed 300 °C machine with a dryer and a 0.6 nozzle should run plain "+
			"nylon outright, got %q: %v", c.Verdict, joined(c))
	}
}

// 0.4 does extrude plain nylon; it is just harder work. That is a caution, not
// a refusal, and the difference matters because one of them stops you.
func TestPlainNylonOn04IsACautionNotABlock(t *testing.T) {
	p := x1c()
	p.Nozzle = 0.4
	c := CanPrint(p, mustMaterial(t, "pa"))
	if c.Blocked() {
		t.Fatalf("0.4 mm should not block plain nylon: %v", c.Blockers)
	}
	if !strings.Contains(joined(c), "0.6") {
		t.Errorf("should still suggest 0.6 mm:\n%s", joined(c))
	}
}

func TestABSNeedsAnEnclosure(t *testing.T) {
	p := x1c()
	p.Enclosed = false
	c := CanPrint(p, mustMaterial(t, "abs"))
	if !c.Blocked() {
		t.Fatalf("ABS on an open frame should be refused, got %q", c.Verdict)
	}
	if !strings.Contains(joined(c), "enclosure") {
		t.Errorf("should offer an enclosure:\n%s", joined(c))
	}
	// And it should still mention the fumes, which an enclosure does not fix.
	if !strings.Contains(joined(c), "styrene") {
		t.Errorf("should warn about fumes:\n%s", joined(c))
	}
}

func TestPEEKIsOutOfReachEverywhere(t *testing.T) {
	for _, p := range []Printer{ender3(), x1c()} {
		c := CanPrint(p, mustMaterial(t, "peek"))
		if !c.Blocked() {
			t.Errorf("%s should not claim to print PEEK", p.Name)
		}
	}
}

// An unrecorded specification is not a failing one. Somebody who has typed
// only a name should not be told their printer cannot reach any temperature.
func TestUnknownSpecsAreNotTreatedAsZero(t *testing.T) {
	bare := Printer{Name: "something"}
	c := CanPrint(bare, mustMaterial(t, "pla"))
	for _, b := range c.Blockers {
		if strings.Contains(b, "stops at") || strings.Contains(b, "nozzle") {
			t.Errorf("an unspecified printer should not be blocked on temperature or nozzle: %q", b)
		}
	}
}

func TestTPUWarnsAboutBowden(t *testing.T) {
	c := CanPrint(ender3(), mustMaterial(t, "tpu"))
	if c.Blocked() {
		t.Fatalf("TPU on a bowden machine is unpleasant, not impossible: %v", c.Blockers)
	}
	if !strings.Contains(joined(c), "bowden") {
		t.Errorf("should mention the feed path:\n%s", joined(c))
	}
}

func TestBestForPicksTheMachineThatCanDoIt(t *testing.T) {
	printers := []Printer{ender3(), x1c()}
	c, ok := BestFor(printers, mustMaterial(t, "abs"))
	if !ok {
		t.Fatal("the X1C should be able to run ABS")
	}
	if c.Printer != "Bambu Lab X1C" {
		t.Errorf("picked %q, want the enclosed machine", c.Printer)
	}
}

func TestBestForReportsNothingWhenNothingCan(t *testing.T) {
	if _, ok := BestFor([]Printer{ender3()}, mustMaterial(t, "pc")); ok {
		t.Error("a stock Ender 3 cannot run polycarbonate")
	}
}

func TestCheapestFixPrefersTheShorterShoppingList(t *testing.T) {
	printers := []Printer{ender3(), x1c()}
	c, ok := CheapestFix(printers, mustMaterial(t, "pa-cf"))
	if !ok {
		t.Fatal("want a route to printing nylon CF")
	}
	// The X1C needs a wider nozzle; the Ender needs a hotend, a chamber, a
	// nozzle and a dryer.
	if c.Printer != "Bambu Lab X1C" {
		t.Errorf("cheapest fix is on %q, want the X1C", c.Printer)
	}
}

// Every material has to be reachable by somebody, or the entry is noise. PEEK
// is the deliberate exception and is marked as such.
func TestEveryMaterialIsPrintableOnSomething(t *testing.T) {
	best := Printer{
		Name: "a very good printer", Bed: Volume{300, 300, 300},
		Nozzle: 0.8, NozzleKind: "hardened", MaxHotend: 350, MaxBed: 130,
		Enclosed: true, DirectDrive: true, Dryer: true,
	}
	for _, m := range Materials {
		c := CanPrint(best, m)
		if m.Key == "peek" {
			if !c.Blocked() {
				t.Errorf("PEEK should still be out of reach at 350 °C")
			}
			continue
		}
		if c.Blocked() {
			t.Errorf("%s is unreachable even on a well-specified machine: %v", m.Name, c.Blockers)
		}
	}
}

func TestPrinterRoundTrip(t *testing.T) {
	s := newTestApp(t).store
	in := Printer{
		Name: "Prusa MK4S", Bed: Volume{250, 210, 220},
		Nozzle: 0.6, NozzleKind: "hardened", MaxHotend: 290, MaxBed: 120,
		Enclosed: true, DirectDrive: true, Dryer: true, Notes: "nozzle swapped",
	}
	id, err := s.AddPrinter(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetPrinter(id)
	if err != nil {
		t.Fatal(err)
	}
	in.ID, in.CreatedAt = got.ID, got.CreatedAt
	if got != in {
		t.Errorf("round trip changed the printer:\n got %+v\nwant %+v", got, in)
	}

	got.Nozzle, got.NozzleKind = 0.4, "brass"
	if err := s.UpdatePrinter(got); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetPrinter(id)
	if after.Nozzle != 0.4 || after.HardNozzle() {
		t.Errorf("update did not take: %+v", after)
	}

	if err := s.DeletePrinter(id); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListPrinters(); len(list) != 0 {
		t.Errorf("printer survived deletion: %v", list)
	}
}

func TestAPrinterNeedsAName(t *testing.T) {
	s := newTestApp(t).store
	if _, err := s.AddPrinter(Printer{Name: "   "}); err == nil {
		t.Error("a nameless printer should be refused")
	}
}

// A printer is better evidence than a line of text, so its bed should be the
// one a suggestion is measured against.
func TestPrintersFeedCapabilities(t *testing.T) {
	tools := []Tool{{Name: "Prusa MINI", Kind: "3d-printer", Detail: "180x180x180"}}
	caps := SummariseWorkshop(tools, []Printer{{Name: "Voron 2.4", Bed: Volume{350, 350, 350}}})
	if !caps.Print3D {
		t.Fatal("a recorded printer should mean you can print")
	}
	if caps.BuildVolume.X != 350 {
		t.Errorf("want the roomiest bed, got %v", caps.BuildVolume)
	}
	if !caps.Fits(300, 300, 300) {
		t.Error("a 300 mm part should fit a 350 mm bed")
	}
}

func TestPrintersAloneGivePrintCapability(t *testing.T) {
	caps := SummariseWorkshop(nil, []Printer{{Name: "Ender 3", Bed: Volume{220, 220, 250}}})
	if !caps.Print3D || !caps.BuildVolume.Known() {
		t.Errorf("a printer with no tool list should still count: %+v", caps)
	}
}

func TestPresetsAreUsable(t *testing.T) {
	if len(PrinterPresets) == 0 {
		t.Fatal("no presets")
	}
	for _, p := range PrinterPresets {
		if !p.Bed.Known() {
			t.Errorf("%s has no bed size", p.Name)
		}
		if p.MaxHotend == 0 || p.MaxBed == 0 || p.Nozzle == 0 {
			t.Errorf("%s is missing a figure the checker needs: %+v", p.Name, p.Printer)
		}
		if _, ok := PresetByName(p.Name); !ok {
			t.Errorf("%s cannot be looked up by its own name", p.Name)
		}
	}
}
