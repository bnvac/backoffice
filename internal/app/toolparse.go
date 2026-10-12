package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Reading a list of tools the way somebody actually writes one.
//
// Nobody is going to fill in a form forty times to describe their bench. They
// will paste whatever they already have -- a note on their phone, a shopping
// history, a list with bullets and headings and half-remembered model numbers.
// This turns that into rows, and then the import screen shows what it made of
// it so a bad guess can be fixed before anything is saved. Interpreting without
// showing the interpretation would just be a different way of being wrong.

// ParseToolList reads free text into tools. It never fails: a line it cannot
// classify becomes a tool of kind "other" rather than being dropped, because
// the person who typed it knows something the classifier does not.
func ParseToolList(raw string) []Tool {
	lines := splitToolLines(raw)

	var out []Tool
	section := "" // a "3D printing:" heading applies to the lines under it
	for _, line := range lines {
		// A blank line closes the section. People group a list under a heading
		// and then start a new run of unheaded lines; without this the heading
		// carries on to the bottom of the paste and mislabels everything.
		if strings.TrimSpace(line) == "" {
			section = ""
			continue
		}
		if heading, ok := sectionHeading(line); ok {
			section = heading
			continue
		}
		name, detail := splitDetail(cleanToolLine(line))
		if name == "" {
			continue
		}
		name, count := stripCount(name)
		if name == "" {
			continue
		}
		kind := classifyTool(name + " " + detail)
		if kind == "other" && section != "" {
			kind = section
		}
		if count > 1 {
			// Two identical irons do not make you able to do anything a single
			// one cannot, so the count is a note rather than a second row.
			detail = strings.TrimSpace(fmt.Sprintf("%d of them %s", count, detail))
		}
		out = append(out, Tool{
			Name:   tidyToolName(name),
			Kind:   kind,
			Detail: detail,
			Raw:    strings.TrimSpace(line),
		})
	}
	return out
}

// splitToolLines decides where one tool ends and the next begins.
//
// A pasted list is either one per line or one line of comma-separated names,
// and guessing wrongly between them mangles either "Rigol DS1054Z, 50MHz" or
// "strippers, crimper, tweezers". So: if the text has real lines, the lines are
// the answer and commas are left alone. Only a single-line paste is split on
// commas, where they are the only separator available.
func splitToolLines(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	var lines []string
	real := 0
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			lines = append(lines, "") // kept: it is what ends a section
			continue
		}
		// A semicolon is a separator in anyone's hand, on any line.
		for _, part := range strings.Split(line, ";") {
			if strings.TrimSpace(part) != "" {
				lines = append(lines, part)
				real++
			}
		}
	}
	if real > 1 {
		return lines
	}
	if real == 0 {
		return nil
	}
	lines = lines[:0]
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	var out []string
	for _, part := range strings.Split(lines[0], ",") {
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

// sectionHeading spots "3D printing:" or "Soldering:" sitting above a group.
func sectionHeading(line string) (string, bool) {
	trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "-*•#"))
	if !strings.HasSuffix(trimmed, ":") {
		return "", false
	}
	head := strings.TrimSpace(strings.TrimSuffix(trimmed, ":"))
	if head == "" || len(strings.Fields(head)) > 3 {
		return "", false
	}
	kind := classifyTool(head)
	if kind == "other" {
		// A heading nobody recognises still ends the previous section rather
		// than letting it bleed downwards.
		return "", true
	}
	return kind, true
}

var (
	bulletPrefix = regexp.MustCompile(`^\s*(?:[-*•·—>]+|\d+[.)]|\[[ xX]\])\s*`)
	countSuffix  = regexp.MustCompile(`(?i)\s*[x×]\s*(\d{1,2})\s*$`)
	countPrefix  = regexp.MustCompile(`(?i)^\s*(\d{1,2})\s*[x×]\s+`)
	volumeRe     = regexp.MustCompile(`(?i)(\d{2,4}(?:\.\d+)?)\s*[x×*]\s*(\d{2,4}(?:\.\d+)?)\s*[x×*]\s*(\d{2,4}(?:\.\d+)?)`)
)

