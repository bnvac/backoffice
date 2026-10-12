package app

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The inventory says what you can build things *from*. This says what you can
// build them *with* -- the printer, the iron, the calipers -- because half of
// deciding whether a project is realistic is knowing whether you can make the
// box for it, and the shelf alone never answers that.

// Profile is the one row describing whoever set this up. It is not a login;
// accounts live in the users table. This is the context a suggestion needs.
type Profile struct {
	Name        string
	Bench       string // where the work happens: "garage", "spare room"
	Units       string // mm or in
	Skill       string
	Interests   string
	OnboardedAt time.Time
	CreatedAt   time.Time
}

func (p Profile) Onboarded() bool { return !p.OnboardedAt.IsZero() }

// Greeting is what the dashboard says at the top.
func (p Profile) Greeting() string {
	if p.Name == "" {
		return "Welcome back"
	}
	return "Welcome back, " + p.Name
}

// Tool is one thing you own that does work to a part.
type Tool struct {
	ID        int64
	Name      string
	Kind      string
	Detail    string
	Notes     string
	Raw       string
	CreatedAt time.Time
}

// KindLabel is the human name for the slug.
func (t Tool) KindLabel() string {
	if label, ok := toolKindLabels[t.Kind]; ok {
		return label
	}
	return "Other"
}

func (t Tool) Icon() string {
	if icon, ok := toolKindIcons[t.Kind]; ok {
		return icon
	}
	return "•"
}

var toolKindLabels = map[string]string{
	"3d-printer":  "3D printing",
	"laser":       "Laser",
	"cnc":         "CNC and milling",
	"soldering":   "Soldering",
	"smd":         "SMD and rework",
	"measurement": "Measuring",
	"test":        "Test and debug",
	"power":       "Power",
	"programming": "Programming and flashing",
	"hand":        "Hand tools",
	"fabrication": "Cutting and drilling",
	"optics":      "Magnification",
	"labelling":   "Labelling",
	"other":       "Other",
}

var toolKindIcons = map[string]string{
	"3d-printer":  "🖨",
	"laser":       "⚡",
	"cnc":         "🪛",
	"soldering":   "🔥",
	"smd":         "🌡",
	"measurement": "📏",
	"test":        "📈",
	"power":       "🔌",
	"programming": "⌁",
	"hand":        "🔧",
	"fabrication": "🪚",
	"optics":      "🔎",
	"labelling":   "🏷",
	"other":       "•",
}

// toolKindOrder is how the workshop page groups them: the machines that decide
// what is possible first, the drawer of pliers last.
var toolKindOrder = []string{
	"3d-printer", "laser", "cnc", "smd", "soldering", "test",
	"measurement", "power", "programming", "fabrication", "optics",
	"labelling", "hand", "other",
}

// --- capabilities -----------------------------------------------------------

// Capabilities is what the tools add up to. It is deliberately about verbs
// rather than objects: a suggestion cares whether you can print a case, not
// whether the printer is a Prusa.
type Capabilities struct {
	Print3D     bool
	BuildVolume Volume // biggest printer, when one said
	Laser       bool
	CNC         bool
	Solder      bool
	SolderSMD   bool
	Measure     bool // calipers or better
	Scope       bool // oscilloscope or logic analyser
	BenchPower  bool
	Flash       bool // a programmer or debug probe
	Drill       bool
	Label       bool

	Tools int
}

// Volume is a print bed in millimetres.
type Volume struct{ X, Y, Z float64 }

func (v Volume) Known() bool { return v.X > 0 && v.Y > 0 && v.Z > 0 }

func (v Volume) String() string {
	if !v.Known() {
		return ""
	}
	return fmt.Sprintf("%g × %g × %g mm", v.X, v.Y, v.Z)
}

// Fits reports whether something of the given size would go on the bed. It
// tries the part flat and turned ninety degrees, which is what you would do.
func (v Volume) Fits(x, y, z float64) bool {
	if !v.Known() {
		return false
	}
	if z > v.Z {
		return false
	}
	return (x <= v.X && y <= v.Y) || (y <= v.X && x <= v.Y)
}

