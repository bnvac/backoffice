package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type machinesData struct {
	Machines []Machine
	Power    PowerBudget
	Items    []Item
	Edit     *Machine
	Children []Machine
}

// reachTimeout is per machine, and they are all tried at once, so the page
// costs this much at worst however many machines there are.
const reachTimeout = 900 * time.Millisecond

func (a *App) handleMachines(w http.ResponseWriter, r *http.Request) {
	machines, err := a.store.ListMachines()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	items, err := a.store.ListItems(Query{Sort: "name"})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*reachTimeout)
	defer cancel()
	Reachability(ctx, machines, reachTimeout)

	data := machinesData{Machines: machines, Power: BudgetPower(machines), Items: items}
	if id := optionalID(r.URL.Query().Get("edit")); id != nil {
		for i := range machines {
			if machines[i].ID == *id {
				m := machines[i]
				data.Edit = &m
			}
			if machines[i].ParentID != nil && *machines[i].ParentID == *id {
				data.Children = append(data.Children, machines[i])
			}
		}
	}
	a.render(w, r, "machines.html", "Machines", data)
}

func machineFromForm(r *http.Request) Machine {
	// Blank means SSH, 0 means do not check, and anything that is not a number
	// becomes -1 so validation refuses it rather than quietly checking port 0.
	port := 22
	if v := strings.TrimSpace(r.FormValue("port")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			n = -1
		}
		port = n
	}
	watts, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("watts")))
	if watts < 0 {
		watts = 0
	}
	return Machine{
		Name:     r.FormValue("name"),
		Role:     r.FormValue("role"),
		Address:  r.FormValue("address"),
		Port:     port,
		MAC:      r.FormValue("mac"),
		OS:       r.FormValue("os"),
		Image:    r.FormValue("image"),
		ImageSum: r.FormValue("image_sum"),
		Hardware: r.FormValue("hardware"),
		Location: r.FormValue("location"),
		Watts:    watts,
		Services: r.FormValue("services"),
		Notes:    r.FormValue("notes"),
		ItemID:   optionalID(r.FormValue("item_id")),
		ParentID: optionalID(r.FormValue("parent_id")),
	}
}

func (a *App) handleAddMachine(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	m := machineFromForm(r)
	id, err := a.store.AddMachine(m)
	if err != nil {
		redirect(w, r, "/machines", "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "added a machine", "machine", id, m.Name)
	redirect(w, r, "/machines", "Added "+strings.TrimSpace(m.Name), "")
}

func (a *App) handleUpdateMachine(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	m := machineFromForm(r)
	m.ID = id
	if err := a.store.UpdateMachine(m); err != nil {
		redirect(w, r, fmt.Sprintf("/machines?edit=%d#edit", id), "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "changed a machine", "machine", id, m.Name)
	redirect(w, r, "/machines", "Saved "+strings.TrimSpace(m.Name), "")
}

func (a *App) handleDeleteMachine(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := a.store.GetMachine(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeleteMachine(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "removed a machine", "machine", id, m.Name)
	redirect(w, r, "/machines", "Removed "+m.Name, "")
}
