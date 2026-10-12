package app

import (
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Item is a single kind of part on a shelf: a reel of resistors, a tray of
// ESP32s, one specific SBC. Quantity lives on the item, not on a separate
// stock table -- a homelab does not need lot tracking.
type Item struct {
	ID           int64
	Name         string
	Category     string
	Quantity     int
	Location     string
	PartNumber   string
	Manufacturer string
	Value        string
	Link         string
	Notes        string
	FolderID     *int64
	LowStock     int    // reorder when free stock reaches this
	Footprint    string // KiCad land pattern name, e.g. Resistor_SMD:R_0805_2012Metric
	CreatedAt    time.Time
	UpdatedAt    time.Time

	FolderName string
	Tags       []Tag
	Photos     []Photo
	Prices     []Price
	Interfaces []string
	Specs      []Spec
	Refs       []Reference
	Models     []Model      // cases and mounts somebody has drawn for this
	Claims     []Commitment // projects being built that have spoken for this
}

// Pinouts are the references worth showing as pictures on the item page.
func (i Item) Pinouts() []Reference {
	var out []Reference
	for _, r := range i.Refs {
		if r.IsImage() {
			out = append(out, r)
		}
	}
	return out
}

// Docs are the reference links, as opposed to stored images.
func (i Item) Docs() []Reference {
	var out []Reference
	for _, r := range i.Refs {
		if !r.IsImage() {
			out = append(out, r)
		}
	}
	return out
}

// Tag is a label plus the icon shown with it.
type Tag struct {
	Name string
	Icon string
}

// Price is what one source charges for an item. At most one row per source, so
// re-importing from Adafruit updates that figure rather than piling up.
type Price struct {
	Source    string
	Amount    float64
	Currency  string
	URL       string
	LeadDays  int // typical wait for delivery; 0 means nobody has said
	UpdatedAt time.Time
}

// Lead renders the shipping wait for display.
func (p Price) Lead() string {
	switch {
	case p.LeadDays <= 0:
		return ""
	case p.LeadDays == 1:
		return "~1 day"
	default:
		return fmt.Sprintf("~%d days", p.LeadDays)
	}
}

// DefaultLeadDays is the usual wait for shops people actually order from. It is
// only a starting guess -- the figure is editable per price, because it depends
// on where you live as much as on the shop.
func DefaultLeadDays(source string) int {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "amazon":
		return 2
	case "adafruit", "sparkfun", "pimoroni", "the pi hut", "thepihut":
		return 5
	case "digi-key", "digikey", "mouser", "farnell", "rs":
		return 3
	case "ebay":
		return 10
	case "lcsc", "jlcpcb":
		return 12
	case "aliexpress", "banggood", "alibaba":
		return 30
	}
	return 0
}

func (p Price) Display() string { return formatMoney(p.Amount, p.Currency) }

func formatMoney(amount float64, currency string) string {
	if currency == "" {
		currency = "USD"
	}
	symbol := map[string]string{"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥"}[currency]
	if symbol == "" {
		return fmt.Sprintf("%s %.2f", currency, amount)
	}
	return fmt.Sprintf("%s%.2f", symbol, amount)
}

// Best is the cheapest recorded price, or nil when none is known. It returns a
// pointer rather than (Price, bool) because html/template can only call methods
// returning one value, or a value and an error.
func (i Item) Best() *Price {
	var best *Price
	for idx := range i.Prices {
		if best == nil || i.Prices[idx].Amount < best.Amount {
			best = &i.Prices[idx]
		}
	}
	return best
}

// Fastest is the recorded price that arrives soonest, or nil when no source
// has a known lead time. Cheapest and fastest are rarely the same shop, which
// is the whole point of tracking both.
func (i Item) Fastest() *Price {
	var fastest *Price
	for idx := range i.Prices {
		p := &i.Prices[idx]
		if p.LeadDays <= 0 {
			continue
		}
		if fastest == nil || p.LeadDays < fastest.LeadDays {
			fastest = p
		}
	}
	return fastest
}

