package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// handleImportPreview backs the "paste a link" box on the add/edit form. It
// fetches the page server-side and returns what it found as JSON, so the person
// can see and correct every field before anything is saved.
func (a *App) handleImportPreview(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	meta, err := a.fetcher.FetchPage(r.Context(), r.FormValue("url"))
	if err != nil {
		// A bad or unreachable URL is the person's problem to fix, not a server
		// fault, so this stays a 400 with the reason shown in the form.
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"name":        meta.Title,
		"notes":       meta.Description,
		"part_number": meta.PartNumber,
		"link":        meta.URL,
		"image_url":   meta.ImageURL,
		"site":        meta.SiteName,
		"price":       meta.Price,
		"currency":    meta.Currency,
	})
}

// handlePhotoFromURL attaches an image by URL to an existing item, for the case
// where you already have the item and just want a picture on it.
func (a *App) handlePhotoFromURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	dest := fmt.Sprintf("/items/%d", id)
	raw := strings.TrimSpace(r.FormValue("image_url"))
	if raw == "" {
		redirect(w, r, dest, "", "paste an image or page URL first")
		return
	}
	if msg := a.savePhotoFromURL(r.Context(), id, raw); msg != "" {
		redirect(w, r, dest, "", msg)
		return
	}
	redirect(w, r, dest, "Photo added from the web", "")
}

// savePhotoFromURL downloads one image and attaches it, returning a message on
// failure and "" on success. A URL that turns out to be a web page rather than
// an image is retried against that page's preview image, which is what happens
// when someone pastes a product listing instead of a direct image link.
func (a *App) savePhotoFromURL(ctx context.Context, itemID int64, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	body, err := a.fetcher.FetchImage(ctx, raw)
	if err != nil {
		return err.Error()
	}

	name, saveErr := a.photos.Save(bytes.NewReader(body), lastPathSegment(raw))
	if saveErr != nil {
		meta, pageErr := a.fetcher.FetchPage(ctx, raw)
		if pageErr != nil || meta.ImageURL == "" {
			return saveErr.Error()
		}
		if body, err = a.fetcher.FetchImage(ctx, meta.ImageURL); err != nil {
			return err.Error()
		}
		if name, err = a.photos.Save(bytes.NewReader(body), lastPathSegment(meta.ImageURL)); err != nil {
			return err.Error()
		}
	}

	if err := a.store.AddPhoto(itemID, name); err != nil {
		a.photos.Remove(name)
		return err.Error()
	}
	return ""
}

// lastPathSegment gives the fetched file a recognisable name for error messages.
func lastPathSegment(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimSuffix(raw, "/")
	if i := strings.LastIndex(raw, "/"); i >= 0 && i+1 < len(raw) {
		return raw[i+1:]
	}
	if raw == "" {
		return "that URL"
	}
	return raw
}

func writeJSONError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
