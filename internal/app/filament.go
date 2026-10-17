package app

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// A spool is filament you own, counted in grams rather than in units: "one
// spool of PETG" is no use at the printer, "180 g of red PETG" is. Every print
// sent from here takes what the slicer said it would use, and a kitchen scale
// corrects the drift — the spool knows its own empty weight, so weighing it
// gives grams left directly.

type Spool struct {
	ID         int64
	Material   string // a key into Materials
	Brand      string
	ColourName string
	ColourHex  string
	Diameter   float64 // mm
	NetGrams   float64 // filament on it when new
	UsedGrams  float64
	TareGrams  float64 // the empty spool, for weighing
	Price      float64 // what it cost, in the shop's currency; 0 is not known
	PrinterID  *int64  // loaded on
	Location   string
	Notes      string
	DriedAt    time.Time
	EmptyAt    time.Time
	CreatedAt  time.Time

	PrinterName string // joined in
}

func (s Spool) Remaining() float64 { return math.Max(0, s.NetGrams-s.UsedGrams) }

// Percent is for the bar. Rounded down, like every other bar here.
func (s Spool) Percent() int {
	if s.NetGrams <= 0 {
		return 0
	}
	return int(s.Remaining() * 100 / s.NetGrams)
}

// Low is under 150 g — roughly one medium print — which is when it is worth
// knowing before starting a long one.
func (s Spool) Low() bool         { return !s.Empty() && s.Remaining() < 150 }
func (s Spool) Empty() bool       { return !s.EmptyAt.IsZero() }
func (s Spool) Loaded() bool      { return s.PrinterID != nil }
func (s Spool) Family() string    { return ColourFamily(s.ColourHex) }
func (s Spool) Ink() string       { return TextOn(s.ColourHex) }
func (s Spool) PrinterRef() int64 { return derefID(s.PrinterID) }
func (s Spool) CanWeigh() bool    { return s.TareGrams > 0 }

// PerGram is what a gram of this spool cost, or 0 when the price is not known.
func (s Spool) PerGram() float64 {
	if s.Price <= 0 || s.NetGrams <= 0 {
		return 0
	}
	return s.Price / s.NetGrams
}

// Info is the material this spool is, from the list the printing page uses.
func (s Spool) Info() Material {
	m, _ := MaterialByKey(s.Material)
	return m
}

// Label is how a spool is named in a list: colour, then material.
func (s Spool) Label() string {
	name := strings.TrimSpace(s.ColourName)
	mat := s.Info().Name
	if mat == "" {
		mat = strings.ToUpper(s.Material)
	}
	if name == "" {
		return mat
	}
	return name + " " + mat
}

// Swatch is the colour to paint, with a neutral stand-in when none was given.
func (s Spool) Swatch() string {
	if h := normaliseHex(s.ColourHex); h != "" {
		return h
	}
	return "#9a9a9a"
}

// DryNote says whether this spool wants drying before use, from the
// material's appetite for water and when it was last dried. Nylon is wet in a
// day; PETG takes weeks to get bad enough to show.
func (s Spool) DryNote() string {
	if s.Empty() {
		return ""
	}
	m := s.Info()
	since := time.Since(s.DriedAt)
	switch {
	case m.Dry == needMust && s.DriedAt.IsZero():
		return "dry it before printing — " + m.Name + " is wet straight out of the air"
	case m.Dry == needMust && since > 48*time.Hour:
		return fmt.Sprintf("last dried %s ago — dry it again before a print", roughDays(since))
	case m.Dry == needHelps && !s.DriedAt.IsZero() && since > 30*24*time.Hour:
		return fmt.Sprintf("last dried %s ago — worth drying if it strings", roughDays(since))
	}
	return ""
}

func roughDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days < 1:
		return "under a day"
	case days == 1:
		return "a day"
	case days < 60:
		return fmt.Sprintf("%d days", days)
	}
	return fmt.Sprintf("%d months", days/30)
}

// --- densities and slicer names ---------------------------------------------------

// materialDensity is in g/cm³, the typical datasheet figure. It only matters
// when a slicer reports length and not weight, which Cura does.
var materialDensity = map[string]float64{
	"pla": 1.24, "petg": 1.27, "tpu": 1.21, "abs": 1.04, "asa": 1.07, "pp": 0.90,
	"pa": 1.10, "pa-cf": 1.20, "pc": 1.20, "filled-pla": 1.30, "pva": 1.23, "peek": 1.30,
}

func (m Material) Density() float64 {
	if d, ok := materialDensity[m.Key]; ok {
		return d
	}
	return 1.24
}

