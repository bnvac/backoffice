package app

import (
	"fmt"
	"html"
	"sort"
	"strings"
)

// The pin map is a table, and a table is the right thing to have on the bench
// next to you. It is the wrong thing for working out whether a plan makes
// sense -- for that you want to see it. This draws the same rows as a picture:
// the controller down the left, everything hanging off it on the right, and a
// coloured line for each wire.
//
// The colours are not decoration. They are the convention you would use with
// real wire -- red for power, black for ground, yellow and blue for the I2C
// pair -- so that the drawing and the thing on the desk agree with each other.

// wireColour returns the colour for a signal, following bench convention.
func wireColour(pin, signal string) string {
	s := strings.ToUpper(strings.TrimSpace(signal))
	p := strings.ToUpper(strings.TrimSpace(pin))
	switch {
	case s == "GND" || s == "GROUND" || p == "GND" || p == "GROUND":
		return "#3b3b3b"
	case strings.HasPrefix(s, "3V") || strings.HasPrefix(p, "3V") ||
		s == "VCC" || s == "VDD" || s == "VIN" || p == "VCC" || p == "VIN":
		return "#d23b3b"
	case strings.HasPrefix(s, "5V") || strings.HasPrefix(p, "5V"):
		return "#e07b39"
	case s == "SDA":
		return "#d8a200"
	case s == "SCL" || s == "SCK":
		return "#2f7fd0"
	case s == "MOSI" || s == "TX":
		return "#2f9e5a"
	case s == "MISO" || s == "RX":
		return "#8a5cd0"
	case strings.HasPrefix(s, "CS") || s == "SS":
		return "#c05fa8"
	case s == "INT" || s == "IRQ":
		return "#c8462c"
	}
	return "#7a8794"
}

// wireColourNames give the hex codes a name, because a spreadsheet you print
// and take to the bench is no use if it says "#d23b3b" where you need "red".
var wireColourNames = map[string]string{
	"#3b3b3b": "black", "#d23b3b": "red", "#e07b39": "orange",
	"#d8a200": "yellow", "#2f7fd0": "blue", "#2f9e5a": "green",
	"#8a5cd0": "purple", "#c05fa8": "pink", "#c8462c": "brown",
	"#7a8794": "any",
}

func wireColourName(pin, signal string) string {
	if name, ok := wireColourNames[wireColour(pin, signal)]; ok {
		return name
	}
	return "any"
}

// wireStyle is how the wire is drawn: power and ground are thicker, because on
// a real bench getting those wrong is the mistake that costs a board.
func wireStyle(signal, pin string) (width float64, dashed bool) {
	s := strings.ToUpper(signal + " " + pin)
	switch {
	case strings.Contains(s, "GND"), strings.Contains(s, "3V"), strings.Contains(s, "5V"),
		strings.Contains(s, "VCC"), strings.Contains(s, "VIN"):
		return 3, false
	case strings.Contains(s, "INT"), strings.Contains(s, "IRQ"):
		return 2, true
	}
	return 2, false
}

// wiringNode is one device on the right-hand side of the drawing.
type wiringNode struct {
	Name  string
	Wires []PinAssignment
}

