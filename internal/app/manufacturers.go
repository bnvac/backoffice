package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Who made the part. It is a plain column on the item, like category and
// location, so filtering and faceting come for free; this file is about the
// thing a name alone cannot do -- putting the maker's mark next to it, so a
// drawer of anonymous black boards becomes recognisable at a glance.

// Manufacturer is a name with a logo and a website hung off it.
type Manufacturer struct {
	Name      string
	Domain    string
	URL       string
	Logo      string // stored image filename
	Notes     string
	CreatedAt time.Time

	// Filled in by the listing.
	Items  int
	Pieces int
	Value  float64
}

func (m Manufacturer) ValueDisplay() string { return formatMoney(m.Value, "USD") }

// Site is where to send someone who clicks the name.
func (m Manufacturer) Site() string {
	if m.URL != "" {
		return m.URL
	}
	if m.Domain != "" {
		return "https://" + m.Domain
	}
	return ""
}

// Initials is the monogram shown when there is no logo. Two letters from two
// words, or the first two of one.
func (m Manufacturer) Initials() string { return monogram(m.Name) }

func monogram(name string) string {
	fields := strings.Fields(name)
	switch {
	case len(fields) == 0:
		return "?"
	case len(fields) == 1:
		r := []rune(strings.ToUpper(fields[0]))
		if len(r) == 1 {
			return string(r)
		}
		return string(r[:2])
	}
	var out []rune
	for _, f := range fields {
		out = append(out, []rune(strings.ToUpper(f))[0])
		if len(out) == 2 {
			break
		}
	}
	return string(out)
}

// Hue gives each maker a stable colour for its monogram tile, derived from the
// name so it never moves and never needs storing.
func (m Manufacturer) Hue() int {
	sum := sha256.Sum256([]byte(strings.ToLower(m.Name)))
	return int(sum[0])*360/256 + 0
}

// knownManufacturers maps makers onto their websites, so a logo can be found
// without anyone hunting for a URL first. It covers the makers a homelab drawer
// is full of rather than trying to be a directory: the rest are looked up from
// whatever domain you give them.
//
// The keys are spelled the way the maker spells itself, because they are also
// what the shelf gets organised under. Whatever you type is matched against
// them without regard to case, so "rohm" still lands on ROHM.
var knownManufacturers = map[string]string{
	// Boards and modules
	"Espressif": "espressif.com", "Raspberry Pi": "raspberrypi.com",
	"Arduino": "arduino.cc", "Adafruit": "adafruit.com",
	"SparkFun": "sparkfun.com", "Seeed Studio": "seeedstudio.com",
	"M5Stack": "m5stack.com", "Pimoroni": "pimoroni.com",
	"Waveshare": "waveshare.com", "DFRobot": "dfrobot.com",
	"Pololu": "pololu.com", "Olimex": "olimex.com",
	"PJRC": "pjrc.com", "LILYGO": "lilygo.cc",
	"Heltec": "heltec.org", "Ai-Thinker": "ai-thinker.com",
	"WEMOS": "wemos.cc", "BeagleBoard": "beagleboard.org",
	"Radxa": "radxa.com", "Hardkernel": "hardkernel.com",
	"Khadas": "khadas.com", "Orange Pi": "orangepi.org",
	"Banana Pi": "banana-pi.org", "NVIDIA": "nvidia.com",
	"Particle": "particle.io", "Sipeed": "sipeed.com",

	// Silicon
	"Texas Instruments": "ti.com", "STMicroelectronics": "st.com",
	"Microchip": "microchip.com", "Nordic Semiconductor": "nordicsemi.com",
	"NXP": "nxp.com", "Infineon": "infineon.com",
	"Analog Devices": "analog.com", "ON Semiconductor": "onsemi.com",
	"Renesas": "renesas.com", "Silicon Labs": "silabs.com",
	"Toshiba": "toshiba.com", "ROHM": "rohm.com",
	"Diodes Incorporated": "diodes.com", "Winbond": "winbond.com",
	"Bosch Sensortec": "bosch-sensortec.com", "Sensirion": "sensirion.com",
	"Broadcom": "broadcom.com", "Quectel": "quectel.com",

	// Passives, connectors and electromechanical
	"Vishay": "vishay.com", "Murata": "murata.com",
	"TDK": "tdk.com", "Panasonic": "panasonic.com",
	"Yageo": "yageo.com", "KEMET": "kemet.com",
	"Nichicon": "nichicon.com", "Bourns": "bourns.com",
	"Littelfuse": "littelfuse.com", "Molex": "molex.com",
	"JST": "jst-mfg.com", "Amphenol": "amphenol.com",
	"TE Connectivity": "te.com", "Hirose": "hirose.com",
	"Omron": "omron.com", "Alps Alpine": "alpsalpine.com",
	"Samtec": "samtec.com", "Phoenix Contact": "phoenixcontact.com",
	"WAGO": "wago.com", "Keystone": "keyelco.com",

	// Homelab infrastructure
	"Ubiquiti": "ui.com", "MikroTik": "mikrotik.com",
	"TP-Link": "tp-link.com", "NETGEAR": "netgear.com",
	"Zyxel": "zyxel.com", "Synology": "synology.com",
	"QNAP": "qnap.com", "Western Digital": "westerndigital.com",
	"Seagate": "seagate.com", "Samsung": "samsung.com",
	"Kingston": "kingston.com", "SanDisk": "sandisk.com",
	"Crucial": "crucial.com", "Intel": "intel.com",
	"AMD": "amd.com", "Mean Well": "meanwell.com",
	"Anker": "anker.com",
}

