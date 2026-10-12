package app

import (
	"fmt"
	"strings"
	"time"
)

// The build log records what happened. Steps record what is meant to happen,
// which is a different thing and the one you need at eleven at night when you
// cannot remember whether you already flashed the board.
//
// A step is deliberately thin: an order, a line of text, and a tick. Anything
// richer turns into a project management tool, and the parts list is already
// where the detail lives.

type Step struct {
	ID        int64
	ProjectID int64
	Position  int
	Body      string
	DoneAt    time.Time
	CreatedAt time.Time
}

func (s Step) Done() bool { return !s.DoneAt.IsZero() }

// Progress is the state of a step list, worked out once so the page and the
// dashboard cannot disagree about it.
type Progress struct {
	Done  int
	Total int
}

func (p Progress) Any() bool { return p.Total > 0 }

// Percent is for the bar. It rounds down, so a list is never shown as finished
// until it actually is.
func (p Progress) Percent() int {
	if p.Total == 0 {
		return 0
	}
	return p.Done * 100 / p.Total
}

func (p Progress) Complete() bool { return p.Total > 0 && p.Done == p.Total }

// Left is what the heading says next to the bar.
func (p Progress) Left() string {
	switch {
	case p.Total == 0:
		return ""
	case p.Done == p.Total:
		return "all done"
	default:
		return fmt.Sprintf("%d of %d", p.Done, p.Total)
	}
}

// StepProgress reduces a list. Taking the list rather than hitting the database
// means a page that already loaded the steps does not go back for them.
func StepProgress(steps []Step) Progress {
	p := Progress{Total: len(steps)}
	for _, s := range steps {
		if s.Done() {
			p.Done++
		}
	}
	return p
}

// ParseSteps reads a pasted plan. One step per line, with the bullets and
// numbering people paste in taken off, because retyping a checklist you already
// wrote somewhere else is the reason checklists do not get used.
func ParseSteps(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = trimStepMarker(line)
		if line == "" || len(line) > 500 {
			continue
		}
		out = append(out, line)
	}
	return out
}

// trimStepMarker removes a leading bullet, dash, or "3." / "3)" numbering, and
// a leading "[ ]" or "[x]" checkbox from somebody's markdown.
func trimStepMarker(line string) string {
	line = strings.TrimLeft(line, "-*•–—‣ \t")

	// "[ ] solder the headers" and "[x] ..." both become the text.
	if len(line) >= 3 && line[0] == '[' && line[2] == ']' {
		line = strings.TrimSpace(line[3:])
	}

	// A number followed by . or ) and a space. Checked rather than trimmed so
	// that "2.54mm headers" keeps its measurement.
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits < len(line)-1 {
		if c := line[digits]; c == '.' || c == ')' {
			if line[digits+1] == ' ' {
				line = strings.TrimSpace(line[digits+2:])
			}
		}
	}
	return strings.TrimSpace(line)
}

// --- storage -------------------------------------------------------------------

