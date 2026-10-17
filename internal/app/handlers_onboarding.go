package app

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// First boot, and the workshop page it leads to.
//
// The wizard asks three things and every one of them can be skipped, because a
// setup screen that cannot be escaped is worse than no setup screen. What it is
// really for is the tool list: the app cannot know whether to suggest printing
// a case or buying one until somebody says whether there is a printer.

type welcomeData struct {
	Profile        Profile
	Step           string
	Rail           []railStep // the progress dots; empty on the opening page
	Printers       []Printer
	PrinterPresets []Preset
	Tools          []Tool
	Parsed         []Tool
	Raw            string
	Kinds          []struct{ Slug, Label string }
	Providers      []AIProvider
	Presets        []aiPreset
	Items          int
}

// needsOnboarding is checked by the dashboard rather than by a global
// redirect: a link somebody has bookmarked should still work on a fresh
// install, and an empty app is not broken, just empty.
func (a *App) needsOnboarding() bool {
	p, err := a.store.GetProfile()
	return err == nil && !p.Onboarded()
}

func (a *App) handleWelcome(w http.ResponseWriter, r *http.Request) {
	profile, err := a.store.GetProfile()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tools, err := a.store.ListTools()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	providers, err := a.store.ListProviders()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	items, _ := a.store.CountItems()

	step := r.URL.Query().Get("step")
	if step == "" {
		step = "hello"
	}
	printers, _ := a.store.ListPrinters()
	a.render(w, r, "welcome.html", "Welcome", welcomeData{
		Profile: profile, Step: step, Tools: tools, Kinds: ToolKinds(),
		Providers: providers, Presets: AIPresets, Items: items,
		Rail: welcomeRail(step), Printers: printers, PrinterPresets: PrinterPresets,
	})
}

// railStep is one dot on the progress rail.
type railStep struct {
	Key, Label string
	State      string // done | now | later
}

// welcomeOrder is the wizard, in order. The opening page is not on the rail:
// it is the cover, not a step.
var welcomeOrder = []struct{ Key, Label string }{
	{"you", "You"}, {"tools", "Tools"}, {"printer", "Printer"}, {"ai", "Assistant"}, {"done", "Done"},
}

func welcomeRail(step string) []railStep {
	if step == "hello" {
		return nil
	}
	if step == "review" {
		step = "tools" // reviewing the pasted list is still the tools step
	}
	at := -1
	for i, s := range welcomeOrder {
		if s.Key == step {
			at = i
		}
	}
	out := make([]railStep, len(welcomeOrder))
	for i, s := range welcomeOrder {
		state := "later"
		switch {
		case i < at:
			state = "done"
		case i == at:
			state = "now"
		}
		out[i] = railStep{s.Key, s.Label, state}
	}
	return out
}

// RailPercent is how far along the rail's fill is drawn.
func (d welcomeData) RailPercent() int {
	for i, s := range d.Rail {
		if s.State == "now" {
			return i * 100 / (len(d.Rail) - 1)
		}
	}
	return 0
}

// handleSaveProfile writes the profile from either the wizard or the profile
// page, and sends you on to whichever came next.
func (a *App) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	existing, err := a.store.GetProfile()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	existing.Name = strings.TrimSpace(r.FormValue("name"))
	existing.Bench = strings.TrimSpace(r.FormValue("bench"))
	existing.Units = r.FormValue("units")
	existing.Skill = strings.TrimSpace(r.FormValue("skill"))
	existing.Interests = strings.TrimSpace(r.FormValue("interests"))

	if err := a.store.SaveProfile(existing); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "updated the profile", "profile", 0, existing.Name)

	if next := r.FormValue("next"); next != "" {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	redirect(w, r, "/profile", "Saved", "")
}

// handleFinishOnboarding closes the wizard.
func (a *App) handleFinishOnboarding(w http.ResponseWriter, r *http.Request) {
	if err := a.store.MarkOnboarded(); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/", "You are set up — everything below is yours to fill in", "")
}

// --- the workshop -----------------------------------------------------------

type workshopData struct {
	Profile Profile
	Groups  []ToolGroup
	Caps    Capabilities
	Kinds   []struct{ Slug, Label string }
	Count   int
}

func (a *App) handleWorkshop(w http.ResponseWriter, r *http.Request) {
	profile, err := a.store.GetProfile()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tools, err := a.store.ListTools()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.render(w, r, "workshop.html", "Workshop", workshopData{
		Profile: profile, Groups: GroupTools(tools), Caps: a.caps(tools),
		Kinds: ToolKinds(), Count: len(tools),
	})
}

