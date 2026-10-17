package app

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"path"
	"time"
)

// Reading the printer's own history, so every print counts — not just the
// ones sent from here. A print started from the slicer or the touchscreen
// takes its filament off whatever spool is loaded; a print sent from here and
// then cancelled halfway gives back what it did not use.
//
// The printer's job id is kept on each record, so reading the same history
// twice changes nothing.

// SyncResult is one print the printer has finished since the last read.
type SyncResult struct {
	Job      PrintJob
	Printer  Printer
	Spool    *Spool  // what it came off, after the change
	Grams    float64 // what it actually used
	GaveBack float64 // grams returned to the spool because it stopped early
	Seconds  float64 // how long it actually printed
	Thumb    []byte
}

func (r SyncResult) Duration() string {
	return roughDuration(time.Duration(r.Seconds * float64(time.Second)))
}

// syncPrinter reads one printer's history and records what is new.
func (a *App) syncPrinter(ctx context.Context, p Printer) ([]SyncResult, error) {
	link, ok := linkFor(p)
	if !ok {
		return nil, nil
	}
	hr, ok := link.(HistoryReader)
	if !ok {
		return nil, nil
	}

	// First contact: start from now, not the beginning of time — two years of
	// prints should not come off today's spools. A print already under way
	// is included, so pairing mid-print still counts that one.
	if p.HistorySince == 0 {
		since := float64(time.Now().Unix())
		if recent, err := hr.History(ctx, 0, 1, true); err == nil && len(recent) == 1 && recent[0].Outcome() == "" {
			since = recent[0].Start - 1
		} else if err != nil {
			return nil, err
		}
		return nil, a.store.SetHistorySince(p.ID, since)
	}

	jobs, err := hr.History(ctx, p.HistorySince, 100, false)
	if err != nil {
		return nil, err
	}
	// The mark moves to the newest finished print's start, but never past a
	// print still running, so that one is read again once it ends. Reading a
	// print twice is harmless: each is recorded once, by the printer's job id.
	var results []SyncResult
	mark, hold := p.HistorySince, math.Inf(1)
	for _, rj := range jobs {
		if rj.Outcome() == "" {
			hold = math.Min(hold, rj.Start-1)
			continue
		}
		mark = math.Max(mark, rj.Start)
		if rj.ID == "" || a.store.HasRemoteJob(p.ID, rj.ID) {
			continue
		}
		res, err := a.recordRemoteJob(ctx, p, hr, rj)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	mark = math.Min(mark, hold)
	if mark > p.HistorySince {
		a.store.SetHistorySince(p.ID, mark)
	}
	return results, nil
}

// recordRemoteJob matches a finished print to one sent from here, or records
// it as printed from elsewhere, and settles its filament.
func (a *App) recordRemoteJob(ctx context.Context, p Printer, hr HistoryReader, rj RemoteJob) (SyncResult, error) {
	res := SyncResult{Printer: p, Seconds: rj.PrintDuration}
	start := time.Unix(int64(rj.Start), 0)
	end := time.Unix(int64(rj.End), 0)
	name := path.Base(rj.File)

	job, matched := a.store.SentJobFor(p.ID, name, start)
	var spool *Spool
	if matched && job.SpoolID != nil {
		if sp, err := a.store.GetSpool(*job.SpoolID); err == nil {
			spool = &sp
		}
	} else if !matched {
		if sp, ok := a.store.LoadedSpool(p.ID); ok {
			spool = &sp
		}
	}
	diameter, density := 1.75, 1.24 // PLA, when nothing better is known
	if spool != nil {
		diameter, density = spool.Diameter, spool.Info().Density()
	}
	res.Grams = rj.GramsUsed(diameter, density)

	if matched {
		if !job.Returned() && job.SpoolID != nil && res.Grams > 0 {
			// The send took the slicer's estimate. Settle it to what was used:
			// a cancelled print gives most of it back.
			if delta := job.Grams - res.Grams; math.Abs(delta) >= 1 {
				a.store.UseFilament(*job.SpoolID, -delta)
				if delta > 0 {
					res.GaveBack = delta
				}
				job.Grams = res.Grams
			}
		}
		if rj.PrintDuration > 0 {
			job.EstSeconds = rj.PrintDuration
		}
	} else {
		job = PrintJob{PrinterID: p.ID, Name: name, Started: true, Via: "printer",
			SentAt: start, EstSeconds: rj.PrintDuration}
		if spool != nil && res.Grams > 0 {
			id := spool.ID
			job.SpoolID, job.Grams = &id, res.Grams
			a.store.UseFilament(id, res.Grams)
		}
	}
	job.RemoteID, job.Outcome, job.FinishedAt = rj.ID, rj.Outcome(), end

	// The preview: from our own copy of the file if we have one, otherwise
	// from the printer while the file is still on it.
	if job.Thumb == "" {
		var png []byte
		if job.Blob != "" {
			if f, err := a.gcode.Open(job.Blob); err == nil {
				png = GcodeThumbnail(f)
				f.Close()
			}
		}
		if png == nil && rj.Exists {
			png, _ = hr.Thumbnail(ctx, rj.File)
		}
		if png != nil && a.thumbs != nil {
			if blob, _, _, err := a.thumbs.Save(bytes.NewReader(png), ".png"); err == nil {
				job.Thumb = blob
				res.Thumb = png
			}
		}
	} else if a.thumbs != nil {
		res.Thumb = a.readThumb(job.Thumb)
	}

	if matched {
		if err := a.store.SettleJob(job); err != nil {
			return res, err
		}
	} else {
		id, err := a.store.AddPrintJob(job)
		if err != nil {
			return res, err
		}
		job.ID = id
	}
	if job.SpoolID != nil {
		if sp, err := a.store.GetSpool(*job.SpoolID); err == nil {
			res.Spool = &sp
			job.SpoolName = sp.Label()
		}
	}
	res.Job = job
	return res, nil
}

func (a *App) readThumb(blob string) []byte {
	f, err := a.thumbs.Open(blob)
	if err != nil {
		return nil
	}
	defer f.Close()
	var b bytes.Buffer
	b.ReadFrom(f)
	return b.Bytes()
}

// --- storage ---------------------------------------------------------------------

func (s *Store) SetHistorySince(printerID int64, since float64) error {
	_, err := s.db.Exec(`UPDATE printers SET history_since = ? WHERE id = ?`, since, printerID)
	return err
}

func (s *Store) HasRemoteJob(printerID int64, remoteID string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM print_jobs WHERE printer_id = ? AND remote_id = ?`, printerID, remoteID).Scan(&n)
	return n > 0
}

// SentJobFor finds the job sent from here that a printer's history entry is:
// same printer, same file name, sent before the print started (within two
// days — a file can sit on the printer a while before someone presses go),
// and not already matched to another entry.
func (s *Store) SentJobFor(printerID int64, name string, started time.Time) (PrintJob, bool) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM print_jobs
		WHERE printer_id = ? AND remote_id = '' AND via = '' AND error = '' AND name = ?
		  AND sent_at <= ? AND sent_at >= ?
		ORDER BY sent_at DESC, id DESC LIMIT 1`,
		printerID, name, started.Add(2*time.Minute).UTC().Format(time.RFC3339),
		started.Add(-48*time.Hour).UTC().Format(time.RFC3339)).Scan(&id)
	if err != nil {
		return PrintJob{}, false
	}
	j, err := s.GetPrintJob(id)
	return j, err == nil
}

