package media

import (
	"fmt"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
)

// A KiCad footprint is the land pattern a part solders onto: where the pads are,
// how big they are, and how much room the body needs. Knowing that a part is
// "0805" tells you nothing you can act on; seeing the pads at scale next to a
// millimetre ruler tells you whether it will fit where you were going to put it.
//
// A .kicad_mod file is an s-expression, so it is parsed here and drawn as SVG
// rather than pulled in as a dependency. That keeps the binary self-contained
// and means the drawing works offline once the file has been fetched once.

// --- s-expressions ----------------------------------------------------------

// sexp is either a leaf token or a list. KiCad files are small enough that a
// tree of these costs nothing.
type sexp struct {
	atom string
	list []*sexp
}

// head is the first atom of a list, which is what names the node: "pad",
// "fp_line", "at" and so on.
func (s *sexp) head() string {
	if len(s.list) > 0 {
		return s.list[0].atom
	}
	return ""
}

// child returns the first sub-list with the given head.
func (s *sexp) child(name string) *sexp {
	for _, c := range s.list {
		if c.head() == name {
			return c
		}
	}
	return nil
}

// children returns every sub-list with the given head.
func (s *sexp) children(name string) []*sexp {
	var out []*sexp
	for _, c := range s.list {
		if c.head() == name {
			out = append(out, c)
		}
	}
	return out
}

// nums reads the numeric arguments of a node: (at 1.5 -2) yields [1.5, -2].
func (s *sexp) nums() []float64 {
	var out []float64
	for _, c := range s.list[1:] {
		if c.list != nil {
			break
		}
		v, err := strconv.ParseFloat(c.atom, 64)
		if err != nil {
			break
		}
		out = append(out, v)
	}
	return out
}

// words reads the non-numeric arguments: (layers F.Cu F.Mask) yields both.
func (s *sexp) words() []string {
	var out []string
	for _, c := range s.list[1:] {
		if c.list == nil && c.atom != "" {
			out = append(out, c.atom)
		}
	}
	return out
}

// point reads a named coordinate pair, e.g. (start -1 0.625).
func (s *sexp) point(name string) (float64, float64, bool) {
	c := s.child(name)
	if c == nil {
		return 0, 0, false
	}
	n := c.nums()
	if len(n) < 2 {
		return 0, 0, false
	}
	return n[0], n[1], true
}

const maxFootprintTokens = 200_000 // a guard against a pathological file

// parseSexp reads one s-expression document. Quoted strings keep their spaces;
// everything else splits on whitespace and parentheses.
func parseSexp(src string) (*sexp, error) {
	pos, tokens := 0, 0
	var parse func() (*sexp, error)

	skipSpace := func() {
		for pos < len(src) {
			switch src[pos] {
			case ' ', '\t', '\r', '\n':
				pos++
			case '#': // not KiCad, but harmless to tolerate
				for pos < len(src) && src[pos] != '\n' {
					pos++
				}
			default:
				return
			}
		}
	}

	parse = func() (*sexp, error) {
		if tokens++; tokens > maxFootprintTokens {
			return nil, fmt.Errorf("footprint is too complicated to draw")
		}
		skipSpace()
		if pos >= len(src) {
			return nil, fmt.Errorf("unexpected end of footprint")
		}
		switch src[pos] {
		case '(':
			pos++
			node := &sexp{list: []*sexp{}}
			for {
				skipSpace()
				if pos >= len(src) {
					return nil, fmt.Errorf("unclosed ( in footprint")
				}
				if src[pos] == ')' {
					pos++
					return node, nil
				}
				child, err := parse()
				if err != nil {
					return nil, err
				}
				node.list = append(node.list, child)
			}
		case '"':
			pos++
			var b strings.Builder
			for pos < len(src) && src[pos] != '"' {
				if src[pos] == '\\' && pos+1 < len(src) {
					pos++
				}
				b.WriteByte(src[pos])
				pos++
			}
			pos++ // closing quote
			return &sexp{atom: b.String()}, nil
		default:
			start := pos
			for pos < len(src) && !strings.ContainsRune(" \t\r\n()", rune(src[pos])) {
				pos++
			}
			return &sexp{atom: src[start:pos]}, nil
		}
	}

	node, err := parse()
	if err != nil {
		return nil, err
	}
	if h := node.head(); h != "module" && h != "footprint" {
		return nil, fmt.Errorf("that is not a KiCad footprint (it starts with %q)", h)
	}
	return node, nil
}

