package app

import (
	"fmt"
	"sort"
)

// A sub-assembly is a project used as a line in another project: a 5 V supply
// module you design once and then put inside three different things.
//
// Everything that asks "what does this project need" therefore has to look
// through those lines rather than at them -- reservations, the shortfall, the
// cost estimate and the consume step all work on the flattened requirement.
// That flattening happens here, once, so those four cannot disagree.

const maxAssemblyDepth = 8 // deep enough for real work, shallow enough to bound

// requirement is one item and how many of it a project needs in total.
type requirement struct {
	ItemID int64
	Needed int
}

// bomNode is the raw parts list, loaded once for every project so the tree can
// be walked in memory rather than with a query per level.
type bomNode struct {
	ItemID   *int64
	SubID    *int64
	Quantity int
}

func (s *Store) loadBOMs() (map[int64][]bomNode, error) {
	rows, err := s.db.Query(`SELECT project_id, item_id, sub_project_id, quantity FROM project_parts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]bomNode{}
	for rows.Next() {
		var projectID int64
		var n bomNode
		if err := rows.Scan(&projectID, &n.ItemID, &n.SubID, &n.Quantity); err != nil {
			return nil, err
		}
		if n.Quantity < 1 {
			n.Quantity = 1
		}
		out[projectID] = append(out[projectID], n)
	}
	return out, rows.Err()
}

// expand flattens one project's requirements, following sub-assemblies and
// multiplying quantities down the tree. A project that somehow contains itself
// stops at the cycle rather than looping: the guard exists because a parts list
// is edited by hand and nothing stops someone drawing a circle.
func expand(boms map[int64][]bomNode, projectID int64, multiplier, depth int, seen map[int64]bool, into map[int64]int) {
	if depth > maxAssemblyDepth || seen[projectID] {
		return
	}
	seen[projectID] = true
	defer delete(seen, projectID)

	for _, node := range boms[projectID] {
		switch {
		case node.ItemID != nil:
			into[*node.ItemID] += node.Quantity * multiplier
		case node.SubID != nil:
			expand(boms, *node.SubID, node.Quantity*multiplier, depth+1, seen, into)
		}
	}
}

// Requirements is everything one project needs, sub-assemblies included.
func (s *Store) Requirements(projectID int64) (map[int64]int, error) {
	boms, err := s.loadBOMs()
	if err != nil {
		return nil, err
	}
	out := map[int64]int{}
	expand(boms, projectID, 1, 0, map[int64]bool{}, out)
	return out, nil
}

// claimsByItem works out what every project being built has spoken for, with
// sub-assemblies expanded. This is the single place reservations come from.
func (s *Store) claimsByItem() (map[int64][]Commitment, error) {
	rows, err := s.db.Query(`SELECT id, name FROM projects
		WHERE status = 'building' AND consumed_at = '' ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	type building struct {
		id   int64
		name string
	}
	var projects []building
	for rows.Next() {
		var b building
		if err := rows.Scan(&b.id, &b.name); err != nil {
			rows.Close()
			return nil, err
		}
		projects = append(projects, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return map[int64][]Commitment{}, nil
	}

	boms, err := s.loadBOMs()
	if err != nil {
		return nil, err
	}
	out := map[int64][]Commitment{}
	for _, p := range projects {
		needs := map[int64]int{}
		expand(boms, p.id, 1, 0, map[int64]bool{}, needs)

		ids := make([]int64, 0, len(needs))
		for id := range needs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, itemID := range ids {
			out[itemID] = append(out[itemID], Commitment{
				ProjectID: p.id, Project: p.name, Quantity: needs[itemID],
			})
		}
	}
	return out, nil
}

// Buildable is how many complete copies of a project the shelf could supply,
// ignoring what other projects have claimed. It is what a sub-assembly line
// means by "have".
func buildable(boms map[int64][]bomNode, projectID int64, free map[int64]int) int {
	needs := map[int64]int{}
	expand(boms, projectID, 1, 0, map[int64]bool{}, needs)
	if len(needs) == 0 {
		return 0
	}
	best := -1
	for itemID, per := range needs {
		if per <= 0 {
			continue
		}
		n := free[itemID] / per
		if best < 0 || n < best {
			best = n
		}
	}
	if best < 0 {
		return 0
	}
	return best
}

// SubShortfall lists what a sub-assembly line is missing, so the parent project
// can say which underlying parts are the problem rather than just "you cannot
// build two of those".
func (s *Store) SubShortfall(projectID int64, copies int) ([]requirement, error) {
	boms, err := s.loadBOMs()
	if err != nil {
		return nil, err
	}
	needs := map[int64]int{}
	expand(boms, projectID, copies, 0, map[int64]bool{}, needs)

	items, err := s.ListItems(Query{})
	if err != nil {
		return nil, err
	}
	free := map[int64]int{}
	for _, it := range items {
		free[it.ID] = it.Available()
	}

	var out []requirement
	for id, need := range needs {
		if short := need - free[id]; short > 0 {
			out = append(out, requirement{ItemID: id, Needed: short})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Needed > out[j].Needed })
	return out, nil
}

// wouldCycle reports whether making sub a part of parent creates a loop.
func (s *Store) wouldCycle(parent, sub int64) (bool, error) {
	if parent == sub {
		return true, nil
	}
	boms, err := s.loadBOMs()
	if err != nil {
		return false, err
	}
	// Reachable from sub: if the parent is in there, adding the line closes a
	// circle.
	var reaches func(from int64, depth int, seen map[int64]bool) bool
	reaches = func(from int64, depth int, seen map[int64]bool) bool {
		if depth > maxAssemblyDepth || seen[from] {
			return false
		}
		seen[from] = true
		for _, node := range boms[from] {
			if node.SubID == nil {
				continue
			}
			if *node.SubID == parent || reaches(*node.SubID, depth+1, seen) {
				return true
			}
		}
		return false
	}
	return reaches(sub, 0, map[int64]bool{}), nil
}

// AddSubAssembly puts one project inside another.
func (s *Store) AddSubAssembly(parent, sub int64, qty int, note string) error {
	if qty < 1 {
		qty = 1
	}
	cycle, err := s.wouldCycle(parent, sub)
	if err != nil {
		return err
	}
	if cycle {
		return fmt.Errorf("that would make the project contain itself")
	}
	_, err = s.db.Exec(`INSERT INTO project_parts (project_id, sub_project_id, name, quantity, note)
		VALUES (?,?,'',?,?)`, parent, sub, qty, note)
	if err != nil {
		return err
	}
	return s.touchProject(parent)
}