// cleanToolLine strips the decoration around a name.
func cleanToolLine(line string) string {
	line = bulletPrefix.ReplaceAllString(line, "")
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "\"'")
	line = strings.TrimRight(line, ".,")
	return strings.Join(strings.Fields(line), " ")
}

// splitDetail pulls the parenthesised or dashed-off part away from the name,
// so "Ender 3 (220x220x250)" keeps its bed size without it becoming the name.
func splitDetail(line string) (name, detail string) {
	if open := strings.IndexAny(line, "(["); open >= 0 {
		closer := byte(')')
		if line[open] == '[' {
			closer = ']'
		}
		if end := strings.IndexByte(line[open:], closer); end > 0 {
			detail = strings.TrimSpace(line[open+1 : open+end])
			name = strings.TrimSpace(line[:open] + " " + line[open+end+1:])
			return strings.Join(strings.Fields(name), " "), detail
		}
	}
	// " - 50MHz" and " — 4 channel" read as an aside rather than a name.
	for _, sep := range []string{" — ", " – ", " - "} {
		if i := strings.Index(line, sep); i > 0 {
			left, right := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+len(sep):])
			// Only when the right-hand side looks like a note, not a second
			// half of the name: "soldering iron - hakko" should stay together.
			if looksLikeDetail(right) {
				return left, right
			}
		}
	}
	return line, ""
}

// looksLikeDetail is true for things that read as a measurement or a spec.
func looksLikeDetail(s string) bool {
	if s == "" {
		return false
	}
	if volumeRe.MatchString(s) {
		return true
	}
	lower := strings.ToLower(s)
	for _, unit := range []string{"mm", "cm", "inch", "\"", "mhz", "ghz", "khz", "watt", "w ",
		"v ", "amp", "channel", "ch ", "litre", "liter"} {
		if strings.Contains(lower, unit) {
			return true
		}
	}
	return false
}

// stripCount takes "2x soldering iron" or "soldering iron x2" apart into the
// name and how many there are.
func stripCount(name string) (string, int) {
	n := 1
	if m := countPrefix.FindStringSubmatch(name); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 1 && v <= 20 {
			n = v
		}
		name = countPrefix.ReplaceAllString(name, "")
	} else if m := countSuffix.FindStringSubmatch(name); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 1 && v <= 20 {
			n = v
		}
		name = countSuffix.ReplaceAllString(name, "")
	}
	return strings.TrimSpace(name), n
}