// --- the footprint ----------------------------------------------------------

// Footprint is a parsed land pattern, in millimetres, with y increasing
// downwards exactly as KiCad stores it.
type Footprint struct {
	Name        string
	Description string
	Tags        string
	Source      string // where the file came from
	SMD         bool

	Pads    []FootprintPad
	Strokes []FootprintStroke
}

type FootprintPad struct {
	Number      string
	X, Y        float64
	W, H        float64
	Rotation    float64
	Shape       string // rect, roundrect, oval, circle, custom
	Drill       float64
	DrillW      float64 // oval drills
	ThroughHole bool
}

// FootprintStroke is one line of silkscreen, body outline or courtyard.
type FootprintStroke struct {
	Layer  string // "silk", "fab", "courtyard"
	Points [][2]float64
	Width  float64
	Closed bool
}

// ParseFootprint reads a .kicad_mod file. Both the v5 (module ...) and the v7+
// (footprint ...) spellings are accepted, because people paste files from
// whichever KiCad they have.
func ParseFootprint(src string) (*Footprint, error) {
	root, err := parseSexp(src)
	if err != nil {
		return nil, err
	}
	fp := &Footprint{}
	if len(root.list) > 1 {
		fp.Name = root.list[1].atom
	}
	if d := root.child("descr"); d != nil && len(d.list) > 1 {
		fp.Description = d.list[1].atom
	}
	if t := root.child("tags"); t != nil {
		fp.Tags = strings.Join(t.words(), " ")
	}
	if a := root.child("attr"); a != nil {
		for _, w := range a.words() {
			if w == "smd" {
				fp.SMD = true
			}
		}
	}

	for _, node := range root.list {
		switch node.head() {
		case "pad":
			if pad, ok := parsePad(node); ok {
				fp.Pads = append(fp.Pads, pad)
			}
		case "fp_line", "fp_rect", "fp_circle", "fp_arc", "fp_poly":
			if s, ok := parseStroke(node); ok {
				fp.Strokes = append(fp.Strokes, s...)
			}
		}
	}
	if len(fp.Pads) == 0 && len(fp.Strokes) == 0 {
		return nil, fmt.Errorf("that footprint has no pads or outline to draw")
	}
	sort.SliceStable(fp.Pads, func(i, j int) bool {
		a, _ := strconv.Atoi(fp.Pads[i].Number)
		b, _ := strconv.Atoi(fp.Pads[j].Number)
		return a < b
	})
	return fp, nil
}

func parsePad(node *sexp) (FootprintPad, bool) {
	words := node.words()
	if len(words) < 3 {
		return FootprintPad{}, false
	}
	pad := FootprintPad{Number: words[0], Shape: words[2]}
	pad.ThroughHole = words[1] == "thru_hole" || words[1] == "np_thru_hole"

	if at := node.child("at"); at != nil {
		n := at.nums()
		if len(n) < 2 {
			return FootprintPad{}, false
		}
		pad.X, pad.Y = n[0], n[1]
		if len(n) > 2 {
			pad.Rotation = n[2]
		}
	}
	if size := node.child("size"); size != nil {
		if n := size.nums(); len(n) >= 2 {
			pad.W, pad.H = n[0], n[1]
		}
	}
	if drill := node.child("drill"); drill != nil {
		n := drill.nums()
		switch {
		case len(drill.words()) > 0 && drill.words()[0] == "oval" && len(n) >= 2:
			pad.DrillW, pad.Drill = n[0], n[1]
		case len(n) >= 1:
			pad.Drill = n[0]
		}
	}
	if pad.W <= 0 || pad.H <= 0 {
		return FootprintPad{}, false
	}
	return pad, true
}

