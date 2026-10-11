package app

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bnvac/backoffice/internal/media"
)

type modelSearchData struct {
	Item      Item
	Query     string
	Results   []ModelResult
	Notes     []string
	Suggested []string
	Elsewhere []ExternalModelSource
	Sources   []string
}

// handleModelSearch looks for a case, bracket or mount somebody has already
// drawn for this part. It is a GET so a search can be shared and reopened, and
// so the back button behaves.
func (a *App) handleModelSearch(w http.ResponseWriter, r *http.Request) {
	it, ok := a.lookup(w, r)
	if !ok {
		return
	}
	suggested := ModelSearchTerms(it)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" && len(suggested) > 0 {
		query = suggested[0]
	}

	data := modelSearchData{
		Item:      it,
		Query:     query,
		Suggested: suggested,
		Elsewhere: ExternalModelSources(query),
		Sources:   a.models.Sources(),
	}
	if query != "" {
		report := a.models.Search(r.Context(), query, 12)
		data.Results, data.Notes = report.Results, report.Notes
	}
	a.render(w, r, "models.html", "Cases for "+it.Name, data)
}

// handleAttachModel keeps one of the results against the item. The site's
// render is downloaded so the item page shows it without reaching out again,
// and so it survives the model being taken down.
func (a *App) handleAttachModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/items/%d", id)

	m := Model{
		ItemID:    id,
		Source:    strings.TrimSpace(r.FormValue("source")),
		Title:     strings.TrimSpace(r.FormValue("title")),
		URL:       strings.TrimSpace(r.FormValue("url")),
		Author:    strings.TrimSpace(r.FormValue("author")),
		Licence:   strings.TrimSpace(r.FormValue("licence")),
		Downloads: atoiSafe(r.FormValue("downloads")),
		Likes:     atoiSafe(r.FormValue("likes")),
		Note:      strings.TrimSpace(r.FormValue("note")),
	}
	if v, err := parsePrice(r.FormValue("rating")); err == nil {
		m.Rating = v
	}
	if m.URL == "" {
		redirect(w, r, dest, "", "a model needs a link")
		return
	}
	if m.Title == "" {
		m.Title = "Model"
	}
	if m.Source == "" {
		m.Source = sourceFromURL(m.URL)
	}

	// The picture is a nicety: a model whose thumbnail will not download is
	// still worth keeping.
	if img := strings.TrimSpace(r.FormValue("image_url")); img != "" {
		if body, err := a.fetcher.FetchImage(r.Context(), img); err == nil {
			if name, err := a.photos.Save(bytes.NewReader(body), lastPathSegment(img)); err == nil {
				m.Thumb = name
			}
		}
	}

	modelID, err := a.store.AddModel(m)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "kept a model", "item", id, m.Source+": "+m.Title)
	_ = modelID
	// Guidance, not a problem, so it goes in the same message rather than in a
	// warning beside it.
	redirect(w, r, dest,
		"Kept "+m.Title+" — give it the STL below to have the real geometry measured and drawn", "")
}

func (a *App) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	itemID, files, err := a.store.DeleteModel(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	for _, f := range files {
		a.photos.Remove(f)
	}
	redirect(w, r, fmt.Sprintf("/items/%d", itemID), "Model removed", "")
}

// handleModelMesh reads an STL, measures it and draws it.
//
// The file itself is not kept. Its measurements and a render of it are, which
// is everything the page shows -- and a hundred models at forty megabytes each
// is not something an inventory should quietly start storing for you. The link
// back to the model site is where the file lives.
func (a *App) handleModelMesh(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	m, err := a.store.GetModel(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	dest := fmt.Sprintf("/items/%d", m.ItemID)

	raw, source, err := a.meshBytes(r)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}

	mesh, err := media.ParseSTL(raw)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	png, err := mesh.Render()
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	name, err := a.photos.Save(bytes.NewReader(png), "model.png")
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}

	// Replace any previous render rather than leaving it orphaned on disk.
	if m.Preview != "" {
		a.photos.Remove(m.Preview)
	}
	err = a.store.SetModelPreview(id, name, mesh.Dimensions(), mesh.TriangleCount())
	if err != nil {
		a.photos.Remove(name)
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "measured a model", "item", m.ItemID,
		m.Title+" — "+mesh.Dimensions())

	msg := fmt.Sprintf("%s is %s (%s triangles, from %s)",
		m.Title, mesh.Dimensions(), humanCount(mesh.TriangleCount()), source)
	var note string
	if !mesh.FitsIn(220, 220, 250) {
		note = "that will not fit a 220 × 220 × 250 mm bed in one piece"
	}
	redirect(w, r, dest, msg, note)
}

// meshBytes takes the uploaded file, or fetches the URL when one was given.
func (a *App) meshBytes(r *http.Request) (raw []byte, source string, err error) {
	if r.MultipartForm != nil && len(r.MultipartForm.File["stl"]) > 0 {
		fh := r.MultipartForm.File["stl"][0]
		f, err := fh.Open()
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		raw, err := media.ReadSTL(f)
		if err != nil {
			return nil, "", err
		}
		if len(raw) > 0 {
			return raw, fh.Filename, nil
		}
	}
	link := strings.TrimSpace(r.FormValue("stl_url"))
	if link == "" {
		return nil, "", fmt.Errorf("choose an STL file, or give a direct link to one")
	}
	body, _, err := a.fetcher.get(r.Context(), link, "model/stl,application/octet-stream,*/*", media.MaxSTLBytes)
	if err != nil {
		return nil, "", err
	}
	return body, hostOf(link), nil
}

