package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// NexarProvider searches Octopart through Nexar's GraphQL API.
//
// Octopart is the one source here that is about parts rather than products: it
// answers with the manufacturer part number, distributor stock and pricing,
// factory lead times, specifications and a datasheet. That maps onto everything
// this app already models, so an Octopart lookup can fill in an item almost
// completely.
//
// Authentication is OAuth client credentials. A raw access token can be pasted
// in for a quick try, but tokens last a day; client id and secret let the app
// mint its own and keep working.
type NexarProvider struct {
	fetcher *Fetcher

	clientID     string
	clientSecret string
	staticToken  string

	endpoint string // overridden in tests
	tokenURL string

	mu        sync.Mutex
	token     string
	tokenTill time.Time
}

const (
	nexarEndpoint = "https://api.nexar.com/graphql"
	nexarTokenURL = "https://identity.nexar.com/connect/token"
	nexarMaxBytes = 8 << 20
)

func NewNexarProvider(f *Fetcher, clientID, clientSecret, token string) *NexarProvider {
	return &NexarProvider{
		fetcher:      f,
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		staticToken:  strings.TrimSpace(token),
		endpoint:     nexarEndpoint,
		tokenURL:     nexarTokenURL,
	}
}

// base64URLDecode tolerates the unpadded encoding JWTs use.
func base64URLDecode(s string) ([]byte, error) {
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(s)
}

func (n *NexarProvider) Name() string { return "Octopart" }

// Configured reports whether there is any credential to try.
func (n *NexarProvider) Configured() bool {
	return n.staticToken != "" || (n.clientID != "" && n.clientSecret != "")
}

// accessToken returns a usable bearer token, minting a fresh one when the
// cached one is close to expiring.
func (n *NexarProvider) accessToken(ctx context.Context) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.clientID == "" || n.clientSecret == "" {
		if n.staticToken == "" {
			return "", fmt.Errorf("no Nexar credentials configured")
		}
		// A pasted token cannot be renewed; say so plainly once it lapses
		// rather than letting the API return a bare 401.
		if exp, ok := jwtExpiry(n.staticToken); ok && time.Now().After(exp) {
			return "", fmt.Errorf("the pasted NEXAR_TOKEN expired at %s — set NEXAR_CLIENT_ID and "+
				"NEXAR_CLIENT_SECRET so tokens renew themselves", exp.Format(time.RFC3339))
		}
		return n.staticToken, nil
	}

	// Refresh a minute early so a request cannot start with a token that
	// expires mid-flight.
	if n.token != "" && time.Now().Before(n.tokenTill.Add(-time.Minute)) {
		return n.token, nil
	}

	body, err := n.fetcher.PostForm(ctx, n.tokenURL, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {n.clientID},
		"client_secret": {n.clientSecret},
		"scope":         {"supply.domain"},
	}, 1<<20)
	if err != nil {
		return "", fmt.Errorf("could not get a Nexar token: %w", err)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("unexpected response from the Nexar token endpoint")
	}
	if payload.Error != "" {
		return "", fmt.Errorf("Nexar rejected the credentials: %s", payload.Error)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("Nexar returned no access token")
	}

	n.token = payload.AccessToken
	n.tokenTill = time.Now().Add(time.Duration(max(payload.ExpiresIn, 60)) * time.Second)
	return n.token, nil
}

// jwtExpiry reads the exp claim without verifying the signature -- this is only
// used to give a better error message, never to make a trust decision.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

const nexarSearchQuery = `query Search($q: String!, $limit: Int!) {
  supSearchMpn(q: $q, limit: $limit) {
    results {
      part {
        mpn
        manufacturer { name }
        shortDescription
        octopartUrl
        bestImage { url }
        bestDatasheet { name url }
        medianPrice1000 { price currency }
        specs { attribute { name } displayValue }
        sellers(authorizedOnly: false) {
          company { name }
          offers {
            clickUrl
            inventoryLevel
            factoryLeadDays
            prices { quantity price currency }
          }
        }
      }
    }
  }
}`