// LineValue is the cheapest price multiplied by how many are in stock.
func (i Item) LineValue() float64 {
	if best := i.Best(); best != nil {
		return best.Amount * float64(i.Quantity)
	}
	return 0
}

// Thumb is the filename of the cover image, or "" when the item has no photo.
func (i Item) Thumb() string {
	if len(i.Photos) == 0 {
		return ""
	}
	return i.Photos[0].Filename
}

func (i Item) TagString() string {
	names := make([]string, len(i.Tags))
	for j, t := range i.Tags {
		names[j] = t.Name
	}
	return strings.Join(names, ", ")
}

// FolderRef and InFolder exist because html/template cannot dereference or
// compare a *int64, and folder membership is optional.
func (i Item) FolderRef() int64 {
	if i.FolderID == nil {
		return 0
	}
	return *i.FolderID
}

func (i Item) InFolder(id int64) bool { return i.FolderID != nil && *i.FolderID == id }

type Photo struct {
	ID       int64
	ItemID   int64
	Filename string
	Position int
}

// Folder is a user-made grouping shown on the dashboard -- a project, a bench,
// a parts cabinet. Folders nest, and are deliberately separate from location
// (where a thing physically is) and type (what a thing is).
type Folder struct {
	ID       int64
	Name     string
	ParentID *int64
	Depth    int

	// Direct contents.
	ItemCount int
	// Contents including every descendant folder, which is what the dashboard
	// card shows.
	TotalItems  int
	TotalPieces int
	Cover       string
	Children    []*Folder
}

type Store struct{ db *sql.DB }

func openDB(path string) (*sql.DB, error) {
	// _pragma args are how modernc.org/sqlite takes PRAGMAs. WAL keeps the UI
	// responsive while a photo upload is committing.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc's driver is safe for concurrent use, but a single writer avoids
	// SQLITE_BUSY churn entirely on a box this small.
	//
	// One connection has a sharp edge worth knowing about: while a transaction
	// is open it holds that connection, so any query issued on the pool rather
	// than on the transaction waits for a connection that cannot be released
	// until the transaction ends. Everything a transaction needs must therefore
	// be read before it begins.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return db, nil
}

const itemCols = `i.id, i.name, i.category, i.quantity, i.location, i.part_number,
	i.manufacturer, i.value, i.link, i.notes, i.folder_id, i.low_stock, i.footprint,
	i.created_at, i.updated_at, COALESCE(f.name, '')`

func scanItem(s interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var created, updated string
	err := s.Scan(&it.ID, &it.Name, &it.Category, &it.Quantity, &it.Location,
		&it.PartNumber, &it.Manufacturer, &it.Value, &it.Link, &it.Notes,
		&it.FolderID, &it.LowStock, &it.Footprint, &created, &updated, &it.FolderName)
	if err != nil {
		return it, err
	}
	it.CreatedAt, _ = time.Parse(time.RFC3339, created)
	it.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return it, nil
}

// Query describes the filter state of the item grid. Every field is optional.
type Query struct {
	Search       string
	Category     string
	Location     string
	Manufacturer string
	Tags         []string // an item must carry all of these
	Interfaces   []string // and speak all of these
	FolderID     *int64   // limit to one folder (with Recursive, its subtree too)
	Unfiled      bool     // only items in no folder
	Recursive    bool
	Sort         string // "recent" (default), "name", "qty", "low"
}

func (q Query) Any() bool {
	return q.Search != "" || q.Category != "" || q.Location != "" ||
		q.Manufacturer != "" || len(q.Tags) > 0 || len(q.Interfaces) > 0 ||
		q.FolderID != nil || q.Unfiled
}

// values renders the query as URL parameters. Folder scope is carried in the
// path rather than here, so it is deliberately omitted.
func (q Query) values() url.Values {
	v := url.Values{}
	if q.Search != "" {
		v.Set("q", q.Search)
	}
	if q.Category != "" {
		v.Set("category", q.Category)
	}
	if q.Location != "" {
		v.Set("location", q.Location)
	}
	if q.Manufacturer != "" {
		v.Set("manufacturer", q.Manufacturer)
	}
	if q.Sort != "" && q.Sort != "recent" {
		v.Set("sort", q.Sort)
	}
	if q.Unfiled {
		v.Set("unfiled", "1")
	}
	for _, t := range q.Tags {
		v.Add("tag", t)
	}
	for _, i := range q.Interfaces {
		v.Add("io", i)
	}
	return v
}

