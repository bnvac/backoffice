package app

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// An order closes the only loop that was still open: a project says what is
// short, the shopping list says what to buy, and until now the trail stopped
// there. Receiving an order is the exact inverse of building a project -- one
// puts stock on the shelf, the other takes it off, and both record enough of
// what they did to be undone.

// OrderStatuses are the states an order moves through. Only "ordered" counts as
// stock on its way to you.
var OrderStatuses = []string{"draft", "ordered", "arrived", "cancelled"}

type Order struct {
	ID         int64
	Source     string // the shop
	Reference  string // their order number
	Status     string
	PlacedAt   time.Time
	ExpectedAt time.Time
	ArrivedAt  time.Time
	Tracking   string
	Shipping   float64
	Currency   string
	Notes      string
	ProjectID  *int64
	CreatedAt  time.Time
	UpdatedAt  time.Time

	Lines   []OrderLine
	Project string
}

type OrderLine struct {
	ID        int64
	OrderID   int64
	ItemID    *int64
	Name      string
	Quantity  int
	UnitPrice float64
	Currency  string
	Received  int
	Note      string

	Item *Item
}

func (l OrderLine) Label() string {
	if l.Item != nil {
		return l.Item.Name
	}
	return l.Name
}

// Outstanding is how many of this line have not been put on the shelf yet.
func (l OrderLine) Outstanding() int {
	if n := l.Quantity - l.Received; n > 0 {
		return n
	}
	return 0
}

func (l OrderLine) LineTotal() float64 { return l.UnitPrice * float64(l.Quantity) }
func (l OrderLine) Display() string    { return formatMoney(l.LineTotal(), l.Currency) }

// Total is what the order cost, shipping included.
func (o Order) Total() float64 {
	total := o.Shipping
	for _, l := range o.Lines {
		total += l.LineTotal()
	}
	return total
}

func (o Order) Display() string { return formatMoney(o.Total(), o.Currency) }

func (o Order) Pieces() int {
	n := 0
	for _, l := range o.Lines {
		n += l.Quantity
	}
	return n
}

// Outstanding counts what is still owed across the whole order, which is what
// makes a partial delivery visible.
func (o Order) Outstanding() int {
	n := 0
	for _, l := range o.Lines {
		n += l.Outstanding()
	}
	return n
}

func (o Order) Received() bool  { return !o.ArrivedAt.IsZero() }
func (o Order) InFlight() bool  { return o.Status == "ordered" }
func (o Order) Cancelled() bool { return o.Status == "cancelled" }

// Partial means some of it turned up and some of it did not.
func (o Order) Partial() bool {
	return o.Outstanding() > 0 && o.Outstanding() < o.Pieces()
}

// Due describes when the order is expected, in the terms you would use out loud.
func (o Order) Due() string {
	if o.Received() || o.ExpectedAt.IsZero() {
		return ""
	}
	days := int(math.Round(time.Until(o.ExpectedAt).Hours() / 24))
	switch {
	case days < -1:
		return fmt.Sprintf("%s overdue", plural(-days, "day"))
	case days <= 0:
		return "due today"
	case days == 1:
		return "due tomorrow"
	default:
		return fmt.Sprintf("due in %s", plural(days, "day"))
	}
}

func (o Order) Overdue() bool {
	return o.InFlight() && !o.ExpectedAt.IsZero() && time.Now().After(o.ExpectedAt.Add(24*time.Hour))
}

// TookDays is how long the order actually took, once it has arrived.
func (o Order) TookDays() int { return daysBetween(o.PlacedAt, o.ArrivedAt) }

// daysBetween counts whole days from one moment to another.
//
// Both ends are truncated to their date first, because they do not arrive at
// the same precision: a placed date comes from a date picker and is midnight,
// while an arrival is the instant somebody pressed the button. Subtracting one
// from the other directly adds however many hours into the day it happened to
// be, which turns an 18-day delivery into a 19-day one.
func daysBetween(from, to time.Time) int {
	if from.IsZero() || to.IsZero() {
		return 0
	}
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(math.Round(b.Sub(a).Hours() / 24))
}

// --- storage ----------------------------------------------------------------

const orderCols = `id, source, reference, status, placed_at, expected_at, arrived_at,
	tracking, shipping, currency, notes, project_id, created_at, updated_at`

func scanOrder(sc interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var placed, expected, arrived, created, updated string
	err := sc.Scan(&o.ID, &o.Source, &o.Reference, &o.Status, &placed, &expected, &arrived,
		&o.Tracking, &o.Shipping, &o.Currency, &o.Notes, &o.ProjectID, &created, &updated)
	if err != nil {
		return o, err
	}
	o.PlacedAt = parseDay(placed)
	o.ExpectedAt = parseDay(expected)
	o.ArrivedAt = parseDay(arrived)
	o.CreatedAt, _ = time.Parse(time.RFC3339, created)
	o.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return o, nil
}

