package app

import (
	"fmt"
	"strings"
	"time"
)

// A printer is not one capability, it is about eight of them, and which
// filament you can load depends on all of them at once. "Can I print this in
// nylon" is really: does the hotend get hot enough, is there a warm box around
// it, is the nozzle going to survive the glass fibre, is the spool dry. So the
// printer is recorded properly rather than as a line of free text, and the
// answer is worked out instead of guessed.

// Printer is one machine, with the specifications that decide what it can run.
type Printer struct {
	ID    int64
	Name  string
	Bed   Volume
	Notes string

	Nozzle     float64 // mm
	NozzleKind string  // brass | hardened | ruby
	MaxHotend  int     // °C
	MaxBed     int     // °C

	Enclosed    bool
	DirectDrive bool
	Dryer       bool // a filament dryer, or a dehydrator pressed into service

	// How to reach it, if it is on the network. LinkKey is stored so it
	// survives a restart and is never rendered back into a page — the same
	// rule as the AI provider keys.
	LinkKind string // "" | moonraker | octoprint
	LinkURL  string
	LinkKey  string

	// Print hours: what it had done before it was added, and the printer's
	// own lifetime counter when it reports one.
	BaseHours       float64
	ReportedSeconds float64

	CreatedAt time.Time
}

// Connected reports whether there is enough recorded to talk to it.
func (p Printer) Connected() bool {
	_, ok := linkFor(p)
	return ok
}

// HasKey is for the form, which says a key is saved without showing it.
func (p Printer) HasKey() bool { return p.LinkKey != "" }

// LinkLabel names the protocol for the card.
func (p Printer) LinkLabel() string {
	for _, k := range LinkKinds {
		if k.Key == p.LinkKind {
			return k.Label
		}
	}
	return ""
}

// HardNozzle reports whether the nozzle survives filled filament. Brass is gone
// in a few hundred grams of anything with fibre in it.
func (p Printer) HardNozzle() bool {
	switch p.NozzleKind {
	case "hardened", "ruby", "steel", "tungsten":
		return true
	}
	return false
}

// NozzleLabel is the nozzle in the way you would say it out loud.
func (p Printer) NozzleLabel() string {
	if p.Nozzle <= 0 {
		return ""
	}
	kind := nozzleKindLabels[p.NozzleKind]
	if kind == "" {
		kind = "brass"
	}
	return fmt.Sprintf("%g mm %s", p.Nozzle, kind)
}

// Feed is bowden or direct, which is most of whether flexibles are pleasant.
func (p Printer) Feed() string {
	if p.DirectDrive {
		return "direct drive"
	}
	return "bowden"
}

var nozzleKindLabels = map[string]string{
	"brass":    "brass",
	"hardened": "hardened steel",
	"ruby":     "ruby",
	"steel":    "hardened steel",
	"tungsten": "tungsten carbide",
}

// NozzleKinds is the picker, in the order of what people actually own.
var NozzleKinds = []struct{ Key, Label string }{
	{"brass", "Brass"},
	{"hardened", "Hardened steel"},
	{"ruby", "Ruby"},
	{"tungsten", "Tungsten carbide"},
}

// --- materials ---------------------------------------------------------------

// need is how badly a material wants something: not at all, it goes better, or
// there is no point starting without it.
type need int

const (
	needNo need = iota
	needHelps
	needMust
)

// Material is a filament and what it demands of a machine. The temperatures are
// the bottom of the usable window rather than anybody's favourite profile: the
// question here is whether the printer can reach them at all.
type Material struct {
	Key    string
	Name   string
	Also   string // what else it gets called
	Family string // for grouping on the page

	Hotend int // °C, minimum that actually extrudes
	Bed    int // °C, minimum that actually sticks

	Enclosure need
	Dry       need
	Direct    need

	Abrasive bool // filled with glass, carbon, metal or glow powder

	MinNozzle    float64 // below this it jams or cannot be driven
	EasierNozzle float64 // works below this, but you will fight it

	Fumes bool
	Note  string
}