// Path is the page this query belongs on: a folder's own view when scoped to
// one, otherwise the all-items grid.
func (q Query) Path() string {
	if q.FolderID != nil {
		return fmt.Sprintf("/folders/%d", *q.FolderID)
	}
	return "/items"
}

func (q Query) String() string {
	if v := q.values(); len(v) > 0 {
		return q.Path() + "?" + v.Encode()
	}
	return q.Path()
}

// URL returns this query with one parameter replaced. Setting a parameter to
// the value it already has clears it instead, so chips toggle.
func (q Query) URL(key, value string) string {
	v := q.values()
	if v.Get(key) == value && key != "sort" {
		v.Del(key)
	} else if value == "" {
		v.Del(key)
	} else {
		v.Set(key, value)
	}
	if len(v) == 0 {
		return q.Path()
	}
	return q.Path() + "?" + v.Encode()
}

// WithTagToggled adds a tag to the filter, or drops it if already applied.
func (q Query) WithTagToggled(tag string) string {
	return q.toggleMulti("tag", q.Tags, tag)
}

// WithInterfaceToggled does the same for the IO filter row.
func (q Query) WithInterfaceToggled(name string) string {
	return q.toggleMulti("io", q.Interfaces, name)
}

// toggleMulti rebuilds a repeated query parameter with one value flipped.
func (q Query) toggleMulti(key string, current []string, value string) string {
	v := q.values()
	v.Del(key)
	found := false
	for _, existing := range current {
		if strings.EqualFold(existing, value) {
			found = true
			continue
		}
		v.Add(key, existing)
	}
	if !found {
		v.Add(key, value)
	}
	if len(v) == 0 {
		return q.Path()
	}
	return q.Path() + "?" + v.Encode()
}

