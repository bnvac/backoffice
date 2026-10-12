package app

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
	"time"
)

// Six ESP32s on the shelf, and one of them is in the greenhouse running
// something you built in March. Which one, and what exactly? The shelf counts
// boards as a quantity; this records them one at a time, by what is on them.
//
// It does not flash anything. The board is plugged into the machine you are
// sitting at, not the one running this, and the browser cannot reach a serial
// port over plain HTTP. What it does instead is keep the image, its checksum,
// and the command that puts it back on, so reflashing a dead board in a year
// does not start with finding the source.

type Flash struct {
	ID        int64
	ItemID    *int64 // the kind of board, from the shelf
	Unit      string // which one: "#2", "greenhouse", a MAC address
	Firmware  string // what: "greenhouse-logger", "ESPHome", "MicroPython"
	Version   string // a tag, a date, a commit
	File      string // the name it was uploaded as
	Blob      string
	SHA256    string
	Size      int64
	ProjectID *int64
	Notes     string
	FlashedAt time.Time
	CreatedAt time.Time

	ItemName    string // joined in
	ProjectName string
	Current     bool // the latest record for this board and unit
}

func (f Flash) HasFile() bool       { return f.Blob != "" }
func (f Flash) SizeLabel() string   { return humanBytes(f.Size) }
func (f Flash) ShortSum() string    { return shortSum(f.SHA256) }
func (f Flash) ItemRef() int64      { return derefID(f.ItemID) }
func (f Flash) ProjectRef() int64   { return derefID(f.ProjectID) }
func (f Flash) FlashedDate() string { return f.FlashedAt.Local().Format("2 Jan 2006") }

// Board is what to call it when there is no item to link to.
func (f Flash) Board() string {
	if f.ItemName != "" {
		return f.ItemName
	}
	return "a board"
}

func derefID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func shortSum(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}

// FlashHint is how to put this image back on a board, worked out from the
// file. It is a starting point to copy, not a promise: offsets and chip names
// vary, and the hint says where it is guessing.
type FlashHint struct {
	Tool    string
	Command string
	Note    string
}

func HintFor(file, board string) (FlashHint, bool) {
	ext := strings.ToLower(path.Ext(file))
	b := strings.ToLower(board)
	switch ext {
	case ".uf2":
		return FlashHint{
			Tool: "drag and drop",
			Note: "Hold BOOTSEL (or double-tap reset on most other UF2 boards) while plugging it in, " +
				"then copy the file onto the drive that appears. It reboots by itself when the copy finishes.",
		}, true
	case ".bin":
		chip := "esp32"
		for _, c := range []string{"esp32-s3", "esp32-s2", "esp32-c3", "esp32-c6", "esp32-h2", "esp8266"} {
			if strings.Contains(b, c) || strings.Contains(b, strings.ReplaceAll(c, "-", "")) {
				chip = strings.ReplaceAll(c, "-", "")
				break
			}
		}
		return FlashHint{
			Tool:    "esptool",
			Command: fmt.Sprintf("esptool.py --chip %s --port /dev/ttyUSB0 write_flash 0x0 %s", chip, file),
			Note: "0x0 is right for a merged image (bootloader, partitions and app in one, which is what " +
				"ESPHome and Arduino's \"export compiled binary\" produce as *.merged.bin or *.factory.bin). " +
				"An app-only image goes at 0x10000 instead. On a Mac the port is /dev/cu.usbserial-*; on Windows, COM3 or similar.",
		}, true
	case ".hex":
		mcu, prog := "m328p", "arduino"
		switch {
		case strings.Contains(b, "mega"):
			mcu, prog = "m2560", "wiring"
		case strings.Contains(b, "leonardo"), strings.Contains(b, "pro micro"), strings.Contains(b, "32u4"):
			mcu, prog = "m32u4", "avr109"
		}
		return FlashHint{
			Tool:    "avrdude",
			Command: fmt.Sprintf("avrdude -p %s -c %s -P /dev/ttyUSB0 -b 115200 -D -U flash:w:%s:i", mcu, prog, file),
			Note: "Written for an Uno or Nano with the newer bootloader. An old-bootloader Nano wants -b 57600; " +
				"a Leonardo or Pro Micro has to be reset into its bootloader first and appears on a different port for eight seconds.",
		}, true
	case ".elf":
		return FlashHint{
			Tool:    "probe-rs",
			Command: "probe-rs download --chip <your chip> " + file,
			Note:    "An ELF goes on over a debug probe — SWD or JTAG — rather than over USB serial.",
		}, true
	}
	return FlashHint{}, false
}