// Materials is the list the page walks, easy first. Everything here is a
// filament somebody actually buys for a desktop machine.
var Materials = []Material{
	{
		Key: "pla", Name: "PLA", Family: "Easy",
		Hotend: 200, Bed: 55, MinNozzle: 0.25,
		Note: "Stiff, dimensionally honest, and it creeps and softens in a hot car. Fine for anything living indoors.",
	},
	{
		Key: "petg", Name: "PETG", Family: "Easy",
		Hotend: 235, Bed: 75, Dry: needHelps, MinNozzle: 0.25,
		Note: "Tougher than PLA and survives a sunny windowsill. It sticks to smooth PEI hard enough to take the sheet with it, so use a release agent or a textured plate.",
	},
	{
		Key: "tpu", Name: "TPU 95A", Also: "flexible", Family: "Easy",
		Hotend: 215, Bed: 45, Dry: needHelps, Direct: needHelps, MinNozzle: 0.4,
		Note: "Slow it right down. Softer shores than 95A are a direct-drive-only proposition.",
	},
	{
		Key: "abs", Name: "ABS", Family: "Warps",
		Hotend: 240, Bed: 95, Enclosure: needMust, Fumes: true, MinNozzle: 0.25,
		Note: "Lifts off the bed and splits between layers in a draught. Small parts can get away with it; a tall one will not.",
	},
	{
		Key: "asa", Name: "ASA", Family: "Warps",
		Hotend: 245, Bed: 95, Enclosure: needMust, Fumes: true, MinNozzle: 0.25,
		Note: "ABS that does not go chalky in sunlight. The one to use for anything living outdoors.",
	},
	{
		Key: "pp", Name: "Polypropylene", Also: "PP", Family: "Warps",
		Hotend: 230, Bed: 90, Enclosure: needMust, MinNozzle: 0.4,
		Note: "Almost nothing sticks to it, including your bed. Printed on packing tape of the same plastic.",
	},
	{
		Key: "pa", Name: "Nylon", Also: "PA6, PA12", Family: "Engineering",
		Hotend: 250, Bed: 70, Enclosure: needMust, Dry: needMust,
		MinNozzle: 0.4, EasierNozzle: 0.6,
		Note: "Drinks water out of the air in hours. Wet nylon spits, foams and comes out weak — drying is not optional, it is the whole job.",
	},
	{
		Key: "pa-cf", Name: "Nylon CF", Also: "PAHT-CF, PA-GF", Family: "Engineering",
		Hotend: 260, Bed: 80, Enclosure: needMust, Dry: needMust, Abrasive: true,
		MinNozzle: 0.6,
		Note:      "The chopped fibre is what makes it stiff and what destroys a brass nozzle. Short fibres also bridge a small nozzle and clog it, which is why 0.6 is the floor.",
	},
	{
		Key: "pc", Name: "Polycarbonate", Also: "PC", Family: "Engineering",
		Hotend: 270, Bed: 100, Enclosure: needMust, Dry: needMust, MinNozzle: 0.4,
		Note: "Genuinely tough and stays stiff when hot. Warps harder than ABS and wants every degree you can give it.",
	},
	{
		Key: "filled-pla", Name: "Filled PLA", Also: "wood, glow, metal", Family: "Abrasive",
		Hotend: 200, Bed: 55, Abrasive: true, MinNozzle: 0.5,
		Note: "Glow powder is the worst of them — it is effectively grit. Wood and metal fills also block a fine nozzle.",
	},
	{
		Key: "pva", Name: "PVA", Also: "dissolvable support", Family: "Support",
		Hotend: 200, Bed: 60, Dry: needMust, MinNozzle: 0.4,
		Note: "Needs a second extruder to be worth anything, and it is even thirstier than nylon.",
	},
	{
		Key: "peek", Name: "PEEK", Also: "PEI, ULTEM", Family: "Out of reach",
		Hotend: 380, Bed: 130, Enclosure: needMust, Dry: needMust, MinNozzle: 0.4,
		Note: "Wants a 400 °C hotend and a chamber held near 120 °C. Listed so the answer is a clear no rather than a maybe.",
	},
}