// canonicalNames fixes up the other things people type: abbreviations, former
// names, and the sub-brand somebody actually bought the part under. Without it
// "TI" and "Texas Instruments" become two makers with two logos.
var canonicalNames = map[string]string{
	"ti": "Texas Instruments", "st": "STMicroelectronics",
	"rpi": "Raspberry Pi", "raspberry pi foundation": "Raspberry Pi",
	"raspberry pi ltd": "Raspberry Pi",
	"atmel":            "Microchip", "cypress": "Infineon",
	"maxim integrated": "Analog Devices", "onsemi": "ON Semiconductor",
	"silabs": "Silicon Labs", "nordic": "Nordic Semiconductor",
	"seeed": "Seeed Studio", "seeedstudio": "Seeed Studio",
	"espressif systems": "Espressif", "adafruit industries": "Adafruit",
	"sparkfun electronics": "SparkFun", "meanwell": "Mean Well",
	"odroid": "Hardkernel", "teensy": "PJRC",
	"bosch": "Bosch Sensortec", "alps": "Alps Alpine",
	"diodes": "Diodes Incorporated", "te": "TE Connectivity",
}

// makerNames indexes both tables above by lowercase name, so a maker can be
// recognised however it was typed. It is built once rather than kept by hand,
// which is what stops the two tables drifting apart.
var makerNames = func() map[string]string {
	index := make(map[string]string, len(knownManufacturers)+len(canonicalNames))
	for proper := range knownManufacturers {
		index[strings.ToLower(proper)] = proper
	}
	for alias, proper := range canonicalNames {
		index[alias] = proper
	}
	return index
}()

// CanonicalManufacturer settles on one spelling of a name, so that typing
// "raspberry pi" into one part and "Raspberry Pi" into the next does not leave
// the shelf sorted into two piles.
func CanonicalManufacturer(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return ""
	}
	if proper, ok := makerNames[strings.ToLower(name)]; ok {
		return proper
	}
	return name
}

// DomainForManufacturer guesses where a maker lives on the web.
func DomainForManufacturer(name string) string {
	return knownManufacturers[CanonicalManufacturer(name)]
}

// --- storage ----------------------------------------------------------------