// tidyToolName capitalises a name that was typed entirely in lower case, and
// otherwise leaves it alone -- "TS100" and "Prusa MK4" are already right.
func tidyToolName(name string) string {
	if name == "" {
		return name
	}
	first, _, _ := strings.Cut(name, " ")
	for _, r := range first {
		if !unicode.IsLower(r) {
			return name // TS100, FX888D, MK4: already spelled deliberately
		}
	}
	r := []rune(name)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// parseVolume finds a bed size written any of the usual ways.
func parseVolume(s string) Volume {
	m := volumeRe.FindStringSubmatch(s)
	if m == nil {
		return Volume{}
	}
	x, _ := strconv.ParseFloat(m[1], 64)
	y, _ := strconv.ParseFloat(m[2], 64)
	z, _ := strconv.ParseFloat(m[3], 64)
	return Volume{X: x, Y: y, Z: z}
}

// toolKeywords maps what people write onto a kind. Order matters: the first
// match wins, so the specific entries have to come before the general ones --
// "hot air rework" is SMD work, and would otherwise be caught by "air".
var toolKeywords = []struct {
	kind  string
	words []string
}{
	{"smd", []string{"hot air", "hotair", "rework", "reflow", "hot plate", "hotplate",
		"solder paste", "stencil", "pick and place", "preheater", "858d", "smd"}},
	{"labelling", []string{"label printer", "labeller", "labeler", "brother pt",
		"ptouch", "p-touch", "dymo", "niimbot", "thermal printer", "labels"}},
	{"3d-printer", []string{"3d printer", "3dprinter", "3d-printer", "prusa", "ender",
		"bambu", "creality", "anycubic", "elegoo", "voron", "ultimaker", "sovol",
		"artillery", "flashforge", "qidi", "resin printer", "sla printer", "fdm",
		"mk3", "mk3s", "mk4", "core one", "p1s", "p1p", "x1c", "a1 mini", "mars ",
		"saturn ", "photon", "printer"}},
	{"laser", []string{"laser cutter", "laser engraver", "glowforge", "xtool", "k40",
		"co2 laser", "diode laser", "lasercutter", "laser"}},
	{"cnc", []string{"cnc", "shapeoko", "3018", "milling machine", "mill", "lathe",
		"router table", "onefinity", "carvera"}},
	{"test", []string{"oscilloscope", "scope", "logic analyzer", "logic analyser",
		"spectrum analyzer", "function generator", "signal generator", "rigol",
		"saleae", "nanovna", "vna", "frequency counter", "ds1054", "tinysa"}},
	{"measurement", []string{"caliper", "calliper", "micrometer", "dial indicator",
		"depth gauge", "feeler gauge", "steel rule", "tape measure", "scale",
		"multimeter", "dmm", "clamp meter", "thermal camera", "thermometer",
		"lcr meter", "component tester", "fluke", "esr meter"}},
	{"power", []string{"bench power supply", "bench supply", "power supply", "psu",
		"variable supply", "electronic load", "e-load", "dc load", "battery charger",
		"isolation transformer", "variac"}},
	{"programming", []string{"j-link", "jlink", "st-link", "stlink", "debug probe",
		"jtag", "swd", "usb-ttl", "usb to serial", "ftdi", "ch340", "bus pirate",
		"programmer", "usbasp", "picokit", "pickit", "flasher", "eeprom reader"}},
	{"soldering", []string{"soldering iron", "solder station", "soldering station",
		"hakko", "weller", "pinecil", "ts100", "ts80", "fx888", "fx-888", "desolder",
		"solder sucker", "solder wick", "flux", "helping hands", "fume extractor",
		"soldering", "solder"}},
	{"fabrication", []string{"drill press", "pillar drill", "band saw", "bandsaw",
		"table saw", "jigsaw", "hacksaw", "angle grinder", "bench grinder", "sander",
		"belt sander", "welder", "vice", "vise", "bench vice", "step drill",
		"hole saw", "tap and die", "cordless drill", "drill"}},
	{"optics", []string{"microscope", "magnifier", "magnifying", "loupe", "usb scope camera",
		"headband magnifier", "amscope"}},
	{"hand", []string{"screwdriver", "hex key", "allen key", "torx", "plier", "pliers",
		"side cutter", "flush cutter", "snips", "wire stripper", "strippers", "crimp",
		"crimper", "ferrule", "tweezer", "spudger", "hobby knife", "craft knife",
		"scalpel", "heat gun", "hot glue", "glue gun", "clamp", "file set", "deburr",
		"wire wrap", "third hand", "anti-static", "esd strap", "mat", "torque driver",
		"spanner", "wrench", "socket set", "punch", "awl", "ruler", "square"}},
}

// classifyTool decides what kind of tool a line describes.
func classifyTool(text string) string {
	lower := " " + strings.ToLower(text) + " "
	for _, group := range toolKeywords {
		for _, word := range group.words {
			if strings.Contains(lower, word) {
				return group.kind
			}
		}
	}
	return "other"
}

// ToolKinds is the vocabulary offered in the correction dropdown, in the same
// order the workshop page groups them.
func ToolKinds() []struct{ Slug, Label string } {
	out := make([]struct{ Slug, Label string }, 0, len(toolKindOrder))
	for _, kind := range toolKindOrder {
		out = append(out, struct{ Slug, Label string }{kind, toolKindLabels[kind]})
	}
	return out
}
