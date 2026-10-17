package app

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/bnvac/backoffice/internal/media"
)

// page carries everything the layout template needs on every render.
type page struct {
	Title    string
	Path     string // for marking the current section in the nav
	SiteName string
	AuthOn   bool
	User     *User // nil when signed in with the shared password, or with none
	Flash    string
	Error    string
	Bare     bool   // hides the nav, for the sign-in page
	Neb      bool   // the assistant is on, so the chat button is drawn
	NebOff   string // why it is not, when Neb is running but cannot be offered
	Data     any

	// For the account menu in the top right.
	WhoAmI         string // the name to show, however you got here
	Others         []User // the other accounts, to switch to
	CanManageUsers bool
}

// whoAmI is the name on the account button. There are three ways to be using
// this app -- as a named account, behind a shared password, or with no sign-in
// at all -- and the button has to say something sensible in all three. The
// profile name is the fallback because on a single-person install that is the
// only name anybody has given.
func (a *App) whoAmI(r *http.Request) string {
	if u := CurrentUser(r); u != nil {
		return u.Username
	}
	if p, err := a.store.GetProfile(); err == nil && p.Name != "" {
		return p.Name
	}
	return "You"
}

// otherAccounts lists who else could be signed in as. It is empty on an
// install with no accounts, which is the common case and correctly shows no
// switcher at all.
func (a *App) otherAccounts(me *User) []User {
	users, err := a.store.ListUsers()
	if err != nil {
		return nil
	}
	var out []User
	for _, u := range users {
		if me != nil && u.ID == me.ID {
			continue
		}
		out = append(out, u)
	}
	return out
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, title string, data any) {
	me := CurrentUser(r)
	p := page{
		Title:    title,
		Path:     r.URL.Path,
		SiteName: a.cfg.Title,
		AuthOn:   a.auth.Enabled(),
		User:     me,
		Flash:    r.URL.Query().Get("flash"),
		Error:    r.URL.Query().Get("error"),
		// Sign-in and the setup wizard both stand alone: no section nav, so
		// the one thing on the page is the thing to do.
		Bare: name == "login.html" || name == "welcome.html",
		Data: data,
		Neb:  a.assistantOn(),

		WhoAmI:        a.whoAmI(r),
		Others:         a.otherAccounts(me),
		CanManageUsers: me == nil || me.Admin,
	}
	// Neb running but not offered is worth explaining where a model is chosen,
	// and nowhere else.
	if name == "welcome.html" && !p.Neb && a.neb != nil && a.neb.Up() && !a.auth.Enabled() {
		p.NebOff = "Neb is running on this machine, but the assistant stays off until this install " +
			"asks people to sign in — Neb can run commands, and with no password anyone on the " +
			"network could reach it. Set AUTH_PASSWORD, or add an account under Health & backup."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (a *App) fail(w http.ResponseWriter, err error, status int) {
	log.Printf("error: %v", err)
	http.Error(w, err.Error(), status)
}

// redirect sends the browser somewhere with an optional flash message. Using
// the URL rather than a session avoids any server-side flash storage.
func redirect(w http.ResponseWriter, r *http.Request, path, flash, errMsg string) {
	u, _ := url.Parse(path)
	q := u.Query()
	if flash != "" {
		q.Set("flash", flash)
	}
	if errMsg != "" {
		q.Set("error", errMsg)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// --- grid -------------------------------------------------------------------

type indexData struct {
	Items      []Item
	Categories []Facet
	Locations  []Facet
	Makers     []Facet
	Tags       []Facet
	IO         []Facet
	Folders    []*Folder
	Stats      Stats
	Query      Query
	Filtered   bool
}

// queryFromRequest reads the shared filter state used by the grid and by each
// folder view.
func (a *App) queryFromRequest(r *http.Request) Query {
	v := r.URL.Query()
	q := Query{
		Search:       strings.TrimSpace(v.Get("q")),
		Category:     v.Get("category"),
		Location:     v.Get("location"),
		Manufacturer: v.Get("manufacturer"),
		Sort:         v.Get("sort"),
		Unfiled:      v.Get("unfiled") == "1",
	}
	for _, t := range v["tag"] {
		if t = strings.TrimSpace(t); t != "" {
			q.Tags = append(q.Tags, t)
		}
	}
	for _, io := range v["io"] {
		if io = strings.TrimSpace(io); io != "" {
			q.Interfaces = append(q.Interfaces, io)
		}
	}
	return q
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := a.queryFromRequest(r)
	items, err := a.store.ListItems(q)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	cats, err := a.store.Facets("category")
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	locs, err := a.store.Facets("location")
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	stats, err := a.store.Stats()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	tags, err := a.store.TagFacets()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tree, err := a.store.FolderTree()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	io, err := a.store.InterfaceFacets()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	a.render(w, r, "index.html", "All items", indexData{
		Items:      items,
		Categories: cats,
		Locations:  locs,
		Makers:     a.facetsOrNil("manufacturer"),
		Tags:       tags,
		IO:         io,
		Folders:    FlattenFolders(tree),
		Stats:      stats,
		Query:      q,
		Filtered:   q.Any(),
	})
}

// --- single item ------------------------------------------------------------

type itemData struct {
	Item
	Octopart bool // credentials present, so the enrich action is worth offering
	Folders  []*Folder
	History  []PricePoint
	Changes  []PriceChange
	RefKinds []string
	Land     *media.Footprint // the drawing, when this part has a footprint recorded
	LandErr  string
	Maker    Manufacturer
	Incoming int // on its way from an order that has not arrived

	// What is flashed on these and where they are running. Read only if the
	// page will show them; a resistor has neither.
	Flashes  []Flash
	Machines []Machine
	Prints   []PrintJob
	AllItems []Item
	Projects []Project
	FwLimit  string
}

func (a *App) handleItem(w http.ResponseWriter, r *http.Request) {
	it, ok := a.lookup(w, r)
	if !ok {
		return
	}
	history, err := a.store.PriceHistory(it.ID)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	data := itemData{
		Item:     it,
		Octopart: a.search.Nexar() != nil,
		Folders:  a.folderListOrNil(),
		History:  history,
		Changes:  PriceChanges(history),
		RefKinds: RefKinds,
	}
	if incoming, err := a.store.Incoming(); err == nil {
		data.Incoming = incoming[it.ID]
	}
	if it.Manufacturer != "" {
		data.Maker, _ = a.store.GetManufacturer(it.Manufacturer)
	}
	// None of these are worth failing the page over: each is a side panel.
	data.Flashes, _ = a.store.ListFlashes(it.ID, 0)
	data.Machines, _ = a.store.MachinesForItem(it.ID)
	data.Prints, _ = a.store.RecentPrintJobs(it.ID, 6)
	if a.firmware != nil {
		data.FwLimit = a.firmware.Limit()
	}
	// The firmware form wants the lists, but only boards get it, so the
	// whole-shelf query is skipped for parts that cannot run code.
	if ProgrammableItem(it) || len(data.Flashes) > 0 {
		data.AllItems, _ = a.store.ListItems(Query{Sort: "name"})
		data.Projects, _ = a.store.ListProjects()
	}
	// The footprint is drawn from the cached file, so an item page never waits
	// on the network: only the first lookup fetches, and that happens when the
	// footprint is set rather than when the page is opened.
	if it.Footprint != "" {
		if body, source, err := a.store.CachedFootprint(it.Footprint); err == nil {
			if fp, err := media.ParseFootprint(body); err == nil {
				fp.Source = source
				data.Land = fp
			} else {
				data.LandErr = err.Error()
			}
		} else {
			data.LandErr = "not fetched yet — save the footprint again to fetch it"
		}
	}
	a.render(w, r, "item.html", it.Name, data)
}

func (a *App) handleNewForm(w http.ResponseWriter, r *http.Request) {
	// Prefill from the view the person came from, so adding an item while
	// looking at a folder files it there by default.
	item := Item{
		Quantity: 1,
		Name:     strings.TrimSpace(r.URL.Query().Get("name")),
		Location: r.URL.Query().Get("location"),
		Category: r.URL.Query().Get("category"),
		FolderID: optionalID(r.URL.Query().Get("folder")),
	}
	if tag := strings.TrimSpace(r.URL.Query().Get("tag")); tag != "" {
		item.Tags = []Tag{{Name: tag, Icon: IconForTag(tag)}}
	}
	a.render(w, r, "edit.html", "New item", editData{
		Item:       item,
		Categories: a.facetsOrNil("category"),
		Locations:  a.facetsOrNil("location"),
		Makers:     a.facetsOrNil("manufacturer"),
		Tags:       a.tagFacetsOrNil(),
		Folders:    a.folderListOrNil(),
		IOGroups:   InterfaceGroups(),
		IsNew:      true,
	})
}

func (a *App) facetsOrNil(column string) []Facet {
	f, err := a.store.Facets(column)
	if err != nil {
		log.Printf("facets %s: %v", column, err)
	}
	return f
}

func (a *App) tagFacetsOrNil() []Facet {
	f, err := a.store.TagFacets()
	if err != nil {
		log.Printf("tag facets: %v", err)
	}
	return f
}

func (a *App) folderListOrNil() []*Folder {
	tree, err := a.store.FolderTree()
	if err != nil {
		log.Printf("folder tree: %v", err)
		return nil
	}
	return FlattenFolders(tree)
}

type editData struct {
	Item       Item
	Categories []Facet
	Locations  []Facet
	Makers     []Facet
	Tags       []Facet
	Folders    []*Folder
	IOGroups   []InterfaceGroup
	IsNew      bool
}

// HasInterface drives the checkbox state on the edit form.
func (d editData) HasInterface(name string) bool {
	for _, i := range d.Item.Interfaces {
		if i == name {
			return true
		}
	}
	return false
}

func (a *App) handleEditForm(w http.ResponseWriter, r *http.Request) {
	it, ok := a.lookup(w, r)
	if !ok {
		return
	}
	a.render(w, r, "edit.html", "Edit "+it.Name, editData{
		Item:       it,
		Categories: a.facetsOrNil("category"),
		Locations:  a.facetsOrNil("location"),
		Makers:     a.facetsOrNil("manufacturer"),
		Tags:       a.tagFacetsOrNil(),
		Folders:    a.folderListOrNil(),
		IOGroups:   InterfaceGroups(),
	})
}

// itemFromForm reads the shared add/edit form. Quantity falls back to 0 rather
// than erroring, so a blank field never blocks a save.
func itemFromForm(r *http.Request) (Item, error) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return Item{}, errors.New("name is required")
	}
	qty, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("quantity")))
	if qty < 0 {
		qty = 0
	}
	// A form that does not carry the field at all -- the quick-add box, an
	// import, a script -- keeps the default rather than silently setting the
	// reorder line to zero.
	low := defaultLowStock
	if raw, ok := r.Form["low_stock"]; ok && strings.TrimSpace(raw[0]) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(raw[0])); err == nil && n >= 0 {
			low = n
		}
	}
	// The tag field is a comma-separated text input; the picker below it posts
	// additional values under the same name.
	var tags []Tag
	for _, name := range splitTags(strings.Join(append(r.Form["tags"], r.Form["tag"]...), ",")) {
		tags = append(tags, Tag{Name: name, Icon: IconForTag(name)})
	}

	return Item{
		Name:       name,
		Category:   strings.TrimSpace(r.FormValue("category")),
		Quantity:   qty,
		Location:   strings.TrimSpace(r.FormValue("location")),
		PartNumber: strings.TrimSpace(r.FormValue("part_number")),
		// Canonicalised on the way in, so "TI" and "Texas Instruments" do not
		// become two makers with two logos and two filter chips.
		Manufacturer: CanonicalManufacturer(r.FormValue("manufacturer")),
		Value:        strings.TrimSpace(r.FormValue("value")),
		Tags:         tags,
		Link:         strings.TrimSpace(r.FormValue("link")),
		Notes:        strings.TrimSpace(r.FormValue("notes")),
		FolderID:     optionalID(r.FormValue("folder_id")),
		LowStock:     low,
		Interfaces:   r.Form["interface"],
		Specs:        ParseSpecs(r.FormValue("specs")),
	}, nil
}

