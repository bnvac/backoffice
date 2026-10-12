package app

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Finding the paperwork for a part is the chore this is meant to remove: the
// datasheet is on the manufacturer's site, the pinout is halfway down a shop
// listing as an image nobody linked, and the manual is a PDF with a name like
// "final_v3_REAL.pdf". Rather than asking someone to go and get all that, the
// app goes looking with what it already knows -- the product link, the part
// number, and the shops it can search -- and attaches what it finds.

// DocHunt is what one search turned up, in the shape the item page needs.
type DocHunt struct {
	Found    []PageDocument
	Pinouts  int // images downloaded and now shown on the page
	Links    int // datasheets and manuals kept as links
	Photo    bool
	Searched []string // where it looked, so an empty result is explicable
	Notes    []string // why a source gave nothing
}

// Summary is the single line the flash message shows.
func (h DocHunt) Summary() string {
	var bits []string
	if h.Pinouts > 0 {
		bits = append(bits, plural(h.Pinouts, "pinout"))
	}
	if h.Links > 0 {
		bits = append(bits, plural(h.Links, "document"))
	}
	if h.Photo {
		bits = append(bits, "a photo")
	}
	if len(bits) == 0 {
		return ""
	}
	return "Found " + strings.Join(bits, " and ")
}

const (
	maxDocPages  = 4 // product pages to open per hunt
	maxDocLeads  = 4 // documentation pages to follow behind those
	maxHuntPages = 9 // hard ceiling on fetches, so a hunt always terminates
	maxDocsKept  = 8
)

// HuntDocumentation looks for datasheets, pinouts and manuals for one item and
// attaches whatever it finds. Images are downloaded so they show on the page
// and survive the source going away; PDFs stay links, because a datasheet is
// better fetched fresh than kept as a stale copy.
//
// Nothing here overwrites: a document already attached is skipped, so running
// the hunt twice is safe and cheap.
func (a *App) HuntDocumentation(ctx context.Context, it Item) DocHunt {
	var hunt DocHunt

	// What is already attached, so nothing is added twice.
	have := map[string]bool{}
	for _, r := range it.Refs {
		if r.URL != "" {
			have[canonicalDocURL(r.URL)] = true
		}
	}

	var candidates []PageDocument
	var leads []PageDocument
	var photo string

	// Every page is fetched through here so the whole hunt is bounded, and so
	// the same page is never opened twice however many links point at it.
	visited := map[string]bool{}
	fetches := 0
	open := func(link, source string) *PageMeta {
		key := canonicalDocURL(link)
		if link == "" || visited[key] || fetches >= maxHuntPages {
			return nil
		}
		visited[key] = true
		fetches++
		meta, err := a.fetcher.FetchPage(ctx, link)
		if err != nil {
			hunt.Notes = append(hunt.Notes, fmt.Sprintf("%s: %v", source, err))
			return nil
		}
		candidates = append(candidates, meta.Documents...)
		leads = append(leads, meta.Leads...)
		if photo == "" && meta.ImageURL != "" {
			photo = meta.ImageURL
		}
		return meta
	}

	// 1. The item's own link is the best source there is: somebody already
	//    decided that page was about this exact part.
	if it.Link != "" {
		hunt.Searched = append(hunt.Searched, hostOf(it.Link))
		open(it.Link, hostOf(it.Link))
	}

	// 2. The shops that can be searched. A product page found by part number is
	//    trustworthy; one found by a fuzzy name match is not, so only pages
	//    whose part number really matches are opened when we have one.
	query := strings.TrimSpace(it.PartNumber)
	byPartNumber := query != ""
	if !byPartNumber {
		query = it.Name
	}
	if query != "" {
		report := a.search.Search(ctx, query, 6)
		hunt.Notes = append(hunt.Notes, report.Notes...)
		opened := 0
		for _, res := range report.Results {
			if opened >= maxDocPages {
				break
			}
			// A page is only worth opening if it is really about this part.
			// The shop's own part number is the strongest signal, but plenty of
			// shops do not publish one, so a title that names the part counts
			// too -- and nothing else does.
			if byPartNumber &&
				!strings.EqualFold(strings.TrimSpace(res.PartNumber), it.PartNumber) &&
				!strings.Contains(strings.ToLower(res.Title), strings.ToLower(it.PartNumber)) {
				continue
			}
			if res.URL == "" || strings.EqualFold(res.URL, it.Link) {
				continue
			}
			hunt.Searched = append(hunt.Searched, res.Source)
			opened++
			open(res.URL, res.Source)
			if photo == "" && res.ImageURL != "" {
				photo = res.ImageURL
			}
		}
	}

	// 2b. Follow the pages that said they hold documentation without being
	//     documents. This is where the good stuff usually is: a product page
	//     links a "learn guide", and the pinout diagrams are a hop behind that,
	//     which is exactly the hunt people do by hand.
	//
	//     The loop walks by index rather than ranging, because following a lead
	//     turns up further leads -- a product page names a guide, the guide has
	//     a pinouts page, and the pinouts page is where the diagrams are. A
	//     range would have frozen the list before any of that was discovered.
	followed := 0
	for i := 0; i < len(leads) && followed < maxDocLeads; i++ {
		lead := leads[i]
		if visited[canonicalDocURL(lead.URL)] {
			continue
		}
		followed++
		hunt.Searched = append(hunt.Searched, hostOf(lead.URL))
		open(lead.URL, hostOf(lead.URL))
	}

	// 3. Octopart, when it is configured: the one source that is actually an
	//    index of datasheets rather than a shop that happens to link some.
	if nexar := a.search.Nexar(); nexar != nil && it.PartNumber != "" {
		hunt.Searched = append(hunt.Searched, "Octopart")
		detail, err := nexar.Lookup(ctx, it.PartNumber)
		switch {
		case err != nil:
			hunt.Notes = append(hunt.Notes, "Octopart: "+err.Error())
		case detail.Datasheet != nil:
			candidates = append(candidates, *detail.Datasheet)
		}
	}

	// Rank what was found: a pinout is the thing you actually wanted, then the
	// datasheet, then everything else.
	sort.SliceStable(candidates, func(i, j int) bool {
		return docRank(candidates[i].Kind) < docRank(candidates[j].Kind)
	})

	kept := 0
	for _, doc := range candidates {
		if kept >= maxDocsKept || doc.URL == "" {
			continue
		}
		key := canonicalDocURL(doc.URL)
		if have[key] {
			continue
		}
		have[key] = true
		if a.attachDocument(ctx, it.ID, doc) {
			kept++
			hunt.Found = append(hunt.Found, doc)
			if doc.Kind == "pinout" || doc.Kind == "schematic" {
				hunt.Pinouts++
			} else {
				hunt.Links++
			}
		}
	}

	// A part with no photo gets one, since the hunt has already been to the
	// page that has it.
	if photo != "" && len(it.Photos) == 0 {
		if msg := a.savePhotoFromURL(ctx, it.ID, photo); msg == "" {
			hunt.Photo = true
		}
	}
	hunt.Searched = dedupe(hunt.Searched)
	hunt.Notes = dedupe(hunt.Notes)
	return hunt
}

