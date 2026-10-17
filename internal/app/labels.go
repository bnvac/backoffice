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

// --- labels for things that are not parts --------------------------------------

// The same QR idea, for the things around the parts: a board with firmware on
// it, a machine in the cupboard, a spool, a printer. Scanning one opens its
// page, which is where the useful part is — the reflash command, the address,
// the scale form, the maintenance list.

// qrTargets maps a kind of thing to the page its code opens.
var qrTargets = map[string]string{
	"board":   "/firmware/%d",
	"machine": "/machines/%d",
	"spool":   "/filament/%d",
	"printer": "/printing/printers/%d",
}

func (a *App) handleThingQR(w http.ResponseWriter, r *http.Request) {
	format, ok := qrTargets[r.PathValue("kind")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	code, err := qr.Encode(a.BaseURL(r)+fmt.Sprintf(format, id), qr.M, qr.Auto)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.writeBarcode(w, code, labelQRSize, labelQRSize)
}

// thingLabel is one label on a sheet of non-part things.
type thingLabel struct {
	QR     string
	Title  string
	Lines  []string
	Swatch string // spools get their colour printed on the label
	Ink    string
}

var thingKinds = []struct{ Key, Label string }{
	{"spools", "Spools"}, {"boards", "Boards"}, {"machines", "Machines"}, {"printers", "Printers"},
}

func (a *App) handleThingLabels(w http.ResponseWriter, r *http.Request) {
	what := r.PathValue("what")
	var labels []thingLabel
	switch what {
	case "spools":
		spools, _ := a.store.ListSpools(false)
		for _, s := range spools {
			m := s.Info()
			lines := []string{fmt.Sprintf("%s · %d °C nozzle · %d °C bed", m.Name, m.Hotend, m.Bed)}
			if s.Brand != "" {
				lines = append(lines, s.Brand)
			}
			// The empty weight is the number you need at the scale and the one
			// nobody remembers, so it goes on the label.
			if s.TareGrams > 0 {
				lines = append(lines, fmt.Sprintf("%.0f g net · empty spool %.0f g", s.NetGrams, s.TareGrams))
			}
			labels = append(labels, thingLabel{QR: fmt.Sprintf("/qr/spool/%d", s.ID),
				Title: s.Label(), Lines: lines, Swatch: s.Swatch(), Ink: s.Ink()})
		}
	case "boards":
		flashes, _ := a.store.ListFlashes(0, 0)
		for _, f := range flashes {
			if !f.Current {
				continue
			}
			title := f.Board()
			if f.Unit != "" {
				title += " — " + f.Unit
			}
			line := f.Firmware
			if f.Version != "" {
				line += " " + f.Version
			}
			labels = append(labels, thingLabel{QR: fmt.Sprintf("/qr/board/%d", f.ID),
				Title: title, Lines: []string{line, "flashed " + f.FlashedDate()}})
		}
	case "machines":
		machines, _ := a.store.ListMachines()
		for _, m := range machines {
			var lines []string
			for _, s := range []string{m.Role, m.Address, m.OS} {
				if s != "" {
					lines = append(lines, s)
				}
			}
			labels = append(labels, thingLabel{QR: fmt.Sprintf("/qr/machine/%d", m.ID),
				Title: m.Name, Lines: lines})
		}
	case "printers":
		printers, _ := a.store.ListPrinters()
		for _, p := range printers {
			var lines []string
			if p.NozzleLabel() != "" {
				lines = append(lines, p.NozzleLabel())
			}
			if p.Bed.Known() {
				lines = append(lines, p.Bed.String())
			}
			labels = append(labels, thingLabel{QR: fmt.Sprintf("/qr/printer/%d", p.ID),
				Title: p.Name, Lines: lines})
		}
	default:
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "thing-labels.html", "Labels", struct {
		What    string
		Kinds   []struct{ Key, Label string }
		Labels  []thingLabel
		BaseURL string
	}{what, thingKinds, labels, a.BaseURL(r)})
}