// defaultLowStock is the reorder line a new item starts with, and the figure
// the dashboard used before thresholds were per-item.
const defaultLowStock = 2

func (a *App) handleCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	it, err := itemFromForm(r)
	if err != nil {
		redirect(w, r, "/items/new", "", err.Error())
		return
	}
	it.Manufacturer = a.store.SettleManufacturer(it.Manufacturer)
	id, err := a.store.CreateItem(it)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	msg := a.savePhotos(r, id)
	if urlMsg := a.savePhotoFromURL(r.Context(), id, r.FormValue("image_url")); urlMsg != "" {
		msg = strings.TrimPrefix(msg+"; "+urlMsg, "; ")
	}
	a.savePriceFromForm(r, id)
	a.fetchLogoFor(it.Manufacturer)
	a.store.Record(a.actor(r), "added an item", "item", id, it.Name)
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Added "+it.Name, msg)
}

func (a *App) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	it, err := itemFromForm(r)
	if err != nil {
		redirect(w, r, fmt.Sprintf("/items/%d/edit", id), "", err.Error())
		return
	}
	it.ID = id
	it.Manufacturer = a.store.SettleManufacturer(it.Manufacturer)
	if err := a.store.UpdateItem(it); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	msg := a.savePhotos(r, id)
	if urlMsg := a.savePhotoFromURL(r.Context(), id, r.FormValue("image_url")); urlMsg != "" {
		msg = strings.TrimPrefix(msg+"; "+urlMsg, "; ")
	}
	a.savePriceFromForm(r, id)
	a.fetchLogoFor(it.Manufacturer)
	a.store.Record(a.actor(r), "edited an item", "item", id, it.Name)
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Saved", msg)
}

