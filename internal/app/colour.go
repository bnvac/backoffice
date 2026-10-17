package app

import (
	"math"
	"strconv"
	"strings"
)

// Filament comes in colours, and "which red?" is the question at the printer.
// A spool keeps a name ("Galaxy Black") and a hex value for the swatch; the
// hex is what lets the app filter by colour family and match the colour a
// slicer wrote into a G-code file against the spools on the shelf.

// normaliseHex accepts "#f80", "F80", "#FF8800" or "ff8800ff" (a slicer's
// RGBA) and returns "#ff8800", or "" for anything that is not a colour.
func normaliseHex(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "#")
	switch len(s) {
	case 3:
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	case 8:
		s = s[:6] // drop alpha
	case 6:
	default:
		return ""
	}
	if _, err := strconv.ParseUint(s, 16, 32); err != nil {
		return ""
	}
	return "#" + s
}

func hexRGB(hex string) (r, g, b float64, ok bool) {
	hex = normaliseHex(hex)
	if hex == "" {
		return 0, 0, 0, false
	}
	v, _ := strconv.ParseUint(hex[1:], 16, 32)
	return float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff), true
}

// ColourFamily buckets a colour into the words people filter by. Low
// saturation goes by lightness; everything else by hue, with brown split off
// from orange by being dark.
func ColourFamily(hex string) string {
	r, g, b, ok := hexRGB(hex)
	if !ok {
		return ""
	}
	r, g, b = r/255, g/255, b/255
	max, min := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l := (max + min) / 2
	d := max - min
	s := 0.0
	if d > 0 {
		s = d / (1 - math.Abs(2*l-1))
	}

	switch {
	case l < 0.13:
		return "black"
	case l > 0.9 && s < 0.5:
		return "white"
	case s < 0.15:
		if l > 0.75 {
			return "white"
		}
		return "grey"
	}

	var h float64
	switch max {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}

	switch {
	case h < 15 || h >= 345:
		if l > 0.7 {
			return "pink"
		}
		return "red"
	case h < 45:
		if l < 0.4 {
			return "brown"
		}
		return "orange"
	case h < 70:
		if l < 0.35 {
			return "brown"
		}
		return "yellow"
	case h < 165:
		return "green"
	case h < 255:
		return "blue"
	case h < 290:
		return "purple"
	default:
		return "pink"
	}
}

// ColourFamilies is the filter row, in spectrum order with the neutrals last,
// and a representative swatch for each chip.
var ColourFamilies = []struct{ Key, Swatch string }{
	{"red", "#d23b3b"}, {"orange", "#ef8a2b"}, {"yellow", "#f2cc35"},
	{"green", "#3e9e57"}, {"blue", "#3466c2"}, {"purple", "#7d4dbb"},
	{"pink", "#ec7fb4"}, {"brown", "#7a5233"},
	{"black", "#1c1c1c"}, {"grey", "#8a8d91"}, {"white", "#f4f4f2"},
}

// ColourPresets are the colours filament is actually sold in, so adding a
// spool is a click on a swatch rather than a hex lookup. Names are generic on
// purpose; every brand calls its black something else.
var ColourPresets = []struct{ Name, Hex string }{
	{"Black", "#1a1a1a"}, {"White", "#f5f5f2"}, {"Grey", "#8c8f93"}, {"Silver", "#b8bcc2"},
	{"Red", "#c8202f"}, {"Orange", "#f07c1c"}, {"Yellow", "#f5c518"}, {"Green", "#2f9e44"},
	{"Blue", "#1f5fbf"}, {"Light blue", "#69b3e7"}, {"Purple", "#6a3fa0"}, {"Pink", "#ef7fb0"},
	{"Brown", "#6b4423"}, {"Natural", "#e8e0c8"}, {"Olive", "#6b7a3a"}, {"Teal", "#178a8a"},
}

// --- matching -----------------------------------------------------------------

// lab converts sRGB to CIELAB, where straight-line distance roughly tracks how
// different two colours look. In plain RGB a dark blue and a black are further
// apart than two visibly different greens.
func lab(hex string) (l, a, b float64, ok bool) {
	r, g, bl, ok := hexRGB(hex)
	if !ok {
		return 0, 0, 0, false
	}
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	R, G, B := lin(r), lin(g), lin(bl)
	x := (0.4124*R + 0.3576*G + 0.1805*B) / 0.95047
	y := 0.2126*R + 0.7152*G + 0.0722*B
	z := (0.0193*R + 0.1192*G + 0.9505*B) / 1.08883
	f := func(t float64) float64 {
		if t > 0.008856 {
			return math.Cbrt(t)
		}
		return 7.787*t + 16.0/116
	}
	fx, fy, fz := f(x), f(y), f(z)
	return 116*fy - 16, 500 * (fx - fy), 200 * (fy - fz), true
}

// ColourDistance is ΔE76: about 2 is the smallest difference most people see,
// and anything under 10 reads as "the same colour, different batch".
func ColourDistance(a, b string) float64 {
	l1, a1, b1, ok1 := lab(a)
	l2, a2, b2, ok2 := lab(b)
	if !ok1 || !ok2 {
		return math.Inf(1)
	}
	return math.Sqrt((l1-l2)*(l1-l2) + (a1-a2)*(a1-a2) + (b1-b2)*(b1-b2))
}

// TextOn picks black or white text for a swatch, by luminance.
func TextOn(hex string) string {
	r, g, b, ok := hexRGB(hex)
	if !ok {
		return "#000"
	}
	if 0.299*r+0.587*g+0.114*b > 150 {
		return "#111"
	}
	return "#fff"
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
