package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"strings"
	"testing"
)

// newStore gives each test its own photo directory.
func newStore(t *testing.T) *PhotoStore {
	t.Helper()
	s, err := NewPhotoStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewPhotoStore: %v", err)
	}
	return s
}

func TestPhotoSaveWritesThumbnail(t *testing.T) {
	store := newStore(t)
	raw := testJPEG(t, 1200, 800, 0)

	name, err := store.Save(bytes.NewReader(raw), "shot.jpg")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !SafeName.MatchString(name) {
		t.Fatalf("generated name %q is rejected by the media handler's own guard", name)
	}

	cfg, _, err := decodeFile(store.ThumbPath(name))
	if err != nil {
		t.Fatalf("thumbnail unreadable: %v", err)
	}
	if cfg.Width != thumbMax {
		t.Errorf("thumb width = %d, want longest edge scaled to %d", cfg.Width, thumbMax)
	}
	if cfg.Height != 341 { // 800 * 512/1200, truncated
		t.Errorf("thumb height = %d, want aspect ratio preserved (341)", cfg.Height)
	}

	store.Remove(name)
	if _, _, err := decodeFile(store.ThumbPath(name)); err == nil {
		t.Error("Remove left the thumbnail behind")
	}
}

func TestPhotoSaveRejectsNonImages(t *testing.T) {
	store := newStore(t)
	if _, err := store.Save(strings.NewReader("#!/bin/sh\nrm -rf /\n"), "evil.sh"); err == nil {
		t.Error("Save accepted a non-image upload")
	}
}

func TestThumbnailIsRotatedToMatchExif(t *testing.T) {
	store := newStore(t)
	// Orientation 6 means "rotate 90° CW to display": a 1200x800 landscape
	// original is shown by the browser as 800x1200 portrait, and the thumb has
	// to agree or the grid looks sideways next to the detail view.
	raw := testJPEG(t, 1200, 800, 6)

	name, err := store.Save(bytes.NewReader(raw), "portrait.jpg")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg, _, err := decodeFile(store.ThumbPath(name))
	if err != nil {
		t.Fatalf("thumbnail unreadable: %v", err)
	}
	if cfg.Width >= cfg.Height {
		t.Errorf("thumb is %dx%d, want portrait after applying EXIF orientation 6", cfg.Width, cfg.Height)
	}
}

func TestExifOrientationParsing(t *testing.T) {
	for _, want := range []int{1, 3, 6, 8} {
		if got := exifOrientation(testJPEG(t, 8, 8, want)); got != want {
			t.Errorf("exifOrientation = %d, want %d", got, want)
		}
	}
	if got := exifOrientation(testJPEG(t, 8, 8, 0)); got != 1 {
		t.Errorf("exifOrientation with no EXIF = %d, want 1", got)
	}
	if got := exifOrientation([]byte("not a jpeg at all")); got != 1 {
		t.Errorf("exifOrientation on junk = %d, want 1", got)
	}
}

func TestApplyOrientationSwapsAxes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	src.Set(0, 0, color.RGBA{255, 0, 0, 255}) // top-left marker

	rotated := applyOrientation(src, 6) // 90° CW
	if b := rotated.Bounds(); b.Dx() != 2 || b.Dy() != 4 {
		t.Fatalf("rotated bounds = %v, want 2x4", b)
	}
	// Under a 90° CW rotation the old top-left pixel lands at the top-right.
	if r, _, _, _ := rotated.At(1, 0).RGBA(); r>>8 != 255 {
		t.Error("marker pixel did not land at the top-right corner")
	}
	if applyOrientation(src, 1).Bounds() != src.Bounds() {
		t.Error("orientation 1 should be a no-op")
	}
}

func decodeFile(path string) (image.Config, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, "", err
	}
	defer f.Close()
	return image.DecodeConfig(f)
}

// testJPEG renders a solid image and, when orientation is 1..8, splices in a
// minimal EXIF APP1 segment carrying that orientation tag.
func testJPEG(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	raw := buf.Bytes()
	if orientation < 1 || orientation > 8 {
		return raw
	}

	tiff := new(bytes.Buffer)
	tiff.WriteString("II")                              // little-endian
	binary.Write(tiff, binary.LittleEndian, uint16(42)) // magic
	binary.Write(tiff, binary.LittleEndian, uint32(8))  // IFD0 offset
	binary.Write(tiff, binary.LittleEndian, uint16(1))  // one entry
	binary.Write(tiff, binary.LittleEndian, uint16(0x0112))
	binary.Write(tiff, binary.LittleEndian, uint16(3)) // SHORT
	binary.Write(tiff, binary.LittleEndian, uint32(1))
	binary.Write(tiff, binary.LittleEndian, uint16(orientation))
	binary.Write(tiff, binary.LittleEndian, uint16(0)) // pad value field
	binary.Write(tiff, binary.LittleEndian, uint32(0)) // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	app1 := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(len(payload)+2))

	out := append([]byte{0xFF, 0xD8}, app1...)
	out = append(out, payload...)
	return append(out, raw[2:]...) // original minus its SOI
}
