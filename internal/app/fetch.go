package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"

	"github.com/bnvac/backoffice/internal/media"
)

const (
	maxPageBytes = 2 << 20 // enough for any product page's <head>
	fetchTimeout = 15 * time.Second
	maxRedirects = 5
	userAgent    = "Backoffice/1.0 (self-hosted inventory; +https://github.com/bnvac/backoffice)"
)

// Fetcher pulls pages and images from the internet on the server's behalf.
//
// That makes it a request-forgery tool if left unguarded: this process usually
// sits inside a home network, so a pasted URL could otherwise reach a router
// admin page, a NAS, or a cloud metadata endpoint that the person pasting it
// cannot reach themselves. Every connection is therefore checked against the
// address it actually resolved to, on every redirect hop.
type Fetcher struct {
	client       *http.Client
	allowPrivate bool
}

func NewFetcher(allowPrivate bool) *Fetcher {
	f := &Fetcher{allowPrivate: allowPrivate}

	dialer := &net.Dialer{
		Timeout: 8 * time.Second,
		// Control runs after DNS resolution with the address actually being
		// dialled, so a hostname that resolves to a private IP -- including one
		// that only does so on the second lookup -- is still blocked.
		Control: func(network, address string, _ syscall.RawConn) error {
			return f.checkAddr(network, address)
		},
	}
	f.client = &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			DisableKeepAlives:     true,
			Proxy:                 http.ProxyFromEnvironment,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing to follow a %s redirect", req.URL.Scheme)
			}
			return nil
		},
	}
	return f
}

func (f *Fetcher) checkAddr(network, address string) error {
	if !strings.HasPrefix(network, "tcp") {
		return fmt.Errorf("refusing to dial %s", network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("bad address %q", address)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("bad address %q", address)
	}
	if f.allowPrivate {
		return nil
	}
	if !isPublicIP(ip) {
		return fmt.Errorf("refusing to fetch from private address %s "+
			"(set ALLOW_PRIVATE_FETCH=1 to permit LAN sources)", ip)
	}
	return nil
}

// isPublicIP rejects everything a hosted service should never be talked into
// reaching: loopback, LAN ranges, link-local (cloud metadata lives at
// 169.254.169.254), carrier NAT, and IPv6 equivalents.
func isPublicIP(ip netip.Addr) bool {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip.Is4() {
		b := ip.As4()
		switch {
		case b[0] == 100 && b[1] >= 64 && b[1] <= 127: // 100.64/10 carrier NAT
			return false
		case b[0] == 192 && b[1] == 0 && b[2] == 0: // 192.0.0/24 protocol assignments
			return false
		case b[0] >= 240: // 240/4 reserved, includes broadcast
			return false
		}
		return true
	}
	// fc00::/7 unique-local, and 64:ff9b::/96 NAT64 which maps back to IPv4.
	if ip.Is6() {
		b := ip.As16()
		if b[0]&0xfe == 0xfc {
			return false
		}
		if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b {
			return false
		}
	}
	return true
}

// parseTarget validates a user-supplied URL before any connection is attempted.
func parseTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("no URL given")
	}
	// A bare domain is what people actually paste.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("that does not look like a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http and https URLs are supported")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("that URL has no host")
	}
	return u, nil
}

// PageMeta is what a product page, datasheet listing or wiki article gives us
// to prefill an item with.
type PageMeta struct {
	Title       string
	Description string
	ImageURL    string
	SiteName    string
	Price       float64 // when the page states one
	Currency    string
	PartNumber  string
	URL         string

	// Documents linked from the page that look like datasheets or manuals, so
	// an import can attach them without anyone hunting for the PDF.
	Documents []PageDocument
	// Pages worth opening next, when the documents are a hop away.
	Leads []PageDocument
}

// PageDocument is a downloadable reference discovered on a product page.
type PageDocument struct {
	Title string
	URL   string
	Kind  string
}