// savePriceFromForm records the optional price on the add/edit form. A price
// with no source still counts -- it is filed against the link's host, or
// "Unknown" -- because losing a figure someone typed is worse than guessing.
func (a *App) savePriceFromForm(r *http.Request, itemID int64) {
	amount, err := parsePrice(r.FormValue("price"))
	if err != nil || amount <= 0 {
		return
	}
	source := strings.TrimSpace(r.FormValue("price_source"))
	if source == "" {
		source = sourceFromURL(r.FormValue("link"))
	}
	if err := a.store.SetPrice(itemID, Price{
		Source:   source,
		Amount:   amount,
		Currency: strings.ToUpper(orDefault(r.FormValue("currency"), "USD")),
		URL:      strings.TrimSpace(r.FormValue("link")),
		LeadDays: DefaultLeadDays(source),
	}); err != nil {
		log.Printf("save price: %v", err)
	}
}

// sourceFromURL turns a product link into a shop name, so a price imported
// from a page is filed under something recognisable.
func sourceFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "Unknown"
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	if host == "" {
		return "Unknown"
	}
	return strings.ToUpper(host[:1]) + host[1:]
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// Read the name before the row goes, so the audit line says what was
	// deleted rather than just an id that no longer resolves.
	name := ""
	if it, err := a.store.GetItem(id); err == nil {
		name = it.Name
	}
	files, err := a.store.DeleteItem(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "deleted an item", "item", id, name)
	for _, f := range files {
		a.photos.Remove(f)
	}
	redirect(w, r, "/", "Item deleted", "")
}