func docRank(kind string) int {
	switch kind {
	case "pinout":
		return 0
	case "datasheet":
		return 1
	case "schematic":
		return 2
	case "manual":
		return 3
	}
	return 4
}

// attachDocument stores one discovered reference. A pinout or schematic is
// downloaded so it renders on the page; anything else is kept as a link.
func (a *App) attachDocument(ctx context.Context, itemID int64, doc PageDocument) bool {
	ref := Reference{ItemID: itemID, Kind: doc.Kind, Title: doc.Title, URL: doc.URL}
	if ref.Title == "" {
		ref.Title = strings.Title(doc.Kind) //nolint:staticcheck // ASCII kind names
	}
	if doc.Kind == "pinout" || doc.Kind == "schematic" {
		if body, err := a.fetcher.FetchImage(ctx, doc.URL); err == nil {
			if name, err := a.photos.Save(bytes.NewReader(body), lastPathSegment(doc.URL)); err == nil {
				ref.Filename = name
			}
		}
		// A pinout that is a PDF rather than an image still earns its place --
		// it is just a link rather than a picture.
	}
	if _, err := a.store.AddReference(ref); err != nil {
		if ref.Filename != "" {
			a.photos.Remove(ref.Filename)
		}
		return false
	}
	return true
}

// canonicalDocURL strips the query junk shops add, so the same PDF linked twice
// with different tracking parameters is recognised as one document.
func canonicalDocURL(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, "/")
}

// hostOf names a URL's site for the "where it looked" list.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "that link"
	}
	return strings.TrimPrefix(u.Host, "www.")
}
