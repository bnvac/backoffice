package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// ShopifyProvider searches any Shopify storefront.
//
// Shopify ships a public search endpoint that every store built on it exposes:
// /search/suggest.json. It answers one request with titles, prices, images and
// availability -- no key, no account, no scraping of rendered HTML that breaks
// when the theme changes.
//
// That covers a lot of the maker world (The Pi Hut, Pimoroni, SB Components and
// plenty more), which is the practical answer to shops like Amazon and
// AliExpress being closed: rather than fight those two, search the shops that
// publish an interface, and let people add their own.
type ShopifyProvider struct {
	fetcher *Fetcher
	shop    string // hostname, e.g. "thepihut.com"
	label   string // what to call it in the UI
	scheme  string // overridden in tests
}

// defaultShopifyShops are searched out of the box. SHOPIFY_SHOPS replaces this
// list, so anyone can point Backoffice at the shops they actually buy from.
var defaultShopifyShops = []string{"thepihut.com", "shop.pimoroni.com"}

func NewShopifyProvider(f *Fetcher, shop string) *ShopifyProvider {
	return &ShopifyProvider{fetcher: f, shop: shop, label: shopLabel(shop), scheme: "https"}
}

// shopLabel turns a hostname into something worth showing next to a price.
func shopLabel(host string) string {
	h := strings.TrimPrefix(strings.ToLower(host), "www.")
	h = strings.TrimPrefix(h, "shop.")
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	switch h {
	case "thepihut":
		return "The Pi Hut"
	case "pimoroni":
		return "Pimoroni"
	}
	if h == "" {
		return host
	}
	return strings.ToUpper(h[:1]) + h[1:]
}

func (s *ShopifyProvider) Name() string { return s.label }

// shopifySuggest mirrors the shape of /search/suggest.json.
type shopifySuggest struct {
	Resources struct {
		Results struct {
			Products []struct {
				Title     string `json:"title"`
				URL       string `json:"url"`
				Image     string `json:"image"`
				Price     string `json:"price"`
				Vendor    string `json:"vendor"`
				Available bool   `json:"available"`
			} `json:"products"`
		} `json:"results"`
	} `json:"resources"`
}

func (s *ShopifyProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 || limit > 10 {
		limit = 10 // the endpoint caps suggestions low; asking for more is wasted
	}
	endpoint := fmt.Sprintf("%s://%s/search/suggest.json?%s", s.scheme, s.shop, url.Values{
		"q":                {query},
		"resources[type]":  {"product"},
		"resources[limit]": {fmt.Sprint(limit)},
	}.Encode())

	body, _, err := s.fetcher.get(ctx, endpoint, "application/json", maxPageBytes)
	if err != nil {
		return nil, err
	}
	var payload shopifySuggest
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("unexpected response from %s", s.shop)
	}

	products := payload.Resources.Results.Products
	out := make([]SearchResult, 0, len(products))
	for _, p := range products {
		price, _ := parsePrice(p.Price)

		// Product URLs are site-relative and carry search-tracking parameters
		// that would be meaningless once saved.
		link := p.URL
		if strings.HasPrefix(link, "/") {
			link = s.scheme + "://" + s.shop + link
		}
		if u, err := url.Parse(link); err == nil {
			u.RawQuery = ""
			link = u.String()
		}

		stock := "out of stock"
		if p.Available {
			stock = "in stock"
		}
		out = append(out, SearchResult{
			Source:     s.label,
			Title:      tidy(p.Title, 200),
			URL:        link,
			ImageURL:   absoluteImage(p.Image, s.scheme),
			PartNumber: "", // suggestions do not carry the SKU
			Price:      price,
			Currency:   shopCurrency(s.shop),
			Stock:      stock,
		})
	}
	return out, nil
}

// absoluteImage fixes the protocol-relative URLs Shopify sometimes returns.
func absoluteImage(src, scheme string) string {
	if strings.HasPrefix(src, "//") {
		return scheme + ":" + src
	}
	return src
}

// shopCurrency is a best guess from the shop's country, since the suggestion
// endpoint returns a bare number. It is only a label -- the figure is whatever
// the shop quoted -- and it is editable once saved.
func shopCurrency(host string) string {
	switch {
	case strings.HasSuffix(host, ".co.uk"), host == "thepihut.com", host == "shop.pimoroni.com":
		return "GBP"
	case strings.HasSuffix(host, ".de"), strings.HasSuffix(host, ".fr"), strings.HasSuffix(host, ".nl"):
		return "EUR"
	default:
		return "USD"
	}
}
