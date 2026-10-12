package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"strconv"
	"strings"
)

// A model site's thumbnail tells you what somebody's render looked like. The
// mesh tells you whether the thing will fit, which is the question you actually
// have -- so an STL that lands here is parsed, measured, and drawn from its own
// geometry.
//
// It is rasterised to a PNG with a z-buffer rather than emitted as SVG: a real
// print is tens of thousands of triangles, and an SVG with 40,000 paths in it is
// not a preview, it is a denial of service on the browser. Everything here is
// standard library.

const (
	MaxSTLBytes     = 48 << 20 // a 48 MiB STL is about a million triangles
	maxSTLTriangles = 1_200_000
	stlWidth        = 640
	stlHeight       = 480
)

// Mesh is a triangle soup with no topology, which is all an STL ever is.
type Mesh struct {
	Triangles []Triangle
	Min, Max  [3]float64
	Binary    bool
}

type Triangle struct{ A, B, C [3]float64 }

// Size is the bounding box in millimetres. STL carries no units, but every
// slicer and every printer treats them as millimetres, so this does too.
func (m *Mesh) Size() [3]float64 {
	return [3]float64{m.Max[0] - m.Min[0], m.Max[1] - m.Min[1], m.Max[2] - m.Min[2]}
}

// Dimensions is the size written the way you would say it.
func (m *Mesh) Dimensions() string {
	s := m.Size()
	return fmt.Sprintf("%.1f × %.1f × %.1f mm", s[0], s[1], s[2])
}

// FitsIn reports whether the model fits a print bed of the given size, trying
// it both ways round. 220 x 220 is an Ender-sized bed.
func (m *Mesh) FitsIn(bedX, bedY, bedZ float64) bool {
	s := m.Size()
	if s[2] > bedZ {
		return false
	}
	return (s[0] <= bedX && s[1] <= bedY) || (s[1] <= bedX && s[0] <= bedY)
}

func (m *Mesh) TriangleCount() int { return len(m.Triangles) }

// ParseSTL reads either spelling of the format.
//
// The leading word is not a reliable test: plenty of binary exporters write
// "solid" into the 80-byte header. The length is reliable -- a binary STL is
// exactly 84 + 50n bytes -- so that is what decides.
func ParseSTL(raw []byte) (*Mesh, error) {
	if len(raw) < 15 {
		return nil, fmt.Errorf("that file is too short to be an STL")
	}
	if n, ok := binaryTriangleCount(raw); ok {
		return parseBinarySTL(raw, n)
	}
	if bytes.HasPrefix(bytes.TrimLeft(raw[:min(len(raw), 64)], " \t\r\n"), []byte("solid")) {
		return parseASCIISTL(raw)
	}
	return nil, fmt.Errorf("that does not look like an STL file")
}

func binaryTriangleCount(raw []byte) (uint32, bool) {
	if len(raw) < 84 {
		return 0, false
	}
	n := binary.LittleEndian.Uint32(raw[80:84])
	if n == 0 || n > maxSTLTriangles {
		return 0, false
	}
	return n, uint64(len(raw)) == 84+50*uint64(n)
}

func parseBinarySTL(raw []byte, n uint32) (*Mesh, error) {
	m := &Mesh{Triangles: make([]Triangle, 0, n), Binary: true}
	m.resetBounds()
	for i := uint32(0); i < n; i++ {
		// 12 floats per record: a normal we ignore (plenty of files get it
		// wrong) and three vertices, then a 2-byte attribute.
		off := 84 + int(i)*50 + 12
		var t Triangle
		for v := 0; v < 3; v++ {
			for axis := 0; axis < 3; axis++ {
				bits := binary.LittleEndian.Uint32(raw[off+(v*3+axis)*4:])
				value := float64(math.Float32frombits(bits))
				if math.IsNaN(value) || math.IsInf(value, 0) {
					return nil, fmt.Errorf("that STL has a vertex that is not a number")
				}
				switch v {
				case 0:
					t.A[axis] = value
				case 1:
					t.B[axis] = value
				default:
					t.C[axis] = value
				}
			}
		}
		m.add(t)
	}
	if len(m.Triangles) == 0 {
		return nil, fmt.Errorf("that STL has no triangles in it")
	}
	return m, nil
}