// SettleJob writes back what the printer said about a job sent from here.
func (s *Store) SettleJob(j PrintJob) error {
	res, err := s.db.Exec(`UPDATE print_jobs SET remote_id = ?, outcome = ?, finished_at = ?,
		grams = ?, est_seconds = ?, thumb = ? WHERE id = ? AND remote_id = ''`,
		j.RemoteID, j.Outcome, stamp(j.FinishedAt), j.Grams, j.EstSeconds, j.Thumb, j.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- what to say about it ------------------------------------------------------------

// Headline and Body are the notification, and the line on the dashboard.
func (r SyncResult) Headline() string {
	switch r.Job.Outcome {
	case "completed":
		return "Print finished on " + r.Printer.Name
	case "cancelled":
		return "Print cancelled on " + r.Printer.Name
	}
	return "Print failed on " + r.Printer.Name
}

func (r SyncResult) Body() string {
	msg := r.Job.Name
	if r.Seconds > 0 {
		if r.Job.Outcome == "completed" {
			msg += " · " + r.Duration()
		} else {
			msg += " · stopped after " + r.Duration()
		}
	}
	if r.Grams >= 1 {
		msg += fmt.Sprintf(" · %.0f g", r.Grams)
		if r.Spool != nil {
			msg += fmt.Sprintf(" of %s, %.0f g left", r.Spool.Label(), r.Spool.Remaining())
		}
	}
	if r.GaveBack >= 1 && r.Spool != nil {
		msg += fmt.Sprintf(" · %.0f g back on the spool", r.GaveBack)
	}
	if r.Job.Via == "printer" {
		msg += " · started from the printer"
	}
	return msg
}
