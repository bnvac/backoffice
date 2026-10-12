package app

import (
	"fmt"
	"net/http"
	"strings"
)

// Steps post back to the project page rather than returning anything, so the
// whole checklist works with JavaScript off. Ticking a box is a form submit;
// that is slower than it would be with a fetch, and it is the reason the page
// still works on a phone with a flaky connection in a garage.

func (a *App) handleAddSteps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/projects/%d#steps", id)

	// One box takes either a single step or a whole pasted plan. Splitting on
	// newlines means no separate "bulk add" form to find.
	bodies := ParseSteps(r.FormValue("body"))
	if len(bodies) == 0 {
		redirect(w, r, dest, "", "write a step first")
		return
	}
	n, err := a.store.AddSteps(id, bodies)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	msg := "Added a step"
	if n > 1 {
		msg = fmt.Sprintf("Added %d steps", n)
	}
	redirect(w, r, dest, msg, "")
}

func (a *App) handleToggleStep(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	st, err := a.store.ToggleStep(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/projects/%d#steps", st.ProjectID), "", "")
}

func (a *App) handleUpdateStep(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	// The project is read before the write so the redirect has somewhere to go
	// even when the body turns out to be empty.
	_, projectID, err := a.store.stepsAround(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	dest := fmt.Sprintf("/projects/%d#steps", projectID)

	if err := a.store.UpdateStep(id, r.FormValue("body")); err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	redirect(w, r, dest, "", "")
}

func (a *App) handleMoveStep(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	projectID, err := a.store.MoveStep(id, strings.EqualFold(r.PathValue("dir"), "up"))
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/projects/%d#steps", projectID), "", "")
}

func (a *App) handleDeleteStep(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	projectID, err := a.store.DeleteStep(id)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/projects/%d#steps", projectID), "", "")
}

func (a *App) handleResetSteps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.ClearDoneSteps(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/projects/%d#steps", id), "Unticked every step", "")
}
