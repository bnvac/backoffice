package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// Talking to the printer. Two protocols cover nearly every networked hobby
// machine: Moonraker, which fronts Klipper (the Neptune 4 family, Vorons, the
// Sovol and Creality Klipper machines), and OctoPrint, which fronts almost
// anything else on a Pi. Both take a G-code upload over plain HTTP with an
// optional API key, and both will start the print in the same request.
//
// A printer prints G-code. It cannot print an STL; something has to slice it
// first, with a profile for that machine and that filament. This app does not
// slice, and pretending otherwise would mean shipping a slicer inside a
// single binary. So what goes to the printer is G-code you sliced, and what
// this adds over sending straight from the slicer is that the file is kept
// against the part it was for, and can be sent again next month.

// PrintStatus is what a printer says it is doing, the same shape whichever
// protocol it came from.
type PrintStatus struct {
	State    string  // idle | printing | paused | complete | cancelled | error | offline
	File     string  // what it is printing, or last printed
	Progress float64 // 0–100
	Elapsed  time.Duration
	Left     time.Duration // an estimate; zero when there is nothing to base one on
	Message  string        // the printer's own words, or why it could not be asked
}

func (s PrintStatus) Printing() bool { return s.State == "printing" || s.State == "paused" }
func (s PrintStatus) Offline() bool  { return s.State == "offline" }

// Percent rounds down for the bar, as the step checklist does.
func (s PrintStatus) Percent() int { return int(s.Progress) }

// Headline is the one line a card shows.
func (s PrintStatus) Headline() string {
	switch s.State {
	case "printing":
		switch {
		case s.Left > 0 && s.Left < time.Minute:
			return fmt.Sprintf("Printing, %d%%, nearly done", s.Percent())
		case s.Left > 0:
			return fmt.Sprintf("Printing, %d%%, about %s left", s.Percent(), roughDuration(s.Left))
		}
		return fmt.Sprintf("Printing, %d%%", s.Percent())
	case "paused":
		return fmt.Sprintf("Paused at %d%%", s.Percent())
	case "complete":
		return "Finished"
	case "cancelled":
		return "Cancelled"
	case "error":
		return "Error"
	case "offline":
		return "Not answering"
	}
	return "Idle"
}

// roughDuration says "1 h 12 min" rather than "1h12m3.25s": the estimate is
// not good to the second and should not look as if it were.
func roughDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%d h %d min", h, m)
	case h > 0:
		return fmt.Sprintf("%d h", h)
	case m > 0:
		return fmt.Sprintf("%d min", m)
	}
	return "under a minute"
}

// PrinterLink is one connected machine.
type PrinterLink interface {
	Status(ctx context.Context) (PrintStatus, error)
	// Upload sends a G-code file of the given size and, when start is true,
	// starts it. It reports whether the printer says the print started.
	Upload(ctx context.Context, name string, body io.Reader, size int64, start bool) (bool, error)
}

// LinkKinds is the connection picker.
var LinkKinds = []struct{ Key, Label, Hint string }{
	{"", "Not connected", ""},
	{"moonraker", "Moonraker (Klipper)", "http://192.168.1.50:7125 — or the address you open Fluidd or Mainsail at"},
	{"octoprint", "OctoPrint", "http://octopi.local"},
}

// linkFor builds the client for a printer, or reports that it has none.
func linkFor(p Printer) (PrinterLink, bool) {
	base, err := normaliseLinkURL(p.LinkURL)
	if err != nil {
		return nil, false
	}
	switch p.LinkKind {
	case "moonraker":
		return &moonraker{base: base, key: p.LinkKey}, true
	case "octoprint":
		return &octoprint{base: base, key: p.LinkKey}, true
	}
	return nil, false
}

// normaliseLinkURL accepts what people actually type: a bare address, an
// address and port, or a full URL with a trailing slash.
func normaliseLinkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no address")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not an address", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q should start with http:// or https://", raw)
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

// lanClient is not the app's Fetcher, for the same reason the AI client is
// not: the Fetcher refuses private addresses because it follows links from web
// pages, and a printer address is the opposite case — typed into a form by an
// operator, and always on the LAN. Status checks get a short timeout so a
// printer that is switched off does not hold up the page.
var (
	lanStatusClient = &http.Client{Timeout: 4 * time.Second}
	lanUploadClient = &http.Client{} // bounded by the request's context instead
)

