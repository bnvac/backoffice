package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SearchResult is one candidate part found by a provider, in the shape the
// add-item form needs to fill itself in.
type SearchResult struct {
	Source     string  `json:"source"`
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	ImageURL   string  `json:"image_url"`
	PartNumber string  `json:"part_number"`
	Price      float64 `json:"price"`
	Currency   string  `json:"currency"`
	Stock      string  `json:"stock"`
}

// SearchProvider looks up parts by name. Providers are expected to fail softly:
// a source being down or rate-limited must not break the others.
type SearchProvider interface {
	Name() string
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// SearchHub fans a query out to every configured provider.
type SearchHub struct {
	providers []SearchProvider
}

// NewSearchHub wires up every source that can actually be queried. Shops are
// configurable because which ones matter depends on where you live.
func NewSearchHub(f *Fetcher, cfg Config) *SearchHub {
	providers := []SearchProvider{NewAdafruitProvider(f)}
	for _, shop := range cfg.ShopifyShops {
		if shop = strings.TrimSpace(shop); shop != "" {
			providers = append(providers, NewShopifyProvider(f, shop))
		}
	}
	// Octopart only joins in when there are credentials for it, so an
	// unconfigured install is not nagged on every search.
	if nexar := NewNexarProvider(f, cfg.NexarID, cfg.NexarSecret, cfg.NexarToken); nexar.Configured() {
		providers = append(providers, nexar)
	}
	return &SearchHub{providers: providers}
}

// Nexar returns the Octopart provider when one is configured, for the
// part-detail lookup that search alone does not cover.
func (h *SearchHub) Nexar() *NexarProvider {
	if h == nil {
		return nil
	}
	for _, p := range h.providers {
		if n, ok := p.(*NexarProvider); ok {
			return n
		}
	}
	return nil
}

// Sources names what is being searched, for the UI.
func (h *SearchHub) Sources() []string {
	if h == nil {
		return nil
	}
	out := make([]string, 0, len(h.providers))
	for _, p := range h.providers {
		out = append(out, p.Name())
	}
	return out
}

// SearchReport carries results plus a note for each provider that failed, so
// the UI can say which source is unavailable instead of silently thinning out.
type SearchReport struct {
	Results []SearchResult `json:"results"`
	Notes   []string       `json:"notes,omitempty"`
}

func (h *SearchHub) Search(ctx context.Context, query string, limit int) SearchReport {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchReport{Notes: []string{"type something to search for"}}
	}

	type outcome struct {
		results []SearchResult
		note    string
	}
	out := make([]outcome, len(h.providers))

	var wg sync.WaitGroup
	for i, p := range h.providers {
		wg.Add(1)
		go func(i int, p SearchProvider) {
			defer wg.Done()
			res, err := p.Search(ctx, query, limit)
			if err != nil {
				out[i].note = fmt.Sprintf("%s: %v", p.Name(), err)
				return
			}
			out[i].results = res
		}(i, p)
	}
	wg.Wait()

	var report SearchReport
	for _, o := range out {
		if o.note != "" {
			report.Notes = append(report.Notes, o.note)
		}
	}
	// Interleave by rank so one shop with a big catalogue cannot crowd the
	// others out of the visible results.
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

// --- Adafruit ---------------------------------------------------------------

const (
	adafruitCatalogURL = "https://www.adafruit.com/api/products"
	adafruitCatalogMax = 32 << 20 // the whole catalogue is a single JSON array
	adafruitTTL        = 6 * time.Hour
)

// AdafruitProvider searches Adafruit's published product catalogue.
//
// Adafruit has no search endpoint, but it does publish its whole catalogue as
// one JSON document, so the catalogue is fetched once and searched locally.
// That makes every search after the first instant and costs the site one
// request every few hours instead of one per keystroke.
type AdafruitProvider struct {
	fetcher    *Fetcher
	catalogURL string // overridden in tests

	mu       sync.RWMutex
	products []adafruitProduct
	fetched  time.Time
	loading  sync.Mutex
}

// Every field in Adafruit's catalogue is a JSON string, prices and ids
// included, so these are strings here and converted after decoding.
type adafruitProduct struct {
	ID    string `json:"product_id"`
	Name  string `json:"product_name"`
	Price string `json:"product_price"`
	Image string `json:"product_image"`
	MPN   string `json:"product_mpn"`
	Stock string `json:"product_stock"`
	URL   string `json:"product_url"`
}

// stockLabel turns Adafruit's stock field into something readable. It holds
// either a phrase ("in stock") or a count, which can be negative when a line
// is oversold.
func stockLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return raw
	}
	if n > 0 {
		return fmt.Sprintf("%d in stock", n)
	}
	return "out of stock"
}

func (p adafruitProduct) inStock() bool {
	if n, err := strconv.Atoi(strings.TrimSpace(p.Stock)); err == nil {
		return n > 0
	}
	return strings.EqualFold(strings.TrimSpace(p.Stock), "in stock")
}