// FetchPage downloads a page and reads its metadata. It prefers OpenGraph tags,
// which nearly every shop and wiki emits, and falls back to plain HTML.
func (f *Fetcher) FetchPage(ctx context.Context, raw string) (*PageMeta, error) {
	u, err := parseTarget(raw)
	if err != nil {
		return nil, err
	}
	raw2, finalURL, err := f.get(ctx, u.String(), "text/html,application/xhtml+xml", maxPageBytes)
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(strings.NewReader(string(raw2)))
	if err != nil {
		return nil, fmt.Errorf("could not read that page")
	}

	meta, title := collectMeta(doc)
	scan := scanBody(doc)
	pm := &PageMeta{URL: finalURL}
	pm.Title = firstOf(meta, "og:title", "twitter:title", "title")
	if pm.Title == "" {
		pm.Title = title
	}
	pm.Description = firstOf(meta, "og:description", "twitter:description", "description")
	pm.SiteName = firstOf(meta, "og:site_name", "application-name")
	pm.PartNumber = firstOf(meta, "product:retailer_item_id", "product:mfr_part_no", "sku", "mpn")

	if raw := firstOf(meta, "product:price:amount", "og:price:amount"); raw != "" {
		if amount, err := parsePrice(raw); err == nil && amount > 0 {
			pm.Price = amount
			pm.Currency = strings.ToUpper(firstOf(meta, "product:price:currency", "og:price:currency"))
			if pm.Currency == "" {
				pm.Currency = "USD"
			}
		}
	}

	// Image discovery, best source first. Shops that emit OpenGraph give a
	// clean product shot; the rest need falling back through older conventions
	// and finally the biggest picture actually on the page.
	base, baseErr := url.Parse(finalURL)
	absolute := func(ref string) string {
		if ref == "" || baseErr != nil {
			return ""
		}
		abs, err := base.Parse(strings.TrimSpace(ref))
		if err != nil || (abs.Scheme != "http" && abs.Scheme != "https") {
			return ""
		}
		return abs.String()
	}

	candidates := []string{
		firstOf(meta, "og:image:secure_url", "og:image", "twitter:image", "twitter:image:src"),
		meta["image"],     // <meta itemprop="image">
		scan.LinkImageSrc, // <link rel="image_src">
		scan.JSONLDImage,  // schema.org product data
		scan.LargestImage, // last resort: the biggest <img> on the page
	}
	for _, c := range candidates {
		if abs := absolute(c); abs != "" {
			pm.ImageURL = abs
			break
		}
	}

	for _, d := range scan.Documents {
		if abs := absolute(d.URL); abs != "" {
			d.URL = abs
			d.Title = tidy(d.Title, 90)
			pm.Documents = append(pm.Documents, d)
		}
	}
	for _, d := range scan.Leads {
		if abs := absolute(d.URL); abs != "" {
			d.URL = abs
			d.Title = tidy(d.Title, 90)
			pm.Leads = append(pm.Leads, d)
		}
	}

	pm.Title = tidy(pm.Title, 200)
	pm.Description = tidy(pm.Description, 600)
	pm.PartNumber = tidy(pm.PartNumber, 80)
	pm.SiteName = tidy(pm.SiteName, 80)

	if pm.Title == "" && pm.ImageURL == "" {
		return nil, fmt.Errorf("that page had no title or image to import")
	}
	return pm, nil
}

// FetchImage downloads an image URL, returning the raw bytes for PhotoStore to
// validate and thumbnail. It does not trust the served content type; the image
// decoder is the real check.
func (f *Fetcher) FetchImage(ctx context.Context, raw string) ([]byte, error) {
	u, err := parseTarget(raw)
	if err != nil {
		return nil, err
	}
	body, _, err := f.get(ctx, u.String(), "image/*", media.MaxPhotoSize)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("that URL returned an empty response")
	}
	return body, nil
}