// layerClass reduces KiCad's layer names to the three things worth drawing
// differently. Anything else (fabrication notes, user layers) is dropped.
func layerClass(name string) string {
	switch {
	case strings.HasSuffix(name, ".SilkS"):
		return "silk"
	case strings.HasSuffix(name, ".Fab"):
		return "fab"
	case strings.HasSuffix(name, ".CrtYd"):
		return "courtyard"
	}
	return ""
}

func parseStroke(node *sexp) ([]FootprintStroke, bool) {
	layer := ""
	if l := node.child("layer"); l != nil {
		if w := l.words(); len(w) > 0 {
			layer = layerClass(w[0])
		}
	}
	if layer == "" {
		return nil, false
	}
	width := 0.12
	if w := node.child("width"); w != nil {
		if n := w.nums(); len(n) > 0 {
			width = n[0]
		}
	} else if st := node.child("stroke"); st != nil { // KiCad 7+
		if w := st.child("width"); w != nil {
			if n := w.nums(); len(n) > 0 {
				width = n[0]
			}
		}
	}

	base := FootprintStroke{Layer: layer, Width: width}
	switch node.head() {
	case "fp_line":
		sx, sy, ok1 := node.point("start")
		ex, ey, ok2 := node.point("end")
		if !ok1 || !ok2 {
			return nil, false
		}
		base.Points = [][2]float64{{sx, sy}, {ex, ey}}

	case "fp_rect":
		sx, sy, ok1 := node.point("start")
		ex, ey, ok2 := node.point("end")
		if !ok1 || !ok2 {
			return nil, false
		}
		base.Points = [][2]float64{{sx, sy}, {ex, sy}, {ex, ey}, {sx, ey}}
		base.Closed = true

	case "fp_circle":
		cx, cy, ok1 := node.point("center")
		ex, ey, ok2 := node.point("end")
		if !ok1 || !ok2 {
			return nil, false
		}
		base.Points = circlePoints(cx, cy, math.Hypot(ex-cx, ey-cy), 32)
		base.Closed = true

	case "fp_arc":
		pts, ok := arcPoints(node)
		if !ok {
			return nil, false
		}
		base.Points = pts

	case "fp_poly":
		pts := node.child("pts")
		if pts == nil {
			return nil, false
		}
		for _, xy := range pts.children("xy") {
			if n := xy.nums(); len(n) >= 2 {
				base.Points = append(base.Points, [2]float64{n[0], n[1]})
			}
		}
		if len(base.Points) < 2 {
			return nil, false
		}
		base.Closed = true
	}
	return []FootprintStroke{base}, true
}

// arcPoints handles both arc spellings: KiCad 5 gives a centre, a start point
// and a swept angle; KiCad 7 gives three points on the arc.
func arcPoints(node *sexp) ([][2]float64, bool) {
	if mx, my, ok := node.point("mid"); ok {
		sx, sy, ok1 := node.point("start")
		ex, ey, ok2 := node.point("end")
		if !ok1 || !ok2 {
			return nil, false
		}
		cx, cy, r, ok := circleThrough(sx, sy, mx, my, ex, ey)
		if !ok {
			return [][2]float64{{sx, sy}, {mx, my}, {ex, ey}}, true
		}
		a0 := math.Atan2(sy-cy, sx-cx)
		a1 := math.Atan2(my-cy, mx-cx)
		a2 := math.Atan2(ey-cy, ex-cx)
		return arcSweep(cx, cy, r, a0, normaliseSweep(a0, a1, a2)), true
	}

	cx, cy, ok1 := node.point("start") // v5: "start" is the centre
	sx, sy, ok2 := node.point("end")   // and "end" is a point on the arc
	angle := node.child("angle")
	if !ok1 || !ok2 || angle == nil {
		return nil, false
	}
	n := angle.nums()
	if len(n) == 0 {
		return nil, false
	}
	r := math.Hypot(sx-cx, sy-cy)
	return arcSweep(cx, cy, r, math.Atan2(sy-cy, sx-cx), n[0]*math.Pi/180), true
}