// handleParseTools interprets a pasted list and shows what it made of it. It
// deliberately does not save: the review step is the whole point, because a
// classifier that files your oscilloscope under hand tools should be caught by
// you rather than discovered three weeks later in a suggestion.
func (a *App) handleParseTools(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	raw := r.FormValue("raw")
	parsed := ParseToolList(raw)
	if len(parsed) == 0 {
		dest := "/workshop"
		if r.FormValue("wizard") != "" {
			dest = "/welcome?step=tools"
		}
		redirect(w, r, dest, "", "nothing in there looked like a tool — one per line works best")
		return
	}

	profile, _ := a.store.GetProfile()
	tools, _ := a.store.ListTools()
	providers, _ := a.store.ListProviders()
	items, _ := a.store.CountItems()

	step := "review"
	if r.FormValue("wizard") == "" {
		step = "review-only"
	}
	a.render(w, r, "welcome.html", "What I made of that", welcomeData{
		Profile: profile, Step: step, Tools: tools, Parsed: parsed, Raw: raw,
		Kinds: ToolKinds(), Providers: providers, Presets: AIPresets, Items: items,
	})
}

// handleConfirmTools saves the reviewed list, with whatever corrections were
// made to the kinds on the way through.
func (a *App) handleConfirmTools(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	names := r.Form["name"]
	kinds := r.Form["kind"]
	details := r.Form["detail"]
	keep := map[string]bool{}
	for _, idx := range r.Form["keep"] {
		keep[idx] = true
	}

	var tools []Tool
	for i, name := range names {
		if !keep[strconv.Itoa(i)] {
			continue
		}
		t := Tool{Name: strings.TrimSpace(name), Kind: "other"}
		if i < len(kinds) {
			t.Kind = kinds[i]
		}
		if i < len(details) {
			t.Detail = strings.TrimSpace(details[i])
		}
		if t.Name != "" {
			tools = append(tools, t)
		}
	}
	if len(tools) == 0 {
		redirect(w, r, "/workshop", "", "nothing was ticked, so nothing was added")
		return
	}
	added, err := a.store.AddTools(tools)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "added tools", "workshop", 0, fmt.Sprintf("%d tools", added))

	msg := fmt.Sprintf("Added %s to the workshop", plural(added, "tool"))
	if skipped := len(tools) - added; skipped > 0 {
		msg += fmt.Sprintf(" — %d were already there", skipped)
	}
	if r.FormValue("wizard") != "" {
		redirect(w, r, "/welcome?step=printer", msg, "")
		return
	}
	redirect(w, r, "/workshop", msg, "")
}

func (a *App) handleAddTool(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirect(w, r, "/workshop", "", "give the tool a name")
		return
	}
	kind := r.FormValue("kind")
	if kind == "" || kind == "auto" {
		kind = classifyTool(name + " " + r.FormValue("detail"))
	}
	if _, err := a.store.AddTool(Tool{
		Name: name, Kind: kind,
		Detail: strings.TrimSpace(r.FormValue("detail")),
		Notes:  strings.TrimSpace(r.FormValue("notes")),
	}); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/workshop", "Added "+name, "")
}

func (a *App) handleUpdateTool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	if err := a.store.UpdateTool(Tool{
		ID: id, Name: r.FormValue("name"), Kind: r.FormValue("kind"),
		Detail: strings.TrimSpace(r.FormValue("detail")),
		Notes:  strings.TrimSpace(r.FormValue("notes")),
	}); err != nil {
		redirect(w, r, "/workshop", "", err.Error())
		return
	}
	redirect(w, r, "/workshop", "Updated", "")
}

func (a *App) handleDeleteTool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteTool(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/workshop", "Removed", "")
}

// --- the profile page -------------------------------------------------------

type profileData struct {
	Profile  Profile
	Caps     Capabilities
	Tools    int
	Items    int
	Projects int
	Provider AIProvider
	HasAI    bool
	Since    time.Time
}

func (a *App) handleProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := a.store.GetProfile()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tools, err := a.store.ListTools()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	items, _ := a.store.CountItems()
	projects, _ := a.store.ListProjects()
	provider, hasAI := a.store.ActiveProvider()

	a.render(w, r, "profile.html", "Profile", profileData{
		Profile: profile, Caps: a.caps(tools), Tools: len(tools),
		Items: items, Projects: len(projects), Provider: provider, HasAI: hasAI,
		Since: profile.CreatedAt,
	})
}