func (s *Store) ListItems(q Query) ([]Item, error) {
	var where []string
	var args []any

	if term := strings.TrimSpace(q.Search); term != "" {
		// Every word must match somewhere in the item, so typing
		// "esp32 drawer" finds ESP32s stored in a drawer.
		for _, word := range strings.Fields(term) {
			// ESCAPE '\' is required for escapeLike's backslashes to be read as
			// escapes; without it a search for "%" matches every row.
			const field = ` LIKE ? ESCAPE '\' OR `
			where = append(where, `(i.name`+field+`i.category`+field+`i.location`+field+
				`i.part_number`+field+`i.value`+field+`i.notes LIKE ? ESCAPE '\'
				OR EXISTS (SELECT 1 FROM item_tags it JOIN tags t ON t.id = it.tag_id
					WHERE it.item_id = i.id AND t.name LIKE ? ESCAPE '\'))`)
			pattern := "%" + escapeLike(word) + "%"
			for n := 0; n < 7; n++ {
				args = append(args, pattern)
			}
		}
	}
	if q.Category != "" {
		where = append(where, "i.category = ?")
		args = append(args, q.Category)
	}
	if q.Location != "" {
		where = append(where, "i.location = ?")
		args = append(args, q.Location)
	}
	if q.Manufacturer != "" {
		where = append(where, "i.manufacturer = ? COLLATE NOCASE")
		args = append(args, q.Manufacturer)
	}
	// Each tag gets its own EXISTS so multiple tags narrow rather than widen.
	for _, tag := range q.Tags {
		where = append(where, `EXISTS (SELECT 1 FROM item_tags it JOIN tags t ON t.id = it.tag_id
			WHERE it.item_id = i.id AND t.name = ?)`)
		args = append(args, tag)
	}
	// Each interface gets its own EXISTS so several of them narrow the list.
	for _, io := range q.Interfaces {
		where = append(where, `EXISTS (SELECT 1 FROM item_interfaces ii
			WHERE ii.item_id = i.id AND ii.name = ?)`)
		args = append(args, io)
	}
	switch {
	case q.Unfiled:
		where = append(where, "i.folder_id IS NULL")
	case q.FolderID != nil && q.Recursive:
		where = append(where, `i.folder_id IN (
			WITH RECURSIVE subtree(id) AS (
				SELECT ? UNION ALL
				SELECT f.id FROM folders f JOIN subtree s ON f.parent_id = s.id
			) SELECT id FROM subtree)`)
		args = append(args, *q.FolderID)
	case q.FolderID != nil:
		where = append(where, "i.folder_id = ?")
		args = append(args, *q.FolderID)
	}

	order := "i.updated_at DESC"
	switch q.Sort {
	case "name":
		order = "i.name COLLATE NOCASE ASC"
	case "qty":
		order = "i.quantity DESC, i.name COLLATE NOCASE ASC"
	case "low":
		order = "i.quantity ASC, i.name COLLATE NOCASE ASC"
	}

	clause := "1=1"
	if len(where) > 0 {
		clause = strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items i
		LEFT JOIN folders f ON f.id = i.folder_id
		WHERE `+clause+` ORDER BY `+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []Item
	byID := map[int64]int{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		byID[it.ID] = len(items)
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return items, nil
	}
	if err := s.attachPhotos(items, byID); err != nil {
		return nil, err
	}
	for _, attach := range []func([]Item, map[int64]int) error{
		s.attachTags, s.attachPrices, s.attachInterfaces, s.attachClaims,
	} {
		if err := attach(items, byID); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// --- loading related rows -----------------------------------------------------

// Each attach* helper fills one kind of related row into a page of items with a
// single query, rather than a query per item.
//
// They are scoped to the items actually being shown. That matters more than it
// looks: they used to read their whole table and discard what did not match,
// so opening one item read every photo, tag, price and specification in the
// database. On a shelf with a couple of thousand parts that is the difference
// between a page load and a pause.
const idChunk = 900 // comfortably under any SQLite parameter limit

// eachInChunks runs a query once per batch of ids, so an enormous grid cannot
// exceed the bound-parameter limit.
func (s *Store) eachInChunks(ids []int64, build func(placeholders string) string, scan func(*sql.Rows) error) error {
	for start := 0; start < len(ids); start += idChunk {
		batch := ids[start:min(start+idChunk, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.Query(build(placeholders(len(batch))), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			if err := scan(rows); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// itemIDs is the key set of the index every attach helper is handed.
func itemIDs(byID map[int64]int) []int64 {
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Store) attachPhotos(items []Item, byID map[int64]int) error {
	return s.eachInChunks(itemIDs(byID), func(in string) string {
		return `SELECT id, item_id, filename, position FROM photos
			WHERE item_id IN (` + in + `) ORDER BY item_id, position, id`
	}, func(rows *sql.Rows) error {
		var p Photo
		if err := rows.Scan(&p.ID, &p.ItemID, &p.Filename, &p.Position); err != nil {
			return err
		}
		items[byID[p.ItemID]].Photos = append(items[byID[p.ItemID]].Photos, p)
		return nil
	})
}

func (s *Store) attachTags(items []Item, byID map[int64]int) error {
	return s.eachInChunks(itemIDs(byID), func(in string) string {
		return `SELECT it.item_id, t.name, t.icon FROM item_tags it
			JOIN tags t ON t.id = it.tag_id
			WHERE it.item_id IN (` + in + `) ORDER BY t.name COLLATE NOCASE`
	}, func(rows *sql.Rows) error {
		var itemID int64
		var tag Tag
		if err := rows.Scan(&itemID, &tag.Name, &tag.Icon); err != nil {
			return err
		}
		items[byID[itemID]].Tags = append(items[byID[itemID]].Tags, tag)
		return nil
	})
}

func (s *Store) attachPrices(items []Item, byID map[int64]int) error {
	return s.eachInChunks(itemIDs(byID), func(in string) string {
		return `SELECT item_id, source, amount, currency, url, lead_days, updated_at
			FROM prices WHERE item_id IN (` + in + `) ORDER BY amount`
	}, func(rows *sql.Rows) error {
		var itemID int64
		var p Price
		var updated string
		if err := rows.Scan(&itemID, &p.Source, &p.Amount, &p.Currency, &p.URL, &p.LeadDays, &updated); err != nil {
			return err
		}
		p.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		items[byID[itemID]].Prices = append(items[byID[itemID]].Prices, p)
		return nil
	})
}

func (s *Store) GetItem(id int64) (Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items i
		LEFT JOIN folders f ON f.id = i.folder_id WHERE i.id = ?`, id)
	it, err := scanItem(row)
	if err != nil {
		return it, err
	}
	items := []Item{it}
	byID := map[int64]int{it.ID: 0}
	if err := s.attachPhotos(items, byID); err != nil {
		return it, err
	}
	// The detail page is the only place specs and references are shown, so
	// they are loaded here rather than on every grid render.
	for _, attach := range []func([]Item, map[int64]int) error{
		s.attachTags, s.attachPrices, s.attachInterfaces, s.attachClaims,
		s.attachSpecs, s.attachRefs, s.attachModels,
	} {
		if err := attach(items, byID); err != nil {
			return it, err
		}
	}
	return items[0], nil
}

