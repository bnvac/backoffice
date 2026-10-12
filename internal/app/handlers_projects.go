package app

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

// --- projects ---------------------------------------------------------------

type projectsData struct {
	Projects []Project
	// Keyed by project id. A project with no steps is simply absent, and the
	// zero Progress reads as "nothing to show" rather than as nought percent.
	Progress map[int64]Progress
}

func (a *App) handleProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := a.store.ListProjects()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	progress, err := a.store.ProjectProgress()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.render(w, r, "projects.html", "Projects", projectsData{
		Projects: projects, Progress: progress,
	})
}

type projectData struct {
	Project     Project
	Suggestions []Item
	AllItems    []Item
	Pins        PinBudget
	Wiring      []PinAssignment
	PinIdeas    []PinAssignment
	PinClashes  []string
	Diagram     template.HTML
	Legend      []WireLegendEntry
	Log         []LogEntry
	Steps       []Step
	Progress    Progress
	Statuses    []string
	Assemblies  []Project // other projects that could go inside this one
}

func (a *App) handleProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetProject(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	suggestions, err := a.store.SuggestForProject(p, 8)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	all, err := a.store.ListItems(Query{Sort: "name"})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	entries, err := a.store.LogEntries(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	steps, err := a.store.ListSteps(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	wiring, err := a.store.PinAssignments(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	projects, err := a.store.ListProjects()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	// Anything but this project itself; the cycle guard catches the rest when
	// the line is actually added.
	assemblies := make([]Project, 0, len(projects))
	for _, other := range projects {
		if other.ID != id {
			assemblies = append(assemblies, other)
		}
	}

	a.render(w, r, "project.html", p.Name, projectData{
		Project:     p,
		Suggestions: suggestions,
		AllItems:    all,
		Pins:        BudgetPins(p),
		Wiring:      wiring,
		PinIdeas:    SuggestPins(p, wiring),
		PinClashes:  PinConflicts(wiring),
		// The SVG is built here rather than in the template because it is
		// drawing, not markup. It is marked safe because every string that goes
		// into it is escaped on the way in by WiringDiagram itself.
		Diagram:    template.HTML(WiringDiagram(controllerName(p), wiring)),
		Legend:     WireLegend(wiring),
		Log:        entries,
		Steps:      steps,
		Progress:   StepProgress(steps),
		Statuses:   ProjectStatuses,
		Assemblies: assemblies,
	})
}

// handleConsumeProject takes the project's parts off the shelf for real. It is
// a deliberate action rather than a side effect of changing the status, because
// it is the only thing in the app that changes stock without being asked to
// item by item.
func (a *App) handleConsumeProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)
	taken, short, err := a.store.ConsumeProject(id)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "built a project", "project", id,
		fmt.Sprintf("%s taken off the shelf", plural(taken, "piece")))

	msg := fmt.Sprintf("Marked built — %s came off the shelf", plural(taken, "piece"))
	var note string
	if short > 0 {
		note = fmt.Sprintf("%s were not in stock, so nothing was deducted for those", plural(short, "piece"))
	}
	redirect(w, r, dest, msg, note)
}

// handleReturnProject puts back exactly what the build took.
func (a *App) handleReturnProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)
	back, err := a.store.ReturnProject(id)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "unbuilt a project", "project", id,
		fmt.Sprintf("%s returned", plural(back, "piece")))
	redirect(w, r, dest, fmt.Sprintf("Put %s back on the shelf", plural(back, "piece")), "")
}

// --- build log --------------------------------------------------------------

func (a *App) handleAddLogEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)

	body := strings.TrimSpace(r.FormValue("body"))
	hasPhotos := r.MultipartForm != nil && len(r.MultipartForm.File["photos"]) > 0
	if body == "" && !hasPhotos {
		redirect(w, r, dest, "", "write something, or attach a photo")
		return
	}

	entryID, err := a.store.AddLogEntry(id, body, a.actor(r))
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	var failures []string
	if r.MultipartForm != nil {
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
			if err := a.store.AddLogPhoto(entryID, name); err != nil {
				a.photos.Remove(name)
				failures = append(failures, fh.Filename+": "+err.Error())
			}
		}
	}
	redirect(w, r, dest, "Added to the build log", strings.Join(failures, "; "))
}