// Manufacturers lists every maker in the inventory with its counts, whether or
// not anybody has bothered to give it a logo.
func (s *Store) Manufacturers() ([]Manufacturer, error) {
	rows, err := s.db.Query(`SELECT i.manufacturer, COUNT(*), COALESCE(SUM(i.quantity), 0)
		FROM items i WHERE i.manufacturer <> ''
		GROUP BY i.manufacturer COLLATE NOCASE
		ORDER BY COUNT(*) DESC, i.manufacturer COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Manufacturer
	byName := map[string]int{}
	for rows.Next() {
		var m Manufacturer
		if err := rows.Scan(&m.Name, &m.Items, &m.Pieces); err != nil {
			return nil, err
		}
		byName[strings.ToLower(m.Name)] = len(out)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// The details, for the ones that have any.
	details, err := s.db.Query(`SELECT name, domain, url, logo, notes FROM manufacturers`)
	if err != nil {
		return nil, err
	}
	defer details.Close()
	for details.Next() {
		var name, domain, link, logo, notes string
		if err := details.Scan(&name, &domain, &link, &logo, &notes); err != nil {
			return nil, err
		}
		if idx, ok := byName[strings.ToLower(name)]; ok {
			out[idx].Domain, out[idx].URL = domain, link
			out[idx].Logo, out[idx].Notes = logo, notes
		}
	}
	if err := details.Err(); err != nil {
		return nil, err
	}

	// What the shelf is worth per maker, valued at the cheapest known price.
	values, err := s.db.Query(`SELECT manufacturer, COALESCE(SUM(cheapest * quantity), 0) FROM (
		SELECT i.manufacturer AS manufacturer, i.quantity AS quantity, MIN(p.amount) AS cheapest
		FROM items i JOIN prices p ON p.item_id = i.id
		WHERE i.manufacturer <> '' GROUP BY i.id)
		GROUP BY manufacturer COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer values.Close()
	for values.Next() {
		var name string
		var value float64
		if err := values.Scan(&name, &value); err != nil {
			return nil, err
		}
		if idx, ok := byName[strings.ToLower(name)]; ok {
			out[idx].Value = value
		}
	}
	return out, values.Err()
}

// SettleManufacturer picks the spelling this shelf already uses.
//
// CanonicalManufacturer handles the makers in the built-in table, but a small
// supplier nobody has heard of is just as easy to type two ways. Once one of
// its parts is on the shelf, that spelling is the answer -- so the second part
// joins the first instead of starting a pile of its own.
func (s *Store) SettleManufacturer(name string) string {
	name = CanonicalManufacturer(name)
	if name == "" || knownManufacturers[name] != "" {
		return name
	}
	var existing string
	err := s.db.QueryRow(`SELECT manufacturer FROM items
		WHERE manufacturer = ? COLLATE NOCASE AND manufacturer <> ''
		ORDER BY id LIMIT 1`, name).Scan(&existing)
	if err == nil && existing != "" {
		return existing
	}
	return name
}

// MakerLogos maps every maker that has a logo onto its stored image, keyed by
// lowercase name. It is one small query over a table with a few dozen rows in
// it, which is what makes it cheap enough to cache and hand to a template.
func (s *Store) MakerLogos() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, logo FROM manufacturers WHERE logo <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, logo string
		if err := rows.Scan(&name, &logo); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = logo
	}
	return out, rows.Err()
}

// GetManufacturer reads one maker's details, inventing an empty one rather than
// failing when nobody has filled anything in.
func (s *Store) GetManufacturer(name string) (Manufacturer, error) {
	m := Manufacturer{Name: CanonicalManufacturer(name)}
	var created string
	err := s.db.QueryRow(`SELECT name, domain, url, logo, notes, created_at
		FROM manufacturers WHERE name = ?`, name).
		Scan(&m.Name, &m.Domain, &m.URL, &m.Logo, &m.Notes, &created)
	if err != nil {
		return Manufacturer{Name: CanonicalManufacturer(name)}, nil
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return m, nil
}

// SaveManufacturer records the details, creating the row on first use.
func (s *Store) SaveManufacturer(m Manufacturer) error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("a manufacturer needs a name")
	}
	_, err := s.db.Exec(`INSERT INTO manufacturers (name, domain, url, logo, notes, created_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET domain = excluded.domain, url = excluded.url,
			logo = excluded.logo, notes = excluded.notes`,
		m.Name, m.Domain, m.URL, m.Logo, m.Notes, nowRFC3339())
	return err
}

// RenameManufacturer moves every item from one maker to another, which is how
// two spellings of the same company get merged.
func (s *Store) RenameManufacturer(from, to string) (int64, error) {
	to = CanonicalManufacturer(to)
	if to == "" {
		return 0, fmt.Errorf("give the new name")
	}
	res, err := s.db.Exec(`UPDATE items SET manufacturer = ? WHERE manufacturer = ? COLLATE NOCASE`, to, from)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// The old details row is now unused; the new name keeps its own.
	if !strings.EqualFold(from, to) {
		_, _ = s.db.Exec(`DELETE FROM manufacturers WHERE name = ? COLLATE NOCASE`, from)
	}
	return n, nil
}

// --- logos ------------------------------------------------------------------

const faviconService = "https://www.google.com/s2/favicons?sz=128&domain="