// friendlyLinkError turns a transport failure into something actionable. The
// three things that actually go wrong are: the printer is off or the address is
// wrong, the key is wrong, and the address is right but it is the other
// protocol.
func friendlyLinkError(base string, err error, status int) error {
	if err != nil {
		var ne net.Error
		switch {
		case errors.As(err, &ne) && ne.Timeout():
			return fmt.Errorf("%s did not answer in time — is the printer switched on?", base)
		case strings.Contains(err.Error(), "connection refused"):
			return fmt.Errorf("nothing is listening at %s — check the address and port", base)
		case strings.Contains(err.Error(), "no such host"):
			return fmt.Errorf("could not find %s on the network", base)
		}
		return fmt.Errorf("could not reach %s: %v", base, err)
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("the printer refused the API key — check it, or leave it blank if the printer does not use one")
	case http.StatusNotFound:
		return fmt.Errorf("something answered at %s, but not the API this connection expects — is it the other kind?", base)
	case http.StatusConflict:
		return errors.New("the printer is busy and would not take that — is something already printing?")
	}
	return fmt.Errorf("the printer answered with HTTP %d", status)
}

// sizedUpload builds a multipart body around a file whose size is known, and
// works out the exact length of the whole thing in advance.
//
// That length matters. Streaming the form through a pipe would send it with
// chunked transfer encoding, which Go's own servers decode without a word and
// which plenty of small embedded web stacks do not — they read the chunk-size
// lines as part of the file. Sending a Content-Length avoids the question, and
// the file is on disk with a known size anyway, so it costs nothing: only the
// few hundred bytes of headers are held in memory, never the G-code.
func sizedUpload(field, name string, body io.Reader, size int64, extra map[string]string) (io.Reader, int64, string, error) {
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys) // the same request every time, which tests can rely on
	for _, k := range keys {
		if err := mw.WriteField(k, extra[k]); err != nil {
			return nil, 0, "", err
		}
	}
	if _, err := mw.CreateFormFile(field, name); err != nil {
		return nil, 0, "", err
	}
	// What multipart.Writer.Close would write, written by hand so the file can
	// sit between the head and the tail without passing through the writer.
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	length := int64(head.Len()) + size + int64(len(tail))
	return io.MultiReader(&head, io.LimitReader(body, size), strings.NewReader(tail)),
		length, mw.FormDataContentType(), nil
}

func doJSON(ctx context.Context, c *http.Client, req *http.Request, key string, into any) (int, error) {
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return res.StatusCode, nil
	}
	if into == nil {
		return res.StatusCode, nil
	}
	return res.StatusCode, json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(into)
}

// --- Moonraker ------------------------------------------------------------------

type moonraker struct {
	base, key string
}

func (m *moonraker) Status(ctx context.Context) (PrintStatus, error) {
	req, _ := http.NewRequest(http.MethodGet,
		m.base+"/printer/objects/query?print_stats&virtual_sdcard&display_status", nil)
	var out struct {
		Result struct {
			Status struct {
				PrintStats struct {
					State         string  `json:"state"`
					Filename      string  `json:"filename"`
					PrintDuration float64 `json:"print_duration"`
					Message       string  `json:"message"`
				} `json:"print_stats"`
				VirtualSD struct {
					Progress float64 `json:"progress"`
				} `json:"virtual_sdcard"`
				Display struct {
					Message string `json:"message"`
				} `json:"display_status"`
			} `json:"status"`
		} `json:"result"`
	}
	code, err := doJSON(ctx, lanStatusClient, req, m.key, &out)
	if err != nil || code != http.StatusOK {
		return PrintStatus{State: "offline"}, friendlyLinkError(m.base, err, code)
	}

	st := out.Result.Status
	s := PrintStatus{
		File:     st.PrintStats.Filename,
		Progress: clampPercent(st.VirtualSD.Progress * 100),
		Elapsed:  time.Duration(st.PrintStats.PrintDuration * float64(time.Second)),
		Message:  firstNonEmpty(st.PrintStats.Message, st.Display.Message),
	}
	switch st.PrintStats.State {
	case "printing", "paused", "complete", "cancelled", "error":
		s.State = st.PrintStats.State
	default: // "standby", or anything a newer Klipper invents
		s.State = "idle"
	}
	s.Left = estimateLeft(s.Elapsed, s.Progress)
	return s, nil
}

func (m *moonraker) Upload(ctx context.Context, name string, body io.Reader, size int64, start bool) (bool, error) {
	extra := map[string]string{"root": "gcodes"}
	if start {
		extra["print"] = "true"
	}
	rd, length, ctype, err := sizedUpload("file", name, body, size, extra)
	if err != nil {
		return false, err
	}
	req, _ := http.NewRequest(http.MethodPost, m.base+"/server/files/upload", rd)
	req.ContentLength = length
	req.Header.Set("Content-Type", ctype)

	// Moonraker has answered both with and without a "result" wrapper over the
	// years, so both are read.
	var out struct {
		PrintStarted bool `json:"print_started"`
		Result       struct {
			PrintStarted bool `json:"print_started"`
		} `json:"result"`
	}
	code, err := doJSON(ctx, lanUploadClient, req, m.key, &out)
	if err != nil || code < 200 || code > 299 {
		return false, friendlyLinkError(m.base, err, code)
	}
	return out.PrintStarted || out.Result.PrintStarted, nil
}