// MaterialByKey finds one, for the detail view.
func MaterialByKey(key string) (Material, bool) {
	for _, m := range Materials {
		if m.Key == key {
			return m, true
		}
	}
	return Material{}, false
}

// Label is the name plus whatever else it gets sold as.
func (m Material) Label() string {
	if m.Also == "" {
		return m.Name
	}
	return m.Name + " (" + m.Also + ")"
}

// Templates cannot compare a named int against a literal, and should not have
// to know what the constants mean anyway, so the demands come out as words.

func (m Material) EnclosureNeed() string {
	switch m.Enclosure {
	case needMust:
		return "needed"
	case needHelps:
		return "helps"
	}
	return "not needed"
}

func (m Material) DryNeed() string {
	switch m.Dry {
	case needMust:
		return "not optional"
	case needHelps:
		return "worth doing"
	}
	return "not fussy"
}

func (m Material) NozzleWear() string {
	if m.Abrasive {
		return "abrasive — hardened steel only"
	}
	return "brass is fine"
}

// --- the verdict --------------------------------------------------------------

// Upgrade is something to buy, and why. Rough is deliberately a range: the
// point is whether this is a tenner or a new printer.
type Upgrade struct {
	What  string
	Why   string
	Rough string
}

// Check is the answer for one material on one machine.
type Check struct {
	Material Material
	Printer  string
	Verdict  string // yes | maybe | no

	Blockers []string
	Cautions []string
	Upgrades []Upgrade
}

func (c Check) OK() bool      { return c.Verdict == "yes" }
func (c Check) Blocked() bool { return c.Verdict == "no" }

// Headline is the one line the grid shows.
func (c Check) Headline() string {
	switch c.Verdict {
	case "yes":
		return "Load it"
	case "maybe":
		return "With care"
	default:
		return "Not as it stands"
	}
}

