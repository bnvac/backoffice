package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The idea board and the planning that follows it.

type brainstormData struct {
	Ideas    []Idea
	Caps     Capabilities
	Shelf    Shelf
	Provider AIProvider
	HasAI    bool
	Items    int
	Tools    int
	Ask      string
	Filter   string
	Profile  Profile
}

func (d brainstormData) Buildable() int {
	n := 0
	for _, i := range d.Ideas {
		if i.Buildable() {
			n++
		}
	}
	return n
}

func (a *App) handleBrainstorm(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("status")
	ideas, err := a.store.ListIdeas(filter)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	items, err := a.store.ListItems(Query{})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tools, err := a.store.ListTools()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	profile, _ := a.store.GetProfile()
	provider, hasAI := a.store.ActiveProvider()

	a.render(w, r, "brainstorm.html", "Ideas", brainstormData{
		Ideas: ideas, Caps: a.caps(tools), Shelf: ReadShelf(items),
		Provider: provider, HasAI: hasAI, Items: len(items), Tools: len(tools),
		Ask: r.URL.Query().Get("ask"), Filter: filter, Profile: profile,
	})
}

// handleGenerateIdeas fills the board.
//
// Which suggester runs is the user's choice, not a silent fallback: if a model
// is configured and it fails, that is reported rather than quietly swapped for
// the shelf reader, because "here are some ideas" hiding "your API key is
// wrong" is exactly the kind of thing that wastes an afternoon.
func (a *App) handleGenerateIdeas(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	items, err := a.store.ListItems(Query{})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tools, err := a.store.ListTools()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	caps := a.caps(tools)
	ask := strings.TrimSpace(r.FormValue("ask"))
	useModel := r.FormValue("source") == "model"

	var ideas []Idea
	var note string

	if useModel {
		provider, ok := a.store.ActiveProvider()
		if !ok {
			redirect(w, r, "/ideas", "", "no model is switched on — set one up first, or use the shelf")
			return
		}
		profile, _ := a.store.GetProfile()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()

		ideas, err = SuggestWithModel(ctx, provider, items, caps, profile, ask, 6)
		if err != nil {
			redirect(w, r, "/ideas", "", provider.Name+": "+err.Error())
			return
		}
		note = "from " + provider.Name
	} else {
		ideas = SuggestFromShelf(items, caps, 8)
		if len(ideas) == 0 {
			redirect(w, r, "/ideas", "",
				"nothing on the shelf lines up into a project yet — a controller and one sensor is enough to start")
			return
		}
		note = "from your shelf"
	}

	saved, err := a.store.SaveIdeas(ideas)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "generated ideas", "idea", 0, fmt.Sprintf("%d %s", saved, note))

	msg := fmt.Sprintf("Added %s %s", plural(saved, "idea"), note)
	if dupes := len(ideas) - saved; dupes > 0 {
		msg += fmt.Sprintf(" — %d were already on the board", dupes)
	}
	redirect(w, r, "/ideas", msg, "")
}

// --- one idea ---------------------------------------------------------------

type ideaData struct {
	Idea      Idea
	Caps      Capabilities
	Plan      EnclosurePlan
	Models    []ModelResult
	Searched  bool
	SearchErr string
	Notes     []string
	Query     string
	Parts     []Item
}

func (a *App) handleIdea(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	idea, err := a.store.GetIdea(id)
	if err != nil {
		a.fail(w, err, http.StatusNotFound)
		return
	}
	tools, err := a.store.ListTools()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	caps := a.caps(tools)

	// The parts that are really on the shelf, so the enclosure plan can look at
	// their recorded sizes rather than at their names.
	var parts []Item
	for _, p := range idea.Parts {
		if p.ItemID == nil {
			continue
		}
		if it, err := a.store.GetItem(*p.ItemID); err == nil {
			parts = append(parts, it)
		}
	}

	a.render(w, r, "idea.html", idea.Title, ideaData{
		Idea: idea, Caps: caps, Plan: PlanEnclosure(caps, parts), Parts: parts,
	})
}

// handleIdeaCases looks for a printable enclosure for the idea's parts. It is
// a button rather than something the page does on its own, because it reaches
// out to a model site and that should be a decision.
func (a *App) handleIdeaCases(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	idea, err := a.store.GetIdea(id)
	if err != nil {
		a.fail(w, err, http.StatusNotFound)
		return
	}
	tools, _ := a.store.ListTools()
	caps := a.caps(tools)

	var parts []Item
	for _, p := range idea.Parts {
		if p.ItemID == nil {
			continue
		}
		if it, err := a.store.GetItem(*p.ItemID); err == nil {
			parts = append(parts, it)
		}
	}
	plan := PlanEnclosure(caps, parts)

	data := ideaData{Idea: idea, Caps: caps, Plan: plan, Parts: parts, Searched: true}
	if !plan.CanSearch {
		// Not an error: there is simply no printer, and the plan already says
		// what to do instead.
		data.SearchErr = "no 3D printer in the workshop, so there is nothing to print — see the plan above"
		a.render(w, r, "idea.html", idea.Title, data)
		return
	}

	query := strings.TrimSpace(r.FormValue("q"))
	if query == "" && len(plan.SearchTerms) > 0 {
		query = plan.SearchTerms[0]
	}
	if query == "" {
		query = idea.Title + " case"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	report := a.models.Search(ctx, query, 12)
	data.Models = report.Results
	data.Query = query
	// The hub reports which sites answered and which did not, and that is worth
	// showing: "nothing found" and "the site was unreachable" are different
	// answers and lead to different next moves.
	data.Notes = report.Notes
	a.render(w, r, "idea.html", idea.Title, data)
}

func (a *App) handleIdeaStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	status := r.FormValue("status")
	if err := a.store.SetIdeaStatus(id, status); err != nil {
		redirect(w, r, "/ideas", "", err.Error())
		return
	}
	redirect(w, r, "/ideas", "Moved to "+status, "")
}

func (a *App) handleDeleteIdea(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteIdea(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/ideas", "Removed from the board", "")
}

// handlePlanIdea turns an idea into a project with its parts already listed,
// which is where the wiring, the pin budget and the build log take over.
func (a *App) handlePlanIdea(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	projectID, err := a.store.PlanIdea(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "planned an idea", "project", projectID, "")
	redirect(w, r, fmt.Sprintf("/projects/%d", projectID),
		"Planned — the parts came across, wiring is below", "")
}
