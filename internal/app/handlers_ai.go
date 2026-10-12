package app

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// The settings page for the model, and the one honest thing it does: a Test
// button that makes a real round trip and reports what actually happened.
// "Saved" is not the same as "works", and a key with a typo in it should be
// found here rather than the first time you press Brainstorm.

type aiData struct {
	Providers []AIProvider
	Presets   []aiPreset
	Active    AIProvider
	HasActive bool
}

func (a *App) handleAISettings(w http.ResponseWriter, r *http.Request) {
	providers, err := a.store.ListProviders()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	active, has := a.store.ActiveProvider()
	a.render(w, r, "ai.html", "Model", aiData{
		Providers: providers, Presets: AIPresets, Active: active, HasActive: has,
	})
}

// providerFromForm reads the editable fields. The key is only read when
// something was typed, because the form shows a placeholder rather than the
// stored value and an empty box means "leave it alone".
func providerFromForm(r *http.Request) AIProvider {
	kind := r.FormValue("kind")
	switch kind {
	case "ollama", "openai", "anthropic":
	default:
		kind = "openai"
	}
	return AIProvider{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Kind:     kind,
		Endpoint: strings.TrimSpace(r.FormValue("endpoint")),
		Model:    strings.TrimSpace(r.FormValue("model")),
		APIKey:   strings.TrimSpace(r.FormValue("api_key")),
	}
}

func (a *App) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	p := providerFromForm(r)
	if p.Name == "" {
		p.Name = p.KindLabel()
	}
	id, err := a.store.AddProvider(p)
	if err != nil {
		redirect(w, r, "/settings/ai", "", err.Error())
		return
	}
	// The first one configured becomes the active one, because otherwise
	// nothing happens after setting it up and it looks broken.
	if others, _ := a.store.ListProviders(); len(others) == 1 {
		a.store.ActivateProvider(id)
	}
	// Never record the key, or anything derived from it, in the activity log.
	a.store.Record(a.actor(r), "added a model endpoint", "ai", id, p.Name)

	dest := "/settings/ai"
	if r.FormValue("wizard") != "" {
		dest = "/welcome?step=done"
	}
	redirect(w, r, dest, "Added "+p.Name+" — test it to check it answers", "")
}

func (a *App) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	p := providerFromForm(r)
	p.ID = id
	if err := a.store.UpdateProvider(p); err != nil {
		redirect(w, r, "/settings/ai", "", err.Error())
		return
	}
	redirect(w, r, "/settings/ai", "Saved", "")
}

func (a *App) handleActivateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	if r.FormValue("off") != "" {
		if err := a.store.DeactivateProviders(); err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		redirect(w, r, "/settings/ai", "Model switched off — suggestions will come from your shelf", "")
		return
	}
	if err := a.store.ActivateProvider(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/settings/ai", "Switched over", "")
}

func (a *App) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteProvider(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/settings/ai", "Removed", "")
}

func (a *App) handleClearProviderKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.ClearProviderKey(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/settings/ai", "Key cleared", "")
}

// handleTestProvider asks the model something trivial and reports the answer.
// The question is deliberately one with a checkable reply, so a model that
// connects but returns nonsense is visible too.
func (a *App) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.store.GetProvider(id)
	if err != nil {
		a.fail(w, err, http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	started := time.Now()
	reply, err := p.Ask(ctx, "You are being tested. Answer in one short sentence.",
		"Say the words: the bench is ready.", false)
	took := time.Since(started).Round(10 * time.Millisecond)

	if err != nil {
		a.store.RecordProviderStatus(id, "failed: "+err.Error())
		redirect(w, r, "/settings/ai", "", p.Name+": "+err.Error())
		return
	}
	reply = strings.TrimSpace(reply)
	if len(reply) > 120 {
		reply = reply[:120] + "…"
	}
	a.store.RecordProviderStatus(id, "ok in "+took.String())
	redirect(w, r, "/settings/ai", p.Name+" answered in "+took.String()+": "+reply, "")
}