// FetchLogo finds a maker's mark and stores it.
//
// The maker's own site is tried first, because an apple-touch icon or an
// OpenGraph image is a real logo at a usable size. Plenty of sites offer only a
// .ico, which nothing in the standard library decodes, and plenty of others
// refuse a non-browser client outright -- so the fallback is a favicon service,
// which returns a PNG for both of those cases. What it cannot do is invent a
// logo for a company with no website, and there the monogram tile stands in.
func (a *App) FetchLogo(ctx context.Context, m Manufacturer) (string, error) {
	domain := strings.TrimSpace(m.Domain)
	if domain == "" {
		domain = DomainForManufacturer(m.Name)
	}
	if domain == "" && m.URL != "" {
		domain = hostOf(m.URL)
	}
	if domain == "" {
		return "", fmt.Errorf("no website known for %s — give it one, or upload a logo", m.Name)
	}
	domain = strings.TrimPrefix(strings.ToLower(domain), "www.")

	var problems []string
	for _, candidate := range a.logoCandidates(ctx, domain) {
		body, err := a.fetcher.FetchImage(ctx, candidate)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		name, err := a.photos.Save(bytes.NewReader(body), lastPathSegment(candidate))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		return name, nil
	}
	if len(problems) == 0 {
		problems = append(problems, "nothing usable found")
	}
	return "", fmt.Errorf("could not fetch a logo from %s (%s)", domain, problems[0])
}

// logoCandidates lists the images worth trying, best first.
func (a *App) logoCandidates(ctx context.Context, domain string) []string {
	var out []string
	site := "https://" + domain

	if icons := a.siteIcons(ctx, site); len(icons) > 0 {
		out = append(out, icons...)
	}
	// The favicon service last: it always answers with a PNG, including for the
	// sites that would not talk to us at all, but it is only 48-128 pixels.
	out = append(out, faviconService+url.QueryEscape(domain))
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// siteIcons reads a homepage for the icons it declares. Only formats that can
// actually be decoded are returned -- an .ico or an .svg is worse than useless
// here, because it would be stored and then fail to render.
func (a *App) siteIcons(ctx context.Context, site string) []string {
	body, finalURL, err := a.fetcher.get(ctx, site, "text/html", maxPageBytes)
	if err != nil {
		return nil
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	base, err := url.Parse(finalURL)
	if err != nil {
		return nil
	}

	type candidate struct {
		href string
		size int
	}
	var icons []candidate
	var ogImage string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "link":
				rel := strings.ToLower(attr(n, "rel"))
				if strings.Contains(rel, "icon") && !strings.Contains(rel, "mask-icon") {
					if href := attr(n, "href"); usableIconFormat(href) {
						icons = append(icons, candidate{href, iconSize(attr(n, "sizes"), rel)})
					}
				}
			case "meta":
				if ogImage == "" && strings.EqualFold(attr(n, "property"), "og:image") {
					ogImage = attr(n, "content")
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// Biggest declared icon first: a 180px apple-touch icon beats a 16px one.
	sort.SliceStable(icons, func(i, j int) bool { return icons[i].size > icons[j].size })

	var out []string
	seen := map[string]bool{}
	add := func(ref string) {
		if ref == "" {
			return
		}
		abs, err := base.Parse(strings.TrimSpace(ref))
		if err != nil || (abs.Scheme != "http" && abs.Scheme != "https") {
			return
		}
		if link := abs.String(); !seen[link] {
			seen[link] = true
			out = append(out, link)
		}
	}
	for _, c := range icons {
		add(c.href)
	}
	// An OpenGraph image is a banner rather than a mark, so it is a last resort
	// before the favicon service.
	add(ogImage)
	return out
}

// usableIconFormat keeps the formats nothing here can decode out of the list.
func usableIconFormat(href string) bool {
	clean := strings.ToLower(href)
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	switch {
	case strings.HasSuffix(clean, ".ico"), strings.HasSuffix(clean, ".svg"):
		return false
	case strings.HasSuffix(clean, ".png"), strings.HasSuffix(clean, ".jpg"),
		strings.HasSuffix(clean, ".jpeg"), strings.HasSuffix(clean, ".webp"),
		strings.HasSuffix(clean, ".gif"):
		return true
	}
	// No extension at all is common for CDN-served icons; it is worth a try,
	// since the magic bytes decide once it has been fetched.
	return !strings.Contains(clean, ".")
}

// iconSize reads the declared pixel size, treating an apple-touch icon as a
// good bet when no size is given -- they are 180px by convention.
func iconSize(sizes, rel string) int {
	if w, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(sizes)), "x"); ok {
		if n := atoiSafe(w); n > 0 {
			return n
		}
	}
	if strings.Contains(rel, "apple-touch") {
		return 180
	}
	return 32
}