// nexarPart mirrors the slice of the schema this app uses.
type nexarPart struct {
	MPN              string                      `json:"mpn"`
	Manufacturer     struct{ Name string }       `json:"manufacturer"`
	ShortDescription string                      `json:"shortDescription"`
	OctopartURL      string                      `json:"octopartUrl"`
	BestImage        *struct{ URL string }       `json:"bestImage"`
	BestDatasheet    *struct{ Name, URL string } `json:"bestDatasheet"`
	MedianPrice1000  *struct {
		Price    float64 `json:"price"`
		Currency string  `json:"currency"`
	} `json:"medianPrice1000"`
	Specs []struct {
		Attribute    struct{ Name string } `json:"attribute"`
		DisplayValue string                `json:"displayValue"`
	} `json:"specs"`
	Sellers []struct {
		Company struct{ Name string } `json:"company"`
		Offers  []struct {
			ClickURL        string `json:"clickUrl"`
			InventoryLevel  int    `json:"inventoryLevel"`
			FactoryLeadDays *int   `json:"factoryLeadDays"`
			Prices          []struct {
				Quantity int     `json:"quantity"`
				Price    float64 `json:"price"`
				Currency string  `json:"currency"`
			} `json:"prices"`
		} `json:"offers"`
	} `json:"sellers"`
}

// query runs a GraphQL document and surfaces the API's own error text, which
// is where quota and plan problems appear.
func (n *NexarProvider) query(ctx context.Context, doc string, vars map[string]any, out any) error {
	token, err := n.accessToken(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"query": doc, "variables": vars})
	if err != nil {
		return err
	}

	body, err := n.fetcher.PostJSON(ctx, n.endpoint, payload,
		map[string]string{"Authorization": "Bearer " + token}, nexarMaxBytes)
	if err != nil {
		return err
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("unexpected response from Octopart")
	}
	if len(envelope.Errors) > 0 {
		// "You have exceeded your part limit of N" arrives here on a 200, so
		// it has to be passed through rather than treated as a transport error.
		return fmt.Errorf("%s", envelope.Errors[0].Message)
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("Octopart returned no data")
	}
	return json.Unmarshal(envelope.Data, out)
}

func (n *NexarProvider) searchParts(ctx context.Context, q string, limit int) ([]nexarPart, error) {
	var data struct {
		SupSearchMpn struct {
			Results []struct {
				Part nexarPart `json:"part"`
			} `json:"results"`
		} `json:"supSearchMpn"`
	}
	if err := n.query(ctx, nexarSearchQuery, map[string]any{"q": q, "limit": limit}, &data); err != nil {
		return nil, err
	}
	parts := make([]nexarPart, 0, len(data.SupSearchMpn.Results))
	for _, r := range data.SupSearchMpn.Results {
		parts = append(parts, r.Part)
	}
	return parts, nil
}

func (n *NexarProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if !n.Configured() {
		return nil, nil // not set up: stay quiet rather than nagging on every search
	}
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	parts, err := n.searchParts(ctx, query, limit)
	if err != nil {
		return nil, err
	}

	out := make([]SearchResult, 0, len(parts))
	for _, p := range parts {
		best := bestOffer(p)
		res := SearchResult{
			Source:     "Octopart",
			Title:      tidy(partTitle(p), 200),
			URL:        p.OctopartURL,
			PartNumber: p.MPN,
			Currency:   "USD",
		}
		if p.BestImage != nil {
			res.ImageURL = p.BestImage.URL
		}
		if best != nil {
			res.Price, res.Currency = best.Price, best.Currency
			res.Stock = fmt.Sprintf("%d at %s", best.Stock, best.Seller)
			if res.URL == "" {
				res.URL = best.URL
			}
		} else if p.MedianPrice1000 != nil {
			res.Price, res.Currency = p.MedianPrice1000.Price, p.MedianPrice1000.Currency
			res.Stock = "median of 1000"
		}
		out = append(out, res)
	}
	return out, nil
}