// CanPrint works out whether this machine runs this filament, what stops it,
// and what would fix it. Everything it reports comes from a specification the
// printer actually recorded: an unknown figure is never treated as a failure,
// because "you did not say" and "it cannot" are different answers.
func CanPrint(p Printer, m Material) Check {
	c := Check{Material: m, Printer: p.Name, Verdict: "yes"}

	block := func(text string) { c.Blockers = append(c.Blockers, text) }
	warn := func(text string) { c.Cautions = append(c.Cautions, text) }
	buy := func(what, why, rough string) {
		c.Upgrades = append(c.Upgrades, Upgrade{What: what, Why: why, Rough: rough})
	}

	if p.MaxHotend > 0 && m.Hotend > p.MaxHotend {
		block(fmt.Sprintf("needs about %d °C at the nozzle; this one stops at %d °C", m.Hotend, p.MaxHotend))
		if m.Hotend <= 300 {
			buy("an all-metal hotend good for 300 °C",
				fmt.Sprintf("%s will not melt properly below %d °C, and a PTFE-lined throat starts giving off fumes above 240 °C", m.Name, m.Hotend),
				"£25–60")
		} else {
			buy(fmt.Sprintf("a high-temperature hotend rated past %d °C", m.Hotend),
				"this is industrial-machine territory rather than a part swap",
				"£300+")
		}
	}

	if p.MaxBed > 0 && m.Bed > p.MaxBed {
		block(fmt.Sprintf("wants a %d °C bed; this one stops at %d °C", m.Bed, p.MaxBed))
		buy("a bed that reaches "+fmt.Sprint(m.Bed)+" °C",
			"usually a mains-voltage heater and an SSR rather than a new printer",
			"£30–70")
	}

	if m.Enclosure == needMust && !p.Enclosed {
		block("warps off an open bed — it needs still, warm air around it")
		buy("an enclosure",
			"a cardboard box or a grow tent over the printer does most of the job; the point is no draught",
			"£0–150")
	} else if m.Enclosure == needHelps && !p.Enclosed {
		warn("goes better with a cover over the printer, though it will manage without")
	}

	// The nozzle can be wrong in two ways at once — too narrow and too soft —
	// and both are fixed by buying one nozzle. So the reasons are collected
	// first and turned into a single line at the end; telling somebody twice
	// to buy a 0.6 mm hardened nozzle reads like the app cannot count.
	var (
		wantSize = p.Nozzle
		wantHard = p.HardNozzle()
		whyBuy   []string
	)
	if m.Abrasive && p.Nozzle > 0 && !p.HardNozzle() {
		block("the fibre in it will eat a brass nozzle inside a spool")
		wantHard = true
		whyBuy = append(whyBuy,
			"brass wears open as you print, so the holes creep wider and the part stops matching the model")
	}
	switch {
	case p.Nozzle > 0 && m.MinNozzle > 0 && p.Nozzle < m.MinNozzle:
		block(fmt.Sprintf("needs a %g mm nozzle or bigger; this one is %g mm", m.MinNozzle, p.Nozzle))
		wantSize = m.MinNozzle
		whyBuy = append(whyBuy,
			"anything narrower bridges across the opening and blocks the melt zone")
	case p.Nozzle > 0 && m.EasierNozzle > 0 && p.Nozzle < m.EasierNozzle:
		warn(fmt.Sprintf("%g mm works, but %g mm is far less prone to clogging", p.Nozzle, m.EasierNozzle))
		wantSize = m.EasierNozzle
		whyBuy = append(whyBuy,
			"wider channel, fewer jams, and the extra width barely shows on a functional part")
	}
	if len(whyBuy) > 0 {
		kind := ""
		if wantHard {
			kind = " hardened steel"
		}
		price := "£5–15"
		if wantHard {
			price = "£8–20"
		}
		buy(fmt.Sprintf("a %g mm%s nozzle", wantSize, kind),
			strings.Join(whyBuy, "; and "), price)
	}

	if m.Dry == needMust && !p.Dryer {
		block("has to go on dry, and it is wet within a day of opening the bag")
		buy("a filament dryer",
			"a food dehydrator with the trays out does the same job for less",
			"£30–90")
	} else if m.Dry == needHelps && !p.Dryer {
		warn("prints better dry — stringing and a rough surface usually mean a damp spool")
	}

	if m.Direct != needNo && !p.DirectDrive {
		warn("a bowden tube makes this hard work: the filament buckles in the tube instead of feeding")
		buy("a direct drive extruder",
			"shortens the path from gear to nozzle to a few centimetres, which is what flexibles need",
			"£40–120")
	}

	if m.Fumes {
		warn("gives off styrene while it runs — put it somewhere you are not sitting all day")
	}

	switch {
	case len(c.Blockers) > 0:
		c.Verdict = "no"
	case len(c.Cautions) > 0:
		c.Verdict = "maybe"
	}
	return c
}

// CheckAll runs the whole material list against one printer.
func CheckAll(p Printer) []Check {
	out := make([]Check, 0, len(Materials))
	for _, m := range Materials {
		out = append(out, CanPrint(p, m))
	}
	return out
}

// BestFor picks the machine most likely to run a material: the one that is not
// blocked, and among those the one needing the fewest cautions. Returns false
// when nothing you own will take it.
func BestFor(printers []Printer, m Material) (Check, bool) {
	var best Check
	found := false
	for _, p := range printers {
		c := CanPrint(p, m)
		if c.Blocked() {
			continue
		}
		if !found || len(c.Cautions) < len(best.Cautions) {
			best, found = c, true
		}
	}
	return best, found
}

// CheapestFix is the shortest route to printing a material at all: the machine
// needing the fewest upgrades, with the list of what to buy. It is what the
// page shows when every printer says no.
func CheapestFix(printers []Printer, m Material) (Check, bool) {
	var best Check
	found := false
	for _, p := range printers {
		c := CanPrint(p, m)
		if !found || len(c.Upgrades) < len(best.Upgrades) {
			best, found = c, true
		}
	}
	return best, found
}

// --- presets -------------------------------------------------------------------