func (s *Store) CreateItem(it Item) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.Exec(`INSERT INTO items
		(name, category, quantity, location, part_number, manufacturer, value, link,
		 notes, folder_id, low_stock, footprint, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.Name, it.Category, it.Quantity, it.Location, it.PartNumber, it.Manufacturer,
		it.Value, it.Link, it.Notes, it.FolderID, max(it.LowStock, 0), it.Footprint, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := setTagsTx(tx, id, it.Tags); err != nil {
		return 0, err
	}
	if err := setInterfacesTx(tx, id, it.Interfaces); err != nil {
		return 0, err
	}
	if err := setSpecsTx(tx, id, it.Specs); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) UpdateItem(it Item) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`UPDATE items SET
		name=?, category=?, quantity=?, location=?, part_number=?, manufacturer=?,
		value=?, link=?, notes=?, folder_id=?, low_stock=?, updated_at=?
		WHERE id=?`,
		it.Name, it.Category, it.Quantity, it.Location, it.PartNumber, it.Manufacturer,
		it.Value, it.Link, it.Notes, it.FolderID, max(it.LowStock, 0),
		time.Now().UTC().Format(time.RFC3339), it.ID)
	if err != nil {
		return err
	}
	if err := setTagsTx(tx, it.ID, it.Tags); err != nil {
		return err
	}
	if err := setInterfacesTx(tx, it.ID, it.Interfaces); err != nil {
		return err
	}
	if err := setSpecsTx(tx, it.ID, it.Specs); err != nil {
		return err
	}
	return tx.Commit()
}

// setTagsTx replaces an item's tags wholesale, then drops any tag row that no
// longer has items so the tag filter never lists dead tags.
func setTagsTx(tx *sql.Tx, itemID int64, tags []Tag) error {
	if _, err := tx.Exec(`DELETE FROM item_tags WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for _, t := range tags {
		if err := attachTagTx(tx, itemID, t.Name); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`DELETE FROM tags WHERE id NOT IN (SELECT tag_id FROM item_tags)`)
	return err
}

// AdjustQuantity applies a relative change and returns the clamped result, so
// the +/- buttons in the grid never need to read-modify-write from the client.
func (s *Store) AdjustQuantity(id int64, delta int) (int, error) {
	_, err := s.db.Exec(`UPDATE items SET quantity = MAX(0, quantity + ?), updated_at = ? WHERE id = ?`,
		delta, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return 0, err
	}
	var q int
	err = s.db.QueryRow(`SELECT quantity FROM items WHERE id = ?`, id).Scan(&q)
	return q, err
}

