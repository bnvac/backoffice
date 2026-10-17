package app

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PrintJob is one G-code file sent to one printer. The file itself is kept,
// named by its hash, so the same print can go again next month without finding
// the slicer project — which is most of the reason to send from here at all.
type PrintJob struct {
	ID        int64
	PrinterID int64
	ItemID    *int64 // the part this was printed for, if any
	ProjectID *int64 // the build it was printed for, if any
	Name      string // the file name as sliced, which is what the printer shows
	Blob      string // the stored copy
	SHA256    string
	Size      int64
	Started   bool   // the printer said it started
	Error     string // why it did not arrive, when it did not
	SentAt    time.Time

	// What it used, from the slicer's own figures in the file.
	SpoolID    *int64
	Grams      float64
	EstSeconds float64
	ReturnedAt time.Time // filament put back because it did not print

	// What the printer itself recorded, once its history has been read.
	RemoteID   string
	Outcome    string // "" not known | completed | cancelled | failed
	FinishedAt time.Time
	Via        string // "" sent from here | "printer" started from the slicer or screen
	Thumb      string // the slicer's preview, in the thumbs store

	Printer   string // joined in for the list
	ItemName  string
	SpoolName string
	SpoolHex  string
}

func (j PrintJob) Returned() bool  { return !j.ReturnedAt.IsZero() }
func (j PrintJob) SpoolRef() int64 { return derefID(j.SpoolID) }
func (j PrintJob) Estimate() string {
	if j.EstSeconds <= 0 {
		return ""
	}
	return roughDuration(time.Duration(j.EstSeconds * float64(time.Second)))
}

// CanReturn is whether "it did not print" means anything: there is filament to
// put back, or estimated hours to stop counting.
func (j PrintJob) CanReturn() bool {
	// Once the printer has said how it went, its word stands.
	return j.OK() && !j.Returned() && j.Outcome == "" && ((j.SpoolID != nil && j.Grams > 0) || j.EstSeconds > 0)
}

// GivesBack is whether marking it as not printed returns any filament.
func (j PrintJob) GivesBack() bool { return j.SpoolID != nil && j.Grams > 0 }

// OutcomeLabel is how it ended, in a word, once the printer has said.
func (j PrintJob) OutcomeLabel() string {
	switch j.Outcome {
	case "completed":
		return "printed"
	case "cancelled":
		return "cancelled"
	case "failed":
		return "failed"
	}
	return ""
}

func (j PrintJob) OK() bool          { return j.Error == "" }
func (j PrintJob) SizeLabel() string { return humanBytes(j.Size) }

// ItemRef exists because a template cannot dereference *int64.
func (j PrintJob) ItemRef() int64 {
	if j.ItemID == nil {
		return 0
	}
	return *j.ItemID
}

// sentStamp is now, unless the job carries its own time (one read back from
// the printer's history happened when the printer says it did).
func sentStamp(t time.Time) string {
	if t.IsZero() {
		return nowRFC3339()
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Store) AddPrintJob(j PrintJob) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO print_jobs (printer_id, item_id, name, blob, sha256,
		size, started, error, sent_at, spool_id, grams, est_seconds, project_id,
		remote_id, outcome, finished_at, via, thumb)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.PrinterID, j.ItemID, j.Name, j.Blob, j.SHA256, j.Size, j.Started, j.Error, sentStamp(j.SentAt),
		j.SpoolID, j.Grams, j.EstSeconds, j.ProjectID, j.RemoteID, j.Outcome, stamp(j.FinishedAt), j.Via, j.Thumb)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// JobFilter narrows the history. Zero fields do not filter.
type JobFilter struct{ Item, Printer, Spool, Project int64 }

// RecentPrintJobs is the history, newest first, across every printer or for
// one item (pass itemID 0 for all).
func (s *Store) RecentPrintJobs(itemID int64, limit int) ([]PrintJob, error) {
	return s.PrintJobs(JobFilter{Item: itemID}, limit)
}