// parseDay accepts both a full timestamp and the plain date a <input type=date>
// submits, because both end up in these columns.
func parseDay(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	t, _ := time.Parse("2006-01-02", s)
	return t
}

// Day renders a date for an <input type=date>.
func Day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02")
}

func (o Order) PlacedDay() string   { return Day(o.PlacedAt) }
func (o Order) ExpectedDay() string { return Day(o.ExpectedAt) }

func (s *Store) ListOrders() ([]Order, error) {
	rows, err := s.db.Query(`SELECT ` + orderCols + ` FROM orders
		ORDER BY CASE status WHEN 'ordered' THEN 0 WHEN 'draft' THEN 1 ELSE 2 END,
			COALESCE(NULLIF(expected_at, ''), placed_at) DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Order
	byID := map[int64]int{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		byID[o.ID] = len(out)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Lines for every order in one query, as everywhere else.
	lines, err := s.db.Query(`SELECT id, order_id, item_id, name, quantity, unit_price,
		currency, received, note FROM order_lines ORDER BY order_id, id`)
	if err != nil {
		return nil, err
	}
	defer lines.Close()
	for lines.Next() {
		var l OrderLine
		if err := lines.Scan(&l.ID, &l.OrderID, &l.ItemID, &l.Name, &l.Quantity,
			&l.UnitPrice, &l.Currency, &l.Received, &l.Note); err != nil {
			return nil, err
		}
		if idx, ok := byID[l.OrderID]; ok {
			out[idx].Lines = append(out[idx].Lines, l)
		}
	}
	return out, lines.Err()
}

func (s *Store) GetOrder(id int64) (Order, error) {
	o, err := scanOrder(s.db.QueryRow(`SELECT `+orderCols+` FROM orders WHERE id = ?`, id))
	if err != nil {
		return o, err
	}
	if o.ProjectID != nil {
		_ = s.db.QueryRow(`SELECT name FROM projects WHERE id = ?`, *o.ProjectID).Scan(&o.Project)
	}

	rows, err := s.db.Query(`SELECT id, order_id, item_id, name, quantity, unit_price,
		currency, received, note FROM order_lines WHERE order_id = ? ORDER BY id`, id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	var itemIDs []int64
	for rows.Next() {
		var l OrderLine
		if err := rows.Scan(&l.ID, &l.OrderID, &l.ItemID, &l.Name, &l.Quantity,
			&l.UnitPrice, &l.Currency, &l.Received, &l.Note); err != nil {
			return o, err
		}
		if l.ItemID != nil {
			itemIDs = append(itemIDs, *l.ItemID)
		}
		o.Lines = append(o.Lines, l)
	}
	if err := rows.Err(); err != nil {
		return o, err
	}

	if len(itemIDs) > 0 {
		items, err := s.ListItems(Query{})
		if err != nil {
			return o, err
		}
		byID := map[int64]*Item{}
		for i := range items {
			byID[items[i].ID] = &items[i]
		}
		for i := range o.Lines {
			if o.Lines[i].ItemID != nil {
				o.Lines[i].Item = byID[*o.Lines[i].ItemID]
			}
		}
	}
	return o, nil
}

func (s *Store) CreateOrder(o Order) (int64, error) {
	if o.Currency == "" {
		o.Currency = "USD"
	}
	if o.Status == "" {
		o.Status = "draft"
	}
	now := nowRFC3339()
	res, err := s.db.Exec(`INSERT INTO orders
		(source, reference, status, placed_at, expected_at, tracking, shipping,
		 currency, notes, project_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.Source, o.Reference, o.Status, Day(o.PlacedAt), Day(o.ExpectedAt),
		o.Tracking, o.Shipping, o.Currency, o.Notes, o.ProjectID, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateOrder(o Order) error {
	// An arrived order keeps its status: the receipt, not the dropdown, is what
	// decides whether stock has moved.
	var arrived string
	if err := s.db.QueryRow(`SELECT arrived_at FROM orders WHERE id = ?`, o.ID).Scan(&arrived); err != nil {
		return err
	}
	if arrived != "" {
		o.Status = "arrived"
	}
	_, err := s.db.Exec(`UPDATE orders SET source=?, reference=?, status=?, placed_at=?,
		expected_at=?, tracking=?, shipping=?, currency=?, notes=?, project_id=?, updated_at=?
		WHERE id=?`,
		o.Source, o.Reference, o.Status, Day(o.PlacedAt), Day(o.ExpectedAt),
		o.Tracking, o.Shipping, o.Currency, o.Notes, o.ProjectID, nowRFC3339(), o.ID)
	return err
}

func (s *Store) DeleteOrder(id int64) error {
	_, err := s.db.Exec(`DELETE FROM orders WHERE id = ?`, id)
	return err
}

func (s *Store) AddOrderLine(orderID int64, l OrderLine) error {
	if l.Quantity < 1 {
		l.Quantity = 1
	}
	if l.Currency == "" {
		l.Currency = "USD"
	}
	if l.ItemID == nil && strings.TrimSpace(l.Name) == "" {
		return fmt.Errorf("a line needs a part, or at least a name")
	}
	_, err := s.db.Exec(`INSERT INTO order_lines
		(order_id, item_id, name, quantity, unit_price, currency, note)
		VALUES (?,?,?,?,?,?,?)`,
		orderID, l.ItemID, strings.TrimSpace(l.Name), l.Quantity, l.UnitPrice, l.Currency, l.Note)
	if err != nil {
		return err
	}
	return s.touchOrder(orderID)
}

func (s *Store) DeleteOrderLine(lineID int64) (int64, error) {
	var orderID int64
	var received int
	err := s.db.QueryRow(`SELECT order_id, received FROM order_lines WHERE id = ?`, lineID).
		Scan(&orderID, &received)
	if err != nil {
		return 0, err
	}
	if received > 0 {
		return orderID, fmt.Errorf("that line has already been put on the shelf — un-receive the order first")
	}
	if _, err := s.db.Exec(`DELETE FROM order_lines WHERE id = ?`, lineID); err != nil {
		return orderID, err
	}
	return orderID, s.touchOrder(orderID)
}

func (s *Store) touchOrder(id int64) error {
	_, err := s.db.Exec(`UPDATE orders SET updated_at = ? WHERE id = ?`, nowRFC3339(), id)
	return err
}

// ReceiveOrder puts an order's outstanding quantities on the shelf.
//
// It records what each line actually received rather than assuming, so a
// partial delivery can be received now and again later without double-counting,
// and un-receiving puts back precisely what was added.
//
// It also updates each line's price, because what you actually paid is a better
// figure than what the shop's page said last time it was scraped.
func (s *Store) ReceiveOrder(id int64) (received int, err error) {
	o, err := s.GetOrder(id)
	if err != nil {
		return 0, err
	}
	if o.Outstanding() == 0 {
		return 0, fmt.Errorf("every line on this order is already on the shelf")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := nowRFC3339()
	arrivedAt := time.Now().UTC()
	for _, l := range o.Lines {
		outstanding := l.Outstanding()
		if outstanding == 0 {
			continue
		}
		if l.ItemID != nil {
			_, err := tx.Exec(`UPDATE items SET quantity = quantity + ?, updated_at = ? WHERE id = ?`,
				outstanding, now, *l.ItemID)
			if err != nil {
				return 0, err
			}
		}
		_, err := tx.Exec(`UPDATE order_lines SET received = quantity WHERE id = ?`, l.ID)
		if err != nil {
			return 0, err
		}
		received += outstanding
	}
	_, err = tx.Exec(`UPDATE orders SET status='arrived', arrived_at=?, updated_at=? WHERE id=?`,
		arrivedAt.Format(time.RFC3339), now, id)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	// How long it really took, measured against the arrival just recorded --
	// the copy of the order loaded above still says it has not arrived.
	lead := daysBetween(o.PlacedAt, arrivedAt)
	if lead <= 0 {
		lead = DefaultLeadDays(o.Source)
	}

	// Prices are recorded outside the transaction: a price is a nicety, and
	// failing to write one must not undo a delivery that really arrived.
	for _, l := range o.Lines {
		if l.ItemID == nil || l.UnitPrice <= 0 || o.Source == "" {
			continue
		}
		_ = s.SetPrice(*l.ItemID, Price{
			Source: o.Source, Amount: l.UnitPrice, Currency: l.Currency, LeadDays: lead,
		})
	}
	return received, nil
}

// UnreceiveOrder takes back exactly what receiving added.
func (s *Store) UnreceiveOrder(id int64) (int, error) {
	o, err := s.GetOrder(id)
	if err != nil {
		return 0, err
	}
	if !o.Received() {
		return 0, fmt.Errorf("this order has not been received")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := nowRFC3339()
	total := 0
	for _, l := range o.Lines {
		if l.Received == 0 {
			continue
		}
		if l.ItemID != nil {
			// Clamped at zero: the parts may have been used since.
			_, err := tx.Exec(`UPDATE items SET quantity = MAX(0, quantity - ?), updated_at = ? WHERE id = ?`,
				l.Received, now, *l.ItemID)
			if err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(`UPDATE order_lines SET received = 0 WHERE id = ?`, l.ID); err != nil {
			return 0, err
		}
		total += l.Received
	}
	_, err = tx.Exec(`UPDATE orders SET status='ordered', arrived_at='', updated_at=? WHERE id=?`, now, id)
	if err != nil {
		return 0, err
	}
	return total, tx.Commit()
}

// OrderFromShortfall drafts an order holding everything a project is short of,
// grouped by the shop that is cheapest for each part. It returns one order per
// shop, because that is how you actually buy.
func (s *Store) OrderFromShortfall(projectID int64) ([]int64, error) {
	p, err := s.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	short := p.Shortfall()
	if len(short) == 0 {
		return nil, fmt.Errorf("nothing is missing from %s", p.Name)
	}

	// Group by the cheapest known source, with everything unpriced in one
	// "unknown" bucket rather than dropped.
	type line struct {
		part  ProjectPart
		price *Price
	}
	bySource := map[string][]line{}
	var order []string
	for _, part := range short {
		source := ""
		var price *Price
		if part.Item != nil {
			if best := part.Item.Best(); best != nil {
				source, price = best.Source, best
			}
		}
		if _, seen := bySource[source]; !seen {
			order = append(order, source)
		}
		bySource[source] = append(bySource[source], line{part, price})
	}
	sort.Strings(order)

	var ids []int64
	for _, source := range order {
		id, err := s.CreateOrder(Order{
			Source:    source,
			Status:    "draft",
			ProjectID: &projectID,
			Notes:     "Shortfall for " + p.Name,
			Currency:  "USD",
		})
		if err != nil {
			return ids, err
		}
		for _, l := range bySource[source] {
			ol := OrderLine{Quantity: l.part.Short(), Note: "for " + p.Name}
			if l.part.Item != nil {
				id := l.part.Item.ID
				ol.ItemID = &id
			} else {
				ol.Name = l.part.Label()
			}
			if l.price != nil {
				ol.UnitPrice, ol.Currency = l.price.Amount, l.price.Currency
			}
			if err := s.AddOrderLine(id, ol); err != nil {
				return ids, err
			}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// --- what the shops actually do ---------------------------------------------

// LeadTimeActual is how long a shop really takes, measured from your own
// delivered orders rather than from what its website claims.
type LeadTimeActual struct {
	Source   string
	Orders   int
	MinDays  int
	MaxDays  int
	MeanDays int
	Quoted   int // the figure recorded against prices from this source
}

// Drift is the gap between the quote and reality, which is the number worth
// knowing when a build is waiting on a part.
func (l LeadTimeActual) Drift() string {
	if l.Quoted <= 0 {
		return ""
	}
	switch d := l.MeanDays - l.Quoted; {
	case d > 1:
		return fmt.Sprintf("%s slower than the %s quoted", plural(d, "day"), plural(l.Quoted, "day"))
	case d < -1:
		return fmt.Sprintf("%s faster than the %s quoted", plural(-d, "day"), plural(l.Quoted, "day"))
	}
	return "about as quoted"
}

func (s *Store) LeadTimes() ([]LeadTimeActual, error) {
	rows, err := s.db.Query(`SELECT source, placed_at, arrived_at FROM orders
		WHERE arrived_at <> '' AND placed_at <> '' AND source <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byShop := map[string][]int{}
	for rows.Next() {
		var source, placed, arrived string
		if err := rows.Scan(&source, &placed, &arrived); err != nil {
			return nil, err
		}
		p, a := parseDay(placed), parseDay(arrived)
		if p.IsZero() || a.IsZero() || a.Before(p) {
			continue
		}
		byShop[source] = append(byShop[source], daysBetween(p, a))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []LeadTimeActual
	for source, days := range byShop {
		sort.Ints(days)
		sum := 0
		for _, d := range days {
			sum += d
		}
		actual := LeadTimeActual{
			Source: source, Orders: len(days),
			MinDays: days[0], MaxDays: days[len(days)-1],
			MeanDays: int(math.Round(float64(sum) / float64(len(days)))),
		}
		err := s.db.QueryRow(`SELECT MAX(lead_days) FROM prices WHERE source = ?`, source).
			Scan(&actual.Quoted)
		if err != nil && err != sql.ErrNoRows {
			actual.Quoted = 0
		}
		out = append(out, actual)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Orders > out[j].Orders })
	return out, nil
}

// Incoming is how many of an item are on their way, so a part that is out of
// stock but already ordered does not look like something to order again.
func (s *Store) Incoming() (map[int64]int, error) {
	rows, err := s.db.Query(`SELECT ol.item_id, SUM(ol.quantity - ol.received)
		FROM order_lines ol JOIN orders o ON o.id = ol.order_id
		WHERE ol.item_id IS NOT NULL AND o.status = 'ordered' AND ol.quantity > ol.received
		GROUP BY ol.item_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = int(n)
	}
	return out, rows.Err()
}

// ProjectRef exists because html/template cannot dereference a *int64.
func (o Order) ProjectRef() int64 {
	if o.ProjectID == nil {
		return 0
	}
	return *o.ProjectID
}
