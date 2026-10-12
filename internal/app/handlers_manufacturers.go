package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type manufacturersData struct {
	Makers  []Manufacturer
	Unknown int // items with nobody recorded as having made them
}

func (d manufacturersData) Items() int {
	n := 0
	for _, m := range d.Makers {
		n += m.Items
	}
	return n
}

func (d manufacturersData) WithLogos() int {
	n := 0
	for _, m := range d.Makers {
		if m.Logo != "" {
			n++
		}
	}
	return n
}

func (a *App) handleManufacturers(w http.ResponseWriter, r *http.Request) {
	makers, err := a.store.Manufacturers()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	var unknown int
	if err := a.store.db.QueryRow(`SELECT COUNT(*) FROM items WHERE manufacturer = ''`).Scan(&unknown); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.render(w, r, "manufacturers.html", "Manufacturers",
		manufacturersData{Makers: makers, Unknown: unknown})
}

// handleFetchLogo goes and gets the maker's mark. It is a deliberate action
// rather than something that happens while you are typing a part in: it reaches
// out to the internet, and doing that silently on every save would be rude.
func (a *App) handleFetchLogo(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	name := CanonicalManufacturer(r.FormValue("name"))
	if name == "" {
		redirect(w, r, "/manufacturers", "", "which manufacturer?")
		return
	}
	dest := "/manufacturers"

	m, err := a.store.GetManufacturer(name)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	if domain := strings.TrimSpace(r.FormValue("domain")); domain != "" {
		m.Domain = strings.TrimPrefix(strings.ToLower(hostOrDomain(domain)), "www.")
	}
	if m.Domain == "" {
		m.Domain = DomainForManufacturer(name)
	}
	if notes := strings.TrimSpace(r.FormValue("notes")); notes != "" {
		m.Notes = notes
	}

	// An uploaded file always wins: it is the only way to get a logo for a
	// maker with no website, and the only way to overrule a bad guess.
	if r.MultipartForm != nil && len(r.MultipartForm.File["logo"]) > 0 {
		fh := r.MultipartForm.File["logo"][0]
		f, err := fh.Open()
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		stored, err := a.photos.Save(f, fh.Filename)
		f.Close()
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		a.replaceLogo(&m, stored)
		if err := a.saveMaker(m); err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		redirect(w, r, dest, "Logo set for "+m.Name, "")
		return
	}

	logo, err := a.FetchLogo(r.Context(), m)
	if err != nil {
		// The domain is still worth keeping even when the logo could not be
		// fetched: it makes the name clickable, and the fetch can be retried.
		if m.Domain != "" || m.Notes != "" {
			if serr := a.saveMaker(m); serr != nil {
				a.fail(w, serr, http.StatusInternalServerError)
				return
			}
		}
		redirect(w, r, dest, "", err.Error())
		return
	}
	a.replaceLogo(&m, logo)
	if err := a.saveMaker(m); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "fetched a logo", "manufacturer", 0, m.Name)
	redirect(w, r, dest, "Logo found for "+m.Name+" at "+m.Domain, "")
}

// replaceLogo swaps in a new logo and unlinks the old one, so repeated fetches
// do not leave a trail of orphaned images.
func (a *App) replaceLogo(m *Manufacturer, stored string) {
	if m.Logo != "" && m.Logo != stored {
		a.photos.Remove(m.Logo)
	}
	m.Logo = stored
}

// hostOrDomain accepts either a bare domain or a full URL, since people paste
// whichever is in their clipboard.
func hostOrDomain(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") || strings.Contains(raw, "/") {
		if h := hostOf(raw); h != "that link" {
			return h
		}
	}
	return raw
}

func (a *App) handleClearLogo(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	name := CanonicalManufacturer(r.FormValue("name"))
	m, err := a.store.GetManufacturer(name)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	if m.Logo != "" {
		a.photos.Remove(m.Logo)
		m.Logo = ""
	}
	if err := a.saveMaker(m); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/manufacturers", "Logo removed from "+m.Name, "")
}

