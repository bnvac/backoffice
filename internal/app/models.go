package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Somebody has almost certainly already drawn a case for your part. This finds
// it, and -- when the geometry is available -- measures it, so the question
// "will this actually fit?" has an answer before you spend six hours printing
// the wrong thing.
//
// The sources here are the ones that can genuinely be queried from a server.
// Several big model sites cannot: they serve a bot challenge or a JavaScript
// shell to anything that is not a browser, so rather than pretending, those get
// a link that opens their own search in yours.

// ModelResult is one model found on a site.
type ModelResult struct {
	Source    string  `json:"source"`
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	ImageURL  string  `json:"image_url"`
	Author    string  `json:"author"`
	Licence   string  `json:"licence"`
	Downloads int     `json:"downloads"`
	Likes     int     `json:"likes"`
	Rating    float64 `json:"rating"`
}

// Stats is the popularity line under a result. A model with ten thousand
// downloads has been printed successfully by a lot of people, which is the only
// review signal these sites really offer.
func (r ModelResult) Stats() string {
	var bits []string
	if r.Rating > 0 {
		bits = append(bits, fmt.Sprintf("★ %.1f", r.Rating))
	}
	if r.Downloads > 0 {
		bits = append(bits, fmt.Sprintf("%s downloads", humanCount(r.Downloads)))
	}
	if r.Likes > 0 {
		bits = append(bits, fmt.Sprintf("%s likes", humanCount(r.Likes)))
	}
	return strings.Join(bits, " · ")
}

func humanCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

// ModelProvider searches one site. Like the part providers, a source being down
// must not break the others.
type ModelProvider interface {
	Name() string
	SearchModels(ctx context.Context, query string, limit int) ([]ModelResult, error)
}

// ModelHub fans a search out to whichever sites are usable.
type ModelHub struct {
	providers []ModelProvider
}

func NewModelHub(f *Fetcher, cfg Config) *ModelHub {
	h := &ModelHub{providers: []ModelProvider{NewPrintablesProvider(f)}}
	if t := strings.TrimSpace(cfg.ThingiverseToken); t != "" {
		h.providers = append(h.providers, NewThingiverseProvider(f, t))
	}
	return h
}

func (h *ModelHub) Sources() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.providers))
	for _, p := range h.providers {
		out = append(out, p.Name())
	}
	return out
}

// ModelReport carries results plus a note per source that failed.
type ModelReport struct {
	Results []ModelResult
	Notes   []string
}