func (a *App) handleDeleteLogEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	projectID, files, err := a.store.DeleteLogEntry(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, f := range files {
		a.photos.Remove(f)
	}
	redirect(w, r, fmt.Sprintf("/projects/%d", projectID), "Entry removed", "")
}

func (a *App) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirect(w, r, "/projects", "", "a project needs a name")
		return
	}
	id, err := a.store.CreateProject(name, strings.TrimSpace(r.FormValue("notes")))
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/projects/%d", id), "Created "+name, "")
}

func (a *App) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirect(w, r, dest, "", "a project needs a name")
		return
	}
	status := orDefault(r.FormValue("status"), "planning")
	controller := optionalID(r.FormValue("controller_id"))
	if err := a.store.UpdateProject(id, name, strings.TrimSpace(r.FormValue("notes")), status, controller); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "changed a project", "project", id, name+" · "+status)

	// Moving to "building" is the moment the parts stop being available to
	// anything else, so say so rather than leaving it to be discovered.
	if status == "building" {
		redirect(w, r, dest, "Saved — this project's parts are now reserved against your other projects", "")
		return
	}
	redirect(w, r, dest, "Saved", "")
}

func (a *App) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteProject(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/projects", "Project deleted", "")
}

// handleAddProjectPart adds a line to the bill of materials. The line either
// points at something owned, or names something not owned yet -- which is what
// makes the shortfall list useful.
func (a *App) handleAddProjectPart(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)

	qty, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("quantity")))
	itemID := optionalID(r.FormValue("item_id"))
	name := strings.TrimSpace(r.FormValue("name"))

	if itemID == nil && name == "" {
		redirect(w, r, dest, "", "pick a part you own, or type the name of one you need")
		return
	}
	err := a.store.AddProjectPart(id, itemID, name, qty, strings.TrimSpace(r.FormValue("note")))
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, dest, "Added to the parts list", "")
}

func (a *App) handleDeleteProjectPart(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	projectID, err := a.store.DeleteProjectPart(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/projects/%d", projectID), "Removed", "")
}

// --- references -------------------------------------------------------------

// handleAddReference attaches a datasheet, pinout or manual. A pinout given as
// a URL is downloaded so it is visible on the page, and survives the source
// going away.
func (a *App) handleAddReference(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/items/%d", id)

	kind := orDefault(r.FormValue("kind"), "reference")
	title := strings.TrimSpace(r.FormValue("title"))
	link := strings.TrimSpace(r.FormValue("url"))

	ref := Reference{ItemID: id, Kind: kind, Title: title, URL: link}
	var imageNote string

	// An uploaded file wins; otherwise try to store the link as an image, and
	// fall back to keeping it as a plain link when it is a PDF or a web page.
	if r.MultipartForm != nil && len(r.MultipartForm.File["file"]) > 0 {
		fh := r.MultipartForm.File["file"][0]
		f, err := fh.Open()
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		name, err := a.photos.Save(f, fh.Filename)
		f.Close()
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		ref.Filename = name
		if ref.Title == "" {
			ref.Title = fh.Filename
		}
	} else if link != "" && kind == "pinout" {
		// A pinout is only useful if you can look at it, so try to store the
		// image. When that fails the link is still worth keeping -- but say so,
		// rather than silently downgrading it to a bare link.
		body, err := a.fetcher.FetchImage(r.Context(), link)
		if err == nil {
			var name string
			if name, err = a.photos.Save(bytes.NewReader(body), lastPathSegment(link)); err == nil {
				ref.Filename = name
			}
		}
		if err != nil {
			imageNote = fmt.Sprintf("kept as a link — the image could not be fetched (%v)", err)
		}
	}

	if ref.Title == "" && ref.URL == "" && ref.Filename == "" {
		redirect(w, r, dest, "", "give the reference a link, a file, or at least a title")
		return
	}
	if ref.Title == "" {
		ref.Title = strings.Title(kind) //nolint:staticcheck // ASCII kind names only
	}

	if _, err := a.store.AddReference(ref); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, dest, "Reference added", imageNote)
}

