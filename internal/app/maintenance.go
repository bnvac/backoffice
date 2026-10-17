package app

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// A printer wears by the hour, not by the calendar: belts stretch and lead
// screws dry out in proportion to how much it has printed. So maintenance is
// counted in print hours where that is the honest measure, and in days where
// it is not (dust gets into fans whether it prints or not).
//
// Print hours come from the printer when it keeps count — Moonraker does — and
// otherwise from the slicer's estimate for every print sent from here, plus
// whatever the machine had done before it was added.

type MaintTask struct {
	ID         int64
	PrinterID  int64
	Task       string
	EveryHours float64
	EveryDays  int
	LastHours  float64
	LastAt     time.Time
	Notes      string
	CreatedAt  time.Time
}

// TaskState is a task measured against where the printer is now.
type TaskState struct {
	MaintTask
	HoursLeft float64 // negative when overdue
	DaysLeft  float64
	Due       bool
	Soon      bool // inside the last tenth of its interval
}

// Headline is the phrase the list shows, from whichever limit comes first.
func (t TaskState) Headline() string {
	byHours := t.EveryHours > 0
	byDays := t.EveryDays > 0
	// When both apply, speak about the one that runs out first, comparing
	// hours left against days left as if the printer ran a few hours a day.
	useHours := byHours && (!byDays || t.HoursLeft/3 < t.DaysLeft)
	switch {
	case useHours && t.HoursLeft <= 0:
		return fmt.Sprintf("overdue by %s", hoursText(-t.HoursLeft))
	case useHours:
		return fmt.Sprintf("in %s of printing", hoursText(t.HoursLeft))
	case byDays && t.DaysLeft <= 0:
		return fmt.Sprintf("overdue by %s", daysText(-t.DaysLeft))
	case byDays:
		return "in " + daysText(t.DaysLeft)
	}
	return ""
}

func (t TaskState) Interval() string {
	var parts []string
	if t.EveryHours > 0 {
		parts = append(parts, "every "+hoursText(t.EveryHours)+" of printing")
	}
	if t.EveryDays > 0 {
		parts = append(parts, "every "+daysText(float64(t.EveryDays)))
	}
	return strings.Join(parts, " or ")
}

func hoursText(h float64) string {
	if h < 1 {
		return "under an hour"
	}
	n := int(math.Round(h))
	if n == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", n)
}

func daysText(d float64) string {
	n := int(math.Ceil(d))
	switch {
	case n <= 0:
		return "today"
	case n == 1:
		return "1 day"
	case n%7 == 0 && n <= 56:
		if n == 7 {
			return "1 week"
		}
		return fmt.Sprintf("%d weeks", n/7)
	}
	return fmt.Sprintf("%d days", n)
}

// State works a task out against the printer's hours now.
func (t MaintTask) State(hoursNow float64, now time.Time) TaskState {
	s := TaskState{MaintTask: t}
	if t.EveryHours > 0 {
		s.HoursLeft = t.LastHours + t.EveryHours - hoursNow
		if s.HoursLeft <= 0 {
			s.Due = true
		} else if s.HoursLeft <= t.EveryHours/10 {
			s.Soon = true
		}
	}
	if t.EveryDays > 0 {
		s.DaysLeft = float64(t.EveryDays) - now.Sub(t.LastAt).Hours()/24
		if s.DaysLeft <= 0 {
			s.Due = true
		} else if s.DaysLeft <= float64(t.EveryDays)/10 {
			s.Soon = true
		}
	}
	return s
}

// MaintPresets are starting points, not a manufacturer's schedule: generic
// jobs for a desktop FDM printer with intervals people commonly use. The page
// says so.
var MaintPresets = []MaintTask{
	{Task: "Clean the build plate with IPA", EveryHours: 10},
	{Task: "Check belt tension", EveryHours: 100},
	{Task: "Lubricate the Z lead screws", EveryHours: 200},
	{Task: "Check the nozzle for wear and partial clogs", EveryHours: 250},
	{Task: "Tighten frame and gantry bolts", EveryHours: 300},
	{Task: "Blow the dust out of the fans and the electronics bay", EveryDays: 90},
}

// --- print hours ----------------------------------------------------------------

// Hours is the printer's running total and where the figure came from.
type Hours struct {
	Total  float64
	Source string // "printer" | "estimate"
}

func (h Hours) Label() string {
	if h.Source == "printer" {
		return fmt.Sprintf("%.0f h, as the printer counts it", h.Total)
	}
	return fmt.Sprintf("about %.0f h, from what was sent from here", h.Total)
}

// PrinterHours prefers the printer's own counter. Without one it adds up the
// slicer's estimate for every print that arrived and was not marked as never
// printed — the same rule the filament follows, so the two cannot disagree
// about whether a print happened. A file that arrived while the printer was
// busy and was started later from its screen still counts.
func (s *Store) PrinterHours(p Printer) (Hours, error) {
	if p.ReportedSeconds > 0 {
		return Hours{Total: p.BaseHours + p.ReportedSeconds/3600, Source: "printer"}, nil
	}
	var secs sql.NullFloat64
	err := s.db.QueryRow(`SELECT SUM(est_seconds) FROM print_jobs
		WHERE printer_id = ? AND error = '' AND returned_at = ''`, p.ID).Scan(&secs)
	if err != nil {
		return Hours{}, err
	}
	return Hours{Total: p.BaseHours + secs.Float64/3600, Source: "estimate"}, nil
}

// RecordReportedSeconds keeps the printer's own counter, so the maintenance
// list still works on a day the printer is switched off.
func (s *Store) RecordReportedSeconds(printerID int64, secs float64) error {
	_, err := s.db.Exec(`UPDATE printers SET reported_seconds = ?, reported_at = ? WHERE id = ?`,
		secs, nowRFC3339(), printerID)
	return err
}

