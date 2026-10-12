package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bnvac/backoffice/internal/media"
)

// Where footprints come from. KiCad's official library is a public git
// repository, so one file can be fetched by name without an API key, an
// account, or a rate limit worth worrying about. GitLab is the project's own
// home and GitHub is its mirror; both are tried because one of them being down
// should not mean no drawing.
var footprintMirrors = []struct{ name, pattern string }{
	{"KiCad library (GitLab)", "https://gitlab.com/kicad/libraries/kicad-footprints/-/raw/master/%s.pretty/%s.kicad_mod"},
	{"KiCad library (GitHub)", "https://raw.githubusercontent.com/KiCad/kicad-footprints/master/%s.pretty/%s.kicad_mod"},
}

const maxFootprintBytes = 1 << 20

// footprintLibraries guesses which library a bare footprint name lives in, for
// the common case where a BOM's footprint column has lost the "Library:" half.
// Prefixes are tried in order and the first file that exists wins.
var footprintLibraries = []struct {
	prefix string
	libs   []string
}{
	{"R_", []string{"Resistor_SMD", "Resistor_THT"}},
	{"C_", []string{"Capacitor_SMD", "Capacitor_THT"}},
	{"L_", []string{"Inductor_SMD", "Inductor_THT"}},
	{"D_", []string{"Diode_SMD", "Diode_THT"}},
	{"LED_", []string{"LED_SMD", "LED_THT"}},
	{"Q_", []string{"Package_TO_SOT_SMD", "Package_TO_SOT_THT"}},
	{"F_", []string{"Fuse"}},
	{"SW_", []string{"Button_Switch_SMD", "Button_Switch_THT"}},
	{"Crystal", []string{"Crystal"}},
	{"DIP-", []string{"Package_DIP"}},
	{"SOIC-", []string{"Package_SO"}},
	{"SSOP-", []string{"Package_SO"}},
	{"TSSOP-", []string{"Package_SO"}},
	{"MSOP-", []string{"Package_SO"}},
	{"SOT-", []string{"Package_TO_SOT_SMD"}},
	{"TO-", []string{"Package_TO_SOT_THT", "Package_TO_SOT_SMD"}},
	{"QFN-", []string{"Package_DFN_QFN"}},
	{"DFN-", []string{"Package_DFN_QFN"}},
	{"LQFP-", []string{"Package_QFP"}},
	{"TQFP-", []string{"Package_QFP"}},
	{"QFP-", []string{"Package_QFP"}},
	{"BGA-", []string{"Package_BGA"}},
	{"PinHeader_", []string{"Connector_PinHeader_2.54mm", "Connector_PinHeader_1.27mm", "Connector_PinHeader_2.00mm"}},
	{"PinSocket_", []string{"Connector_PinSocket_2.54mm", "Connector_PinSocket_1.27mm"}},
	{"USB_", []string{"Connector_USB"}},
	{"TerminalBlock", []string{"TerminalBlock", "TerminalBlock_Phoenix", "TerminalBlock_TE-Connectivity"}},
	{"JST_", []string{"Connector_JST"}},
	{"Molex_", []string{"Connector_Molex"}},
	{"MountingHole", []string{"MountingHole"}},
	{"Fiducial", []string{"Fiducial"}},
	{"Relay_", []string{"Relay_THT", "Relay_SMD"}},
	{"Buzzer", []string{"Buzzer_Beeper"}},
	{"Espressif", []string{"RF_Module"}},
	{"ESP32", []string{"RF_Module"}},
	{"ESP-", []string{"RF_Module"}},
}