// MaterialFromSlicer reads the filament type a slicer wrote ("PETG", "PLA+",
// "PA-CF", "PolyTerra PLA") as one of the materials this app knows. It returns
// "" when it cannot tell, which is better than a confident wrong answer.
func MaterialFromSlicer(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	filled := has("CF", "GF", "CARBON", "GLASS")
	// Longest and most specific names first. "PA" is two letters and turns up
	// inside brand names ("PanChroma PETG"), so nylon is only considered once
	// everything with a more distinctive name has had its turn.
	switch {
	case has("PEEK", "PEI", "ULTEM", "PEKK"):
		return "peek"
	case has("PETG", "PCTG", "PET-G"):
		return "petg"
	case has("PVA", "BVOH"):
		return "pva"
	case has("TPU", "TPE", "FLEX"):
		return "tpu"
	case has("ASA"):
		return "asa"
	case has("ABS"):
		return "abs"
	case has("PLA"):
		if has("WOOD", "GLOW", "METAL", "MARBLE", "STONE") || filled {
			return "filled-pla"
		}
		return "pla"
	case has("NYLON", "PAHT", "PA6", "PA12", "PA-", "PA ") || s == "PA":
		if filled {
			return "pa-cf"
		}
		return "pa"
	case s == "PC" || has("POLYCARB") || strings.HasPrefix(s, "PC-") || strings.HasPrefix(s, "PC "):
		return "pc"
	case s == "PP" || has("POLYPROP"):
		return "pp"
	}
	return ""
}

// --- choosing a spool for a print ------------------------------------------------

// SpoolMatch is a suggested spool and why.
type SpoolMatch struct {
	Spool  Spool
	Reason string
	Found  bool
}

// PickSpool chooses the spool a print most likely uses. The one loaded on the
// printer wins if its material fits, because that is what will actually go
// through the nozzle. Otherwise the nearest colour among spools of the right
// material, and on a tie the one with least left, so old spools get used up.
func PickSpool(spools []Spool, printerID int64, meta GcodeMeta) SpoolMatch {
	want := MaterialFromSlicer(meta.Material)
	fits := func(s Spool) bool { return want == "" || s.Material == want }

	for _, s := range spools {
		if !s.Empty() && s.PrinterRef() == printerID && printerID > 0 && fits(s) {
			return SpoolMatch{Spool: s, Found: true, Reason: "loaded on the printer"}
		}
	}

	var candidates []Spool
	for _, s := range spools {
		if !s.Empty() && want != "" && s.Material == want {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return SpoolMatch{}
	}
	colour := normaliseHex(meta.Colour)
	sort.SliceStable(candidates, func(i, j int) bool {
		if colour != "" {
			di, dj := ColourDistance(colour, candidates[i].Swatch()), ColourDistance(colour, candidates[j].Swatch())
			if math.Abs(di-dj) > 5 {
				return di < dj
			}
		}
		return candidates[i].Remaining() < candidates[j].Remaining()
	})
	best := candidates[0]
	reason := "the " + best.Info().Name + " spool with least left"
	if colour != "" && ColourDistance(colour, best.Swatch()) < 25 {
		reason = "closest in colour to what it was sliced with"
	}
	return SpoolMatch{Spool: best, Found: true, Reason: reason}
}

// --- storage ---------------------------------------------------------------------

const spoolColumns = `s.id, s.material, s.brand, s.colour_name, s.colour_hex, s.diameter,
	s.net_grams, s.used_grams, s.tare_grams, s.printer_id, s.location, s.notes, s.dried_at,
	s.empty_at, s.created_at, COALESCE(p.name, ''), s.price`

const spoolFrom = ` FROM spools s LEFT JOIN printers p ON p.id = s.printer_id`

func scanSpool(row rowScanner) (Spool, error) {
	var s Spool
	var printer sql.NullInt64
	var dried, empty, created string
	err := row.Scan(&s.ID, &s.Material, &s.Brand, &s.ColourName, &s.ColourHex, &s.Diameter,
		&s.NetGrams, &s.UsedGrams, &s.TareGrams, &printer, &s.Location, &s.Notes, &dried,
		&empty, &created, &s.PrinterName, &s.Price)
	if err != nil {
		return Spool{}, err
	}
	if printer.Valid {
		v := printer.Int64
		s.PrinterID = &v
	}
	s.DriedAt, _ = time.Parse(time.RFC3339, dried)
	s.EmptyAt, _ = time.Parse(time.RFC3339, empty)
	s.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return s, nil
}

// ListSpools returns the shelf, loaded spools first, then by material and
// colour. Finished spools are left out unless asked for.
func (s *Store) ListSpools(includeEmpty bool) ([]Spool, error) {
	q := `SELECT ` + spoolColumns + spoolFrom
	if !includeEmpty {
		q += ` WHERE s.empty_at = ''`
	}
	q += ` ORDER BY s.empty_at <> '', s.printer_id IS NULL, s.material, s.colour_name COLLATE NOCASE, s.id`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Spool
	for rows.Next() {
		sp, err := scanSpool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *Store) GetSpool(id int64) (Spool, error) {
	return scanSpool(s.db.QueryRow(`SELECT `+spoolColumns+spoolFrom+` WHERE s.id = ?`, id))
}

func (s *Store) checkSpool(sp *Spool) error {
	if _, ok := MaterialByKey(sp.Material); !ok {
		return fmt.Errorf("pick what material it is")
	}
	if sp.NetGrams <= 0 {
		return fmt.Errorf("say how much filament was on it new — usually 1000 g")
	}
	if sp.Diameter <= 0 {
		sp.Diameter = 1.75
	}
	sp.ColourHex = normaliseHex(sp.ColourHex)
	return nil
}

func (s *Store) AddSpool(sp Spool) (int64, error) {
	if err := s.checkSpool(&sp); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO spools (material, brand, colour_name, colour_hex, diameter,
		net_grams, used_grams, tare_grams, location, notes, dried_at, empty_at, created_at, price)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,'',?,?)`,
		sp.Material, strings.TrimSpace(sp.Brand), strings.TrimSpace(sp.ColourName), sp.ColourHex,
		sp.Diameter, sp.NetGrams, math.Max(0, sp.UsedGrams), math.Max(0, sp.TareGrams),
		strings.TrimSpace(sp.Location), strings.TrimSpace(sp.Notes), stamp(sp.DriedAt), nowRFC3339(),
		math.Max(0, sp.Price))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateSpool(sp Spool) error {
	if err := s.checkSpool(&sp); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE spools SET material = ?, brand = ?, colour_name = ?, colour_hex = ?,
		diameter = ?, net_grams = ?, tare_grams = ?, location = ?, notes = ?, price = ? WHERE id = ?`,
		sp.Material, strings.TrimSpace(sp.Brand), strings.TrimSpace(sp.ColourName), sp.ColourHex,
		sp.Diameter, sp.NetGrams, math.Max(0, sp.TareGrams), strings.TrimSpace(sp.Location),
		strings.TrimSpace(sp.Notes), math.Max(0, sp.Price), sp.ID)
	return err
}

