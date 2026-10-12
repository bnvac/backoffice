package app

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Bench calculators. Every one of them is the sum you would otherwise do on a
// phone in the middle of wiring something up, and every one that lands on a
// component value then asks the inventory whether you already own it -- which
// is the part a website calculator cannot do.

// --- component values -------------------------------------------------------

// unitScale maps SI prefixes onto multipliers. Both the micro sign and the
// letter u are accepted because keyboards mostly cannot produce the former.
var unitScale = map[string]float64{
	"p": 1e-12, "n": 1e-9, "u": 1e-6, "µ": 1e-6, "μ": 1e-6,
	"m": 1e-3, "": 1, "k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9,
}

// ParseComponentValue reads the ways people write a part value: "4.7k", "4k7",
// "220R", "0.1uF", "10 kΩ", "100nF", "1M5". It returns the value in base units
// and which quantity it is ("R", "F", "H", or "" when the unit is not stated).
func ParseComponentValue(raw string) (float64, string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, "", false
	}
	s = strings.NewReplacer("Ω", "R", "ohm", "R", "Ohm", "R", "OHM", "R", "ohms", "R").Replace(s)
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return 0, "", false
	}

	// Trailing unit letter, e.g. "100nF" or "10uH".
	unit := ""
	switch last := s[len(s)-1]; last {
	case 'F', 'f':
		unit, s = "F", s[:len(s)-1]
	case 'H', 'h':
		unit, s = "H", s[:len(s)-1]
	case 'R':
		unit = "R"
		s = s[:len(s)-1]
		// "220R" ends in the unit; "4R7" uses it as the decimal point.
		if s == "" {
			return 0, "", false
		}
	}
	if s == "" {
		return 0, "", false
	}

	// The prefix-as-decimal-point form: 4k7, 1M5, 2R2, 4u7.
	for prefix, scale := range unitScale {
		if prefix == "" {
			continue
		}
		if i := strings.Index(s, prefix); i > 0 && i < len(s)-1 {
			whole, frac := s[:i], s[i+1:]
			if isDigits(whole) && isDigits(frac) {
				v, err := strconv.ParseFloat(whole+"."+frac, 64)
				if err == nil {
					// 4k7 is a resistor and 4u7 is a capacitor; nobody writes a
					// resistor in microhms or a capacitor in kilofarads.
					if unit == "" {
						switch prefix {
						case "k", "K", "M", "G":
							unit = "R"
						case "p", "n", "u", "µ", "μ":
							unit = "F"
						}
					}
					return v * scale, unit, true
				}
			}
		}
	}
	if i := strings.IndexAny(s, "R"); i > 0 && i < len(s)-1 {
		if whole, frac := s[:i], s[i+1:]; isDigits(whole) && isDigits(frac) {
			if v, err := strconv.ParseFloat(whole+"."+frac, 64); err == nil {
				return v, "R", true
			}
		}
	}

	// The ordinary form: digits then an optional prefix.
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.' || s[end] == '-') {
		end++
	}
	if end == 0 {
		return 0, "", false
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0, "", false
	}
	scale, ok := unitScale[s[end:]]
	if !ok {
		return 0, "", false
	}
	if unit == "" && s[end:] != "" && s[end:] != "m" {
		unit = "R" // a bare "10k" in an inventory is a resistor
	}
	return v * scale, unit, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FormatOhms renders a resistance the way it would be written on a schematic.
func FormatOhms(v float64) string { return formatSI(v, "Ω") }

// FormatFarads renders a capacitance, preferring the prefix a part is sold in.
func FormatFarads(v float64) string { return formatSI(v, "F") }

func formatSI(v float64, unit string) string {
	if v == 0 {
		return "0 " + unit
	}
	abs := math.Abs(v)
	prefixes := []struct {
		scale  float64
		symbol string
	}{
		{1e9, "G"}, {1e6, "M"}, {1e3, "k"}, {1, ""},
		{1e-3, "m"}, {1e-6, "µ"}, {1e-9, "n"}, {1e-12, "p"},
	}
	for _, p := range prefixes {
		if abs >= p.scale*0.999 {
			return trimZeros(v/p.scale) + " " + p.symbol + unit
		}
	}
	return trimZeros(v/1e-12) + " p" + unit
}