func partTitle(p nexarPart) string {
	name := p.MPN
	if p.Manufacturer.Name != "" {
		name = p.Manufacturer.Name + " " + name
	}
	if p.ShortDescription != "" {
		name += " — " + p.ShortDescription
	}
	return name
}

// Offer is one distributor's terms for a part.
type Offer struct {
	Seller   string
	Price    float64
	Currency string
	Stock    int
	LeadDays int
	URL      string
}

// bestOffer picks the cheapest single-unit price from a distributor that has
// stock, which is what someone buying one of something actually wants.
func bestOffer(p nexarPart) *Offer {
	offers := allOffers(p)
	if len(offers) == 0 {
		return nil
	}
	return &offers[0]
}

// allOffers flattens the seller/offer/price-break tree into one comparable list,
// cheapest first, preferring distributors that have stock.
func allOffers(p nexarPart) []Offer {
	var out []Offer
	for _, s := range p.Sellers {
		for _, o := range s.Offers {
			// Price breaks are quantity tiers; the single-unit price is the
			// smallest quantity quoted.
			var unit *struct {
				Quantity int     `json:"quantity"`
				Price    float64 `json:"price"`
				Currency string  `json:"currency"`
			}
			for i := range o.Prices {
				if unit == nil || o.Prices[i].Quantity < unit.Quantity {
					unit = &o.Prices[i]
				}
			}
			if unit == nil || unit.Price <= 0 {
				continue
			}
			lead := 0
			if o.FactoryLeadDays != nil {
				lead = *o.FactoryLeadDays
			}
			out = append(out, Offer{
				Seller: s.Company.Name, Price: unit.Price, Currency: unit.Currency,
				Stock: o.InventoryLevel, LeadDays: lead, URL: o.ClickURL,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		// In stock beats out of stock; then cheapest wins.
		if (out[i].Stock > 0) != (out[j].Stock > 0) {
			return out[i].Stock > 0
		}
		return out[i].Price < out[j].Price
	})
	return out
}

// PartDetail is everything Octopart knows about one part, in the shapes this
// app stores.
type PartDetail struct {
	MPN          string
	Manufacturer string
	Description  string
	ImageURL     string
	OctopartURL  string
	Datasheet    *PageDocument
	Specs        []Spec
	Offers       []Offer
}

// Lookup fetches the full picture for one part number, for the "enrich from
// Octopart" action on an item.
func (n *NexarProvider) Lookup(ctx context.Context, mpn string) (*PartDetail, error) {
	if !n.Configured() {
		return nil, fmt.Errorf("Octopart is not configured (set NEXAR_CLIENT_ID and NEXAR_CLIENT_SECRET)")
	}
	parts, err := n.searchParts(ctx, mpn, 5)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("Octopart has no part matching %q", mpn)
	}

	// Prefer an exact part-number match; the first result is only the best
	// guess at a name.
	chosen := parts[0]
	for _, p := range parts {
		if strings.EqualFold(strings.TrimSpace(p.MPN), strings.TrimSpace(mpn)) {
			chosen = p
			break
		}
	}

	detail := &PartDetail{
		MPN:          chosen.MPN,
		Manufacturer: chosen.Manufacturer.Name,
		Description:  chosen.ShortDescription,
		OctopartURL:  chosen.OctopartURL,
		Offers:       allOffers(chosen),
	}
	if chosen.BestImage != nil {
		detail.ImageURL = chosen.BestImage.URL
	}
	if chosen.BestDatasheet != nil && chosen.BestDatasheet.URL != "" {
		title := chosen.BestDatasheet.Name
		if title == "" {
			title = chosen.MPN + " datasheet"
		}
		detail.Datasheet = &PageDocument{Title: title, URL: chosen.BestDatasheet.URL, Kind: "datasheet"}
	}
	for _, s := range chosen.Specs {
		if s.Attribute.Name == "" || s.DisplayValue == "" {
			continue
		}
		detail.Specs = append(detail.Specs, Spec{Name: s.Attribute.Name, Value: s.DisplayValue})
	}
	return detail, nil
}