// handleQuantity backs the +/- buttons. It answers JSON for the inline path and
// falls back to a redirect when JavaScript is off.
func (a *App) handleQuantity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	delta, err := strconv.Atoi(r.FormValue("delta"))
	if err != nil {
		a.fail(w, fmt.Errorf("bad delta: %w", err), http.StatusBadRequest)
		return
	}
	qty, err := a.store.AdjustQuantity(id, delta)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "changed a count", "item", id, fmt.Sprintf("%+d, now %d", delta, qty))
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"quantity": qty})
		return
	}
	back := r.FormValue("return")
	if back == "" {
		back = fmt.Sprintf("/items/%d", id)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// --- photos -----------------------------------------------------------------

func (a *App) handleUploadPhotos(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	msg := a.savePhotos(r, id)
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Photos updated", msg)
}

// savePhotos stores every uploaded file against an item and returns a
// human-readable summary of anything that failed (empty when all went well).
func (a *App) savePhotos(r *http.Request, itemID int64) string {
	if r.MultipartForm == nil {
		return ""
	}
	var failures []string
	for _, fh := range r.MultipartForm.File["photos"] {
		if fh.Size == 0 {
			continue
		}
		f, err := fh.Open()
		if err != nil {
			failures = append(failures, fh.Filename+": "+err.Error())
			continue
		}
		name, err := a.photos.Save(f, fh.Filename)
		f.Close()
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if err := a.store.AddPhoto(itemID, name); err != nil {
			a.photos.Remove(name)
			failures = append(failures, fh.Filename+": "+err.Error())
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return strings.Join(failures, "; ")
}

func (a *App) handleDeletePhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	itemID, filename, err := a.store.DeletePhoto(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.photos.Remove(filename)
	redirect(w, r, fmt.Sprintf("/items/%d", itemID), "Photo removed", "")
}

func (a *App) handleCoverPhoto(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	itemID, err := a.store.SetCoverPhoto(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/items/%d", itemID), "Cover photo set", "")
}

func (a *App) handleMedia(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !media.SafeName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	// Filenames are random and never reused, so they can cache forever.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, a.photos.Path(name))
}

func (a *App) handleThumb(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !media.SafeName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")

	// Some valid images cannot be decoded by the standard library, so no
	// thumbnail exists for them. Serving the original keeps the grid intact --
	// browsers decode what Go could not.
	path := a.photos.ThumbPath(name)
	if _, err := os.Stat(path); err != nil {
		path = a.photos.Path(name)
	}
	http.ServeFile(w, r, path)
}

// --- export -----------------------------------------------------------------

func (a *App) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.ListItems(Query{Sort: "name"})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="inventory.csv"`)

	cw := csv.NewWriter(w)
	defer cw.Flush()
	cw.Write([]string{"id", "name", "category", "quantity", "location", "part_number",
		"manufacturer", "value", "tags", "link", "notes", "folder", "photos", "updated_at"})
	for _, it := range items {
		cw.Write([]string{
			strconv.FormatInt(it.ID, 10), it.Name, it.Category, strconv.Itoa(it.Quantity),
			it.Location, it.PartNumber, it.Manufacturer, it.Value, it.TagString(),
			it.Link, it.Notes, it.FolderName,
			strconv.Itoa(len(it.Photos)), it.UpdatedAt.Format("2006-01-02 15:04"),
		})
	}
}

// --- auth pages -------------------------------------------------------------

func (a *App) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if !a.auth.Enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, "login.html", "Sign in", map[string]any{
		"Next":     safeNext(r.URL.Query().Get("next")),
		"Accounts": a.auth.Accounts(),
		// Switching accounts from the header lands here with the name already
		// filled in; the password is still required, because a switcher that
		// skipped it would be a hole rather than a convenience.
		"As":     strings.TrimSpace(r.URL.Query().Get("as")),
		"Shared": a.auth.password != "",
	})
}

// handleLogin accepts either a named account or, when one is still configured,
// the shared password on its own. A username is tried first so that an account
// whose password happens to equal AUTH_PASSWORD still signs in as itself.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.auth.Enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if username != "" {
		u, ok := a.store.Authenticate(username, password)
		if !ok {
			redirect(w, r, "/login", "", "Wrong username or password")
			return
		}
		a.store.TouchUser(u.ID)
		a.auth.issue(w, r, u.ID)
		a.store.Record(u.Username, "signed in", "user", u.ID, "")
		http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
		return
	}
	if !a.auth.Check(password) {
		msg := "Wrong password"
		if a.auth.Accounts() {
			msg = "Wrong username or password"
		}
		redirect(w, r, "/login", "", msg)
		return
	}
	a.auth.issue(w, r, 0)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	a.auth.clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext keeps the post-login redirect on this site: only same-origin,
// absolute paths are allowed through.
func safeNext(next string) string {
	// Browsers read "/\evil.com" the same as "//evil.com", so a backslash in
	// second place is refused along with the double slash.
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// --- helpers ----------------------------------------------------------------

// parseForm reads a submission whether or not it carries file uploads, so a
// plain urlencoded POST (curl, a script, a form with no photo picker) is still
// validated normally instead of failing at the parser.
func parseForm(r *http.Request) error {
	err := r.ParseMultipartForm(8 << 20)
	if err == nil || errors.Is(err, http.ErrNotMultipart) {
		return nil
	}
	return err
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func (a *App) lookup(w http.ResponseWriter, r *http.Request) (Item, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return Item{}, false
	}
	it, err := a.store.GetItem(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return Item{}, false
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return Item{}, false
	}
	return it, true
}
