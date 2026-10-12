package app

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"strings"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/code128"
	"github.com/boombuler/barcode/qr"
)

// Labels turn the inventory into something usable at the bench: stick one on a
// drawer, scan it with a phone, and the item's page opens.
//
// A QR code carries the item's full URL, so any phone camera opens the page
// with no app involved. A Code 128 barcode carries the part number instead,
// which is what a USB barcode scanner -- a keyboard, as far as the computer is
// concerned -- can type into a search box.

const (
	labelQRSize       = 512
	labelBarcodeW     = 600
	labelBarcodeH     = 160
	maxBarcodePayload = 80
)

// BaseURL works out the address a phone should open, preferring an explicit
// setting because the server usually sits behind a reverse proxy and cannot see
// its own public name.
func (a *App) BaseURL(r *http.Request) string {
	if a.cfg.BaseURL != "" {
		return strings.TrimRight(a.cfg.BaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host
}

// handleItemQR renders a QR code pointing at the item.
func (a *App) handleItemQR(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target := fmt.Sprintf("%s/items/%d", a.BaseURL(r), id)

	// Medium recovery: enough to survive a scuffed label without making the
	// code so dense it stops scanning at small print sizes.
	code, err := qr.Encode(target, qr.M, qr.Auto)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.writeBarcode(w, code, labelQRSize, labelQRSize)
}

// handleItemBarcode renders a Code 128 barcode of the part number, falling back
// to the item id when there is no part number to encode.
func (a *App) handleItemBarcode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	it, err := a.store.GetItem(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	payload := barcodePayload(it)
	code, err := code128.Encode(payload)
	if err != nil {
		a.fail(w, fmt.Errorf("%q cannot be encoded as a barcode: %w", payload, err), http.StatusBadRequest)
		return
	}
	a.writeBarcode(w, code, labelBarcodeW, labelBarcodeH)
}

// barcodePayload picks what a linear barcode should carry. Code 128 only covers
// ASCII, and long strings make an unreadably wide label, so anything unsuitable
// falls back to the item id -- which always scans and always resolves.
func barcodePayload(it Item) string {
	pn := strings.TrimSpace(it.PartNumber)
	if pn == "" || len(pn) > maxBarcodePayload || !isASCII(pn) {
		return fmt.Sprintf("BO-%d", it.ID)
	}
	return pn
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 || s[i] < 32 {
			return false
		}
	}
	return true
}

func (a *App) writeBarcode(w http.ResponseWriter, code barcode.Barcode, width, height int) {
	scaled, err := barcode.Scale(code, width, height)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	// Draw onto an opaque white background: printers and scanners both dislike
	// transparency, and a label is going on white paper anyway.
	canvas := image.NewRGBA(scaled.Bounds())
	for y := scaled.Bounds().Min.Y; y < scaled.Bounds().Max.Y; y++ {
		for x := scaled.Bounds().Min.X; x < scaled.Bounds().Max.X; x++ {
			c := scaled.At(x, y)
			if _, _, _, alpha := c.RGBA(); alpha == 0 {
				c = color.White
			}
			canvas.Set(x, y, c)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(buf.Bytes())
}

// --- printable label sheet --------------------------------------------------

type labelsData struct {
	Items   []Item
	Query   Query
	Folders []*Folder
	Code    string // "qr" or "barcode"
	Scope   string // what this sheet covers, for the heading
	BaseURL string
	Compact bool
}

// handleLabels renders a printable sheet. It reuses the grid's filters, so
// "labels for everything in Drawer 3" is the same query as viewing Drawer 3.
func (a *App) handleLabels(w http.ResponseWriter, r *http.Request) {
	q := a.queryFromRequest(r)
	q.Sort = orDefault(q.Sort, "name")

	if id := optionalID(r.URL.Query().Get("folder")); id != nil {
		q.FolderID = id
		q.Recursive = r.URL.Query().Get("all") == "1"
	}

	items, err := a.store.ListItems(q)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	tree, err := a.store.FolderTree()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}

	code := "qr"
	if r.URL.Query().Get("code") == "barcode" {
		code = "barcode"
	}

	a.render(w, r, "labels.html", "Labels", labelsData{
		Items:   items,
		Query:   q,
		Folders: FlattenFolders(tree),
		Code:    code,
		Scope:   labelScope(q, items),
		BaseURL: a.BaseURL(r),
		Compact: r.URL.Query().Get("compact") == "1",
	})
}

func labelScope(q Query, items []Item) string {
	switch {
	case q.FolderID != nil:
		return fmt.Sprintf("%d item(s) in this folder", len(items))
	case q.Location != "":
		return fmt.Sprintf("%d item(s) in %s", len(items), q.Location)
	case q.Any():
		return fmt.Sprintf("%d matching item(s)", len(items))
	default:
		return fmt.Sprintf("all %d item(s)", len(items))
	}
}

// LabelsURL builds the link to this sheet from a grid query, so "print labels
// for what I am looking at" needs no extra state.
func (q Query) LabelsURL(code string) string {
	v := q.values()
	if code != "" {
		v.Set("code", code)
	}
	if q.FolderID != nil {
		v.Set("folder", fmt.Sprint(*q.FolderID))
		if q.Recursive {
			v.Set("all", "1")
		}
	}
	if len(v) == 0 {
		return "/labels"
	}
	return "/labels?" + v.Encode()
}

var _ = url.Values{}
