package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The labels have been printable for a while; this is the half that reads them
// back. A phone camera pointed at a drawer should land on that part's page with
// the count buttons under your thumb, which is the whole reason for putting a
// code on the box in the first place.

// handleScan serves the scanner page. The scanning itself uses the browser's
// own BarcodeDetector, so there is no library to ship and nothing leaves the
// device -- but it is not in every browser, so the page also takes a typed code
// and works fine that way.
func (a *App) handleScan(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "scan.html", "Scan", nil)
}

// resolveCode turns whatever came off a label into an item.
//
// Three things can be on a label: the URL a QR code carries, the "BO-<id>"
// fallback a barcode uses when a part has no usable part number, and the part
// number itself. All three are accepted, and a bare part number is looked up
// against the inventory the same way search would.
func (a *App) resolveCode(code string) (Item, string, bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		return Item{}, "nothing scanned", false
	}

	// A QR code from this app carries a URL ending in /items/<id>.
	if u, err := url.Parse(code); err == nil && u.Host != "" {
		if id, ok := itemIDFromPath(u.Path); ok {
			if it, err := a.store.GetItem(id); err == nil {
				return it, "", true
			}
			return Item{}, fmt.Sprintf("that label points at item %d, which no longer exists", id), false
		}
	}
	// The barcode fallback for parts with no part number.
	if rest, ok := strings.CutPrefix(strings.ToUpper(code), "BO-"); ok {
		if id, err := strconv.ParseInt(rest, 10, 64); err == nil {
			if it, err := a.store.GetItem(id); err == nil {
				return it, "", true
			}
			return Item{}, fmt.Sprintf("no item %s any more", rest), false
		}
	}

	// Otherwise it is a part number -- either ours, or the manufacturer's
	// barcode on the bag it came in, which is just as good. Asking the database
	// for the one match beats reading the whole inventory to compare strings.
	var id int64
	err := a.store.db.QueryRow(`SELECT id FROM items WHERE part_number = ? COLLATE NOCASE
		ORDER BY id LIMIT 1`, code).Scan(&id)
	if err == nil {
		if it, err := a.store.GetItem(id); err == nil {
			return it, "", true
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Item{}, err.Error(), false
	}
	// A last pass over names, so scanning a shop's own barcode label still
	// finds something when the part number was never filled in.
	matches, err := a.store.ListItems(Query{Search: code})
	if err == nil && len(matches) == 1 {
		return matches[0], "", true
	}
	if len(matches) > 1 {
		return Item{}, fmt.Sprintf("%q matches %d items — none of them by part number", code, len(matches)), false
	}
	return Item{}, fmt.Sprintf("nothing here has the code %q", code), false
}

func itemIDFromPath(path string) (int64, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "items" {
			if id, err := strconv.ParseInt(parts[i+1], 10, 64); err == nil {
				return id, true
			}
		}
	}
	return 0, false
}

// handleLookup answers a scan. It replies with JSON when asked, so the scanner
// page can show the part without navigating away and keep the camera running;
// otherwise it redirects, which is what a typed code in a plain form does.
func (a *App) handleLookup(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		code = r.FormValue("code")
	}
	it, problem, ok := a.resolveCode(code)

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": problem, "code": code})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": it.ID, "name": it.Name, "quantity": it.Quantity,
			"available": it.Available(), "location": it.Location,
			"part_number": it.PartNumber, "thumb": it.Thumb(),
			"url": fmt.Sprintf("/items/%d", it.ID),
		})
		return
	}

	if !ok {
		// A code that matched nothing is a reasonable thing to want to add, so
		// the search page is a better landing spot than an error.
		redirect(w, r, "/items?q="+url.QueryEscape(code), "", problem)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/items/%d", it.ID), http.StatusSeeOther)
}
