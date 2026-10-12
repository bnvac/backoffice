package app

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Stock is where "how many do I have" stops being a single number.
//
// A part can be on the shelf and still unavailable, because a project that is
// being built has claimed it. Rather than decrementing quantity when a project
// starts -- which loses the difference between "set aside" and "used up" --
// claims are derived from the bills of materials of projects marked building.
// Stock only actually moves when a project is marked built, and that move is
// recorded so it can be undone.

// Commitment is one project's claim on one item.
type Commitment struct {
	ProjectID int64
	Project   string
	Quantity  int
}

// Committed is how many of this item are spoken for by projects being built.
func (i Item) Committed() int {
	n := 0
	for _, c := range i.Claims {
		n += c.Quantity
	}
	return n
}

// Available is what is left once every building project has taken its share.
// It never goes below zero; Oversubscribed is the flag for the case where the
// claims exceed what is actually there.
func (i Item) Available() int {
	if n := i.Quantity - i.Committed(); n > 0 {
		return n
	}
	return 0
}

// Oversubscribed means more has been promised to projects than exists.
func (i Item) Oversubscribed() bool { return i.Committed() > i.Quantity }

// Contended means at least two projects are claiming this part at once, which
// is the case worth naming: each of them looks fine on its own page.
func (i Item) Contended() bool { return len(i.Claims) > 1 }

// ClaimNote is the one-line explanation shown next to a contended part.
func (i Item) ClaimNote() string {
	if len(i.Claims) == 0 {
		return ""
	}
	return fmt.Sprintf("%s committed across %s, you own %d",
		plural(i.Committed(), "piece"), plural(len(i.Claims), "project"), i.Quantity)
}

// Threshold is the low-stock line for this item, falling back to the default
// for rows written before thresholds existed.
func (i Item) Threshold() int {
	if i.LowStock < 0 {
		return 0
	}
	return i.LowStock
}

// Low reports whether free stock has reached the point worth reordering at. It
// measures what is available rather than what is on the shelf, because parts
// promised to a build are not parts you can use.
func (i Item) Low() bool { return i.Available() <= i.Threshold() }

// attachClaims records which building projects have spoken for each item on the
// page. Claims come from the flattened requirement, so a project that reaches a
// part through a sub-assembly holds it just as firmly as one that lists it
// directly.
func (s *Store) attachClaims(items []Item, byID map[int64]int) error {
	claims, err := s.claimsByItem()
	if err != nil {
		return err
	}
	for itemID, cs := range claims {
		if idx, ok := byID[itemID]; ok {
			items[idx].Claims = append(items[idx].Claims, cs...)
		}
	}
	return nil
}

// SetThreshold changes the low-stock line for one item.
func (s *Store) SetThreshold(itemID int64, n int) error {
	if n < 0 {
		n = 0
	}
	_, err := s.db.Exec(`UPDATE items SET low_stock = ? WHERE id = ?`, n, itemID)
	return err
}

// LowStock lists everything at or under its own threshold, scarcest first. It
// is what the dashboard's reorder list is built from.
func (s *Store) LowStock(limit int) ([]Item, error) {
	items, err := s.ListItems(Query{})
	if err != nil {
		return nil, err
	}
	var low []Item
	for _, it := range items {
		if it.Low() {
			low = append(low, it)
		}
	}
	sortByScarcity(low)
	if limit > 0 && len(low) > limit {
		low = low[:limit]
	}
	return low, nil
}