func (s *Store) PrintJobs(f JobFilter, limit int) ([]PrintJob, error) {
	q := `SELECT j.id, j.printer_id, j.item_id, j.name, j.blob, j.sha256, j.size, j.started,
		j.error, j.sent_at, p.name, COALESCE(i.name, ''), j.spool_id, j.grams, j.est_seconds,
		j.returned_at, COALESCE(sp.colour_name, ''), COALESCE(sp.material, ''),
		COALESCE(sp.colour_hex, ''), j.project_id, j.remote_id, j.outcome, j.finished_at, j.via, j.thumb
		FROM print_jobs j
		JOIN printers p ON p.id = j.printer_id
		LEFT JOIN items i ON i.id = j.item_id
		LEFT JOIN spools sp ON sp.id = j.spool_id`
	var where []string
	args := []any{}
	if f.Item > 0 {
		where, args = append(where, "j.item_id = ?"), append(args, f.Item)
	}
	if f.Printer > 0 {
		where, args = append(where, "j.printer_id = ?"), append(args, f.Printer)
	}
	if f.Spool > 0 {
		where, args = append(where, "j.spool_id = ?"), append(args, f.Spool)
	}
	if f.Project > 0 {
		where, args = append(where, "j.project_id = ?"), append(args, f.Project)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += ` ORDER BY j.sent_at DESC, j.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PrintJob
	for rows.Next() {
		var j PrintJob
		var item, spool, project sql.NullInt64
		var sent, returned, colourName, material, finished string
		if err := rows.Scan(&j.ID, &j.PrinterID, &item, &j.Name, &j.Blob, &j.SHA256, &j.Size,
			&j.Started, &j.Error, &sent, &j.Printer, &j.ItemName, &spool, &j.Grams,
			&j.EstSeconds, &returned, &colourName, &material, &j.SpoolHex, &project,
			&j.RemoteID, &j.Outcome, &finished, &j.Via, &j.Thumb); err != nil {
			return nil, err
		}
		j.FinishedAt, _ = time.Parse(time.RFC3339, finished)
		if project.Valid {
			id := project.Int64
			j.ProjectID = &id
		}
		if material != "" {
			// Named the same way as everywhere else, rather than rebuilt in SQL.
			j.SpoolName = Spool{ColourName: colourName, Material: material}.Label()
		}
		if item.Valid {
			id := item.Int64
			j.ItemID = &id
		}
		if spool.Valid {
			id := spool.Int64
			j.SpoolID = &id
		}
		j.SentAt, _ = time.Parse(time.RFC3339, sent)
		j.ReturnedAt, _ = time.Parse(time.RFC3339, returned)
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) GetPrintJob(id int64) (PrintJob, error) {
	var j PrintJob
	var item, spool, project sql.NullInt64
	var sent, returned, finished string
	err := s.db.QueryRow(`SELECT id, printer_id, item_id, name, blob, sha256, size, started,
		error, sent_at, spool_id, grams, est_seconds, returned_at, project_id,
		remote_id, outcome, finished_at, via, thumb FROM print_jobs WHERE id = ?`, id).
		Scan(&j.ID, &j.PrinterID, &item, &j.Name, &j.Blob, &j.SHA256, &j.Size, &j.Started,
			&j.Error, &sent, &spool, &j.Grams, &j.EstSeconds, &returned, &project,
			&j.RemoteID, &j.Outcome, &finished, &j.Via, &j.Thumb)
	if err != nil {
		return PrintJob{}, err
	}
	if project.Valid {
		v := project.Int64
		j.ProjectID = &v
	}
	if item.Valid {
		v := item.Int64
		j.ItemID = &v
	}
	if spool.Valid {
		v := spool.Int64
		j.SpoolID = &v
	}
	j.SentAt, _ = time.Parse(time.RFC3339, sent)
	j.ReturnedAt, _ = time.Parse(time.RFC3339, returned)
	j.FinishedAt, _ = time.Parse(time.RFC3339, finished)
	return j, nil
}

// ReturnJobFilament marks a print as never having happened: its filament goes
// back on the spool and its hours stop counting. Once only, in one
// transaction, so a double click cannot give the filament back twice.
func (s *Store) ReturnJobFilament(id int64) (PrintJob, error) {
	j, err := s.GetPrintJob(id)
	if err != nil {
		return j, err
	}
	if !j.CanReturn() {
		return j, fmt.Errorf("nothing to put back for that print")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return j, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE print_jobs SET returned_at = ? WHERE id = ? AND returned_at = ''`,
		nowRFC3339(), id)
	if err != nil {
		return j, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return j, fmt.Errorf("that print's filament was already put back")
	}
	if j.GivesBack() {
		if _, err := tx.Exec(`UPDATE spools SET used_grams = MAX(0, used_grams - ?) WHERE id = ?`,
			j.Grams, *j.SpoolID); err != nil {
			return j, err
		}
	}
	return j, tx.Commit()
}