// SummariseTools turns a tool list into capabilities.
func SummariseTools(tools []Tool) Capabilities {
	c := Capabilities{Tools: len(tools)}
	for _, t := range tools {
		switch t.Kind {
		case "3d-printer":
			c.Print3D = true
			if v := parseVolume(t.Detail + " " + t.Name + " " + t.Raw); v.Known() {
				// Keep the roomiest bed: it is the one that decides what fits.
				if v.X*v.Y*v.Z > c.BuildVolume.X*c.BuildVolume.Y*c.BuildVolume.Z {
					c.BuildVolume = v
				}
			}
		case "laser":
			c.Laser = true
		case "cnc":
			c.CNC, c.Drill = true, true
		case "soldering":
			c.Solder = true
		case "smd":
			c.Solder, c.SolderSMD = true, true
		case "measurement":
			c.Measure = true
		case "test":
			c.Scope = true
		case "power":
			c.BenchPower = true
		case "programming":
			c.Flash = true
		case "fabrication":
			c.Drill = true
		case "labelling":
			c.Label = true
		}
	}
	return c
}

// Fits asks the question the way a caller means it: will this print here?
// False when there is no printer at all, which is the right answer.
func (c Capabilities) Fits(x, y, z float64) bool {
	return c.Print3D && c.BuildVolume.Fits(x, y, z)
}

// Can lists the capabilities in plain words, for the profile page and for the
// prompt that goes to a model.
func (c Capabilities) Can() []string {
	var out []string
	add := func(on bool, text string) {
		if on {
			out = append(out, text)
		}
	}
	if c.Print3D {
		what := "3D print parts"
		if c.BuildVolume.Known() {
			what += " up to " + c.BuildVolume.String()
		}
		out = append(out, what)
	}
	add(c.Laser, "laser cut")
	add(c.CNC, "machine or mill")
	add(c.SolderSMD, "solder surface-mount parts")
	add(c.Solder && !c.SolderSMD, "solder through-hole parts")
	add(c.Measure, "measure parts accurately")
	add(c.Scope, "look at signals on a scope or analyser")
	add(c.BenchPower, "power a board from a bench supply")
	add(c.Flash, "flash and debug a chip over SWD or JTAG")
	add(c.Drill, "cut and drill an enclosure")
	add(c.Label, "print labels")
	return out
}

// Missing is the other half: the things worth knowing you cannot do, because
// they change what a suggestion should say.
func (c Capabilities) Missing() []string {
	var out []string
	if !c.Print3D {
		out = append(out, "no 3D printer — enclosures have to be bought or adapted")
	}
	if !c.Solder {
		out = append(out, "no soldering iron — stick to plug-together modules")
	}
	if !c.Measure {
		out = append(out, "no calipers — sizes have to come from datasheets")
	}
	return out
}

// --- storage ----------------------------------------------------------------

// GetProfile always returns a profile. A fresh install has an empty one, which
// is exactly what tells the app to run onboarding.
func (s *Store) GetProfile() (Profile, error) {
	var p Profile
	var onboarded, created string
	err := s.db.QueryRow(`SELECT name, bench, units, skill, interests, onboarded_at, created_at
		FROM profile WHERE id = 1`).
		Scan(&p.Name, &p.Bench, &p.Units, &p.Skill, &p.Interests, &onboarded, &created)
	if err == sql.ErrNoRows {
		return Profile{Units: "mm"}, nil
	}
	if err != nil {
		return Profile{}, err
	}
	p.OnboardedAt, _ = time.Parse(time.RFC3339, onboarded)
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return p, nil
}

// SaveProfile writes the single row, keeping the date it was first finished.
func (s *Store) SaveProfile(p Profile) error {
	if p.Units != "in" {
		p.Units = "mm"
	}
	onboarded := ""
	if !p.OnboardedAt.IsZero() {
		onboarded = p.OnboardedAt.Format(time.RFC3339)
	}
	_, err := s.db.Exec(`INSERT INTO profile (id, name, bench, units, skill, interests, onboarded_at, created_at)
		VALUES (1,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, bench = excluded.bench,
			units = excluded.units, skill = excluded.skill, interests = excluded.interests,
			onboarded_at = CASE WHEN profile.onboarded_at <> '' THEN profile.onboarded_at
			                    ELSE excluded.onboarded_at END`,
		p.Name, p.Bench, p.Units, p.Skill, p.Interests, onboarded, nowRFC3339())
	return err
}

