package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// --- dashboard --------------------------------------------------------------

type dashboardData struct {
	Projects []Project
	Folders  []*Folder
	Flat     []*Folder
	Stats    Stats
	Recent   []Item
	LowStock []Item
	Tags     []Facet
	Unfiled  int
	Setup    bool // first boot: offer the wizard rather than an empty page
	Profile  Profile
	Ideas    []Idea
	// Only the printers that can be asked. Their status is fetched by script
	// after the page has rendered, so a printer that is switched off never
	// holds up the dashboard.
	Printers     []Printer
	Progress     map[int64]Progress
	Filament     FilamentSummary
	Due          map[int64]int // maintenance due per printer
	Sensors      []Sensor
	SensorAlerts []string
}

// handleDashboard is the landing page: folders first, then what changed and
// what is running out.
func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	tree, err := a.store.FolderTree()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	stats, err := a.store.Stats()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	recent, err := a.store.ListItems(Query{Sort: "recent"})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	low, err := a.store.LowStock(8)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tags, err := a.store.TagFacets()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	projects, err := a.store.ListProjects()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	profile, _ := a.store.GetProfile()
	// Ideas that are worth doing something about, which is the ones you kept.
	ideas, _ := a.store.ListIdeas("keeping")
	var linked []Printer
	sensors, _ := a.store.ListSensors()
	var alerts []string
	for _, s := range sensors {
		alerts = append(alerts, s.Alerts()...)
	}
	due := map[int64]int{}
	if printers, err := a.store.ListPrinters(); err == nil {
		for _, p := range printers {
			if p.Connected() {
				linked = append(linked, p)
			}
		}
		due = a.store.MaintDue(printers)
	}
	spools, _ := a.store.ListSpools(false)

	progress, _ := a.store.ProjectProgress()

	a.render(w, r, "dashboard.html", "", dashboardData{
		Printers:     linked,
		Progress:     progress,
		Filament:     SummariseFilament(spools),
		Due:          due,
		Sensors:      sensors,
		SensorAlerts: alerts,
		Setup:        !profile.Onboarded(),
		Profile:      profile,
		Ideas:        firstN(ideas, 3),
		Projects:     firstN(projects, 4),
		Folders:      tree,
		Flat:         FlattenFolders(tree),
		Stats:        stats,
		Recent:       firstN(recent, 12),
		LowStock:     low,
		Tags:         firstN(tags, 24),
		Unfiled:      stats.Unfiled,
	})
}

// firstN caps a slice for the dashboard panels.
func firstN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// lowStock keeps only the genuinely low items -- an inventory where nothing is
// running out should show an empty panel, not its fullest shelves.
// --- folder view ------------------------------------------------------------

type folderData struct {
	Folder      Folder
	Breadcrumb  []Folder
	Children    []*Folder
	AllFolders  []*Folder
	Items       []Item
	Categories  []Facet
	Locations   []Facet
	Tags        []Facet
	IO          []Facet
	Stats       Stats
	Query       Query
	Recursive   bool
	TotalItems  int
	TotalPieces int
}

func (a *App) handleFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	folder, err := a.store.GetFolder(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	q := a.queryFromRequest(r)
	q.FolderID = &id
	q.Recursive = r.URL.Query().Get("all") == "1"

	items, err := a.store.ListItems(q)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	breadcrumb, err := a.store.Ancestors(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tree, err := a.store.FolderTree()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	// Pull this folder's node out of the tree for its rolled-up counts.
	var node *Folder
	for _, f := range FlattenFolders(tree) {
		if f.ID == id {
			node = f
			break
		}
	}
	data := folderData{
		Folder:     folder,
		Breadcrumb: breadcrumb,
		AllFolders: FlattenFolders(tree),
		Items:      items,
		Query:      q,
		Recursive:  q.Recursive,
	}
	if node != nil {
		data.Children = node.Children
		data.TotalItems = node.TotalItems
		data.TotalPieces = node.TotalPieces
	}
	data.Categories, _ = a.store.Facets("category")
	data.Locations, _ = a.store.Facets("location")
	data.Tags, _ = a.store.TagFacets()
	data.IO, _ = a.store.InterfaceFacets()

	a.render(w, r, "folder.html", folder.Name, data)
}

func (a *App) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	back := safeNext(r.FormValue("return"))
	if name == "" {
		redirect(w, r, back, "", "a folder needs a name")
		return
	}
	parent := optionalID(r.FormValue("parent_id"))
	id, err := a.store.CreateFolder(name, parent)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/folders/%d", id), "Created "+name, "")
}

func (a *App) handleRenameFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/folders/%d", id)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirect(w, r, dest, "", "a folder needs a name")
		return
	}
	if err := a.store.RenameFolder(id, name, optionalID(r.FormValue("parent_id"))); err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	redirect(w, r, dest, "Folder updated", "")
}

func (a *App) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	folder, err := a.store.GetFolder(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	if err := a.store.DeleteFolder(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	dest := "/"
	if folder.ParentID != nil {
		dest = fmt.Sprintf("/folders/%d", *folder.ParentID)
	}
	redirect(w, r, dest, "Deleted folder "+folder.Name+"; its items are now unfiled", "")
}

// handleMoveItem is the one-click "file this somewhere" action from an item page.
func (a *App) handleMoveItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	if err := a.store.MoveItem(id, optionalID(r.FormValue("folder_id"))); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Moved", "")
}

// optionalID reads a form field that may be blank, meaning "no folder".
func optionalID(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}