// footprintCandidates turns whatever was typed into a list of library/name
// pairs to try. A fully qualified "Library:Name" is taken at its word.
func footprintCandidates(raw string) [][2]string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return nil
	}
	if lib, short, ok := strings.Cut(name, ":"); ok {
		lib, short = strings.TrimSpace(lib), strings.TrimSpace(short)
		if lib != "" && short != "" {
			return [][2]string{{lib, short}}
		}
		name = short
	}

	var out [][2]string
	seen := map[string]bool{}
	add := func(lib string) {
		if key := lib + ":" + name; !seen[key] {
			seen[key] = true
			out = append(out, [2]string{lib, name})
		}
	}
	for _, entry := range footprintLibraries {
		if strings.HasPrefix(name, entry.prefix) {
			for _, lib := range entry.libs {
				add(lib)
			}
		}
	}
	// A last resort for names that match nothing above: the generic packages.
	for _, lib := range []string{"Package_SO", "Package_DIP", "Connector_Generic"} {
		add(lib)
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// safeFootprintPart keeps a name from escaping its path. The library is a
// directory name and the footprint is a file name; neither may contain a slash
// or a traversal.
func safeFootprintPart(s string) bool {
	if s == "" || len(s) > 120 || strings.Contains(s, "..") {
		return false
	}
	return !strings.ContainsAny(s, "/\\?#%")
}

// FetchFootprint finds a footprint by name, caching the file so it is fetched
// once and drawn forever after -- including with the network unplugged.
func (a *App) FetchFootprint(ctx context.Context, name string) (*media.Footprint, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("give a footprint name, like Resistor_SMD:R_0805_2012Metric")
	}
	if cached, source, err := a.store.CachedFootprint(name); err == nil {
		fp, perr := media.ParseFootprint(cached)
		if perr == nil {
			fp.Source = source
			return fp, nil
		}
		// A cached file that no longer parses is worth replacing.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	candidates := footprintCandidates(name)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%q does not look like a footprint name", name)
	}

	var tried []string
	for _, c := range candidates {
		lib, short := c[0], c[1]
		if !safeFootprintPart(lib) || !safeFootprintPart(short) {
			continue
		}
		for _, mirror := range footprintMirrors {
			link := fmt.Sprintf(mirror.pattern, url.PathEscape(lib), url.PathEscape(short))
			body, _, err := a.fetcher.get(ctx, link, "text/plain", maxFootprintBytes)
			if err != nil {
				continue
			}
			fp, err := media.ParseFootprint(string(body))
			if err != nil {
				tried = append(tried, lib)
				continue
			}
			fp.Source = mirror.name
			// Cache under what was asked for, so the next lookup is a hit even
			// when the library had to be guessed.
			if err := a.store.CacheFootprint(name, mirror.name, string(body)); err != nil {
				return fp, nil // the drawing is fine; only the cache failed
			}
			return fp, nil
		}
		tried = append(tried, lib)
	}
	return nil, fmt.Errorf("no footprint called %q in the KiCad library (tried %s) — "+
		"give it as Library:Name, or paste the .kicad_mod file",
		name, strings.Join(dedupe(tried), ", "))
}

// StoreFootprintFile saves a .kicad_mod someone uploaded or pasted, for parts
// whose footprint is not in the official library.
func (a *App) StoreFootprintFile(name, body string) (*media.Footprint, error) {
	fp, err := media.ParseFootprint(body)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		name = fp.Name
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("that file does not name its footprint, so give it a name")
	}
	fp.Source = "uploaded"
	return fp, a.store.CacheFootprint(name, "uploaded", body)
}

func (s *Store) CachedFootprint(name string) (body, source string, err error) {
	err = s.db.QueryRow(`SELECT body, source FROM footprints WHERE name = ?`, name).Scan(&body, &source)
	return body, source, err
}

func (s *Store) CacheFootprint(name, source, body string) error {
	_, err := s.db.Exec(`INSERT INTO footprints (name, source, body, fetched_at) VALUES (?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET source = excluded.source, body = excluded.body,
			fetched_at = excluded.fetched_at`,
		name, source, body, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) SetFootprint(itemID int64, name string) error {
	_, err := s.db.Exec(`UPDATE items SET footprint = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(name), nowRFC3339(), itemID)
	return err
}