// PostJSON sends a JSON request body through the same guarded client the rest
// of the app uses, so an API endpoint gets the same SSRF and size protections
// as a pasted link.
func (f *Fetcher) PostJSON(ctx context.Context, target string, payload []byte, headers map[string]string, limit int64) ([]byte, error) {
	if _, err := parseTarget(target); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := f.client.Do(req)
	if err != nil {
		return nil, cleanFetchError(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, cleanFetchError(err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("that response is larger than %d MiB", limit>>20)
	}
	// A GraphQL endpoint answers 200 with an errors array, so the status is
	// only interesting when it denies the request outright.
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return body, fmt.Errorf("the API rejected these credentials (HTTP %d)", res.StatusCode)
	}
	if res.StatusCode >= 500 {
		return body, fmt.Errorf("the API is unavailable (HTTP %d)", res.StatusCode)
	}
	return body, nil
}

// PostForm posts urlencoded data, which is what OAuth token endpoints take.
func (f *Fetcher) PostForm(ctx context.Context, target string, form url.Values, limit int64) ([]byte, error) {
	if _, err := parseTarget(target); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	res, err := f.client.Do(req)
	if err != nil {
		return nil, cleanFetchError(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, cleanFetchError(err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return body, fmt.Errorf("token request failed (HTTP %d)", res.StatusCode)
	}
	return body, nil
}

// header is an extra request header, for the few sources that authenticate.
// Credentials belong here rather than in the query string, which would put them
// in error messages and in whatever logs the far end keeps.
type header struct{ key, value string }

func (f *Fetcher) get(ctx context.Context, target, accept string, limit int64, extra ...header) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en")
	for _, h := range extra {
		req.Header.Set(h.key, h.value)
	}

	res, err := f.client.Do(req)
	if err != nil {
		return nil, "", cleanFetchError(err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, "", fmt.Errorf("%s returned HTTP %d", req.URL.Host, res.StatusCode)
	}
	// limit+1 so an oversized body is detected rather than silently truncated.
	body, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, "", cleanFetchError(err)
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("that response is larger than %d MiB", limit>>20)
	}
	return body, res.Request.URL.String(), nil
}

// cleanFetchError unwraps net/http's layered errors so the browser shows the
// reason (usually the private-address refusal) rather than a URL dump.
func cleanFetchError(err error) error {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i > 0 {
		if tail := msg[i+2:]; strings.Contains(tail, "refusing") || strings.Contains(tail, "redirect") {
			return fmt.Errorf("%s", tail)
		}
	}
	// A raw dial error names the resolver and socket, which tells the person
	// pasting a link nothing useful.
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return fmt.Errorf("no such domain: %s", dns.Name)
		}
		return fmt.Errorf("could not look up %s", dns.Name)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return fmt.Errorf("that site took too long to respond")
		}
		return fmt.Errorf("could not reach %s", ue.URL)
	}
	return err
}

// collectMeta indexes every <meta> by its name or property, plus <title>.
func collectMeta(doc *html.Node) (map[string]string, string) {
	meta := map[string]string{}
	var title string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "meta":
				var key, content string
				for _, a := range n.Attr {
					switch strings.ToLower(a.Key) {
					case "property", "name", "itemprop":
						if key == "" {
							key = strings.ToLower(strings.TrimSpace(a.Val))
						}
					case "content":
						content = a.Val
					}
				}
				if key != "" && content != "" && meta[key] == "" {
					meta[key] = content
				}
			case "title":
				if title == "" && n.FirstChild != nil {
					title = n.FirstChild.Data
				}
			case "body":
				return // everything worth reading is in <head>
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return meta, title
}

func firstOf(meta map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(meta[k]); v != "" {
			return v
		}
	}
	return ""
}

// tidy collapses whitespace and trims to a length that fits the form fields.
func tidy(s string, max int) string {
	// Real shops often double-encode their OpenGraph text, so the parser hands
	// back a literal "&#39;" where an apostrophe belongs. Unescaping again
	// fixes those; text that was only encoded once is already plain and passes
	// through untouched.
	s = html.UnescapeString(s)
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len(s) > max {
		// Cut on a rune boundary, and prefer the last word break.
		s = s[:max]
		if i := strings.LastIndexAny(s, " ,;"); i > max/2 {
			s = s[:i]
		}
		s = strings.ToValidUTF8(s, "") + "…"
	}
	return s
}

// bodyScan is what the page's body offers once the <head> has been exhausted.
type bodyScan struct {
	LinkImageSrc string
	JSONLDImage  string
	LargestImage string
	Documents    []PageDocument
	// Pages that say they hold documentation without being documents
	// themselves: a shop's "learn guide", a product's "downloads" tab. The
	// datasheet is usually one hop behind one of these rather than on the
	// product page itself.
	Leads []PageDocument
}

// docPattern recognises the link text and file names that shops use for the
// documents worth keeping: datasheets, pinouts, schematics and manuals.
var docPattern = []struct{ match, kind string }{
	{"datasheet", "datasheet"},
	{"data sheet", "datasheet"},
	{"pinout", "pinout"},
	{"schematic", "schematic"},
	{"manual", "manual"},
	{"user guide", "manual"},
	{"reference", "reference"},
	// Not a document itself, but the page shops park the documents on. It
	// only ever becomes a lead, never a reference, because looksLikeDocument
	// rejects anything without a file extension.
	{"download", "reference"},
}

// diagramKind recognises an inline image that is worth keeping as a reference
// rather than as decoration. It is deliberately strict: "pinout" and
// "schematic" appear in the alt text of the real thing and almost nowhere else,
// whereas a looser word like "diagram" would drag in every marketing render.
func diagramKind(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "pinout"), strings.Contains(t, "pin out"),
		strings.Contains(t, "pin-out"), strings.Contains(t, "pin map"),
		strings.Contains(t, "pinmap"):
		return "pinout"
	case strings.Contains(t, "schematic"):
		return "schematic"
	case strings.Contains(t, "dimensions"), strings.Contains(t, "mechanical drawing"):
		return "reference"
	}
	return ""
}