// WiringDiagram renders the project's pin assignments as an SVG.
//
// It returns "" when there is nothing to draw, which the template treats as
// "no diagram yet" rather than as an empty picture.
func WiringDiagram(controller string, wiring []PinAssignment) string {
	if len(wiring) == 0 {
		return ""
	}
	nodes := groupWiring(wiring)
	if controller == "" {
		controller = "Controller"
	}

	const (
		rowH      = 26.0 // one wire
		nodeGap   = 18.0
		padTop    = 46.0
		padBottom = 24.0
		boxW      = 190.0
		leftX     = 30.0
		rightX    = 360.0
		width     = 620.0
	)

	// Height is whatever the rows need: every wire gets its own line, and the
	// controller's pin list is exactly as long as the wire list.
	rows := len(wiring)
	nodeRows := 0.0
	for _, n := range nodes {
		nodeRows += float64(len(n.Wires))*rowH + nodeGap + 14
	}
	nodeRows -= nodeGap // the last box needs no gap after it
	ctrlH := float64(rows)*rowH + 26
	height := padTop + maxFloat(ctrlH, nodeRows) + padBottom

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" `+
		`role="img" aria-label="Wiring diagram" class="wiring-svg">`, width, height)
	b.WriteString(`<style>
	.wd-box { fill: var(--panel, #fff); stroke: var(--line, #ccd); stroke-width: 1.5; rx: 8; }
	.wd-title { font: 600 13px system-ui, sans-serif; fill: var(--text, #111); }
	.wd-pin { font: 11px ui-monospace, monospace; fill: var(--text, #111); }
	.wd-sig { font: 10px system-ui, sans-serif; fill: var(--muted, #667); }
	.wd-clash { fill: #c0392b; font: 600 10px system-ui, sans-serif; }
	</style>`)

	// The controller.
	fmt.Fprintf(&b, `<rect class="wd-box" x="%.0f" y="%.0f" width="%.0f" height="%.0f"/>`,
		leftX, padTop-18, boxW, ctrlH)
	fmt.Fprintf(&b, `<text class="wd-title" x="%.0f" y="%.0f">%s</text>`,
		leftX+12, padTop-24, html.EscapeString(controller))

	// Walk the nodes, drawing each device box and the wires back to the pins.
	// The controller's pins are listed in the same order as the wires leave it,
	// so no line has to cross another unless the assignment order forces it.
	y := padTop
	row := 0
	for _, node := range nodes {
		nodeH := float64(len(node.Wires))*rowH + 16
		fmt.Fprintf(&b, `<rect class="wd-box" x="%.0f" y="%.0f" width="%.0f" height="%.0f"/>`,
			rightX, y-14, boxW+40, nodeH)
		fmt.Fprintf(&b, `<text class="wd-title" x="%.0f" y="%.0f">%s</text>`,
			rightX+12, y-20, html.EscapeString(node.Name))

		for i, w := range node.Wires {
			leftY := padTop + float64(row)*rowH + 6
			rightY := y + float64(i)*rowH + 6
			colour := wireColour(w.Pin, w.Signal)
			stroke, dashed := wireStyle(w.Signal, w.Pin)

			dash := ""
			if dashed {
				dash = ` stroke-dasharray="5 4"`
			}
			// A gentle S-curve reads better than a straight line when the two
			// ends are at different heights, which they usually are.
			midX := (leftX + boxW + rightX) / 2
			fmt.Fprintf(&b, `<path d="M %.0f %.0f C %.0f %.0f, %.0f %.0f, %.0f %.0f" `+
				`fill="none" stroke="%s" stroke-width="%.1f"%s/>`,
				leftX+boxW, leftY-4, midX, leftY-4, midX, rightY-4, rightX, rightY-4,
				colour, stroke, dash)

			fmt.Fprintf(&b, `<text class="wd-pin" x="%.0f" y="%.0f">%s</text>`,
				leftX+12, leftY, html.EscapeString(w.Pin))
			if len(w.Clashes) > 0 {
				fmt.Fprintf(&b, `<text class="wd-clash" x="%.0f" y="%.0f">used twice</text>`,
					leftX+boxW-78, leftY)
			}
			label := w.Signal
			if label == "" {
				label = w.Pin
			}
			fmt.Fprintf(&b, `<text class="wd-sig" x="%.0f" y="%.0f">%s</text>`,
				rightX+12, rightY, html.EscapeString(label))
			row++
		}
		y += nodeH + nodeGap
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// controllerName is what the left-hand box in the diagram is called: the board
// the project is built around when one has been picked, and a plain label when
// nobody has said yet.
func controllerName(p Project) string {
	if p.Controller != nil {
		return p.Controller.Name
	}
	return "Controller"
}

// groupWiring collects the assignments by what they connect to, keeping the
// order the pins were assigned in so the drawing matches the table.
func groupWiring(wiring []PinAssignment) []wiringNode {
	var order []string
	byName := map[string][]PinAssignment{}
	for _, w := range wiring {
		name := w.Where()
		if name == "" {
			name = "Unlabelled"
		}
		if _, seen := byName[name]; !seen {
			order = append(order, name)
		}
		byName[name] = append(byName[name], w)
	}
	out := make([]wiringNode, 0, len(order))
	for _, name := range order {
		out = append(out, wiringNode{Name: name, Wires: byName[name]})
	}
	return out
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// WireLegend is the colour key shown under the diagram, built from the signals
// actually present so it never explains a colour that is not on the drawing.
type WireLegendEntry struct {
	Signal string
	Colour string
}

func WireLegend(wiring []PinAssignment) []WireLegendEntry {
	byColour := map[string]string{}
	for _, w := range wiring {
		label := strings.TrimSpace(w.Signal)
		if label == "" {
			label = strings.TrimSpace(w.Pin)
		}
		if label == "" {
			continue
		}
		colour := wireColour(w.Pin, w.Signal)
		if existing, ok := byColour[colour]; !ok || len(label) < len(existing) {
			byColour[colour] = label
		}
	}
	out := make([]WireLegendEntry, 0, len(byColour))
	for colour, label := range byColour {
		out = append(out, WireLegendEntry{Signal: label, Colour: colour})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signal < out[j].Signal })
	return out
}