// MarkOnboarded closes the wizard for good.
func (s *Store) MarkOnboarded() error {
	_, err := s.db.Exec(`INSERT INTO profile (id, onboarded_at, created_at) VALUES (1,?,?)
		ON CONFLICT(id) DO UPDATE SET onboarded_at = ?`,
		nowRFC3339(), nowRFC3339(), nowRFC3339())
	return err
}

func (s *Store) ListTools() ([]Tool, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, detail, notes, raw, created_at
		FROM tools ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Tool
	for rows.Next() {
		var t Tool
		var created string
		if err := rows.Scan(&t.ID, &t.Name, &t.Kind, &t.Detail, &t.Notes, &t.Raw, &created); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ToolGroup is one heading on the workshop page.
type ToolGroup struct {
	Kind  string
	Label string
	Icon  string
	Tools []Tool
}

// GroupTools arranges the list for display, in a fixed order so the page does
// not reshuffle itself every time a tool is added.
func GroupTools(tools []Tool) []ToolGroup {
	byKind := map[string][]Tool{}
	for _, t := range tools {
		byKind[t.Kind] = append(byKind[t.Kind], t)
	}
	var out []ToolGroup
	for _, kind := range toolKindOrder {
		if list, ok := byKind[kind]; ok {
			out = append(out, ToolGroup{
				Kind: kind, Label: toolKindLabels[kind], Icon: toolKindIcons[kind], Tools: list,
			})
			delete(byKind, kind)
		}
	}
	// Anything with a kind the vocabulary has forgotten still gets shown.
	var rest []string
	for kind := range byKind {
		rest = append(rest, kind)
	}
	sort.Strings(rest)
	for _, kind := range rest {
		out = append(out, ToolGroup{Kind: kind, Label: kind, Icon: "•", Tools: byKind[kind]})
	}
	return out
}

func (s *Store) AddTool(t Tool) (int64, error) {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return 0, fmt.Errorf("a tool needs a name")
	}
	if t.Kind == "" {
		t.Kind = "other"
	}
	res, err := s.db.Exec(`INSERT INTO tools (name, kind, detail, notes, raw, created_at)
		VALUES (?,?,?,?,?,?)`, t.Name, t.Kind, t.Detail, t.Notes, t.Raw, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddTools stores a whole interpreted list, skipping anything already there by
// name so running the importer twice does not double the workshop.
func (s *Store) AddTools(tools []Tool) (int, error) {
	existing, err := s.ListTools()
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, t := range existing {
		seen[strings.ToLower(t.Name)] = true
	}
	added := 0
	for _, t := range tools {
		key := strings.ToLower(strings.TrimSpace(t.Name))
		if key == "" || seen[key] {
			continue
		}
		if _, err := s.AddTool(t); err != nil {
			return added, err
		}
		seen[key] = true
		added++
	}
	return added, nil
}

func (s *Store) UpdateTool(t Tool) error {
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("a tool needs a name")
	}
	_, err := s.db.Exec(`UPDATE tools SET name = ?, kind = ?, detail = ?, notes = ? WHERE id = ?`,
		strings.TrimSpace(t.Name), t.Kind, t.Detail, t.Notes, t.ID)
	return err
}

func (s *Store) DeleteTool(id int64) error {
	_, err := s.db.Exec(`DELETE FROM tools WHERE id = ?`, id)
	return err
}

// CountItems is how many parts are on the shelf, for the pages that only want
// the number and not the rows.
func (s *Store) CountItems() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n)
	return n, err
}

// Capabilities reads the workshop and reduces it in one step, which is what
// nearly every caller actually wants. Printers count: a bed recorded on the
// printing page is the figure a suggestion should be measured against.
func (s *Store) Capabilities() (Capabilities, error) {
	tools, err := s.ListTools()
	if err != nil {
		return Capabilities{}, err
	}
	printers, err := s.ListPrinters()
	if err != nil {
		return Capabilities{}, err
	}
	return SummariseWorkshop(tools, printers), nil
}