// sortByScarcity puts the parts in most trouble first: oversubscribed, then
// out of stock, then closest to their threshold.
func sortByScarcity(items []Item) {
	slack := func(i Item) int {
		if i.Oversubscribed() {
			return i.Quantity - i.Committed() // negative, so it sorts first
		}
		return i.Available() - i.Threshold()
	}
	sort.SliceStable(items, func(i, j int) bool {
		if a, b := slack(items[i]), slack(items[j]); a != b {
			return a < b
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}

// --- consuming a project ----------------------------------------------------

// ConsumeProject takes a project's parts off the shelf, records exactly how
// many of each it took, and marks the project built. Running it twice is a
// no-op: a project that has already been consumed is left alone.
//
// It deducts what is actually there when the shelf cannot cover a line, rather
// than refusing or driving stock negative -- the shortfall is already visible
// on the project page, and blocking the whole action over one missing resistor
// would just push people to fix the numbers by hand.
func (s *Store) ConsumeProject(id int64) (taken int, short int, err error) {
	p, err := s.GetProject(id)
	if err != nil {
		return 0, 0, err
	}
	if p.Consumed() {
		return 0, 0, fmt.Errorf("this project's parts have already been taken off the shelf")
	}

	// Several lines can point at the same item, and a sub-assembly reaches
	// items that are not on this list at all, so the flattened requirement is
	// what gets taken -- the same figure the reservation used. It is worked out
	// before the transaction opens: the pool holds one connection, so a query
	// issued while a transaction is live would wait on itself forever.
	wanted, err := s.Requirements(id)
	if err != nil {
		return 0, 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	order := make([]int64, 0, len(wanted))
	for itemID := range wanted {
		order = append(order, itemID)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, part := range p.Parts {
		if part.ItemID == nil && part.SubProjectID == nil {
			short += part.Quantity // named but not owned, so nothing to take
		}
	}

	for _, itemID := range order {
		want := wanted[itemID]
		var have int
		if err := tx.QueryRow(`SELECT quantity FROM items WHERE id = ?`, itemID).Scan(&have); err != nil {
			return 0, 0, err
		}
		got := want
		if got > have {
			got, short = have, short+(want-have)
		}
		if got <= 0 {
			continue
		}
		_, err := tx.Exec(`UPDATE items SET quantity = quantity - ?, updated_at = ? WHERE id = ?`,
			got, now, itemID)
		if err != nil {
			return 0, 0, err
		}
		_, err = tx.Exec(`INSERT INTO project_consumption (project_id, item_id, quantity)
			VALUES (?,?,?) ON CONFLICT(project_id, item_id) DO UPDATE SET quantity = quantity + excluded.quantity`,
			id, itemID, got)
		if err != nil {
			return 0, 0, err
		}
		taken += got
	}

	_, err = tx.Exec(`UPDATE projects SET status = 'done', consumed_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id)
	if err != nil {
		return 0, 0, err
	}
	return taken, short, tx.Commit()
}

// ReturnProject undoes ConsumeProject, putting back exactly what was taken.
func (s *Store) ReturnProject(id int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var consumed string
	if err := tx.QueryRow(`SELECT consumed_at FROM projects WHERE id = ?`, id).Scan(&consumed); err != nil {
		return 0, err
	}
	if consumed == "" {
		return 0, fmt.Errorf("this project has not taken any parts off the shelf")
	}

	rows, err := tx.Query(`SELECT item_id, quantity FROM project_consumption WHERE project_id = ?`, id)
	if err != nil {
		return 0, err
	}
	back := map[int64]int{}
	for rows.Next() {
		var itemID int64
		var qty int
		if err := rows.Scan(&itemID, &qty); err != nil {
			rows.Close()
			return 0, err
		}
		back[itemID] = qty
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	total := 0
	for itemID, qty := range back {
		_, err := tx.Exec(`UPDATE items SET quantity = quantity + ?, updated_at = ? WHERE id = ?`,
			qty, now, itemID)
		if err != nil {
			return 0, err
		}
		total += qty
	}
	if _, err := tx.Exec(`DELETE FROM project_consumption WHERE project_id = ?`, id); err != nil {
		return 0, err
	}
	_, err = tx.Exec(`UPDATE projects SET status = 'building', consumed_at = '', updated_at = ? WHERE id = ?`,
		now, id)
	if err != nil {
		return 0, err
	}
	return total, tx.Commit()
}