func (s *Store) DeleteSpool(id int64) error {
	_, err := s.db.Exec(`DELETE FROM spools WHERE id = ?`, id)
	return err
}

// UseFilament takes grams off a spool, or gives them back when negative. It is
// one statement so two prints finishing at once cannot lose an update.
func (s *Store) UseFilament(id int64, grams float64) error {
	_, err := s.db.Exec(`UPDATE spools SET used_grams = MAX(0, used_grams + ?) WHERE id = ?`, grams, id)
	return err
}

// WeighSpool sets what is left from a scale reading: the whole thing on the
// scale, minus what the empty spool weighs. It refuses when the empty weight
// is unknown, because a guess there is the whole error.
func (s *Store) WeighSpool(id int64, gross float64) (Spool, error) {
	sp, err := s.GetSpool(id)
	if err != nil {
		return Spool{}, err
	}
	if sp.TareGrams <= 0 {
		return sp, fmt.Errorf("say what the empty spool weighs first — it is usually printed on the side, or weigh an empty one")
	}
	if gross <= 0 {
		return sp, fmt.Errorf("enter what the scale reads, in grams")
	}
	left := math.Max(0, gross-sp.TareGrams)
	if left > sp.NetGrams*1.05 {
		return sp, fmt.Errorf("that is more filament than the spool held new — check the empty weight")
	}
	used := math.Max(0, sp.NetGrams-left)
	if _, err := s.db.Exec(`UPDATE spools SET used_grams = ? WHERE id = ?`, used, id); err != nil {
		return sp, err
	}
	sp.UsedGrams = used
	return sp, nil
}

// LoadSpool puts a spool on a printer, taking off whatever was there. A
// printer with one extruder has one spool on it; the app does not pretend
// otherwise. A nil printer unloads.
func (s *Store) LoadSpool(id int64, printerID *int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if printerID != nil {
		if _, err := tx.Exec(`UPDATE spools SET printer_id = NULL WHERE printer_id = ?`, *printerID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE spools SET printer_id = ? WHERE id = ?`, printerID, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkSpoolDried(id int64) error {
	_, err := s.db.Exec(`UPDATE spools SET dried_at = ? WHERE id = ?`, nowRFC3339(), id)
	return err
}

// FinishSpool marks it used up and takes it off the printer. It is kept, so
// its prints still say what they were printed in.
func (s *Store) FinishSpool(id int64) error {
	_, err := s.db.Exec(`UPDATE spools SET empty_at = ?, printer_id = NULL, used_grams = net_grams
		WHERE id = ?`, nowRFC3339(), id)
	return err
}

func (s *Store) LoadedSpool(printerID int64) (Spool, bool) {
	sp, err := scanSpool(s.db.QueryRow(`SELECT `+spoolColumns+spoolFrom+
		` WHERE s.printer_id = ? AND s.empty_at = '' LIMIT 1`, printerID))
	return sp, err == nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// FilamentSummary is the dashboard's line about it.
type FilamentSummary struct {
	Grams  float64
	Spools int
	Low    int
}

func (f FilamentSummary) Kilos() string { return fmt.Sprintf("%.1f kg", f.Grams/1000) }

func SummariseFilament(spools []Spool) FilamentSummary {
	var f FilamentSummary
	for _, s := range spools {
		if s.Empty() {
			continue
		}
		f.Spools++
		f.Grams += s.Remaining()
		if s.Low() {
			f.Low++
		}
	}
	return f
}
