package app

import (
	"strings"
	"time"
)

// The build log is the thing you wish you had written down six months later:
// what you changed, what it measured, which pin you got wrong the first time.
// Entries are dated, attributed, and can carry photos, because "the blue wire
// goes here" is a picture rather than a sentence.

type LogEntry struct {
	ID        int64
	ProjectID int64
	Body      string
	Author    string
	CreatedAt time.Time

	Photos []LogPhoto
}

type LogPhoto struct {
	ID       int64
	EntryID  int64
	Filename string
	Position int
}

// Lines splits an entry for rendering, so a pasted multi-line note keeps its
// shape without the template trusting raw HTML.
func (e LogEntry) Lines() []string {
	var out []string
	for _, line := range strings.Split(e.Body, "\n") {
		out = append(out, strings.TrimRight(line, " \t"))
	}
	return out
}

func (s *Store) AddLogEntry(projectID int64, body, author string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO log_entries (project_id, body, author, created_at)
		VALUES (?,?,?,?)`, projectID, body, author, nowRFC3339())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, s.touchProject(projectID)
}

func (s *Store) AddLogPhoto(entryID int64, filename string) error {
	var next int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(position)+1, 0) FROM log_photos WHERE entry_id = ?`, entryID).Scan(&next)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO log_photos (entry_id, filename, position) VALUES (?,?,?)`,
		entryID, filename, next)
	return err
}

// LogEntries returns a project's notebook, newest first.
func (s *Store) LogEntries(projectID int64) ([]LogEntry, error) {
	rows, err := s.db.Query(`SELECT id, project_id, body, author, created_at
		FROM log_entries WHERE project_id = ? ORDER BY created_at DESC, id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogEntry
	byID := map[int64]int{}
	for rows.Next() {
		var e LogEntry
		var created string
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Body, &e.Author, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, created)
		byID[e.ID] = len(out)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	photos, err := s.db.Query(`SELECT lp.id, lp.entry_id, lp.filename, lp.position
		FROM log_photos lp JOIN log_entries le ON le.id = lp.entry_id
		WHERE le.project_id = ? ORDER BY lp.entry_id, lp.position, lp.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer photos.Close()
	for photos.Next() {
		var p LogPhoto
		if err := photos.Scan(&p.ID, &p.EntryID, &p.Filename, &p.Position); err != nil {
			return nil, err
		}
		if idx, ok := byID[p.EntryID]; ok {
			out[idx].Photos = append(out[idx].Photos, p)
		}
	}
	return out, photos.Err()
}

// DeleteLogEntry removes an entry and reports its photo files so the caller can
// unlink them.
func (s *Store) DeleteLogEntry(id int64) (projectID int64, files []string, err error) {
	if err = s.db.QueryRow(`SELECT project_id FROM log_entries WHERE id = ?`, id).Scan(&projectID); err != nil {
		return 0, nil, err
	}
	files, err = s.photoFilenames(`SELECT filename FROM log_photos WHERE entry_id = ?`, id)
	if err != nil {
		return projectID, nil, err
	}
	_, err = s.db.Exec(`DELETE FROM log_entries WHERE id = ?`, id)
	return projectID, files, err
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
