package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestParseSTLBinaryAndASCII(t *testing.T) {
	m, err := ParseSTL(binarySTLCube(t, 60, 31, 48))
	if err != nil {
		t.Fatalf("binary STL: %v", err)
	}
	if !m.Binary {
		t.Error("a binary STL was read as ASCII")
	}
	if m.TriangleCount() != 12 {
		t.Errorf("got %d triangles, want 12", m.TriangleCount())
	}
	if got := m.Dimensions(); got != "60.0 × 31.0 × 48.0 mm" {
		t.Errorf("dimensions = %q", got)
	}
	if !m.FitsIn(220, 220, 250) {
		t.Error("a 60x31x48 box should fit a 220x220x250 bed")
	}
	if m.FitsIn(50, 50, 250) {
		t.Error("a 60 mm box should not fit a 50 mm bed")
	}
	// The long way round counts: 31 x 60 fits a bed that 60 x 31 also fits, but
	// a 40 x 70 bed only takes it one way.
	if !m.FitsIn(40, 70, 250) {
		t.Error("the model should be allowed to be turned 90 degrees on the bed")
	}

	a, err := ParseSTL([]byte(asciiSTLTriangle))
	if err != nil {
		t.Fatalf("ASCII STL: %v", err)
	}
	if a.Binary {
		t.Error("an ASCII STL was read as binary")
	}
	if a.TriangleCount() != 1 {
		t.Fatalf("got %d triangles, want 1", a.TriangleCount())
	}
	if got := a.Dimensions(); got != "10.0 × 20.0 × 5.0 mm" {
		t.Errorf("ASCII dimensions = %q", got)
	}
}

func TestParseSTLDoesNotTrustTheLeadingWord(t *testing.T) {
	// Plenty of exporters write "solid" into a binary file's 80-byte header.
	// Reading that as ASCII yields nothing, so the length is what must decide.
	raw := binarySTLCube(t, 10, 10, 10)
	copy(raw, "solid ExportedBySomethingCareless")

	m, err := ParseSTL(raw)
	if err != nil {
		t.Fatalf("binary STL with a solid header: %v", err)
	}
	if !m.Binary || m.TriangleCount() != 12 {
		t.Errorf("read as binary=%v with %d triangles, want binary with 12", m.Binary, m.TriangleCount())
	}
}

func TestParseSTLRefusesRubbish(t *testing.T) {
	cases := map[string][]byte{
		"empty":        {},
		"short":        []byte("solid"),
		"not an STL":   []byte("this is a text file that goes on for a while but is not a model"),
		"a PNG":        testPNG(t),
		"no triangles": []byte("solid empty\nendsolid empty\n"),
	}
	for name, raw := range cases {
		if m, err := ParseSTL(raw); err == nil {
			t.Errorf("ParseSTL(%s) returned %d triangles, want a refusal", name, m.TriangleCount())
		}
	}
}

func TestRenderSTLProducesAPicture(t *testing.T) {
	m, err := ParseSTL(binarySTLCube(t, 40, 40, 40))
	if err != nil {
		t.Fatalf("ParseSTL: %v", err)
	}
	raw, err := m.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the render is not a readable PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != stlWidth || b.Dy() != stlHeight {
		t.Errorf("render is %dx%d, want %dx%d", b.Dx(), b.Dy(), stlWidth, stlHeight)
	}

	// A cube seen from the corner shows three faces at three brightnesses, and
	// covers a good part of the frame. Both are worth checking: a render that
	// silently produced an empty background would otherwise pass.
	shades := map[uint32]int{}
	painted := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 == 0x1b && g>>8 == 0x1e && bl>>8 == 0x25 {
				continue // background
			}
			painted++
			shades[r>>8]++
		}
	}
	if painted < stlWidth*stlHeight/8 {
		t.Errorf("only %d pixels were painted; the model is missing or tiny", painted)
	}
	if len(shades) < 3 {
		t.Errorf("the cube rendered in %d shades, want three faces lit differently", len(shades))
	}

	// The model should be the right way up: with the light from above, the top
	// face is the brightest, and it must sit above the middle of the frame.
	var brightest uint32
	for shade := range shades {
		if shades[shade] > 200 && shade > brightest {
			brightest = shade
		}
	}
	topRow, bottomRow := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if r, _, _, _ := img.At(x, y).RGBA(); r>>8 == brightest {
				if y < stlHeight/2 {
					topRow++
				} else {
					bottomRow++
				}
			}
		}
	}
	if topRow <= bottomRow {
		t.Errorf("the brightest face is mostly below the middle (%d above, %d below) — the model is upside down",
			topRow, bottomRow)
	}
}

func TestRenderHandlesADegenerateMesh(t *testing.T) {
	// Three collinear points: zero area, and every rasteriser's favourite way
	// to divide by zero.
	flat := "solid f\nfacet normal 0 0 1\nouter loop\n" +
		"vertex 0 0 0\nvertex 1 1 1\nvertex 2 2 2\nendloop\nendfacet\nendsolid f\n"
	m, err := ParseSTL([]byte(flat))
	if err != nil {
		t.Fatalf("ParseSTL: %v", err)
	}
	if _, err := m.Render(); err != nil && !strings.Contains(err.Error(), "no size") {
		t.Errorf("Render on a degenerate mesh: %v", err)
	}
}

// binarySTLCube writes a real binary STL of a box, so the parser is tested
// against the format rather than against a fixture of its own making.
func binarySTLCube(t *testing.T, sx, sy, sz float64) []byte {
	t.Helper()
	// Eight corners, twelve triangles.
	v := [8][3]float64{
		{0, 0, 0}, {sx, 0, 0}, {sx, sy, 0}, {0, sy, 0},
		{0, 0, sz}, {sx, 0, sz}, {sx, sy, sz}, {0, sy, sz},
	}
	faces := [12][3]int{
		{0, 2, 1}, {0, 3, 2}, // bottom
		{4, 5, 6}, {4, 6, 7}, // top
		{0, 1, 5}, {0, 5, 4},
		{1, 2, 6}, {1, 6, 5},
		{2, 3, 7}, {2, 7, 6},
		{3, 0, 4}, {3, 4, 7},
	}

	var b bytes.Buffer
	b.Write(make([]byte, 80)) // header
	binary.Write(&b, binary.LittleEndian, uint32(len(faces)))
	for _, f := range faces {
		for i := 0; i < 3; i++ { // the normal, which the parser ignores
			binary.Write(&b, binary.LittleEndian, float32(0))
		}
		for _, idx := range f {
			for axis := 0; axis < 3; axis++ {
				binary.Write(&b, binary.LittleEndian, float32(v[idx][axis]))
			}
		}
		binary.Write(&b, binary.LittleEndian, uint16(0))
	}
	return b.Bytes()
}

const asciiSTLTriangle = `solid tri
  facet normal 0 0 1
    outer loop
      vertex 0 0 0
      vertex 10 0 0
      vertex 0 20 5
    endloop
  endfacet
endsolid tri
`

// testPNG is a tiny valid PNG, used to check that a real image is still
// refused as a mesh.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}