// handleMergeManufacturer folds one spelling of a maker into another.
func (a *App) handleMergeManufacturer(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	from := strings.TrimSpace(r.FormValue("from"))
	to := strings.TrimSpace(r.FormValue("to"))
	if from == "" || to == "" {
		redirect(w, r, "/manufacturers", "", "give both names")
		return
	}
	n, err := a.renameMaker(from, to)
	if err != nil {
		redirect(w, r, "/manufacturers", "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "merged manufacturers", "manufacturer", 0, from+" → "+to)
	redirect(w, r, "/manufacturers",
		fmt.Sprintf("Moved %s from %s to %s", plural(int(n), "item"), from, CanonicalManufacturer(to)), "")
}

// fetchLogoFor picks up a logo for a maker nobody has set up yet.
//
// It runs in the background, on its own context, for two reasons: saving a part
// must not sit waiting on somebody else's web server, and the request's context
// is cancelled the moment the response is written, which would abort the fetch
// halfway. It also only runs for makers whose website is already known, so an
// ordinary save never turns into a search of the internet.
func (a *App) fetchLogoFor(name string) {
	if !a.autoLogos {
		return
	}
	name = CanonicalManufacturer(name)
	if name == "" {
		return
	}
	m, err := a.store.GetManufacturer(name)
	if err != nil || m.Logo != "" {
		return
	}
	if m.Domain == "" {
		if m.Domain = DomainForManufacturer(name); m.Domain == "" {
			return // unknown maker: wait to be told where it lives
		}
	}
	if !a.claimFetch(name) {
		return // somebody is already fetching this one
	}
	go func() {
		defer a.releaseFetch(name)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		logo, err := a.FetchLogo(ctx, m)
		if err != nil {
			log.Printf("logo for %s: %v", m.Name, err)
			return
		}
		// Somebody may have uploaded one by hand while this was in flight. A
		// hand-picked logo outranks a guess, so the download is discarded
		// rather than left orphaned on disk.
		if current, err := a.store.GetManufacturer(m.Name); err == nil && current.Logo != "" {
			a.photos.Remove(logo)
			return
		}
		m.Logo = logo
		if err := a.saveMaker(m); err != nil {
			log.Printf("logo for %s: %v", m.Name, err)
		}
	}()
}

// saveMaker and renameMaker are the write paths every handler goes through, so
// that the logo cache is dropped in one place rather than at each call site --
// a cache invalidated in six places is a cache that eventually is not.
func (a *App) saveMaker(m Manufacturer) error {
	err := a.store.SaveManufacturer(m)
	a.forgetLogos()
	return err
}

func (a *App) renameMaker(from, to string) (int64, error) {
	n, err := a.store.RenameManufacturer(from, to)
	a.forgetLogos()
	return n, err
}

// logoFor gives a template the maker's stored logo, or "" for a maker without
// one -- which is the signal to draw the monogram instead.
//
// Every card in a grid asks, so this reads a cache rather than the database.
// The cache is dropped whenever a logo changes, which is rare and deliberate:
// somebody uploading a file, clearing one, or a background fetch landing.
func (a *App) logoFor(name string) string {
	if name == "" {
		return ""
	}
	a.logoMu.RLock()
	logos, fresh := a.logos, a.logosFresh
	a.logoMu.RUnlock()

	if !fresh {
		loaded, err := a.store.MakerLogos()
		if err != nil {
			return "" // a missing logo is a monogram, not an error page
		}
		a.logoMu.Lock()
		a.logos, a.logosFresh = loaded, true
		a.logoMu.Unlock()
		logos = loaded
	}
	return logos[strings.ToLower(name)]
}

// forgetLogos drops the cache, so the next page picks up the change.
func (a *App) forgetLogos() {
	a.logoMu.Lock()
	a.logosFresh = false
	a.logoMu.Unlock()
}

// claimFetch reserves a maker for one background fetch, and reports whether the
// caller got it. Saving several parts from the same maker in a row otherwise
// starts a download per part, and all but the last is wasted.
func (a *App) claimFetch(name string) bool {
	a.fetchLock.Lock()
	defer a.fetchLock.Unlock()
	if a.fetching[name] {
		return false
	}
	if a.fetching == nil {
		a.fetching = map[string]bool{}
	}
	a.fetching[name] = true
	return true
}

func (a *App) releaseFetch(name string) {
	a.fetchLock.Lock()
	defer a.fetchLock.Unlock()
	delete(a.fetching, name)
}