func parseASCIISTL(raw []byte) (*Mesh, error) {
	m := &Mesh{}
	m.resetBounds()

	var verts [][3]float64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.EqualFold(fields[0], "vertex") {
			continue
		}
		var p [3]float64
		bad := false
		for axis := 0; axis < 3; axis++ {
			v, err := strconv.ParseFloat(fields[axis+1], 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				bad = true
				break
			}
			p[axis] = v
		}
		if bad {
			continue
		}
		verts = append(verts, p)
		if len(verts) == 3 {
			m.add(Triangle{verts[0], verts[1], verts[2]})
			verts = verts[:0]
			if len(m.Triangles) > maxSTLTriangles {
				return nil, fmt.Errorf("that STL has more than %d triangles", maxSTLTriangles)
			}
		}
	}
	if len(m.Triangles) == 0 {
		return nil, fmt.Errorf("that STL has no triangles in it")
	}
	return m, nil
}

func (m *Mesh) resetBounds() {
	m.Min = [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)}
	m.Max = [3]float64{math.Inf(-1), math.Inf(-1), math.Inf(-1)}
}

func (m *Mesh) add(t Triangle) {
	m.Triangles = append(m.Triangles, t)
	for _, p := range [][3]float64{t.A, t.B, t.C} {
		for axis := 0; axis < 3; axis++ {
			m.Min[axis] = math.Min(m.Min[axis], p[axis])
			m.Max[axis] = math.Max(m.Max[axis], p[axis])
		}
	}
}

// --- drawing ----------------------------------------------------------------