// Preset is a machine filled in for you. These are starting points from the
// published specifications, not gospel: nozzles get swapped, hotends get
// upgraded, and the form is there to be corrected.
type Preset struct {
	Printer
	Maker string
}

var PrinterPresets = []Preset{
	{Maker: "Prusa", Printer: Printer{Name: "Prusa MK4S", Bed: Volume{250, 210, 220},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 290, MaxBed: 120, DirectDrive: true}},
	{Maker: "Prusa", Printer: Printer{Name: "Prusa MK3S+", Bed: Volume{250, 210, 210},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 300, MaxBed: 120, DirectDrive: true}},
	{Maker: "Prusa", Printer: Printer{Name: "Prusa MINI+", Bed: Volume{180, 180, 180},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 280, MaxBed: 100}},
	{Maker: "Bambu Lab", Printer: Printer{Name: "Bambu Lab A1", Bed: Volume{256, 256, 256},
		Nozzle: 0.4, NozzleKind: "hardened", MaxHotend: 300, MaxBed: 100, DirectDrive: true}},
	{Maker: "Bambu Lab", Printer: Printer{Name: "Bambu Lab A1 mini", Bed: Volume{180, 180, 180},
		Nozzle: 0.4, NozzleKind: "hardened", MaxHotend: 300, MaxBed: 80, DirectDrive: true}},
	{Maker: "Bambu Lab", Printer: Printer{Name: "Bambu Lab P1S", Bed: Volume{256, 256, 256},
		Nozzle: 0.4, NozzleKind: "hardened", MaxHotend: 300, MaxBed: 100, DirectDrive: true, Enclosed: true}},
	{Maker: "Bambu Lab", Printer: Printer{Name: "Bambu Lab X1C", Bed: Volume{256, 256, 256},
		Nozzle: 0.4, NozzleKind: "hardened", MaxHotend: 300, MaxBed: 110, DirectDrive: true, Enclosed: true}},
	{Maker: "Creality", Printer: Printer{Name: "Ender 3 V2", Bed: Volume{220, 220, 250},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 240, MaxBed: 100}},
	{Maker: "Creality", Printer: Printer{Name: "Ender 3 S1 Pro", Bed: Volume{220, 220, 270},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 300, MaxBed: 110, DirectDrive: true}},
	{Maker: "Creality", Printer: Printer{Name: "Creality K1", Bed: Volume{220, 220, 250},
		Nozzle: 0.4, NozzleKind: "hardened", MaxHotend: 300, MaxBed: 100, DirectDrive: true, Enclosed: true}},
	// The Neptune 4 family ship running Klipper, with Moonraker and Fluidd
	// already on the network, so the preset says so and only the address is
	// left to fill in.
	{Maker: "Elegoo", Printer: Printer{Name: "Elegoo Neptune 4", Bed: Volume{225, 225, 265},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 300, MaxBed: 110, DirectDrive: true,
		LinkKind: "moonraker"}},
	{Maker: "Elegoo", Printer: Printer{Name: "Elegoo Neptune 4 Pro", Bed: Volume{225, 225, 265},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 300, MaxBed: 110, DirectDrive: true,
		LinkKind: "moonraker"}},
	{Maker: "Voron", Printer: Printer{Name: "Voron 2.4 (350)", Bed: Volume{350, 350, 350},
		Nozzle: 0.4, NozzleKind: "brass", MaxHotend: 300, MaxBed: 120, DirectDrive: true, Enclosed: true,
		LinkKind: "moonraker"}},
}

// PresetByName finds one for the add form.
func PresetByName(name string) (Printer, bool) {
	for _, p := range PrinterPresets {
		if strings.EqualFold(p.Name, name) {
			return p.Printer, true
		}
	}
	return Printer{}, false
}

// --- storage ---------------------------------------------------------------------

// printerColumns and scanPrinter are shared by every read, so a new column is
// added in one place rather than in four queries that can drift apart.
const printerColumns = `id, name, bed_x, bed_y, bed_z, nozzle, nozzle_kind, max_hotend,
	max_bed, enclosed, direct_drive, dryer, notes, link_kind, link_url, link_key,
	base_hours, reported_seconds, created_at`