// remoteImageHosts are the only hosts the thumbnail proxy will fetch from.
//
// Search results are transient, so downloading every thumbnail to disk would be
// wasteful -- but letting the browser load them straight from the model site
// tells that site the IP of everyone who opens the page, and leaves the results
// blank for an install with no direct internet access. Proxying fixes both.
// The list is closed rather than open, so this is a thumbnail proxy for the
// sites we search and not a general-purpose one pointed at your network.
var remoteImageHosts = map[string]bool{
	"media.printables.com": true,
	"cdn.thingiverse.com":  true,
	"cdn.thangs.com":       true,
}

// handleRemoteImage serves a search result's thumbnail through the same
// SSRF-guarded fetcher everything else uses, and keeps a copy on disk.
//
// Without the cache, every look at a page of results re-fetched a dozen images
// from somebody else's server: slow, rude, and it means the same page looks
// different when the site is down. Cached, a case you have looked at once
// stays visible offline. The cache is keyed by the URL, lives beside the
// photos, and is disposable -- deleting it costs a refetch and nothing else.
func (a *App) handleRemoteImage(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	u, err := parseTarget(raw)
	if err != nil {
		http.Error(w, "bad image URL", http.StatusBadRequest)
		return
	}
	if !remoteImageHosts[strings.ToLower(u.Hostname())] {
		http.Error(w, "that host is not one of the model sites", http.StatusForbidden)
		return
	}

	serve := func(body []byte) {
		ext := media.SniffFormat(body)
		if ext == "" {
			http.Error(w, "that was not an image", http.StatusBadGateway)
			return
		}
		if ext == "jpg" {
			ext = "jpeg"
		}
		w.Header().Set("Content-Type", "image/"+ext)
		// A thumbnail at a content URL never changes, so let the browser keep
		// it for a day too and skip the round trip entirely.
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Write(body)
	}

	if body, ok := a.photos.CachedRemote(u.String()); ok {
		serve(body)
		return
	}
	body, err := a.fetcher.FetchImage(r.Context(), u.String())
	if err != nil {
		http.Error(w, "could not fetch that image", http.StatusBadGateway)
		return
	}
	// A cache that cannot be written is not worth failing the request over.
	a.photos.CacheRemote(u.String(), body)
	serve(body)
}

// --- searching prints on their own ------------------------------------------

// The per-item case search starts from a part and guesses the words. This one
// starts from nothing: a plain search box for when you just want a bracket, a
// knob or a drawer insert, with no part on the shelf to hang it off.

type printSearchData struct {
	Query     string
	Results   []ModelResult
	Notes     []string
	Sources   []string
	Elsewhere []ExternalModelSource
	Items     []Item // to hang a result off, if you want to keep one
	Searched  bool
	Caps      Capabilities
}

func (a *App) handlePrints(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	data := printSearchData{
		Query:     query,
		Sources:   a.models.Sources(),
		Elsewhere: ExternalModelSources(query),
		Searched:  query != "",
	}
	if tools, err := a.store.ListTools(); err == nil {
		data.Caps = SummariseTools(tools)
	}
	if query != "" {
		report := a.models.Search(r.Context(), query, 24)
		data.Results, data.Notes = report.Results, report.Notes
		// Only needed once there is something to keep.
		if items, err := a.store.ListItems(Query{Sort: "name"}); err == nil {
			data.Items = items
		}
	}
	title := "Prints"
	if query != "" {
		title = query
	}
	a.render(w, r, "prints.html", title, data)
}

// handleKeepPrint hangs a result found here off a part, reusing the same
// attach path the per-item search uses so a kept print behaves identically
// however it was found.
func (a *App) handleKeepPrint(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	back := "/prints"
	if q := strings.TrimSpace(r.FormValue("q")); q != "" {
		back += "?q=" + url.QueryEscape(q)
	}
	itemID, err := strconv.ParseInt(r.FormValue("item_id"), 10, 64)
	if err != nil || itemID <= 0 {
		redirect(w, r, back, "", "pick which part this is for")
		return
	}
	it, err := a.store.GetItem(itemID)
	if err != nil {
		redirect(w, r, back, "", "that part is not on the shelf any more")
		return
	}

	m := Model{
		ItemID:  itemID,
		Source:  strings.TrimSpace(r.FormValue("source")),
		Title:   strings.TrimSpace(r.FormValue("title")),
		URL:     strings.TrimSpace(r.FormValue("url")),
		Author:  strings.TrimSpace(r.FormValue("author")),
		Licence: strings.TrimSpace(r.FormValue("licence")),
	}
	if m.URL == "" {
		redirect(w, r, back, "", "that result has no link")
		return
	}
	if m.Title == "" {
		m.Title = "Model"
	}
	if m.Source == "" {
		m.Source = sourceFromURL(m.URL)
	}
	// Same as the per-item path: the picture is a nicety, not a requirement.
	if img := strings.TrimSpace(r.FormValue("image_url")); img != "" {
		if body, err := a.fetcher.FetchImage(r.Context(), img); err == nil {
			if name, err := a.photos.Save(bytes.NewReader(body), lastPathSegment(img)); err == nil {
				m.Thumb = name
			}
		}
	}
	if _, err := a.store.AddModel(m); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "kept a model", "item", itemID, m.Source+": "+m.Title)
	redirect(w, r, back, "Kept "+m.Title+" against "+it.Name, "")
}
