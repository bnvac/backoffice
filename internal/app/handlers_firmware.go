package app

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
)

type firmwareData struct {
	Flashes  []Flash
	Items    []Item
	Projects []Project
	Limit    string
}

// handleFirmware is every board and what is on it.
func (a *App) handleFirmware(w http.ResponseWriter, r *http.Request) {
	flashes, err := a.store.ListFlashes(0, 0)
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
	a.render(w, r, "firmware.html", "Firmware", firmwareData{
		Flashes: flashes, Items: items, Projects: projects, Limit: a.firmware.Limit(),
	})
}

// handleFlash is one record: the checksum in full and how to put it back on.
func (a *App) handleFlash(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	f, err := a.store.GetFlash(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Whether this is the current record only means anything next to the
	// others for the same board, so it is worked out from that list rather
	// than left at the zero value a single fetch gives — which read as
	// "replaced" on the newest record of all.
	if siblings, err := a.store.ListFlashes(f.ItemRef(), 0); err == nil {
		for _, s := range siblings {
			if s.ID == f.ID {
				f.Current = s.Current
			}
		}
	}
	hint, hasHint := HintFor(f.File, f.ItemName)
	a.render(w, r, "flash.html", f.Firmware, struct {
		Flash   Flash
		Hint    FlashHint
		HasHint bool
	}{f, hint, hasHint})
}

func (a *App) handleAddFlash(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFirmware+(1<<20))
	if err := parseForm(r); err != nil {
		redirect(w, r, "/firmware", "", "that file is bigger than "+a.firmware.Limit())
		return
	}
	back := safeNext(r.FormValue("return"))
	if back == "" || back == "/" {
		back = "/firmware"
	}

	f := Flash{
		ItemID:    optionalID(r.FormValue("item_id")),
		Unit:      r.FormValue("unit"),
		Firmware:  r.FormValue("firmware"),
		Version:   r.FormValue("version"),
		ProjectID: optionalID(r.FormValue("project_id")),
		Notes:     r.FormValue("notes"),
	}
	if d := strings.TrimSpace(r.FormValue("flashed_at")); d != "" {
		if t, err := time.ParseInLocation("2006-01-02", d, time.Local); err == nil {
			f.FlashedAt = t
		}
	}

	// The image is optional: knowing what is on a board is worth recording
	// even when the file is long gone.
	if file, header, err := r.FormFile("image"); err == nil {
		defer file.Close()
		blob, sum, size, err := a.firmware.Save(file, path.Ext(header.Filename))
		if errors.Is(err, errTooBig) {
			redirect(w, r, back, "", "that file is bigger than "+a.firmware.Limit())
			return
		}
		if err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		f.File, f.Blob, f.SHA256, f.Size = path.Base(header.Filename), blob, sum, size
	}

	id, err := a.store.AddFlash(f)
	if err != nil {
		redirect(w, r, back, "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "recorded firmware", "flash", id, f.Firmware)
	redirect(w, r, fmt.Sprintf("/firmware/%d", id), "Recorded "+strings.TrimSpace(f.Firmware), "")
}

func (a *App) handleDeleteFlash(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.store.DeleteFlash(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/firmware", "Removed that record", "")
}

func (a *App) handleFlashFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	f, err := a.store.GetFlash(id)
	if err != nil || !f.HasFile() {
		http.NotFound(w, r)
		return
	}
	a.serveBlob(w, r, a.firmware, f.Blob, f.File, "application/octet-stream")
}