func (h *ModelHub) Search(ctx context.Context, query string, limit int) ModelReport {
	query = strings.TrimSpace(query)
	if query == "" {
		return ModelReport{Notes: []string{"say what the case is for"}}
	}
	if h == nil || len(h.providers) == 0 {
		return ModelReport{Notes: []string{"no model sites are configured"}}
	}

	type outcome struct {
		results []ModelResult
		note    string
	}
	out := make([]outcome, len(h.providers))
	var wg sync.WaitGroup
	for i, p := range h.providers {
		wg.Add(1)
		go func(i int, p ModelProvider) {
			defer wg.Done()
			res, err := p.SearchModels(ctx, query, limit)
			if err != nil {
				out[i].note = fmt.Sprintf("%s: %v", p.Name(), err)
				return
			}
			out[i].results = res
		}(i, p)
	}
	wg.Wait()

	var report ModelReport
	for _, o := range out {
		if o.note != "" {
			report.Notes = append(report.Notes, o.note)
		}
	}
	// Interleave, so one site with a big catalogue cannot crowd out the other.
	for round := 0; ; round++ {
		added := false
		for _, o := range out {
			if round < len(o.results) {
				report.Results = append(report.Results, o.results[round])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return report
}

// --- Printables --------------------------------------------------------------

const (
	printablesAPI   = "https://api.printables.com/graphql/"
	printablesMedia = "https://media.printables.com/"
	printablesSite  = "https://www.printables.com/model/"
)

// PrintablesProvider searches Printables through the same GraphQL endpoint its
// own website uses. No key, no account.
type PrintablesProvider struct {
	fetcher  *Fetcher
	endpoint string // overridden in tests
}

func NewPrintablesProvider(f *Fetcher) *PrintablesProvider {
	return &PrintablesProvider{fetcher: f, endpoint: printablesAPI}
}

func (p *PrintablesProvider) Name() string { return "Printables" }

// The query asks for exactly the fields shown, and nothing else: a GraphQL
// endpoint that is not ours is not somewhere to go fishing.
const printablesQuery = `query Search($q: String!, $limit: Int!) {
	searchPrints2(query: $q, limit: $limit) {
		items {
			id name slug ratingAvg likesCount downloadCount nsfw
			image { filePath }
			user { publicUsername }
		}
	}
}`

type printablesResponse struct {
	Data struct {
		Search struct {
			Items []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Slug      string `json:"slug"`
				Rating    string `json:"ratingAvg"`
				Likes     int    `json:"likesCount"`
				Downloads int    `json:"downloadCount"`
				NSFW      bool   `json:"nsfw"`
				Image     *struct {
					FilePath string `json:"filePath"`
				} `json:"image"`
				User *struct {
					Username string `json:"publicUsername"`
				} `json:"user"`
			} `json:"items"`
		} `json:"searchPrints2"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (p *PrintablesProvider) SearchModels(ctx context.Context, query string, limit int) ([]ModelResult, error) {
	if limit <= 0 || limit > 30 {
		limit = 12
	}
	body, err := json.Marshal(map[string]any{
		"query":     printablesQuery,
		"variables": map[string]any{"q": query, "limit": limit},
	})
	if err != nil {
		return nil, err
	}
	// The endpoint checks the origin, so it is sent -- honestly, as ourselves.
	raw, err := p.fetcher.PostJSON(ctx, p.endpoint, body, map[string]string{
		"Origin": "https://www.printables.com",
	}, 1<<20)
	if err != nil {
		return nil, err
	}

	var res printablesResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("could not read the reply from Printables")
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("%s", res.Errors[0].Message)
	}

	out := make([]ModelResult, 0, len(res.Data.Search.Items))
	for _, item := range res.Data.Search.Items {
		if item.NSFW {
			continue
		}
		r := ModelResult{
			Source:    "Printables",
			Title:     tidy(item.Name, 140),
			URL:       printablesSite + item.ID + "-" + item.Slug,
			Downloads: item.Downloads,
			Likes:     item.Likes,
		}
		if item.Image != nil && item.Image.FilePath != "" {
			r.ImageURL = printablesMedia + strings.TrimPrefix(item.Image.FilePath, "/")
		}
		if item.User != nil {
			r.Author = tidy(item.User.Username, 60)
		}
		// The rating comes back as a string with far more precision than it
		// deserves.
		if v, err := strconv.ParseFloat(item.Rating, 64); err == nil {
			r.Rating = v
		}
		out = append(out, r)
	}
	return out, nil
}

// --- Thingiverse -------------------------------------------------------------

const thingiverseAPI = "https://api.thingiverse.com"

// ThingiverseProvider needs an app token, which is free but does mean
// registering. Without one it is not registered at all, so an unconfigured
// install is never nagged about it.
type ThingiverseProvider struct {
	fetcher  *Fetcher
	token    string
	endpoint string
}

func NewThingiverseProvider(f *Fetcher, token string) *ThingiverseProvider {
	return &ThingiverseProvider{fetcher: f, token: token, endpoint: thingiverseAPI}
}

func (t *ThingiverseProvider) Name() string { return "Thingiverse" }

func (t *ThingiverseProvider) SearchModels(ctx context.Context, query string, limit int) ([]ModelResult, error) {
	if limit <= 0 || limit > 30 {
		limit = 12
	}
	link := fmt.Sprintf("%s/search/%s/?type=things&per_page=%d",
		t.endpoint, url.PathEscape(query), limit)

	raw, _, err := t.fetcher.get(ctx, link, "application/json", 1<<20,
		header{"Authorization", "Bearer " + t.token})
	if err != nil {
		return nil, err
	}

	var res struct {
		Hits []struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			URL       string `json:"public_url"`
			Thumbnail string `json:"thumbnail"`
			Likes     int    `json:"like_count"`
			Downloads int    `json:"download_count"`
			IsNSFW    bool   `json:"is_nsfw"`
			Creator   *struct {
				Name string `json:"name"`
			} `json:"creator"`
			License string `json:"license"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("could not read the reply from Thingiverse")
	}

	out := make([]ModelResult, 0, len(res.Hits))
	for _, h := range res.Hits {
		if h.IsNSFW {
			continue
		}
		r := ModelResult{
			Source: "Thingiverse", Title: tidy(h.Name, 140), URL: h.URL,
			ImageURL: h.Thumbnail, Likes: h.Likes, Downloads: h.Downloads,
			Licence: tidy(h.License, 60),
		}
		if r.URL == "" {
			r.URL = fmt.Sprintf("https://www.thingiverse.com/thing:%d", h.ID)
		}
		if h.Creator != nil {
			r.Author = tidy(h.Creator.Name, 60)
		}
		out = append(out, r)
	}
	return out, nil
}

// --- the sites that cannot be searched from a server -------------------------

// ExternalModelSource is a model site this app cannot query. MakerWorld's API
// answers but returns nothing without a signed session, Thangs and Cults3D
// refuse non-browser clients outright, and Yeggi serves a bot challenge. Rather
// than pretend, the UI opens their own search in your browser -- where those
// pages work perfectly well, and the resulting link pastes straight back in.
type ExternalModelSource struct {
	Name      string
	SearchURL string
	Note      string
}

func ExternalModelSources(query string) []ExternalModelSource {
	q := url.QueryEscape(query)
	return []ExternalModelSource{
		{"MakerWorld", "https://makerworld.com/en/search/models?keyword=" + q,
			"needs a signed-in session; opens in your browser"},
		{"Thangs", "https://thangs.com/search/" + q + "?scope=all",
			"blocks automated lookups; searches across other sites too"},
		{"Cults3D", "https://cults3d.com/en/search?q=" + q,
			"blocks automated lookups"},
		{"GrabCAD", "https://grabcad.com/library?query=" + q,
			"mechanical CAD rather than prints; needs an account"},
	}
}

// --- stored models ------------------------------------------------------------

// Model is a model attached to an item.
type Model struct {
	ID         int64
	ItemID     int64
	Source     string
	Title      string
	URL        string
	Author     string
	Licence    string
	Downloads  int
	Likes      int
	Rating     float64
	Thumb      string // the site's render, stored
	Preview    string // our render of the real geometry, stored
	Dimensions string
	Triangles  int
	Note       string
	Position   int
	CreatedAt  time.Time
}

// Measured means an STL was read, so the numbers are the model's own rather
// than a description of it.
func (m Model) Measured() bool { return m.Dimensions != "" }

// TriangleText renders the mesh complexity the same way the counts elsewhere
// are rendered: 225706 is a number, 225.7k is a size.
func (m Model) TriangleText() string { return humanCount(m.Triangles) }

func (m Model) Stats() string {
	return ModelResult{Rating: m.Rating, Downloads: m.Downloads, Likes: m.Likes}.Stats()
}

// Image is whichever picture to show: our own render when there is one, since
// it is of the actual geometry, otherwise the site's.
func (m Model) Image() string {
	if m.Preview != "" {
		return m.Preview
	}
	return m.Thumb
}

func (s *Store) attachModels(items []Item, byID map[int64]int) error {
	return s.eachInChunks(itemIDs(byID), func(in string) string {
		return `SELECT id, item_id, source, title, url, author, licence,
			downloads, likes, rating, thumb, preview, dimensions, triangles, note, position, created_at
			FROM models WHERE item_id IN (` + in + `) ORDER BY item_id, position, id`
	}, func(rows *sql.Rows) error {
		var m Model
		var created string
		err := rows.Scan(&m.ID, &m.ItemID, &m.Source, &m.Title, &m.URL, &m.Author,
			&m.Licence, &m.Downloads, &m.Likes, &m.Rating, &m.Thumb, &m.Preview,
			&m.Dimensions, &m.Triangles, &m.Note, &m.Position, &created)
		if err != nil {
			return err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, created)
		items[byID[m.ItemID]].Models = append(items[byID[m.ItemID]].Models, m)
		return nil
	})
}

func (s *Store) AddModel(m Model) (int64, error) {
	var next int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(position)+1, 0) FROM models WHERE item_id = ?`,
		m.ItemID).Scan(&next)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO models
		(item_id, source, title, url, author, licence, downloads, likes, rating,
		 thumb, preview, dimensions, triangles, note, position, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ItemID, m.Source, m.Title, m.URL, m.Author, m.Licence, m.Downloads,
		m.Likes, m.Rating, m.Thumb, m.Preview, m.Dimensions, m.Triangles,
		m.Note, next, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetModel(id int64) (Model, error) {
	var m Model
	var created string
	err := s.db.QueryRow(`SELECT id, item_id, source, title, url, author, licence,
		downloads, likes, rating, thumb, preview, dimensions, triangles, note, position, created_at
		FROM models WHERE id = ?`, id).Scan(&m.ID, &m.ItemID, &m.Source, &m.Title,
		&m.URL, &m.Author, &m.Licence, &m.Downloads, &m.Likes, &m.Rating, &m.Thumb,
		&m.Preview, &m.Dimensions, &m.Triangles, &m.Note, &m.Position, &created)
	if err != nil {
		return m, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return m, nil
}

// SetModelPreview records a render made from the real geometry.
func (s *Store) SetModelPreview(id int64, preview, dimensions string, triangles int) error {
	_, err := s.db.Exec(`UPDATE models SET preview = ?, dimensions = ?, triangles = ? WHERE id = ?`,
		preview, dimensions, triangles, id)
	return err
}

// DeleteModel removes a model and reports its stored images so the caller can
// unlink them.
func (s *Store) DeleteModel(id int64) (itemID int64, files []string, err error) {
	m, err := s.GetModel(id)
	if err != nil {
		return 0, nil, err
	}
	for _, f := range []string{m.Thumb, m.Preview} {
		if f != "" {
			files = append(files, f)
		}
	}
	_, err = s.db.Exec(`DELETE FROM models WHERE id = ?`, id)
	return m.ItemID, files, err
}

// ModelSearchTerms suggests what to search for. A part number finds the exact
// case; the name finds the ones drawn for the family; and a bare "case" on the
// end is what people actually title these things.
func ModelSearchTerms(it Item) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[strings.ToLower(s)] {
			seen[strings.ToLower(s)] = true
			out = append(out, s)
		}
	}
	if it.PartNumber != "" {
		add(it.PartNumber + " case")
	}
	add(it.Name + " case")
	add(it.Name + " enclosure")
	add(it.Name + " mount")
	if it.PartNumber != "" {
		add(it.PartNumber)
	}
	return out
}
