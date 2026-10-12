package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const liveSearchLimit = 8

// handleQuickAdd takes a single pasted link and turns it into an item in one
// step: metadata, photo, price, and any datasheet or pinout the page links to.
//
// The slow path -- add form, paste, fetch, review, save -- is still there and
// still better when the page guesses wrong. This is for the common case of
// "I just bought this, put it in the inventory".
func (a *App) handleQuickAdd(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	link := strings.TrimSpace(r.FormValue("url"))
	if link == "" {
		redirect(w, r, "/", "", "paste a product link first")
		return
	}

	meta, err := a.fetcher.FetchPage(r.Context(), link)
	if err != nil {
		redirect(w, r, "/", "", err.Error())
		return
	}

	name := meta.Title
	if name == "" {
		name = lastPathSegment(meta.URL)
	}
	it := Item{
		Name:       name,
		Quantity:   1,
		Link:       meta.URL,
		Notes:      meta.Description,
		PartNumber: meta.PartNumber,
		FolderID:   optionalID(r.FormValue("folder_id")),
	}
	id, err := a.store.CreateItem(it)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	var problems []string
	if meta.ImageURL != "" {
		if msg := a.savePhotoFromURL(r.Context(), id, meta.ImageURL); msg != "" {
			problems = append(problems, "photo: "+msg)
		}
	}
	if meta.Price > 0 {
		source := meta.SiteName
		if source == "" {
			source = sourceFromURL(meta.URL)
		}
		err := a.store.SetPrice(id, Price{
			Source: source, Amount: meta.Price,
			Currency: orDefault(meta.Currency, "USD"), URL: meta.URL,
			LeadDays: DefaultLeadDays(source),
		})
		if err != nil {
			problems = append(problems, "price: "+err.Error())
		}
	}

	// Attach whatever the page linked to. A pinout is fetched as an image so it
	// is visible; everything else stays a link.
	refs := a.attachDiscoveredDocuments(r, id, meta.Documents)

	flash := fmt.Sprintf("Added %s", name)
	if refs > 0 {
		flash += fmt.Sprintf(" with %d reference(s)", refs)
	}
	redirect(w, r, fmt.Sprintf("/items/%d", id), flash, strings.Join(problems, "; "))
}

// attachDiscoveredDocuments saves the datasheets and pinouts a page linked to,
// and reports how many stuck.
func (a *App) attachDiscoveredDocuments(r *http.Request, itemID int64, docs []PageDocument) int {
	saved := 0
	for _, d := range docs {
		ref := Reference{ItemID: itemID, Kind: d.Kind, Title: d.Title, URL: d.URL}
		// Pinouts are pictures; storing them means they render on the page and
		// survive the shop reorganising its assets.
		if d.Kind == "pinout" {
			if body, err := a.fetcher.FetchImage(r.Context(), d.URL); err == nil {
				if name, err := a.photos.Save(strings.NewReader(string(body)), lastPathSegment(d.URL)); err == nil {
					ref.Filename = name
				}
			}
		}
		if _, err := a.store.AddReference(ref); err == nil {
			saved++
		}
	}
	return saved
}

// handleLiveSearch backs the type-ahead on the search boxes. It returns the
// inventory's own matches -- not shop results -- because the question while
// typing is almost always "do I already have one of these?".
func (a *App) handleLiveSearch(w http.ResponseWriter, r *http.Request) {
	q := a.queryFromRequest(r)
	q.Sort = "name"

	type hit struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Location string `json:"location"`
		Folder   string `json:"folder"`
		Thumb    string `json:"thumb"`
		Quantity int    `json:"quantity"`
		Price    string `json:"price"`
	}
	out := struct {
		Results []hit `json:"results"`
		Total   int   `json:"total"`
	}{Results: []hit{}}

	if strings.TrimSpace(q.Search) == "" {
		writeJSON(w, out)
		return
	}

	items, err := a.store.ListItems(q)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Total = len(items)

	for _, it := range firstN(items, liveSearchLimit) {
		h := hit{
			ID: it.ID, Name: it.Name, Location: it.Location,
			Folder: it.FolderName, Thumb: it.Thumb(), Quantity: it.Quantity,
		}
		if best := it.Best(); best != nil {
			h.Price = best.Display()
		}
		out.Results = append(out.Results, h)
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