// normaliseSweep works out which way round the arc goes from the midpoint.
func normaliseSweep(a0, amid, a1 float64) float64 {
	wrap := func(a float64) float64 {
		for a <= -math.Pi {
			a += 2 * math.Pi
		}
		for a > math.Pi {
			a -= 2 * math.Pi
		}
		return a
	}
	total := wrap(a1 - a0)
	if wrap(amid-a0)*total < 0 {
		// The midpoint is the other side, so the arc takes the long way round.
		if total > 0 {
			total -= 2 * math.Pi
		} else {
			total += 2 * math.Pi
		}
	}
	return total
}

func arcSweep(cx, cy, r, from, sweep float64) [][2]float64 {
	steps := int(math.Max(6, math.Abs(sweep)/(math.Pi/16)))
	pts := make([][2]float64, 0, steps+1)
	for i := 0; i <= steps; i++ {
		a := from + sweep*float64(i)/float64(steps)
		pts = append(pts, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return pts
}

func circlePoints(cx, cy, r float64, steps int) [][2]float64 {
	pts := make([][2]float64, 0, steps)
	for i := 0; i < steps; i++ {
		a := 2 * math.Pi * float64(i) / float64(steps)
		pts = append(pts, [2]float64{cx + r*math.Cos(a), cy + r*math.Sin(a)})
	}
	return pts
}

// circleThrough finds the circle passing through three points.
func circleThrough(x1, y1, x2, y2, x3, y3 float64) (cx, cy, r float64, ok bool) {
	d := 2 * (x1*(y2-y3) + x2*(y3-y1) + x3*(y1-y2))
	if math.Abs(d) < 1e-9 {
		return 0, 0, 0, false // the points are in a line
	}
	s1, s2, s3 := x1*x1+y1*y1, x2*x2+y2*y2, x3*x3+y3*y3
	cx = (s1*(y2-y3) + s2*(y3-y1) + s3*(y1-y2)) / d
	cy = (s1*(x3-x2) + s2*(x1-x3) + s3*(x2-x1)) / d
	return cx, cy, math.Hypot(x1-cx, y1-cy), true
}

// --- measurements -----------------------------------------------------------

// Bounds is the extent of the drawing in millimetres.
type Bounds struct{ MinX, MinY, MaxX, MaxY float64 }

func (b Bounds) Width() float64  { return b.MaxX - b.MinX }
func (b Bounds) Height() float64 { return b.MaxY - b.MinY }

// Extent is the courtyard when there is one -- the space the part actually
// claims on the board -- and otherwise everything drawn.
func (f *Footprint) Extent() Bounds { return f.bounds(true) }

func (f *Footprint) bounds(courtyardOnly bool) Bounds {
	b := Bounds{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	add := func(x, y float64) {
		b.MinX, b.MinY = math.Min(b.MinX, x), math.Min(b.MinY, y)
		b.MaxX, b.MaxY = math.Max(b.MaxX, x), math.Max(b.MaxY, y)
	}
	found := false
	for _, s := range f.Strokes {
		if courtyardOnly && s.Layer != "courtyard" {
			continue
		}
		for _, p := range s.Points {
			add(p[0], p[1])
			found = true
		}
	}
	if found {
		return b
	}
	if courtyardOnly {
		return f.bounds(false) // no courtyard drawn, fall back to everything
	}
	for _, p := range f.Pads {
		add(p.X-p.W/2, p.Y-p.H/2)
		add(p.X+p.W/2, p.Y+p.H/2)
	}
	if math.IsInf(b.MinX, 1) {
		return Bounds{}
	}
	return b
}

// Size is the part's footprint written the way you would say it out loud.
func (f *Footprint) Size() string {
	b := f.Extent()
	if b.Width() <= 0 || b.Height() <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f × %.2f mm", b.Width(), b.Height())
}

// Pitch is the spacing between adjacent pads, when they are evenly spaced. It
// is the number you need to know whether a part fits a breadboard.
func (f *Footprint) Pitch() string {
	if len(f.Pads) < 2 {
		return ""
	}
	// Gather the distances between consecutive numbered pads and keep the
	// smallest, rounded, if they agree.
	var gaps []float64
	for i := 1; i < len(f.Pads); i++ {
		d := math.Hypot(f.Pads[i].X-f.Pads[i-1].X, f.Pads[i].Y-f.Pads[i-1].Y)
		if d > 0.01 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) == 0 {
		return ""
	}
	sort.Float64s(gaps)
	smallest := gaps[0]
	for _, g := range gaps {
		if math.Abs(g-smallest) > 0.02 && math.Abs(math.Mod(g, smallest)) > 0.02 {
			return "" // irregular, so quoting one number would mislead
		}
	}
	if math.Abs(smallest-2.54) < 0.02 {
		return "2.54 mm (0.1\", breadboard)"
	}
	return fmt.Sprintf("%.2f mm", smallest)
}

func (f *Footprint) PadCount() int { return len(f.Pads) }

func (f *Footprint) Mounting() string {
	through := 0
	for _, p := range f.Pads {
		if p.ThroughHole {
			through++
		}
	}
	switch {
	case through == 0 && len(f.Pads) > 0:
		return "surface mount"
	case through == len(f.Pads):
		return "through hole"
	case through > 0:
		return "mixed through-hole and surface mount"
	}
	return ""
}

// --- drawing ----------------------------------------------------------------

const (
	svgPad    = 1.2 // millimetres of margin around the drawing
	svgPixels = 14  // rendered pixels per millimetre
	svgMinPx  = 120 // an 0805 is 6 mm across; at 14 px/mm that is unreadable
	svgMaxPx  = 460 // and a big connector should not take over the page
)

// svgSize turns a drawing's millimetre extent into pixels. A fixed number of
// pixels per millimetre keeps parts comparable at a glance -- a TO-220 really
// does look bigger than an 0805 -- but the very small and the very large are
// pulled back towards something you can actually see.
func svgSize(w, h float64) (float64, float64) {
	scale := float64(svgPixels)
	if longest := math.Max(w, h) * scale; longest < svgMinPx {
		scale = svgMinPx / math.Max(w, h)
	} else if longest > svgMaxPx {
		scale = svgMaxPx / math.Max(w, h)
	}
	return w * scale, h * scale
}

// SVG draws the footprint. Colours come from CSS custom properties so the
// drawing follows the page's theme rather than being baked in.
func (f *Footprint) SVG() template.HTML {
	b := f.bounds(false)
	minX, minY := b.MinX-svgPad, b.MinY-svgPad
	w, h := b.Width()+2*svgPad, b.Height()+2*svgPad
	if w <= 0 || h <= 0 {
		return ""
	}

	pxW, pxH := svgSize(w, h)
	var out strings.Builder
	fmt.Fprintf(&out,
		`<svg class="footprint" viewBox="%s %s %s %s" width="%.0f" height="%.0f" `+
			`role="img" aria-label="%s footprint, %s" xmlns="http://www.w3.org/2000/svg">`,
		mm(minX), mm(minY), mm(w), mm(h), pxW, pxH,
		template.HTMLEscapeString(f.Name), template.HTMLEscapeString(f.Size()))

	// Courtyard and fabrication outlines sit under the copper.
	for _, layer := range []string{"courtyard", "fab", "silk"} {
		for _, s := range f.Strokes {
			if s.Layer == layer {
				out.WriteString(strokeSVG(s))
			}
		}
	}
	for _, p := range f.Pads {
		out.WriteString(padSVG(p))
	}
	// Pad numbers last, so nothing covers them.
	for _, p := range f.Pads {
		if p.Number == "" || p.Number == "\"\"" {
			continue
		}
		size := math.Min(math.Min(p.W, p.H)*0.62, 0.9)
		if size < 0.28 {
			continue // the pad is too small to letter without it turning to mud
		}
		fmt.Fprintf(&out,
			`<text class="fp-num" x="%s" y="%s" font-size="%s" text-anchor="middle" dominant-baseline="central">%s</text>`,
			mm(p.X), mm(p.Y), mm(size), template.HTMLEscapeString(p.Number))
	}
	out.WriteString(`</svg>`)
	return template.HTML(out.String()) //nolint:gosec // every value above is a number or escaped
}

func strokeSVG(s FootprintStroke) string {
	if len(s.Points) < 2 {
		return ""
	}
	var d strings.Builder
	for i, p := range s.Points {
		if i == 0 {
			fmt.Fprintf(&d, "M%s %s", mm(p[0]), mm(p[1]))
			continue
		}
		fmt.Fprintf(&d, "L%s %s", mm(p[0]), mm(p[1]))
	}
	if s.Closed {
		d.WriteString("Z")
	}
	width := s.Width
	if width <= 0 {
		width = 0.1
	}
	return fmt.Sprintf(`<path class="fp-%s" d="%s" stroke-width="%s" fill="none" `+
		`stroke-linecap="round" stroke-linejoin="round"/>`, s.Layer, d.String(), mm(width))
}

func padSVG(p FootprintPad) string {
	var shape string
	switch p.Shape {
	case "circle":
		shape = fmt.Sprintf(`<circle class="fp-pad" cx="0" cy="0" r="%s"/>`, mm(p.W/2))
	case "oval":
		r := math.Min(p.W, p.H) / 2
		shape = fmt.Sprintf(`<rect class="fp-pad" x="%s" y="%s" width="%s" height="%s" rx="%s"/>`,
			mm(-p.W/2), mm(-p.H/2), mm(p.W), mm(p.H), mm(r))
	case "roundrect":
		shape = fmt.Sprintf(`<rect class="fp-pad" x="%s" y="%s" width="%s" height="%s" rx="%s"/>`,
			mm(-p.W/2), mm(-p.H/2), mm(p.W), mm(p.H), mm(math.Min(p.W, p.H)*0.25))
	default: // rect, trapezoid, custom -- a rectangle is the honest approximation
		shape = fmt.Sprintf(`<rect class="fp-pad" x="%s" y="%s" width="%s" height="%s"/>`,
			mm(-p.W/2), mm(-p.H/2), mm(p.W), mm(p.H))
	}

	// Pin 1 gets a ring, which is the convention every datasheet uses and the
	// only thing that tells you which way round the part goes.
	if p.Number == "1" {
		shape += fmt.Sprintf(`<rect class="fp-one" x="%s" y="%s" width="%s" height="%s" fill="none"/>`,
			mm(-p.W/2-0.28), mm(-p.H/2-0.28), mm(p.W+0.56), mm(p.H+0.56))
	}
	if p.Drill > 0 {
		dw := p.DrillW
		if dw <= 0 {
			dw = p.Drill
		}
		if dw == p.Drill {
			shape += fmt.Sprintf(`<circle class="fp-drill" cx="0" cy="0" r="%s"/>`, mm(p.Drill/2))
		} else {
			shape += fmt.Sprintf(`<rect class="fp-drill" x="%s" y="%s" width="%s" height="%s" rx="%s"/>`,
				mm(-dw/2), mm(-p.Drill/2), mm(dw), mm(p.Drill), mm(math.Min(dw, p.Drill)/2))
		}
	}

	transform := fmt.Sprintf("translate(%s %s)", mm(p.X), mm(p.Y))
	if p.Rotation != 0 {
		// KiCad measures pad rotation anticlockwise; SVG measures it clockwise.
		transform += fmt.Sprintf(" rotate(%s)", mm(-p.Rotation))
	}
	return fmt.Sprintf(`<g transform="%s">%s</g>`, transform, shape)
}

// mm formats a millimetre figure compactly and without a locale.
func mm(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}