func trimZeros(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// E24 is the standard resistor series most through-hole and 1% parts come in.
// E12 is a subset of it, which is why only one table is needed.
var E24 = []float64{
	1.0, 1.1, 1.2, 1.3, 1.5, 1.6, 1.8, 2.0, 2.2, 2.4, 2.7, 3.0,
	3.3, 3.6, 3.9, 4.3, 4.7, 5.1, 5.6, 6.2, 6.8, 7.5, 8.2, 9.1,
}

// NearestStandard rounds a computed value to the closest one you can buy.
func NearestStandard(v float64) float64 {
	if v <= 0 {
		return 0
	}
	decade := math.Pow(10, math.Floor(math.Log10(v)))
	best, bestErr := 0.0, math.Inf(1)
	// The decade above is a candidate too, because the top of a decade is
	// nearer the next one than it is to 9.1. Candidates are visited in
	// ascending order and ties take the later one, so a value exactly between
	// two standard parts rounds up -- for a series resistor that errs towards
	// less current, which is the safe direction.
	for _, mult := range []float64{decade, decade * 10} {
		for _, e := range E24 {
			cand := e * mult
			if err := math.Abs(cand - v); err <= bestErr {
				best, bestErr = cand, err
			}
		}
	}
	return best
}

// --- what you already own ---------------------------------------------------

// ValueMatch is an inventory item close to a value a calculator produced.
type ValueMatch struct {
	Item    Item
	Value   float64
	Percent float64 // how far off the target it is
}

func (m ValueMatch) Off() string {
	if math.Abs(m.Percent) < 0.05 {
		return "exact"
	}
	return fmt.Sprintf("%+.1f%%", m.Percent)
}

// MatchStock finds items whose value is within tolerance of the target, closest
// first. unit is "R" or "F"; items whose value does not parse are skipped
// rather than guessed at.
func MatchStock(items []Item, target float64, unit string, tolerance float64, limit int) []ValueMatch {
	if target <= 0 {
		return nil
	}
	var out []ValueMatch
	for _, it := range items {
		v, u, ok := ParseComponentValue(it.Value)
		if !ok || v <= 0 {
			continue
		}
		// An unlabelled value is only a candidate for resistance, where a bare
		// number is the convention.
		if u != unit && !(u == "" && unit == "R") {
			continue
		}
		off := (v - target) / target * 100
		if math.Abs(off) > tolerance {
			continue
		}
		out = append(out, ValueMatch{Item: it, Value: v, Percent: off})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return math.Abs(out[i].Percent) < math.Abs(out[j].Percent)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// --- the calculators --------------------------------------------------------

// CalcResult is what a calculator returns: the answer in words, the numbers
// worth showing, and optionally a component value to go looking for on the
// shelf.
type CalcResult struct {
	Headline string
	Rows     []CalcRow
	Warnings []string

	// Set when the answer is a component you might own.
	Target     float64
	TargetUnit string // "R" or "F"
	Matches    []ValueMatch
}

type CalcRow struct {
	Label string
	Value string
}

func (r *CalcResult) add(label, value string) {
	r.Rows = append(r.Rows, CalcRow{Label: label, Value: value})
}

func (r *CalcResult) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Calculator describes one calculator for the tools page: its inputs, and the
// function that turns them into an answer.
type Calculator struct {
	Key    string
	Name   string
	Blurb  string
	Inputs []CalcInput
	Run    func(in CalcInputs) (CalcResult, error)
}

type CalcInput struct {
	Key         string
	Label       string
	Unit        string
	Placeholder string
	Options     []string // renders a <select> instead of a number field
}

// CalcInputs is the submitted form, already trimmed.
type CalcInputs map[string]string

// Num reads a numeric field, tolerating engineering notation so "4k7" works
// wherever a plain number does.
func (in CalcInputs) Num(key string) (float64, bool) {
	raw := strings.TrimSpace(in[key])
	if raw == "" {
		return 0, false
	}
	if v, err := strconv.ParseFloat(raw, 64); err == nil {
		return v, true
	}
	if v, _, ok := ParseComponentValue(raw); ok {
		return v, true
	}
	return 0, false
}

func (in CalcInputs) require(keys ...string) (map[string]float64, error) {
	out := make(map[string]float64, len(keys))
	var missing []string
	for _, k := range keys {
		v, ok := in.Num(k)
		if !ok {
			missing = append(missing, k)
			continue
		}
		out[k] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("fill in %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Calculators is the catalogue rendered on the tools page.
var Calculators = []Calculator{
	{
		Key:   "led",
		Name:  "LED series resistor",
		Blurb: "The resistor that keeps an LED at the current it is rated for.",
		Inputs: []CalcInput{
			{Key: "vs", Label: "Supply", Unit: "V", Placeholder: "5"},
			{Key: "vf", Label: "LED forward voltage", Unit: "V", Placeholder: "2.0"},
			{Key: "i", Label: "Current", Unit: "mA", Placeholder: "10"},
		},
		Run: func(in CalcInputs) (CalcResult, error) {
			v, err := in.require("vs", "vf", "i")
			if err != nil {
				return CalcResult{}, err
			}
			if v["i"] <= 0 {
				return CalcResult{}, fmt.Errorf("current has to be more than zero")
			}
			if v["vf"] >= v["vs"] {
				return CalcResult{}, fmt.Errorf("the LED needs %.2f V but the supply is %.2f V — it will not light", v["vf"], v["vs"])
			}
			amps := v["i"] / 1000
			r := (v["vs"] - v["vf"]) / amps
			std := NearestStandard(r)
			actual := (v["vs"] - v["vf"]) / std * 1000

			res := CalcResult{
				Headline:   FormatOhms(std),
				Target:     std,
				TargetUnit: "R",
			}
			res.add("Exact value", FormatOhms(r))
			res.add("Nearest E24", FormatOhms(std))
			res.add("Current with that resistor", fmt.Sprintf("%.1f mA", actual))
			res.add("Dissipated in the resistor", fmt.Sprintf("%.3f W", (v["vs"]-v["vf"])*actual/1000))
			if p := (v["vs"] - v["vf"]) * actual / 1000; p > 0.2 {
				res.warn("that is %.2f W — a 1/4 W resistor is marginal, use 1/2 W or more", p)
			}
			return res, nil
		},
	},
	{
		Key:   "divider",
		Name:  "Resistor divider",
		Blurb: "Scale a voltage down, and check the current it wastes doing so.",
		Inputs: []CalcInput{
			{Key: "vin", Label: "Input", Unit: "V", Placeholder: "12"},
			{Key: "r1", Label: "R1 (top)", Unit: "Ω", Placeholder: "10k"},
			{Key: "r2", Label: "R2 (bottom)", Unit: "Ω", Placeholder: "3k3"},
			{Key: "vout", Label: "…or the output you want", Unit: "V", Placeholder: "3.3"},
		},
		Run: func(in CalcInputs) (CalcResult, error) {
			vin, ok := in.Num("vin")
			if !ok || vin == 0 {
				return CalcResult{}, fmt.Errorf("give the input voltage")
			}
			r1, hasR1 := in.Num("r1")
			r2, hasR2 := in.Num("r2")
			vout, hasVout := in.Num("vout")

			var res CalcResult
			switch {
			case hasR1 && hasR2:
				if r1+r2 <= 0 {
					return CalcResult{}, fmt.Errorf("the resistors add up to zero")
				}
				out := vin * r2 / (r1 + r2)
				current := vin / (r1 + r2)
				res.Headline = fmt.Sprintf("%.3f V out", out)
				res.add("Output", fmt.Sprintf("%.4f V", out))
				res.add("Divider current", fmt.Sprintf("%.3f mA", current*1000))
				res.add("Wasted continuously", fmt.Sprintf("%.1f mW", vin*current*1000))
				res.add("Source impedance", FormatOhms(r1*r2/(r1+r2)))
				if current*1000 < 0.01 {
					res.warn("under 10 µA through the divider — an ADC input will load it and read low")
				}
			case hasVout && hasR1:
				if vout >= vin {
					return CalcResult{}, fmt.Errorf("a divider can only go down: %.2f V out of %.2f V in is not possible", vout, vin)
				}
				want := r1 * vout / (vin - vout)
				std := NearestStandard(want)
				res.Headline = "R2 = " + FormatOhms(std)
				res.Target, res.TargetUnit = std, "R"
				res.add("Exact R2", FormatOhms(want))
				res.add("Nearest E24", FormatOhms(std))
				res.add("Output with that pair", fmt.Sprintf("%.3f V", vin*std/(r1+std)))
				res.add("Divider current", fmt.Sprintf("%.3f mA", vin/(r1+std)*1000))
			default:
				return CalcResult{}, fmt.Errorf("give R1 and R2, or R1 and the output voltage you want")
			}
			return res, nil
		},
	},
	{
		Key:   "rc",
		Name:  "RC filter",
		Blurb: "Corner frequency of a resistor and capacitor, either way round.",
		Inputs: []CalcInput{
			{Key: "r", Label: "Resistor", Unit: "Ω", Placeholder: "10k"},
			{Key: "c", Label: "Capacitor", Unit: "F", Placeholder: "100n"},
			{Key: "f", Label: "…or the corner you want", Unit: "Hz", Placeholder: "1000"},
		},
		Run: func(in CalcInputs) (CalcResult, error) {
			r, hasR := in.Num("r")
			c, hasC := in.Num("c")
			f, hasF := in.Num("f")

			var res CalcResult
			switch {
			case hasR && hasC:
				if r <= 0 || c <= 0 {
					return CalcResult{}, fmt.Errorf("both values have to be more than zero")
				}
				fc := 1 / (2 * math.Pi * r * c)
				res.Headline = fmt.Sprintf("%s corner", formatSI(fc, "Hz"))
				res.add("Corner frequency", formatSI(fc, "Hz"))
				res.add("Time constant", formatSI(r*c, "s"))
				res.add("Settles (5τ)", formatSI(5*r*c, "s"))
			case hasF && hasC:
				if f <= 0 || c <= 0 {
					return CalcResult{}, fmt.Errorf("both values have to be more than zero")
				}
				want := 1 / (2 * math.Pi * f * c)
				std := NearestStandard(want)
				res.Headline = "R = " + FormatOhms(std)
				res.Target, res.TargetUnit = std, "R"
				res.add("Exact R", FormatOhms(want))
				res.add("Nearest E24", FormatOhms(std))
				res.add("Corner with that resistor", formatSI(1/(2*math.Pi*std*c), "Hz"))
			case hasF && hasR:
				if f <= 0 || r <= 0 {
					return CalcResult{}, fmt.Errorf("both values have to be more than zero")
				}
				want := 1 / (2 * math.Pi * f * r)
				res.Headline = "C = " + FormatFarads(want)
				res.Target, res.TargetUnit = want, "F"
				res.add("Exact C", FormatFarads(want))
				res.add("Corner", formatSI(f, "Hz"))
			default:
				return CalcResult{}, fmt.Errorf("give any two of resistor, capacitor and frequency")
			}
			return res, nil
		},
	},
	{
		Key:   "trace",
		Name:  "PCB trace width",
		Blurb: "How wide a track has to be to carry a current without cooking (IPC-2221).",
		Inputs: []CalcInput{
			{Key: "i", Label: "Current", Unit: "A", Placeholder: "2"},
			{Key: "rise", Label: "Allowed temperature rise", Unit: "°C", Placeholder: "10"},
			{Key: "copper", Label: "Copper weight", Unit: "oz", Placeholder: "1"},
			{Key: "layer", Label: "Layer", Options: []string{"external", "internal"}},
		},
		Run: func(in CalcInputs) (CalcResult, error) {
			v, err := in.require("i", "rise", "copper")
			if err != nil {
				return CalcResult{}, err
			}
			if v["i"] <= 0 || v["rise"] <= 0 || v["copper"] <= 0 {
				return CalcResult{}, fmt.Errorf("current, rise and copper weight all have to be more than zero")
			}
			// IPC-2221: A = (I / (k * dT^0.44))^(1/0.725), area in square mils.
			k := 0.048
			layer := in["layer"]
			if layer == "internal" {
				k = 0.024
			}
			area := math.Pow(v["i"]/(k*math.Pow(v["rise"], 0.44)), 1/0.725)
			thicknessMils := 1.378 * v["copper"] // 1 oz of copper is 1.378 mil
			widthMils := area / thicknessMils

			res := CalcResult{Headline: fmt.Sprintf("%.1f mil wide", widthMils)}
			res.add("Width", fmt.Sprintf("%.1f mil (%.2f mm)", widthMils, widthMils*0.0254))
			res.add("Cross-section", fmt.Sprintf("%.0f mil²", area))
			res.add("Copper thickness", fmt.Sprintf("%.2f mil", thicknessMils))
			if layer == "internal" {
				res.warn("internal layers cannot shed heat, hence roughly double the width")
			}
			if v["i"] > 10 {
				res.warn("above 10 A the IPC curve is extrapolated — treat this as a lower bound")
			}
			return res, nil
		},
	},
	{
		Key:   "ohm",
		Name:  "Ohm's law",
		Blurb: "Any two of volts, amps, ohms and watts give the other two.",
		Inputs: []CalcInput{
			{Key: "v", Label: "Voltage", Unit: "V"},
			{Key: "i", Label: "Current", Unit: "A"},
			{Key: "r", Label: "Resistance", Unit: "Ω"},
			{Key: "p", Label: "Power", Unit: "W"},
		},
		Run: func(in CalcInputs) (CalcResult, error) {
			v, hasV := in.Num("v")
			i, hasI := in.Num("i")
			r, hasR := in.Num("r")
			p, hasP := in.Num("p")

			switch {
			case hasV && hasI:
				r, p = safeDiv(v, i), v*i
			case hasV && hasR:
				i = safeDiv(v, r)
				p = v * i
			case hasV && hasP:
				i = safeDiv(p, v)
				r = safeDiv(v, i)
			case hasI && hasR:
				v = i * r
				p = v * i
			case hasI && hasP:
				v = safeDiv(p, i)
				r = safeDiv(v, i)
			case hasR && hasP:
				i = math.Sqrt(safeDiv(p, r))
				v = i * r
			default:
				return CalcResult{}, fmt.Errorf("give any two values")
			}
			if math.IsInf(r, 0) || math.IsNaN(r) {
				return CalcResult{}, fmt.Errorf("those values divide by zero")
			}
			res := CalcResult{Headline: fmt.Sprintf("%.4g V · %.4g A · %s · %.4g W", v, i, FormatOhms(r), p)}
			res.add("Voltage", fmt.Sprintf("%.5g V", v))
			res.add("Current", fmt.Sprintf("%.5g A (%.4g mA)", i, i*1000))
			res.add("Resistance", FormatOhms(r))
			res.add("Power", fmt.Sprintf("%.5g W", p))
			if r > 0 && !math.IsInf(r, 0) {
				res.Target, res.TargetUnit = NearestStandard(r), "R"
			}
			return res, nil
		},
	},
	{
		Key:   "regulator",
		Name:  "Linear regulator heat",
		Blurb: "What a linear regulator burns off, and whether it needs a heatsink.",
		Inputs: []CalcInput{
			{Key: "vin", Label: "Input", Unit: "V", Placeholder: "12"},
			{Key: "vout", Label: "Output", Unit: "V", Placeholder: "5"},
			{Key: "i", Label: "Load current", Unit: "mA", Placeholder: "500"},
		},
		Run: func(in CalcInputs) (CalcResult, error) {
			v, err := in.require("vin", "vout", "i")
			if err != nil {
				return CalcResult{}, err
			}
			if v["vout"] > v["vin"] {
				return CalcResult{}, fmt.Errorf("a linear regulator cannot step up")
			}
			amps := v["i"] / 1000
			burned := (v["vin"] - v["vout"]) * amps
			delivered := v["vout"] * amps

			res := CalcResult{Headline: fmt.Sprintf("%.2f W as heat", burned)}
			res.add("Dissipated as heat", fmt.Sprintf("%.3f W", burned))
			res.add("Delivered to the load", fmt.Sprintf("%.3f W", delivered))
			if drawn := v["vin"] * amps; drawn > 0 {
				res.add("Efficiency", fmt.Sprintf("%.0f%%", delivered/drawn*100))
			}
			res.add("Rise on a bare TO-220 (~60 °C/W)", fmt.Sprintf("%.0f °C above ambient", burned*60))
			switch {
			case burned > 2:
				res.warn("over 2 W — this needs a real heatsink, or a switching regulator instead")
			case burned > 1:
				res.warn("over 1 W — a bare TO-220 will be too hot to touch")
			}
			return res, nil
		},
	},
	{
		Key:   "battery",
		Name:  "Battery life",
		Blurb: "How long a pack lasts at a given draw, including a duty cycle.",
		Inputs: []CalcInput{
			{Key: "cap", Label: "Capacity", Unit: "mAh", Placeholder: "2000"},
			{Key: "active", Label: "Active draw", Unit: "mA", Placeholder: "80"},
			{Key: "sleep", Label: "Sleep draw", Unit: "mA", Placeholder: "0.05"},
			{Key: "duty", Label: "Awake", Unit: "%", Placeholder: "5"},
		},
		Run: func(in CalcInputs) (CalcResult, error) {
			v, err := in.require("cap", "active")
			if err != nil {
				return CalcResult{}, err
			}
			sleep, _ := in.Num("sleep")
			duty, hasDuty := in.Num("duty")
			if !hasDuty {
				duty = 100
			}
			if duty < 0 || duty > 100 {
				return CalcResult{}, fmt.Errorf("the duty cycle is a percentage between 0 and 100")
			}
			avg := v["active"]*duty/100 + sleep*(100-duty)/100
			if avg <= 0 {
				return CalcResult{}, fmt.Errorf("average draw works out as zero")
			}
			hours := v["cap"] / avg

			res := CalcResult{Headline: humanHours(hours)}
			res.add("Average draw", fmt.Sprintf("%.3f mA", avg))
			res.add("Runtime", humanHours(hours))
			res.add("Runtime at 80% usable capacity", humanHours(hours*0.8))
			if duty < 100 {
				res.add("Always-on equivalent", humanHours(v["cap"]/v["active"]))
			}
			res.warn("real cells lose capacity in the cold and as they age — treat this as the optimistic figure")
			return res, nil
		},
	},
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return math.Inf(1)
	}
	return a / b
}

func humanHours(h float64) string {
	switch {
	case h < 1:
		return fmt.Sprintf("%.0f minutes", h*60)
	case h < 48:
		return fmt.Sprintf("%.1f hours", h)
	case h < 24*90:
		return fmt.Sprintf("%.1f days", h/24)
	default:
		return fmt.Sprintf("%.1f months", h/24/30)
	}
}

// CalculatorByKey finds one calculator for the request handler.
func CalculatorByKey(key string) *Calculator {
	for i := range Calculators {
		if Calculators[i].Key == key {
			return &Calculators[i]
		}
	}
	return nil
}