// Render draws the mesh as a shaded PNG, seen from the front-left-above corner
// that every 3D tool uses as its default view because it shows three faces at
// once.
func (m *Mesh) Render() ([]byte, error) {
	if len(m.Triangles) == 0 {
		return nil, fmt.Errorf("nothing to draw")
	}
	img := image.NewRGBA(image.Rect(0, 0, stlWidth, stlHeight))

	// A flat background rather than transparency: the preview is shown on both
	// a light and a dark page, and a shaded grey model on transparency
	// disappears into one of them.
	bg := color.RGBA{0x1b, 0x1e, 0x25, 0xff}
	for i := range img.Pix {
		switch i % 4 {
		case 0:
			img.Pix[i] = bg.R
		case 1:
			img.Pix[i] = bg.G
		case 2:
			img.Pix[i] = bg.B
		case 3:
			img.Pix[i] = bg.A
		}
	}

	// Rotate 35 degrees about Z then 60 about X: the standard three-quarter
	// view. Z is up in an STL, and stays up here.
	const (
		yaw   = 35 * math.Pi / 180
		pitch = 60 * math.Pi / 180
	)
	sinY, cosY := math.Sin(yaw), math.Cos(yaw)
	sinP, cosP := math.Sin(pitch), math.Cos(pitch)

	centre := [3]float64{
		(m.Min[0] + m.Max[0]) / 2,
		(m.Min[1] + m.Max[1]) / 2,
		(m.Min[2] + m.Max[2]) / 2,
	}
	project := func(p [3]float64) [3]float64 {
		x, y, z := p[0]-centre[0], p[1]-centre[1], p[2]-centre[2]
		x, y = x*cosY-y*sinY, x*sinY+y*cosY
		y, z = y*cosP-z*sinP, y*sinP+z*cosP
		// Screen y grows downwards, and the pitch has already put the top of
		// the model at negative y, so it is carried through as-is. Larger z is
		// nearer the camera, which is what the depth test assumes.
		return [3]float64{x, y, z}
	}

	// One pass to find the projected extent, so the model fills the frame
	// whatever its size.
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	projected := make([][3][3]float64, len(m.Triangles))
	for i, t := range m.Triangles {
		for j, p := range [][3]float64{t.A, t.B, t.C} {
			q := project(p)
			projected[i][j] = q
			minX, minY = math.Min(minX, q[0]), math.Min(minY, q[1])
			maxX, maxY = math.Max(maxX, q[0]), math.Max(maxY, q[1])
		}
	}
	spanX, spanY := maxX-minX, maxY-minY
	if spanX <= 0 && spanY <= 0 {
		return nil, fmt.Errorf("that model has no size")
	}
	const margin = 24.0
	scale := math.Min(
		(stlWidth-2*margin)/math.Max(spanX, 1e-9),
		(stlHeight-2*margin)/math.Max(spanY, 1e-9),
	)
	offX := stlWidth/2 - (minX+spanX/2)*scale
	offY := stlHeight/2 - (minY+spanY/2)*scale

	depth := make([]float64, stlWidth*stlHeight)
	for i := range depth {
		depth[i] = math.Inf(-1)
	}

	// A light over the viewer's left shoulder, plus enough ambient that a face
	// turned away is still shape rather than a silhouette.
	light := normalise([3]float64{-0.4, -0.6, 0.7})

	for i := range projected {
		tri := projected[i]
		var sx, sy [3]float64
		for j := 0; j < 3; j++ {
			sx[j] = tri[j][0]*scale + offX
			sy[j] = tri[j][1]*scale + offY
		}
		// The face's own normal says which way it points. Back faces are
		// skipped: they are the inside of the model and cannot be seen.
		normal := faceNormal(tri)
		if normal[2] <= 0 {
			continue
		}
		shade := 0.28 + 0.72*math.Max(0, dot(normal, light))
		col := color.RGBA{
			R: clamp8(0x7c * shade * 1.35),
			G: clamp8(0x8e * shade * 1.30),
			B: clamp8(0xb5 * shade * 1.22),
			A: 0xff,
		}
		fillTriangle(img, depth, sx, sy, [3]float64{tri[0][2], tri[1][2], tri[2][2]}, col)
	}

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// fillTriangle rasterises one face with a z-buffer, so a model with parts
// behind other parts comes out right rather than in draw order.
func fillTriangle(img *image.RGBA, depth []float64, sx, sy, sz [3]float64, col color.RGBA) {
	minX := int(math.Floor(min3(sx[0], sx[1], sx[2])))
	maxX := int(math.Ceil(max3(sx[0], sx[1], sx[2])))
	minY := int(math.Floor(min3(sy[0], sy[1], sy[2])))
	maxY := int(math.Ceil(max3(sy[0], sy[1], sy[2])))
	if maxX < 0 || maxY < 0 || minX >= stlWidth || minY >= stlHeight {
		return
	}
	minX, minY = max(minX, 0), max(minY, 0)
	maxX, maxY = min(maxX, stlWidth-1), min(maxY, stlHeight-1)

	// The signed area doubles as the barycentric denominator.
	denom := (sy[1]-sy[2])*(sx[0]-sx[2]) + (sx[2]-sx[1])*(sy[0]-sy[2])
	if math.Abs(denom) < 1e-12 {
		return // degenerate, and there are always a few
	}

	for y := minY; y <= maxY; y++ {
		py := float64(y) + 0.5
		for x := minX; x <= maxX; x++ {
			px := float64(x) + 0.5
			w0 := ((sy[1]-sy[2])*(px-sx[2]) + (sx[2]-sx[1])*(py-sy[2])) / denom
			w1 := ((sy[2]-sy[0])*(px-sx[2]) + (sx[0]-sx[2])*(py-sy[2])) / denom
			w2 := 1 - w0 - w1
			if w0 < 0 || w1 < 0 || w2 < 0 {
				continue
			}
			z := w0*sz[0] + w1*sz[1] + w2*sz[2]
			idx := y*stlWidth + x
			if z <= depth[idx] {
				continue
			}
			depth[idx] = z
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = col.R, col.G, col.B, 0xff
		}
	}
}

func faceNormal(t [3][3]float64) [3]float64 {
	u := [3]float64{t[1][0] - t[0][0], t[1][1] - t[0][1], t[1][2] - t[0][2]}
	v := [3]float64{t[2][0] - t[0][0], t[2][1] - t[0][1], t[2][2] - t[0][2]}
	return normalise([3]float64{
		u[1]*v[2] - u[2]*v[1],
		u[2]*v[0] - u[0]*v[2],
		u[0]*v[1] - u[1]*v[0],
	})
}

func normalise(v [3]float64) [3]float64 {
	l := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if l < 1e-12 {
		return [3]float64{0, 0, 1}
	}
	return [3]float64{v[0] / l, v[1] / l, v[2] / l}
}

func dot(a, b [3]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v)
}

func min3(a, b, c float64) float64 { return math.Min(a, math.Min(b, c)) }
func max3(a, b, c float64) float64 { return math.Max(a, math.Max(b, c)) }

// ReadSTL pulls an STL off a reader with a size limit.
func ReadSTL(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxSTLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxSTLBytes {
		return nil, fmt.Errorf("that STL is larger than %d MiB", MaxSTLBytes>>20)
	}
	return raw, nil
}