// --- OctoPrint ------------------------------------------------------------------

type octoprint struct {
	base, key string
}

func (o *octoprint) Status(ctx context.Context) (PrintStatus, error) {
	req, _ := http.NewRequest(http.MethodGet, o.base+"/api/job", nil)
	var out struct {
		Job struct {
			File struct {
				Name string `json:"name"`
			} `json:"file"`
		} `json:"job"`
		Progress struct {
			Completion    *float64 `json:"completion"`
			PrintTime     *float64 `json:"printTime"`
			PrintTimeLeft *float64 `json:"printTimeLeft"`
		} `json:"progress"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	code, err := doJSON(ctx, lanStatusClient, req, o.key, &out)
	if err != nil || code != http.StatusOK {
		return PrintStatus{State: "offline"}, friendlyLinkError(o.base, err, code)
	}

	s := PrintStatus{File: out.Job.File.Name, Message: out.Error}
	if out.Progress.Completion != nil {
		s.Progress = clampPercent(*out.Progress.Completion)
	}
	if out.Progress.PrintTime != nil {
		s.Elapsed = time.Duration(*out.Progress.PrintTime) * time.Second
	}
	if out.Progress.PrintTimeLeft != nil {
		s.Left = time.Duration(*out.Progress.PrintTimeLeft) * time.Second
	}

	// OctoPrint's state is a human string, "Printing from SD" and all.
	state := strings.ToLower(out.State)
	switch {
	case strings.HasPrefix(state, "printing"), strings.HasPrefix(state, "starting"),
		strings.HasPrefix(state, "resuming"), strings.HasPrefix(state, "finishing"):
		s.State = "printing"
	case strings.HasPrefix(state, "paus"):
		s.State = "paused"
	case strings.HasPrefix(state, "cancel"):
		s.State = "cancelled"
	case strings.HasPrefix(state, "error"), strings.Contains(state, "offline after error"):
		s.State = "error"
	case strings.HasPrefix(state, "offline"), strings.HasPrefix(state, "closed"):
		// OctoPrint itself answered, but it is not connected to the printer.
		s.State = "offline"
		s.Message = "OctoPrint is running but not connected to the printer"
	default:
		s.State = "idle"
	}
	if s.Left == 0 {
		s.Left = estimateLeft(s.Elapsed, s.Progress)
	}
	return s, nil
}

func (o *octoprint) Upload(ctx context.Context, name string, body io.Reader, size int64, start bool) (bool, error) {
	extra := map[string]string{}
	if start {
		extra["select"], extra["print"] = "true", "true"
	}
	rd, length, ctype, err := sizedUpload("file", name, body, size, extra)
	if err != nil {
		return false, err
	}
	req, _ := http.NewRequest(http.MethodPost, o.base+"/api/files/local", rd)
	req.ContentLength = length
	req.Header.Set("Content-Type", ctype)

	var out struct {
		Done bool `json:"done"`
	}
	code, err := doJSON(ctx, lanUploadClient, req, o.key, &out)
	if err != nil || code < 200 || code > 299 {
		return false, friendlyLinkError(o.base, err, code)
	}
	// OctoPrint does not say whether the print started, only that the file
	// arrived; asking for it to print and getting a 201 is as good as it gets.
	return start, nil
}

// --- helpers --------------------------------------------------------------------

// estimateLeft extrapolates from progress so far. It is a straight line, which
// is wrong for every print with a different first layer speed, so it waits
// until the print is far enough in for the line to mean something.
func estimateLeft(elapsed time.Duration, percent float64) time.Duration {
	if percent < 3 || percent >= 100 || elapsed <= 0 {
		return 0
	}
	total := time.Duration(float64(elapsed) * 100 / percent)
	return total - elapsed
}

func clampPercent(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	}
	return p
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// GcodeName is the name the file gets on the printer. It keeps whatever the
// slicer called it, because that is the name you will look for on the
// printer's screen, but strips any directory and anything a shell or the
// printer's file list would choke on.
func GcodeName(original string) string {
	base := path.Base(strings.ReplaceAll(original, "\\", "/"))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	name := strings.Trim(b.String(), "._")
	if name == "" {
		name = "print"
	}
	if !IsGcode(name) {
		name += ".gcode"
	}
	return name
}

// IsGcode says whether a file name looks like something a printer can run.
func IsGcode(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".gcode", ".gco", ".g":
		return true
	}
	return false
}

// notGcodeHint explains a refused file in terms of what to do next.
func notGcodeHint(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".stl", ".3mf", ".obj", ".step", ".stp":
		return "that is a model, not something a printer can run — slice it first (OrcaSlicer has a profile for the Neptune 4), then send the G-code"
	case ".bgcode":
		return "that is Prusa's binary G-code, which Klipper and OctoPrint cannot read — export plain G-code from the slicer instead"
	}
	return "that does not look like G-code — the file should end in .gcode"
}
