package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const searchLimit = 12

// handleSearch looks a part up by name instead of by URL. It answers with what
// each source knows plus a note for anything that could not be reached, so the
// form can show partial results honestly rather than an empty list.
func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(r.FormValue("q"))
	if query == "" {
		writeJSONError(w, "type a part name to search for", http.StatusBadRequest)
		return
	}

	report := a.search.Search(r.Context(), query, searchLimit)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"results":  report.Results,
		"notes":    report.Notes,
		"external": ExternalSources(query),
	})
}

// --- prices -----------------------------------------------------------------

// handleSetPrice records what one source charges. Sources are free text so a
// local shop or a friend's spare box can be priced too.
func (a *App) handleSetPrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/items/%d", id)

	source := strings.TrimSpace(r.FormValue("source"))
	if source == "" {
		redirect(w, r, dest, "", "which shop is this price from?")
		return
	}
	amount, err := parsePrice(r.FormValue("amount"))
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}

	// A blank shipping time falls back to what that shop usually takes, so the
	// common case needs no typing; anything entered wins.
	lead, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("lead_days")))
	if strings.TrimSpace(r.FormValue("lead_days")) == "" {
		lead = DefaultLeadDays(source)
	}
	if lead < 0 {
		lead = 0
	}

	err = a.store.SetPrice(id, Price{
		Source:   source,
		Amount:   amount,
		Currency: strings.ToUpper(strings.TrimSpace(orDefault(r.FormValue("currency"), "USD"))),
		URL:      strings.TrimSpace(r.FormValue("url")),
		LeadDays: lead,
	})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, dest, "Price saved", "")
}

func (a *App) handleDeletePrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	if err := a.store.DeletePrice(id, r.FormValue("source")); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Price removed", "")
}

// --- tags -------------------------------------------------------------------

type tagsData struct {
	Tags []Facet
}

func (a *App) handleTags(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "tags.html", "Tags", tagsData{Tags: a.tagFacetsOrNil()})
}

// handleSetTagIcon changes the emoji shown for a tag. Icons are guessed from
// the tag name when it is first used; this is the override.
func (a *App) handleSetTagIcon(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	icon := strings.TrimSpace(r.FormValue("icon"))

	// One or two glyphs is an icon; a sentence is a mistake.
	if n := len([]rune(icon)); n > 3 {
		redirect(w, r, "/tags", "", "an icon should be a single emoji")
		return
	}
	if icon == "" {
		icon = IconForTag(name)
	}
	if err := a.store.SetTagIcon(name, icon); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/tags", "Updated "+name, "")
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