func (s *Store) MoveItem(id int64, folderID *int64) error {
	_, err := s.db.Exec(`UPDATE items SET folder_id = ?, updated_at = ? WHERE id = ?`,
		folderID, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// DeleteItem removes the row and returns the photo filenames that are now
// orphaned, so the caller can unlink them from disk.
func (s *Store) DeleteItem(id int64) ([]string, error) {
	files, err := s.photoFilenames(`SELECT filename FROM photos WHERE item_id = ?`, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM items WHERE id = ?`, id); err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`DELETE FROM tags WHERE id NOT IN (SELECT tag_id FROM item_tags)`)
	return files, err
}

func (s *Store) photoFilenames(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Store) AddPhoto(itemID int64, filename string) error {
	var next int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(position)+1, 0) FROM photos WHERE item_id = ?`, itemID).Scan(&next)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO photos (item_id, filename, position) VALUES (?,?,?)`, itemID, filename, next)
	return err
}

func (s *Store) DeletePhoto(photoID int64) (itemID int64, filename string, err error) {
	err = s.db.QueryRow(`SELECT item_id, filename FROM photos WHERE id = ?`, photoID).Scan(&itemID, &filename)
	if err != nil {
		return 0, "", err
	}
	_, err = s.db.Exec(`DELETE FROM photos WHERE id = ?`, photoID)
	return itemID, filename, err
}

// SetCoverPhoto moves one photo to the front of an item's photo list.
func (s *Store) SetCoverPhoto(photoID int64) (int64, error) {
	var itemID int64
	if err := s.db.QueryRow(`SELECT item_id FROM photos WHERE id = ?`, photoID).Scan(&itemID); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(`UPDATE photos SET position = position + 1 WHERE item_id = ?`, itemID); err != nil {
		return itemID, err
	}
	_, err := s.db.Exec(`UPDATE photos SET position = 0 WHERE id = ?`, photoID)
	return itemID, err
}

// --- folders ----------------------------------------------------------------

func (s *Store) CreateFolder(name string, parentID *int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO folders (name, parent_id, created_at) VALUES (?,?,?)`,
		name, parentID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetFolder(id int64) (Folder, error) {
	var f Folder
	err := s.db.QueryRow(`SELECT id, name, parent_id FROM folders WHERE id = ?`, id).
		Scan(&f.ID, &f.Name, &f.ParentID)
	return f, err
}

// RenameFolder also handles reparenting. It refuses to move a folder inside its
// own subtree, which would orphan the whole branch from the dashboard.
func (s *Store) RenameFolder(id int64, name string, parentID *int64) error {
	if parentID != nil {
		if *parentID == id {
			return fmt.Errorf("a folder cannot contain itself")
		}
		inside, err := s.isDescendant(*parentID, id)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf("cannot move a folder into one of its own subfolders")
		}
	}
	_, err := s.db.Exec(`UPDATE folders SET name = ?, parent_id = ? WHERE id = ?`, name, parentID, id)
	return err
}

// isDescendant reports whether candidate sits anywhere under root.
func (s *Store) isDescendant(candidate, root int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`WITH RECURSIVE subtree(id) AS (
			SELECT ? UNION ALL
			SELECT f.id FROM folders f JOIN subtree s ON f.parent_id = s.id
		) SELECT COUNT(*) FROM subtree WHERE id = ?`, root, candidate).Scan(&n)
	return n > 0, err
}

// DeleteFolder removes a folder and its subfolders. Items are never deleted --
// they just become unfiled, which is why this needs no photo cleanup.
func (s *Store) DeleteFolder(id int64) error {
	_, err := s.db.Exec(`DELETE FROM folders WHERE id = ?`, id)
	return err
}

// Ancestors returns the path from the root down to and including id, for
// breadcrumbs.
func (s *Store) Ancestors(id int64) ([]Folder, error) {
	rows, err := s.db.Query(`WITH RECURSIVE up(id, name, parent_id, depth) AS (
			SELECT id, name, parent_id, 0 FROM folders WHERE id = ?
			UNION ALL
			SELECT f.id, f.name, f.parent_id, up.depth + 1
			FROM folders f JOIN up ON f.id = up.parent_id
		) SELECT id, name, parent_id FROM up ORDER BY depth DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Name, &f.ParentID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FolderTree loads every folder with its rolled-up counts and a cover photo
// borrowed from something inside it. Counts are rolled up in Go rather than in
// a recursive query per folder: one pass over a homelab's worth of folders is
// cheaper and much easier to follow.
func (s *Store) FolderTree() ([]*Folder, error) {
	all, err := s.flatFolders()
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Folder, len(all))
	for _, f := range all {
		byID[f.ID] = f
	}

	// Direct item and piece counts per folder.
	rows, err := s.db.Query(`SELECT folder_id, COUNT(*), COALESCE(SUM(quantity), 0)
		FROM items WHERE folder_id IS NOT NULL GROUP BY folder_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var items, pieces int
		if err := rows.Scan(&id, &items, &pieces); err != nil {
			rows.Close()
			return nil, err
		}
		if f, ok := byID[id]; ok {
			f.ItemCount, f.TotalItems, f.TotalPieces = items, items, pieces
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// One cover photo per folder, taken from that folder's own items.
	covers, err := s.db.Query(`SELECT i.folder_id, p.filename FROM photos p
		JOIN items i ON i.id = p.item_id
		WHERE i.folder_id IS NOT NULL
		ORDER BY i.folder_id, p.position, p.id`)
	if err != nil {
		return nil, err
	}
	for covers.Next() {
		var id int64
		var file string
		if err := covers.Scan(&id, &file); err != nil {
			covers.Close()
			return nil, err
		}
		if f, ok := byID[id]; ok && f.Cover == "" {
			f.Cover = file
		}
	}
	covers.Close()
	if err := covers.Err(); err != nil {
		return nil, err
	}

	// Link children to parents, dropping any whose parent vanished.
	var roots []*Folder
	for _, f := range all {
		if f.ParentID == nil {
			roots = append(roots, f)
			continue
		}
		if parent, ok := byID[*f.ParentID]; ok {
			parent.Children = append(parent.Children, f)
		} else {
			roots = append(roots, f)
		}
	}
	for _, r := range roots {
		rollUp(r, 0)
	}
	sortFolders(roots)
	return roots, nil
}

func (s *Store) flatFolders() ([]*Folder, error) {
	rows, err := s.db.Query(`SELECT id, name, parent_id FROM folders ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []*Folder
	for rows.Next() {
		f := &Folder{}
		if err := rows.Scan(&f.ID, &f.Name, &f.ParentID); err != nil {
			return nil, err
		}
		all = append(all, f)
	}
	return all, rows.Err()
}

// rollUp adds each folder's descendants into its totals and records its depth.
func rollUp(f *Folder, depth int) {
	f.Depth = depth
	for _, c := range f.Children {
		rollUp(c, depth+1)
		f.TotalItems += c.TotalItems
		f.TotalPieces += c.TotalPieces
		if f.Cover == "" {
			f.Cover = c.Cover
		}
	}
}

func sortFolders(fs []*Folder) {
	sort.SliceStable(fs, func(i, j int) bool {
		return strings.ToLower(fs[i].Name) < strings.ToLower(fs[j].Name)
	})
	for _, f := range fs {
		sortFolders(f.Children)
	}
}

// FlattenFolders turns the tree into a depth-ordered list, for <select> menus
// and the sidebar.
func FlattenFolders(roots []*Folder) []*Folder {
	var out []*Folder
	var walk func([]*Folder)
	walk = func(fs []*Folder) {
		for _, f := range fs {
			out = append(out, f)
			walk(f.Children)
		}
	}
	walk(roots)
	return out
}

// Indent renders a folder's nesting for plain-text contexts like <option>.
func (f *Folder) Indent() string { return strings.Repeat("— ", f.Depth) }

func (f Folder) HasParent(id int64) bool { return f.ParentID != nil && *f.ParentID == id }

// --- facets and stats -------------------------------------------------------

// Facet is one value of a filterable field plus how many items use it. These
// drive the type, location and tag chips, which is why none of them need to be
// configured up front -- they are just whatever you have typed so far.
type Facet struct {
	Value string
	Icon  string
	Count int
}

func (s *Store) Facets(column string) ([]Facet, error) {
	switch column {
	case "category", "location", "manufacturer": // guard: column is interpolated below
	default:
		return nil, fmt.Errorf("facet: unsupported column %q", column)
	}
	return s.facetQuery(`SELECT ` + column + `, COUNT(*) FROM items
		WHERE ` + column + ` <> '' GROUP BY ` + column + ` ORDER BY ` + column + ` COLLATE NOCASE`)
}

// TagFacets lists tags in use, most-used first, with their icons.
func (s *Store) TagFacets() ([]Facet, error) {
	rows, err := s.db.Query(`SELECT t.name, t.icon, COUNT(*) FROM tags t
		JOIN item_tags it ON it.tag_id = t.id
		GROUP BY t.id ORDER BY COUNT(*) DESC, t.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Facet
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Value, &f.Icon, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetTagIcon changes the emoji shown for a tag.
func (s *Store) SetTagIcon(name, icon string) error {
	_, err := s.db.Exec(`UPDATE tags SET icon = ? WHERE name = ?`, icon, name)
	return err
}

func (s *Store) facetQuery(query string, args ...any) ([]Facet, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Facet
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

type Stats struct {
	Items   int
	Pieces  int
	Photos  int
	Folders int
	Tags    int
	Unfiled int
	OutOf   int // items at zero quantity
	Priced  int
	Value   float64 // cheapest known price x quantity, summed
}

func (s Stats) ValueDisplay() string { return formatMoney(s.Value, "USD") }

func (s *Store) Stats() (Stats, error) {
	var st Stats
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(quantity),0),
		COALESCE(SUM(CASE WHEN quantity = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN folder_id IS NULL THEN 1 ELSE 0 END), 0)
		FROM items`).Scan(&st.Items, &st.Pieces, &st.OutOf, &st.Unfiled)
	if err != nil {
		return st, err
	}
	err = s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM photos),
		(SELECT COUNT(*) FROM folders),
		(SELECT COUNT(*) FROM tags)`).Scan(&st.Photos, &st.Folders, &st.Tags)
	if err != nil {
		return st, err
	}
	// Value the shelf at the cheapest price recorded for each item.
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cheapest * quantity), 0) FROM (
		SELECT i.quantity AS quantity, MIN(p.amount) AS cheapest
		FROM items i JOIN prices p ON p.item_id = i.id
		GROUP BY i.id)`).Scan(&st.Priced, &st.Value)
	return st, err
}

// --- prices -----------------------------------------------------------------

// SetPrice records or replaces what one source charges for an item.
func (s *Store) SetPrice(itemID int64, p Price) error {
	if p.Currency == "" {
		p.Currency = "USD"
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Only record history when the figure actually moved, so re-saving the
	// same price does not fill the chart with duplicates.
	var previous float64
	err = tx.QueryRow(`SELECT amount FROM prices WHERE item_id = ? AND source = ?`,
		itemID, p.Source).Scan(&previous)
	changed := err == sql.ErrNoRows || (err == nil && previous != p.Amount)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	_, err = tx.Exec(`INSERT INTO prices (item_id, source, amount, currency, url, lead_days, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(item_id, source) DO UPDATE SET
			amount = excluded.amount, currency = excluded.currency,
			url = excluded.url, lead_days = excluded.lead_days,
			updated_at = excluded.updated_at`,
		itemID, p.Source, p.Amount, p.Currency, p.URL, p.LeadDays, now)
	if err != nil {
		return err
	}
	if changed {
		_, err = tx.Exec(`INSERT INTO price_history (item_id, source, amount, currency, seen_at)
			VALUES (?,?,?,?,?)`, itemID, p.Source, p.Amount, p.Currency, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeletePrice(itemID int64, source string) error {
	_, err := s.db.Exec(`DELETE FROM prices WHERE item_id = ? AND source = ?`, itemID, source)
	return err
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
}