func NewAdafruitProvider(f *Fetcher) *AdafruitProvider {
	return &AdafruitProvider{fetcher: f, catalogURL: adafruitCatalogURL}
}

func (a *AdafruitProvider) Name() string { return "Adafruit" }

func (a *AdafruitProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	products, err := a.catalog(ctx)
	if err != nil {
		return nil, err
	}

	words := strings.Fields(strings.ToLower(query))
	type scored struct {
		p adafruitProduct
		s int
	}
	var hits []scored

	for _, p := range products {
		name := strings.ToLower(p.Name)
		mpn := strings.ToLower(p.MPN)

		score, ok := 0, true
		for _, w := range words {
			switch {
			case strings.Contains(name, w):
				// Earlier matches are more likely to be the real subject.
				score += 10
				if idx := strings.Index(name, w); idx < 20 {
					score += 5
				}
			case strings.Contains(mpn, w):
				score += 8
			default:
				ok = false
			}
		}
		if !ok {
			continue
		}
		// Prefer the concise listing over a bundle mentioning the same part.
		if len(p.Name) < 60 {
			score += 3
		}
		if p.inStock() {
			score += 2
		}
		hits = append(hits, scored{p, score})
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].s > hits[j].s })
	if len(hits) > limit {
		hits = hits[:limit]
	}

	out := make([]SearchResult, 0, len(hits))
	for _, h := range hits {
		price, _ := parsePrice(h.p.Price)
		link := h.p.URL
		if link == "" && h.p.ID != "" {
			link = "https://www.adafruit.com/product/" + h.p.ID
		}
		out = append(out, SearchResult{
			Source:     "Adafruit",
			Title:      tidy(h.p.Name, 200),
			URL:        link,
			ImageURL:   h.p.Image,
			PartNumber: h.p.MPN,
			Price:      price,
			Currency:   "USD",
			Stock:      stockLabel(h.p.Stock),
		})
	}
	return out, nil
}

// catalog returns the cached product list, refreshing it when stale. The
// loading mutex means a burst of searches triggers one download, not several.
func (a *AdafruitProvider) catalog(ctx context.Context) ([]adafruitProduct, error) {
	a.mu.RLock()
	fresh := time.Since(a.fetched) < adafruitTTL && len(a.products) > 0
	cached := a.products
	a.mu.RUnlock()
	if fresh {
		return cached, nil
	}

	a.loading.Lock()
	defer a.loading.Unlock()

	// Another request may have refreshed it while this one waited.
	a.mu.RLock()
	fresh = time.Since(a.fetched) < adafruitTTL && len(a.products) > 0
	cached = a.products
	a.mu.RUnlock()
	if fresh {
		return cached, nil
	}

	body, _, err := a.fetcher.get(ctx, a.catalogURL, "application/json", adafruitCatalogMax)
	if err != nil {
		// Serve a stale catalogue rather than nothing when the site is down.
		if len(cached) > 0 {
			return cached, nil
		}
		return nil, err
	}
	var products []adafruitProduct
	if err := json.Unmarshal(body, &products); err != nil {
		if len(cached) > 0 {
			return cached, nil
		}
		return nil, fmt.Errorf("could not read the product catalogue")
	}

	a.mu.Lock()
	a.products, a.fetched = products, time.Now()
	a.mu.Unlock()
	return products, nil
}

// --- sources that cannot be searched from a server --------------------------

// ExternalSource is a shop this app cannot query directly. Amazon serves bot
// interstitials to non-browser clients (their terms require the Product
// Advertising API, which needs an affiliate account), and AliExpress bounces
// server-side requests through redirects. Rather than pretend, the UI offers a
// link that opens the shop's own search in the browser, where those pages work
// normally -- the resulting URL can be pasted straight back into the importer.
type ExternalSource struct {
	Name      string
	SearchURL string
	Note      string
}

func ExternalSources(query string) []ExternalSource {
	q := url.QueryEscape(query)
	return []ExternalSource{
		{
			Name:      "Amazon",
			SearchURL: "https://www.amazon.com/s?k=" + q,
			Note:      "blocks automated lookups; opens in your browser",
		},
		{
			Name:      "AliExpress",
			SearchURL: "https://www.aliexpress.com/w/wholesale-" + q + ".html",
			Note:      "blocks automated lookups; opens in your browser",
		},
		{
			Name:      "Octopart",
			SearchURL: "https://octopart.com/search?q=" + q,
			Note:      "distributor stock and pricing",
		},
	}
}

// parsePrice reads a price a person typed, tolerating currency symbols and
// thousands separators.
func parsePrice(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else if r == ',' {
			// A comma is a decimal separator in much of the world, and a
			// thousands separator elsewhere; treat it as a decimal point only
			// when no dot is present.
			if !strings.Contains(s, ".") {
				b.WriteRune('.')
			}
		}
	}
	cleaned := b.String()
	if cleaned == "" {
		return 0, fmt.Errorf("%q is not a price", raw)
	}
	v, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a price", raw)
	}
	if v < 0 {
		return 0, fmt.Errorf("a price cannot be negative")
	}
	return v, nil
}