// scanBody walks the whole document for the things the <head> did not provide:
// fallback images, and links to documents worth attaching to the item.
func scanBody(doc *html.Node) bodyScan {
	var out bodyScan
	bestArea := 0
	seen := map[string]bool{}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "link":
				if attr(n, "rel") == "image_src" {
					if out.LinkImageSrc == "" {
						out.LinkImageSrc = attr(n, "href")
					}
				}
			case "script":
				if out.JSONLDImage == "" && strings.Contains(attr(n, "type"), "ld+json") && n.FirstChild != nil {
					out.JSONLDImage = jsonLDImage(n.FirstChild.Data)
				}
			case "img":
				// Width and height attributes are the only size information
				// available without fetching every candidate.
				w, h := atoiSafe(attr(n, "width")), atoiSafe(attr(n, "height"))
				src := attr(n, "src")
				if src == "" {
					src = firstFromSrcset(attr(n, "srcset"))
				}
				if src != "" && !isDecorativeImage(src) && w*h > bestArea {
					bestArea, out.LargestImage = w*h, src
				}
				// Keep something even when no dimensions are given.
				if out.LargestImage == "" && src != "" && !isDecorativeImage(src) {
					out.LargestImage = src
				}
				// A pinout is usually an image sitting in the page, not a file
				// anybody linked, so the alt text and the file name are what
				// give it away. Missing these is why pinouts used to have to be
				// hunted down by hand.
				if src != "" && !seen[src] {
					if kind := diagramKind(attr(n, "alt") + " " + attr(n, "title") + " " + src); kind != "" {
						seen[src] = true
						out.Documents = append(out.Documents, PageDocument{
							Title: tidy(orDefault(attr(n, "alt"), strings.Title(kind)), 90), //nolint:staticcheck // ASCII
							URL:   src, Kind: kind,
						})
					}
				}
			case "a":
				href := attr(n, "href")
				if href == "" || seen[href] {
					break
				}
				text := strings.ToLower(textOf(n) + " " + href)
				for _, p := range docPattern {
					if !strings.Contains(text, p.match) {
						continue
					}
					seen[href] = true
					title := strings.TrimSpace(textOf(n))
					if title == "" {
						title = strings.Title(p.kind) //nolint:staticcheck // ASCII
					}
					doc := PageDocument{Title: tidy(title, 90), URL: href, Kind: p.kind}
					if looksLikeDocument(href) {
						out.Documents = append(out.Documents, doc)
					} else {
						out.Leads = append(out.Leads, doc)
					}
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(out.Documents) > 8 {
		out.Documents = out.Documents[:8]
	}
	if len(out.Leads) > 4 {
		out.Leads = out.Leads[:4]
	}
	return out
}

// looksLikeDocument keeps navigation links out of the reference list: a
// datasheet is a file, not a category page.
func looksLikeDocument(href string) bool {
	clean := strings.ToLower(href)
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	for _, ext := range []string{".pdf", ".zip", ".doc", ".docx", ".png", ".jpg", ".svg"} {
		if strings.HasSuffix(clean, ext) {
			return true
		}
	}
	return false
}

// isDecorativeImage skips the furniture every shop page carries.
func isDecorativeImage(src string) bool {
	s := strings.ToLower(src)
	for _, junk := range []string{"logo", "icon", "sprite", "avatar", "badge", "banner", "pixel", "spacer", ".gif"} {
		if strings.Contains(s, junk) {
			return true
		}
	}
	return strings.HasPrefix(s, "data:")
}

// jsonLDImage pulls the first image out of a schema.org block without needing
// to model the whole vocabulary.
func jsonLDImage(raw string) string {
	var any map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &any); err != nil {
		return ""
	}
	img, ok := any["image"]
	if !ok {
		return ""
	}
	var single string
	if err := json.Unmarshal(img, &single); err == nil {
		return single
	}
	var list []string
	if err := json.Unmarshal(img, &list); err == nil && len(list) > 0 {
		return list[0]
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(img, &obj); err == nil {
		return obj.URL
	}
	return ""
}

func firstFromSrcset(srcset string) string {
	for _, part := range strings.Split(srcset, ",") {
		if fields := strings.Fields(strings.TrimSpace(part)); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
