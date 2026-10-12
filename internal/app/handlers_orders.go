package app

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type ordersData struct {
	Orders   []Order
	Leads    []LeadTimeActual
	Statuses []string
}

// Outstanding totals what is still on its way, across every open order.
func (d ordersData) Outstanding() int {
	n := 0
	for _, o := range d.Orders {
		if o.InFlight() {
			n += o.Outstanding()
		}
	}
	return n
}

func (d ordersData) Overdue() int {
	n := 0
	for _, o := range d.Orders {
		if o.Overdue() {
			n++
		}
	}
	return n
}

func (d ordersData) Spent() float64 {
	var total float64
	for _, o := range d.Orders {
		if !o.Cancelled() {
			total += o.Total()
		}
	}
	return total
}

func (a *App) handleOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := a.store.ListOrders()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	leads, err := a.store.LeadTimes()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.render(w, r, "orders.html", "Orders", ordersData{
		Orders: orders, Leads: leads, Statuses: OrderStatuses,
	})
}

type orderData struct {
	Order    Order
	AllItems []Item
	Projects []Project
	Statuses []string
}

func (a *App) handleOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, err := a.store.GetOrder(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	items, err := a.store.ListItems(Query{Sort: "name"})
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	projects, err := a.store.ListProjects()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.render(w, r, "order.html", o.Source+" order", orderData{
		Order: o, AllItems: items, Projects: projects, Statuses: OrderStatuses,
	})
}

func orderFromForm(r *http.Request) Order {
	shipping, _ := parsePrice(r.FormValue("shipping"))
	return Order{
		Source:     strings.TrimSpace(r.FormValue("source")),
		Reference:  strings.TrimSpace(r.FormValue("reference")),
		Status:     orDefault(r.FormValue("status"), "draft"),
		PlacedAt:   parseDay(r.FormValue("placed_at")),
		ExpectedAt: parseDay(r.FormValue("expected_at")),
		Tracking:   strings.TrimSpace(r.FormValue("tracking")),
		Shipping:   shipping,
		Currency:   strings.ToUpper(orDefault(r.FormValue("currency"), "USD")),
		Notes:      strings.TrimSpace(r.FormValue("notes")),
		ProjectID:  optionalID(r.FormValue("project_id")),
	}
}

func (a *App) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	o := orderFromForm(r)
	if o.Source == "" {
		redirect(w, r, "/orders", "", "say who the order is with")
		return
	}
	id, err := a.store.CreateOrder(o)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "started an order", "order", id, o.Source)
	redirect(w, r, fmt.Sprintf("/orders/%d", id), "Order started", "")
}

func (a *App) handleUpdateOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	o := orderFromForm(r)
	o.ID = id
	if err := a.store.UpdateOrder(o); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "changed an order", "order", id, o.Source+" · "+o.Status)
	redirect(w, r, fmt.Sprintf("/orders/%d", id), "Saved", "")
}

func (a *App) handleDeleteOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	o, err := a.store.GetOrder(id)
	if err == nil && o.Received() {
		redirect(w, r, fmt.Sprintf("/orders/%d", id), "",
			"this order's parts are on the shelf — un-receive it first, or the counts will be wrong")
		return
	}
	if err := a.store.DeleteOrder(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "deleted an order", "order", id, o.Source)
	redirect(w, r, "/orders", "Order deleted", "")
}

func (a *App) handleAddOrderLine(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/orders/%d", id)

	qty, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("quantity")))
	price, _ := parsePrice(r.FormValue("unit_price"))
	err := a.store.AddOrderLine(id, OrderLine{
		ItemID:    optionalID(r.FormValue("item_id")),
		Name:      strings.TrimSpace(r.FormValue("name")),
		Quantity:  qty,
		UnitPrice: price,
		Currency:  strings.ToUpper(orDefault(r.FormValue("currency"), "USD")),
		Note:      strings.TrimSpace(r.FormValue("note")),
	})
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	redirect(w, r, dest, "Added to the order", "")
}

func (a *App) handleDeleteOrderLine(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	orderID, err := a.store.DeleteOrderLine(id)
	if err != nil {
		if orderID > 0 {
			redirect(w, r, fmt.Sprintf("/orders/%d", orderID), "", err.Error())
			return
		}
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, fmt.Sprintf("/orders/%d", orderID), "Removed", "")
}

// handleReceiveOrder is the moment stock arrives. It is the inverse of building
// a project, and like that action it records what it did so it can be undone.
func (a *App) handleReceiveOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dest := fmt.Sprintf("/orders/%d", id)
	n, err := a.store.ReceiveOrder(id)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	o, _ := a.store.GetOrder(id)
	a.store.Record(a.actor(r), "received an order", "order", id,
		fmt.Sprintf("%s from %s", plural(n, "piece"), o.Source))

	// How long it took is worth saying, but it is good news rather than a
	// problem, so it belongs in the same message rather than in a warning.
	msg := fmt.Sprintf("Received — %s went on the shelf", plural(n, "piece"))
	if took := o.TookDays(); took > 0 && o.Source != "" {
		msg += fmt.Sprintf(". %s took %s, which is now its recorded lead time",
			o.Source, plural(took, "day"))
	}
	redirect(w, r, dest, msg, "")
}

func (a *App) handleUnreceiveOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dest := fmt.Sprintf("/orders/%d", id)
	n, err := a.store.UnreceiveOrder(id)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "un-received an order", "order", id, plural(n, "piece"))
	redirect(w, r, dest, fmt.Sprintf("Took %s back off the shelf", plural(n, "piece")), "")
}

// handleOrderFromShortfall drafts orders holding everything a project needs,
// one per shop, since that is how you actually buy.
func (a *App) handleOrderFromShortfall(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dest := fmt.Sprintf("/projects/%d", id)
	ids, err := a.store.OrderFromShortfall(id)
	if err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "drafted orders from a shortfall", "project", id,
		plural(len(ids), "order"))

	if len(ids) == 1 {
		redirect(w, r, fmt.Sprintf("/orders/%d", ids[0]), "Drafted an order from what is missing", "")
		return
	}
	redirect(w, r, "/orders",
		fmt.Sprintf("Drafted %s — one per shop", plural(len(ids), "order")), "")
}
