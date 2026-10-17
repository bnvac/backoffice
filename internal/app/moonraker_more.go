package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// More of what Moonraker knows, none of which needs a camera: the printer's
// own job history (every print, however it was started), the preview image
// the slicer put in the file, and which layer it is on.

// RemoteJob is one entry in the printer's history.
type RemoteJob struct {
	ID            string  `json:"job_id"`
	File          string  `json:"filename"`
	Status        string  `json:"status"`
	Start         float64 `json:"start_time"`
	End           float64 `json:"end_time"`
	PrintDuration float64 `json:"print_duration"`
	FilamentUsed  float64 `json:"filament_used"` // mm
	Exists        bool    `json:"exists"`
	Metadata      struct {
		FilamentTotal  float64 `json:"filament_total"`        // mm the slicer planned
		FilamentWeight float64 `json:"filament_weight_total"` // g the slicer planned
		EstimatedTime  float64 `json:"estimated_time"`
	} `json:"metadata"`
}

// Outcome is the printer's status in this app's three words, or "" while it
// is still going.
func (j RemoteJob) Outcome() string {
	switch j.Status {
	case "in_progress", "":
		return ""
	case "completed":
		return "completed"
	case "cancelled":
		return "cancelled"
	}
	return "failed" // error, klippy_shutdown, klippy_disconnect, server_exit, interrupted
}

// GramsUsed is what actually went through the nozzle. The slicer's own weight
// scaled by how much of the planned length was used is best — it already
// knows the plastic's density — and the length times the spool's density is
// the fallback.
func (j RemoteJob) GramsUsed(diameter, density float64) float64 {
	if j.FilamentUsed <= 0 {
		return 0
	}
	if j.Metadata.FilamentTotal > 0 && j.Metadata.FilamentWeight > 0 {
		return j.Metadata.FilamentWeight * math.Min(1, j.FilamentUsed/j.Metadata.FilamentTotal)
	}
	return GcodeMeta{Length: j.FilamentUsed, Diameter: diameter}.GramsFor(density)
}

// HistoryReader is a link whose printer keeps a job history.
type HistoryReader interface {
	History(ctx context.Context, since float64, limit int, newestFirst bool) ([]RemoteJob, error)
	Thumbnail(ctx context.Context, file string) ([]byte, error)
}

func (m *moonraker) History(ctx context.Context, since float64, limit int, newestFirst bool) ([]RemoteJob, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "order": {"asc"}}
	if newestFirst {
		q.Set("order", "desc")
	}
	if since > 0 {
		q.Set("since", strconv.FormatFloat(since, 'f', 3, 64))
	}
	req, _ := http.NewRequest(http.MethodGet, m.base+"/server/history/list?"+q.Encode(), nil)
	var out struct {
		Result struct {
			Jobs []RemoteJob `json:"jobs"`
		} `json:"result"`
	}
	code, err := doJSON(ctx, lanStatusClient, req, m.key, &out)
	if err != nil || code != http.StatusOK {
		return nil, friendlyLinkError(m.base, err, code)
	}
	return out.Result.Jobs, nil
}

// Thumbnail fetches the largest preview the slicer embedded that is still a
// sensible size, as PNG. Moonraker extracts them when a file arrives.
func (m *moonraker) Thumbnail(ctx context.Context, file string) ([]byte, error) {
	if file == "" {
		return nil, fmt.Errorf("no file")
	}
	req, _ := http.NewRequest(http.MethodGet, m.base+"/server/files/metadata?filename="+url.QueryEscape(file), nil)
	var out struct {
		Result struct {
			Thumbnails []struct {
				Width        int    `json:"width"`
				Height       int    `json:"height"`
				RelativePath string `json:"relative_path"`
			} `json:"thumbnails"`
		} `json:"result"`
	}
	code, err := doJSON(ctx, lanStatusClient, req, m.key, &out)
	if err != nil || code != http.StatusOK {
		return nil, friendlyLinkError(m.base, err, code)
	}
	thumbs := out.Result.Thumbnails
	if len(thumbs) == 0 {
		return nil, fmt.Errorf("the slicer put no preview in %s", file)
	}
	// Largest first, but nothing over 600 px: that is a picture for a phone
	// notification, not a print.
	sort.Slice(thumbs, func(i, j int) bool { return thumbs[i].Width > thumbs[j].Width })
	pick := thumbs[len(thumbs)-1]
	for _, t := range thumbs {
		if t.Width <= 600 {
			pick = t
			break
		}
	}
	rel := path.Join(path.Dir(file), pick.RelativePath)
	if strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("odd thumbnail path %q", pick.RelativePath)
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/server/files/gcodes/"+strings.Join(parts, "/"), nil)
	if m.key != "" {
		req.Header.Set("X-Api-Key", m.key)
	}
	res, err := lanStatusClient.Do(req)
	if err != nil {
		return nil, friendlyLinkError(m.base, err, 0)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, friendlyLinkError(m.base, nil, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if !isPNG(b) {
		return nil, fmt.Errorf("the preview is not a PNG")
	}
	return b, nil
}

func isPNG(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) }

// --- previews in G-code we hold ourselves -----------------------------------------

var thumbBegin = regexp.MustCompile(`^;\s*thumbnail begin (\d+)x(\d+)`)

// GcodeThumbnail pulls the slicer's PNG preview out of a G-code file, the same
// "; thumbnail begin WxH" block Moonraker reads. Orca and PrusaSlicer put it
// at the top, so only the first megabyte is looked at.
func GcodeThumbnail(r io.Reader) []byte {
	sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	var best []byte
	bestW := 0
	var cur strings.Builder
	in, w := false, 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := thumbBegin.FindStringSubmatch(line); m != nil {
			in, w = true, atoiOr(m[1], 0)
			cur.Reset()
			continue
		}
		if !in {
			continue
		}
		if strings.Contains(line, "thumbnail end") {
			in = false
			png, err := base64.StdEncoding.DecodeString(cur.String())
			// The biggest one up to 600 px, as for a printer's own.
			if err == nil && isPNG(png) && w <= 600 && w > bestW {
				best, bestW = png, w
			}
			continue
		}
		cur.WriteString(strings.TrimSpace(strings.TrimPrefix(line, ";")))
	}
	return best
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// --- pairing ------------------------------------------------------------------------

// Found is a Klipper printer that answered.
type Found struct {
	Address  string // http://192.168.1.50:7125
	Host     string // the board's hostname, e.g. mkspi
	State    string // Klipper's own: ready, startup, shutdown, error
	Version  string
	PairedTo string // the printer here already using this address, if any
}

// probeMoonraker asks one address whether Moonraker is there. It is quick to
// say no: the scan asks a few hundred addresses.
func probeMoonraker(ctx context.Context, base string) (Found, bool) {
	req, _ := http.NewRequest(http.MethodGet, base+"/printer/info", nil)
	var out struct {
		Result struct {
			State           string `json:"state"`
			Hostname        string `json:"hostname"`
			SoftwareVersion string `json:"software_version"`
		} `json:"result"`
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	code, err := doJSON(ctx, lanStatusClient, req, "", &out)
	// Moonraker with an API key set answers 401 to an unknown caller, which
	// still means it is there.
	if err == nil && code == http.StatusUnauthorized {
		return Found{Address: base, State: "needs its API key"}, true
	}
	if err != nil || code != http.StatusOK || out.Result.State == "" {
		return Found{}, false
	}
	return Found{Address: base, Host: out.Result.Hostname, State: out.Result.State,
		Version: out.Result.SoftwareVersion}, true
}