// --- storage ---------------------------------------------------------------------

const flashColumns = `f.id, f.item_id, f.unit, f.firmware, f.version, f.file, f.blob, f.sha256,
	f.size, f.project_id, f.notes, f.flashed_at, f.created_at,
	COALESCE(i.name, ''), COALESCE(p.name, '')`

const flashFrom = ` FROM flashes f
	LEFT JOIN items i ON i.id = f.item_id
	LEFT JOIN projects p ON p.id = f.project_id`

func scanFlash(row rowScanner) (Flash, error) {
	var f Flash
	var item, project sql.NullInt64
	var flashed, created string
	err := row.Scan(&f.ID, &item, &f.Unit, &f.Firmware, &f.Version, &f.File, &f.Blob, &f.SHA256,
		&f.Size, &project, &f.Notes, &flashed, &created, &f.ItemName, &f.ProjectName)
	if err != nil {
		return Flash{}, err
	}
	if item.Valid {
		v := item.Int64
		f.ItemID = &v
	}
	if project.Valid {
		v := project.Int64
		f.ProjectID = &v
	}
	f.FlashedAt, _ = time.Parse(time.RFC3339, flashed)
	f.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return f, nil
}

// ListFlashes returns records newest first, filtered to one item or one
// project when either is given, and marks which is current for each board.
func (s *Store) ListFlashes(itemID, projectID int64) ([]Flash, error) {
	q := `SELECT ` + flashColumns + flashFrom
	var where []string
	var args []any
	if itemID > 0 {
		where, args = append(where, "f.item_id = ?"), append(args, itemID)
	}
	if projectID > 0 {
		where, args = append(where, "f.project_id = ?"), append(args, projectID)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY f.flashed_at DESC, f.id DESC"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Flash
	for rows.Next() {
		f, err := scanFlash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	markCurrent(out)
	return out, nil
}

// markCurrent flags the newest record per physical board. The list is already
// newest first, so the first one seen for a board is the current one. A record
// with no unit name is its own board: there is nothing to say two of them are
// the same physical thing.
func markCurrent(list []Flash) {
	seen := map[string]bool{}
	for i := range list {
		unit := strings.ToLower(strings.TrimSpace(list[i].Unit))
		if unit == "" {
			list[i].Current = true
			continue
		}
		key := fmt.Sprintf("%d|%s", derefID(list[i].ItemID), unit)
		if !seen[key] {
			list[i].Current = true
			seen[key] = true
		}
	}
}

func (s *Store) GetFlash(id int64) (Flash, error) {
	return scanFlash(s.db.QueryRow(`SELECT `+flashColumns+flashFrom+` WHERE f.id = ?`, id))
}

func (s *Store) AddFlash(f Flash) (int64, error) {
	if strings.TrimSpace(f.Firmware) == "" {
		return 0, fmt.Errorf("say what firmware it is")
	}
	flashed := f.FlashedAt
	if flashed.IsZero() {
		flashed = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO flashes (item_id, unit, firmware, version, file, blob,
		sha256, size, project_id, notes, flashed_at, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.ItemID, strings.TrimSpace(f.Unit), strings.TrimSpace(f.Firmware),
		strings.TrimSpace(f.Version), f.File, f.Blob, f.SHA256, f.Size, f.ProjectID,
		strings.TrimSpace(f.Notes), flashed.UTC().Format(time.RFC3339), nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteFlash(id int64) error {
	_, err := s.db.Exec(`DELETE FROM flashes WHERE id = ?`, id)
	return err
}

// ProgrammableItem says whether a part runs code of its own, which is whether
// its page should offer to record firmware. It leans on the idea board's role
// rules so there is one opinion in the app about what counts as a board.
func ProgrammableItem(it Item) bool {
	switch roleOf(it) {
	case "controller", "computer":
		return true
	}
	return false
}
