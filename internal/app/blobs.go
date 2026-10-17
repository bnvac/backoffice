package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A long print sliced at fine layers runs to a few hundred megabytes; nothing a
// microcontroller takes comes near the firmware limit. OS images are not
// stored at all — a machine records the image's name and published checksum.
const (
	maxGcode    = 512 << 20
	maxFirmware = 64 << 20
)

// Blobs is a folder of files named by their SHA-256. G-code and firmware both
// live in one: the same file kept twice is stored once, a name can never be
// used to walk out of the folder, and the hash is the checksum you want to
// see next to a firmware image anyway.
type Blobs struct {
	dir string
	max int64
}

func NewBlobs(dir string, max int64) (*Blobs, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Blobs{dir: dir, max: max}, nil
}

// errTooBig is its own error so a handler can say how big is too big.
var errTooBig = errors.New("file is too big")

// Save copies r into the folder and returns the stored name (hash plus the
// extension it came with), the hex digest, and the size. It streams to a temp
// file and renames, so a half-written upload never sits under a real name.
func (b *Blobs) Save(r io.Reader, ext string) (name, sum string, size int64, err error) {
	tmp, err := os.CreateTemp(b.dir, ".upload-*")
	if err != nil {
		return "", "", 0, err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed

	h := sha256.New()
	// One byte over the limit is enough to know it is too big.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, b.max+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", 0, err
	}
	if n > b.max {
		return "", "", 0, errTooBig
	}
	if n == 0 {
		return "", "", 0, errors.New("file is empty")
	}

	sum = hex.EncodeToString(h.Sum(nil))
	name = sum + cleanExt(ext)
	if err := os.Rename(tmp.Name(), filepath.Join(b.dir, name)); err != nil {
		return "", "", 0, err
	}
	return name, sum, n, nil
}

// Open returns a stored file. The name must be exactly what Save produced:
// a 64-character hex digest and an optional short extension.
func (b *Blobs) Open(name string) (*os.File, error) {
	if !validBlobName(name) {
		return nil, os.ErrNotExist
	}
	return os.Open(filepath.Join(b.dir, name))
}

// Path is for handing a stored file to http.ServeContent and friends.
func (b *Blobs) Path(name string) (string, bool) {
	if !validBlobName(name) {
		return "", false
	}
	return filepath.Join(b.dir, name), true
}

// Names lists every stored file, for the backup.
func (b *Blobs) Names() ([]string, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && validBlobName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Limit is how big a file Save will take, for the message when one is refused.
func (b *Blobs) Limit() string { return humanBytes(b.max) }

func validBlobName(name string) bool {
	if len(name) < 64 {
		return false
	}
	for _, c := range name[:64] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return name[64:] == cleanExt(name[64:])
}

// cleanExt keeps a short, boring extension and drops anything else. The
// extension is only there so a download gets a sensible name and type.
func cleanExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if len(ext) > 8 {
		return ""
	}
	for _, c := range ext[1:] {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return ext
}
