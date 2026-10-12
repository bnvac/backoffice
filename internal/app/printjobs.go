package app

import (
	"database/sql"
	"time"
)

// PrintJob is one G-code file sent to one printer. The file itself is kept,
// named by its hash, so the same print can go again next month without finding
// the slicer project — which is most of the reason to send from here at all.
type PrintJob struct {
	ID        int64
	PrinterID int64
	ItemID    *int64 // the part this was printed for, if any
	Name      string // the file name as sliced, which is what the printer shows
	Blob      string // the stored copy
	SHA256    string
	Size      int64
	Started   bool   // the printer said it started
	Error     string // why it did not arrive, when it did not
	SentAt    time.Time

	Printer  string // joined in for the list
	ItemName string
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

func (s *Store) AddPrintJob(j PrintJob) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO print_jobs (printer_id, item_id, name, blob, sha256,
		size, started, error, sent_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		j.PrinterID, j.ItemID, j.Name, j.Blob, j.SHA256, j.Size, j.Started, j.Error, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecentPrintJobs is the history, newest first, across every printer or for
// one item (pass itemID 0 for all).
func (s *Store) RecentPrintJobs(itemID int64, limit int) ([]PrintJob, error) {
	q := `SELECT j.id, j.printer_id, j.item_id, j.name, j.blob, j.sha256, j.size, j.started,
		j.error, j.sent_at, p.name, COALESCE(i.name, '')
		FROM print_jobs j
		JOIN printers p ON p.id = j.printer_id
		LEFT JOIN items i ON i.id = j.item_id`
	args := []any{}
	if itemID > 0 {
		q += ` WHERE j.item_id = ?`
		args = append(args, itemID)
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
		var item sql.NullInt64
		var sent string
		if err := rows.Scan(&j.ID, &j.PrinterID, &item, &j.Name, &j.Blob, &j.SHA256, &j.Size,
			&j.Started, &j.Error, &sent, &j.Printer, &j.ItemName); err != nil {
			return nil, err
		}
		if item.Valid {
			id := item.Int64
			j.ItemID = &id
		}
		j.SentAt, _ = time.Parse(time.RFC3339, sent)
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) GetPrintJob(id int64) (PrintJob, error) {
	var j PrintJob
	var item sql.NullInt64
	var sent string
	err := s.db.QueryRow(`SELECT id, printer_id, item_id, name, blob, sha256, size, started,
		error, sent_at FROM print_jobs WHERE id = ?`, id).
		Scan(&j.ID, &j.PrinterID, &item, &j.Name, &j.Blob, &j.SHA256, &j.Size, &j.Started,
			&j.Error, &sent)
	if err != nil {
		return PrintJob{}, err
	}
	if item.Valid {
		v := item.Int64
		j.ItemID = &v
	}
	j.SentAt, _ = time.Parse(time.RFC3339, sent)
	return j, nil
}