func scanPrinter(row rowScanner) (Printer, error) {
	var p Printer
	var created string
	err := row.Scan(&p.ID, &p.Name, &p.Bed.X, &p.Bed.Y, &p.Bed.Z, &p.Nozzle, &p.NozzleKind,
		&p.MaxHotend, &p.MaxBed, &p.Enclosed, &p.DirectDrive, &p.Dryer, &p.Notes,
		&p.LinkKind, &p.LinkURL, &p.LinkKey, &p.BaseHours, &p.ReportedSeconds, &created)
	if err != nil {
		return Printer{}, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return p, nil
}

func (s *Store) ListPrinters() ([]Printer, error) {
	rows, err := s.db.Query(`SELECT ` + printerColumns + ` FROM printers ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Printer
	for rows.Next() {
		p, err := scanPrinter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPrinter(id int64) (Printer, error) {
	return scanPrinter(s.db.QueryRow(`SELECT `+printerColumns+` FROM printers WHERE id = ?`, id))
}

func (s *Store) AddPrinter(p Printer) (int64, error) {
	if strings.TrimSpace(p.Name) == "" {
		return 0, fmt.Errorf("a printer needs a name")
	}
	res, err := s.db.Exec(`INSERT INTO printers (name, bed_x, bed_y, bed_z, nozzle,
		nozzle_kind, max_hotend, max_bed, enclosed, direct_drive, dryer, notes,
		link_kind, link_url, link_key, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		strings.TrimSpace(p.Name), p.Bed.X, p.Bed.Y, p.Bed.Z, p.Nozzle, p.NozzleKind,
		p.MaxHotend, p.MaxBed, p.Enclosed, p.DirectDrive, p.Dryer, strings.TrimSpace(p.Notes),
		p.LinkKind, strings.TrimSpace(p.LinkURL), p.LinkKey, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdatePrinter(p Printer) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("a printer needs a name")
	}
	_, err := s.db.Exec(`UPDATE printers SET name = ?, bed_x = ?, bed_y = ?, bed_z = ?,
		nozzle = ?, nozzle_kind = ?, max_hotend = ?, max_bed = ?, enclosed = ?,
		direct_drive = ?, dryer = ?, notes = ?, link_kind = ?, link_url = ?, link_key = ?
		WHERE id = ?`,
		strings.TrimSpace(p.Name), p.Bed.X, p.Bed.Y, p.Bed.Z, p.Nozzle, p.NozzleKind,
		p.MaxHotend, p.MaxBed, p.Enclosed, p.DirectDrive, p.Dryer,
		strings.TrimSpace(p.Notes), p.LinkKind, strings.TrimSpace(p.LinkURL), p.LinkKey, p.ID)
	return err
}

func (s *Store) DeletePrinter(id int64) error {
	_, err := s.db.Exec(`DELETE FROM printers WHERE id = ?`, id)
	return err
}

// caps is what every page should ask for. A printer recorded on the printing
// page is better evidence than a line of text in the tool list, so the two are
// always folded together rather than read separately and disagreeing.
func (a *App) caps(tools []Tool) Capabilities {
	printers, err := a.store.ListPrinters()
	if err != nil {
		// A failed read here is not worth breaking a page over: the tools
		// alone still give a usable answer, just a less precise one.
		return SummariseTools(tools)
	}
	return SummariseWorkshop(tools, printers)
}

// SummariseWorkshop folds the printers into the tool capabilities. A recorded
// printer is better evidence than a line of text in the tool list, so its bed
// wins when both say something.
func SummariseWorkshop(tools []Tool, printers []Printer) Capabilities {
	c := SummariseTools(tools)
	for _, p := range printers {
		c.Print3D = true
		if p.Bed.Known() && p.Bed.X*p.Bed.Y*p.Bed.Z > c.BuildVolume.X*c.BuildVolume.Y*c.BuildVolume.Z {
			c.BuildVolume = p.Bed
		}
	}
	return c
}