func (s *Store) SetBaseHours(printerID int64, hours float64) error {
	_, err := s.db.Exec(`UPDATE printers SET base_hours = ? WHERE id = ?`, math.Max(0, hours), printerID)
	return err
}

// --- tasks ----------------------------------------------------------------------

func (s *Store) ListMaint(printerID int64) ([]MaintTask, error) {
	rows, err := s.db.Query(`SELECT id, printer_id, task, every_hours, every_days, last_hours,
		last_at, notes, created_at FROM maintenance WHERE printer_id = ? ORDER BY id`, printerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaintTask
	for rows.Next() {
		var t MaintTask
		var last, created string
		if err := rows.Scan(&t.ID, &t.PrinterID, &t.Task, &t.EveryHours, &t.EveryDays,
			&t.LastHours, &last, &t.Notes, &created); err != nil {
			return nil, err
		}
		t.LastAt, _ = time.Parse(time.RFC3339, last)
		t.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddMaint starts the clock now: a task added today is not overdue today just
// because nobody has recorded doing it before.
func (s *Store) AddMaint(t MaintTask, hoursNow float64) (int64, error) {
	t.Task = strings.TrimSpace(t.Task)
	if t.Task == "" {
		return 0, fmt.Errorf("say what the job is")
	}
	if t.EveryHours <= 0 && t.EveryDays <= 0 {
		return 0, fmt.Errorf("give it an interval — hours of printing, days, or both")
	}
	now := nowRFC3339()
	res, err := s.db.Exec(`INSERT INTO maintenance (printer_id, task, every_hours, every_days,
		last_hours, last_at, notes, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		t.PrinterID, t.Task, math.Max(0, t.EveryHours), max(0, t.EveryDays), hoursNow, now,
		strings.TrimSpace(t.Notes), now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DoneMaint resets the clock and keeps a line in the log.
func (s *Store) DoneMaint(id int64, hoursNow float64, note string) (int64, error) {
	var printerID int64
	var task string
	if err := s.db.QueryRow(`SELECT printer_id, task FROM maintenance WHERE id = ?`, id).
		Scan(&printerID, &task); err != nil {
		return 0, err
	}
	now := nowRFC3339()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE maintenance SET last_hours = ?, last_at = ? WHERE id = ?`,
		hoursNow, now, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO maintenance_log (printer_id, task, hours, note, done_at)
		VALUES (?,?,?,?,?)`, printerID, task, hoursNow, strings.TrimSpace(note), now); err != nil {
		return 0, err
	}
	return printerID, tx.Commit()
}

// MaintPrinter is which printer a task belongs to.
func (s *Store) MaintPrinter(id int64) (int64, error) {
	var printerID int64
	err := s.db.QueryRow(`SELECT printer_id FROM maintenance WHERE id = ?`, id).Scan(&printerID)
	return printerID, err
}

func (s *Store) DeleteMaint(id int64) (int64, error) {
	var printerID int64
	if err := s.db.QueryRow(`SELECT printer_id FROM maintenance WHERE id = ?`, id).Scan(&printerID); err != nil {
		return 0, err
	}
	_, err := s.db.Exec(`DELETE FROM maintenance WHERE id = ?`, id)
	return printerID, err
}

type MaintLog struct {
	Task   string
	Hours  float64
	Note   string
	DoneAt time.Time
}

func (s *Store) MaintHistory(printerID int64, limit int) ([]MaintLog, error) {
	rows, err := s.db.Query(`SELECT task, hours, note, done_at FROM maintenance_log
		WHERE printer_id = ? ORDER BY done_at DESC, id DESC LIMIT ?`, printerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaintLog
	for rows.Next() {
		var l MaintLog
		var done string
		if err := rows.Scan(&l.Task, &l.Hours, &l.Note, &done); err != nil {
			return nil, err
		}
		l.DoneAt, _ = time.Parse(time.RFC3339, done)
		out = append(out, l)
	}
	return out, rows.Err()
}

// MaintDue counts what is due per printer, for the cards and the dashboard,
// without asking any printer anything.
func (s *Store) MaintDue(printers []Printer) map[int64]int {
	out := map[int64]int{}
	now := time.Now()
	for _, p := range printers {
		hours, err := s.PrinterHours(p)
		if err != nil {
			continue
		}
		tasks, err := s.ListMaint(p.ID)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.State(hours.Total, now).Due {
				out[p.ID]++
			}
		}
	}
	return out
}

// --- spares ---------------------------------------------------------------------

// Spares are ordinary shelf items marked as belonging to a printer: nozzles,
// a PTFE tube, a spare hotend fan. The shelf already knows how many there are
// and when that is too few; this only says which printer they are for.

func (s *Store) Spares(printerID int64) ([]Item, error) {
	rows, err := s.db.Query(`SELECT item_id FROM printer_spares WHERE printer_id = ?`, printerID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	// Read after closing: GetItem needs the one connection.
	var out []Item
	for _, id := range ids {
		if it, err := s.GetItem(id); err == nil {
			out = append(out, it)
		}
	}
	return out, nil
}

func (s *Store) AddSpare(printerID, itemID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO printer_spares (printer_id, item_id) VALUES (?, ?)`,
		printerID, itemID)
	return err
}

func (s *Store) RemoveSpare(printerID, itemID int64) error {
	_, err := s.db.Exec(`DELETE FROM printer_spares WHERE printer_id = ? AND item_id = ?`, printerID, itemID)
	return err
}