func (a *App) handleDeleteReference(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	itemID, filename, err := a.store.DeleteReference(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	if filename != "" {
		a.photos.Remove(filename)
	}
	redirect(w, r, fmt.Sprintf("/items/%d", itemID), "Reference removed", "")
}

// --- price refresh ----------------------------------------------------------

// handleRefreshPrice re-checks what a source charges today. Only sources the
// app can actually query are refreshable; the rest stay manual, and say so.
func (a *App) handleRefreshPrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := a.store.GetItem(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	dest := fmt.Sprintf("/items/%d", id)

	// Search by part number when there is one; it is far more precise than
	// the product name.
	query := it.PartNumber
	if query == "" {
		query = it.Name
	}
	report := a.search.Search(r.Context(), query, 5)
	if len(report.Results) == 0 {
		msg := "no searchable source lists this part"
		if len(report.Notes) > 0 {
			msg = strings.Join(report.Notes, "; ")
		}
		redirect(w, r, dest, "", msg)
		return
	}

	best := report.Results[0]
	// A part-number match is only trustworthy if it really matches.
	if it.PartNumber != "" && !strings.EqualFold(best.PartNumber, it.PartNumber) {
		redirect(w, r, dest, "",
			fmt.Sprintf("closest match was %q, which is a different part — set the price by hand", best.Title))
		return
	}
	if best.Price <= 0 {
		redirect(w, r, dest, "", "that source did not publish a price")
		return
	}

	err = a.store.SetPrice(id, Price{
		Source: best.Source, Amount: best.Price,
		Currency: best.Currency, URL: best.URL,
		LeadDays: DefaultLeadDays(best.Source),
	})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, dest, fmt.Sprintf("%s is %s today", best.Source, formatMoney(best.Price, best.Currency)), "")
}

// handleEnrichFromOctopart fills an item in from Octopart: specifications,
// the datasheet, and one price per distributor with its stock and lead time.
//
// It is a separate action rather than part of saving, because it overwrites
// specs and adds prices -- worth doing on purpose, not as a side effect.
func (a *App) handleEnrichFromOctopart(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := a.store.GetItem(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	dest := fmt.Sprintf("/items/%d", id)

	nexar := a.search.Nexar()
	if nexar == nil {
		redirect(w, r, dest, "",
			"Octopart is not configured — set NEXAR_CLIENT_ID and NEXAR_CLIENT_SECRET")
		return
	}

	// A part number is the only thing worth looking up; a product name matches
	// the wrong silicon far too easily.
	mpn := strings.TrimSpace(it.PartNumber)
	if mpn == "" {
		redirect(w, r, dest, "", "add a manufacturer part number first — Octopart looks up parts, not product names")
		return
	}

	detail, err := nexar.Lookup(r.Context(), mpn)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}

	var added []string

	// Octopart knows who made the part, which is the field nobody ever fills in
	// by hand. It is only written when empty, so a name already chosen stands.
	newManufacturer := ""
	if detail.Manufacturer != "" && it.Manufacturer == "" {
		it.Manufacturer = a.store.SettleManufacturer(detail.Manufacturer)
		newManufacturer = it.Manufacturer
		added = append(added, "manufacturer")
	}
	if len(detail.Specs) > 0 {
		it.Specs = detail.Specs
		added = append(added, fmt.Sprintf("%d specs", len(detail.Specs)))
	}
	if newManufacturer != "" || len(detail.Specs) > 0 {
		if err := a.store.UpdateItem(it); err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
	}
	if newManufacturer != "" {
		a.fetchLogoFor(newManufacturer)
	}

	if detail.Datasheet != nil {
		if a.attachDiscoveredDocuments(r, id, []PageDocument{*detail.Datasheet}) > 0 {
			added = append(added, "datasheet")
		}
	}

	// One price per distributor, cheapest first, capped so a widely stocked
	// part does not bury the item page under twenty sellers.
	priced := 0
	seen := map[string]bool{}
	for _, o := range detail.Offers {
		if priced >= 5 {
			break
		}
		if o.Seller == "" || seen[o.Seller] {
			continue
		}
		seen[o.Seller] = true
		err := a.store.SetPrice(id, Price{
			Source: o.Seller, Amount: o.Price, Currency: o.Currency,
			URL: o.URL, LeadDays: o.LeadDays,
		})
		if err == nil {
			priced++
		}
	}
	if priced > 0 {
		added = append(added, fmt.Sprintf("%d distributor prices", priced))
	}

	if len(added) == 0 {
		redirect(w, r, dest, "", "Octopart knows that part but had nothing new to add")
		return
	}
	redirect(w, r, dest, "From Octopart: "+strings.Join(added, ", "), "")
}