func (s *Store) ListSteps(projectID int64) ([]Step, error) {
	rows, err := s.db.Query(`SELECT id, project_id, position, body, done_at, created_at
		FROM project_steps WHERE project_id = ? ORDER BY position, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Step
	for rows.Next() {
		var st Step
		var done, created string
		if err := rows.Scan(&st.ID, &st.ProjectID, &st.Position, &st.Body, &done, &created); err != nil {
			return nil, err
		}
		st.DoneAt, _ = time.Parse(time.RFC3339, done)
		st.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, st)
	}
	return out, rows.Err()
}

// nextStepPosition reads the end of the list. Positions are sparse on purpose:
// deleting the middle of a list must not renumber everything around it.
func (s *Store) nextStepPosition(projectID int64) (int, error) {
	var max *int
	err := s.db.QueryRow(`SELECT MAX(position) FROM project_steps WHERE project_id = ?`,
		projectID).Scan(&max)
	if err != nil {
		return 0, err
	}
	if max == nil {
		return 0, nil
	}
	return *max + 1, nil
}

func (s *Store) AddStep(projectID int64, body string) (int64, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return 0, fmt.Errorf("a step needs something to do")
	}
	pos, err := s.nextStepPosition(projectID)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO project_steps (project_id, position, body, done_at, created_at)
		VALUES (?,?,?,'',?)`, projectID, pos, body, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddSteps stores a whole pasted plan in one go, in the order given.
func (s *Store) AddSteps(projectID int64, bodies []string) (int, error) {
	added := 0
	for _, b := range bodies {
		if _, err := s.AddStep(projectID, b); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

// SetStepDone ticks or unticks. Unticking is as important as ticking: the
// common use is realising you marked the wrong line.
func (s *Store) SetStepDone(id int64, done bool) error {
	stamp := ""
	if done {
		stamp = nowRFC3339()
	}
	_, err := s.db.Exec(`UPDATE project_steps SET done_at = ? WHERE id = ?`, stamp, id)
	return err
}

// ToggleStep flips one, which is what a single checkbox post means.
func (s *Store) ToggleStep(id int64) (Step, error) {
	var st Step
	var done string
	err := s.db.QueryRow(`SELECT id, project_id, position, body, done_at FROM project_steps
		WHERE id = ?`, id).Scan(&st.ID, &st.ProjectID, &st.Position, &st.Body, &done)
	if err != nil {
		return Step{}, err
	}
	st.DoneAt, _ = time.Parse(time.RFC3339, done)
	if err := s.SetStepDone(id, !st.Done()); err != nil {
		return Step{}, err
	}
	return st, nil
}

func (s *Store) UpdateStep(id int64, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("a step needs something to do")
	}
	_, err := s.db.Exec(`UPDATE project_steps SET body = ? WHERE id = ?`, body, id)
	return err
}

func (s *Store) DeleteStep(id int64) (int64, error) {
	var projectID int64
	if err := s.db.QueryRow(`SELECT project_id FROM project_steps WHERE id = ?`, id).
		Scan(&projectID); err != nil {
		return 0, err
	}
	_, err := s.db.Exec(`DELETE FROM project_steps WHERE id = ?`, id)
	return projectID, err
}

// MoveStep shifts one up or down by swapping positions with its neighbour.
// Swapping rather than renumbering keeps it to two writes whatever the length
// of the list, and the single connection makes that worth caring about.
func (s *Store) MoveStep(id int64, up bool) (int64, error) {
	steps, projectID, err := s.stepsAround(id)
	if err != nil {
		return 0, err
	}
	at := -1
	for i, st := range steps {
		if st.ID == id {
			at = i
			break
		}
	}
	if at < 0 {
		return projectID, nil
	}
	swap := at + 1
	if up {
		swap = at - 1
	}
	if swap < 0 || swap >= len(steps) {
		return projectID, nil // already at the end; not an error
	}

	// Read both before touching either: with one connection, a write opened
	// mid-read would wait on a transaction that cannot finish.
	a, b := steps[at], steps[swap]
	tx, err := s.db.Begin()
	if err != nil {
		return projectID, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE project_steps SET position = ? WHERE id = ?`, b.Position, a.ID); err != nil {
		return projectID, err
	}
	if _, err := tx.Exec(`UPDATE project_steps SET position = ? WHERE id = ?`, a.Position, b.ID); err != nil {
		return projectID, err
	}
	return projectID, tx.Commit()
}

// stepsAround loads the list a step belongs to. Two steps can share a position
// if a list was built oddly, so the ordering falls back to id and the swap uses
// whatever order came back rather than assuming positions are distinct.
func (s *Store) stepsAround(id int64) ([]Step, int64, error) {
	var projectID int64
	if err := s.db.QueryRow(`SELECT project_id FROM project_steps WHERE id = ?`, id).
		Scan(&projectID); err != nil {
		return nil, 0, err
	}
	steps, err := s.ListSteps(projectID)
	return steps, projectID, err
}

// ClearDoneSteps unticks the lot, for building the same thing twice.
func (s *Store) ClearDoneSteps(projectID int64) error {
	_, err := s.db.Exec(`UPDATE project_steps SET done_at = '' WHERE project_id = ?`, projectID)
	return err
}

// ProjectProgress is the step state for a list of projects, for the index page
// and the dashboard. One query rather than one per project.
func (s *Store) ProjectProgress() (map[int64]Progress, error) {
	rows, err := s.db.Query(`SELECT project_id, COUNT(*),
		SUM(CASE WHEN done_at <> '' THEN 1 ELSE 0 END)
		FROM project_steps GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]Progress{}
	for rows.Next() {
		var id int64
		var total, done int
		if err := rows.Scan(&id, &total, &done); err != nil {
			return nil, err
		}
		out[id] = Progress{Done: done, Total: total}
	}
	return out, rows.Err()
}
