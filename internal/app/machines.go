package app

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A homelab is a shelf that is switched on. The Pi in the cupboard, the NUC
// under the desk, the NAS: each is a thing you own with things inside it, an
// address, an OS you installed from an image you should be able to name, and
// usually something else it depends on. That is inventory, and it is the same
// shape as the parts shelf — which is why it lives here and a dashboard of
// service tiles does not.

type Machine struct {
	ID       int64
	Name     string
	Role     string // "NAS", "Home Assistant", "Pi-hole"
	Address  string // hostname or IP
	Port     int    // checked to say whether it is up; 0 means do not check
	MAC      string
	OS       string // "Debian 12", "Proxmox VE 8.2"
	Image    string // the file it was installed from
	ImageSum string // that file's SHA-256, as published
	Hardware string // "Pi 5 8GB, 256GB NVMe on a Pimoroni base"
	Location string
	Watts    int // typical draw, for the power budget
	Services string
	Notes    string

	ItemID   *int64 // the part this is, when it came off the shelf
	ParentID *int64 // what it runs on or is plugged into

	CreatedAt time.Time
	UpdatedAt time.Time

	ItemName   string
	ParentName string
	Up         Reach // filled in by Reachability, not stored
}

func (m Machine) ItemRef() int64   { return derefID(m.ItemID) }
func (m Machine) ParentRef() int64 { return derefID(m.ParentID) }
func (m Machine) Checkable() bool  { return m.Address != "" && m.Port > 0 }

// ServiceList is one per line, for the card.
func (m Machine) ServiceList() []string {
	var out []string
	for _, line := range strings.Split(m.Services, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Target is host:port for the check, with IPv6 bracketed properly.
func (m Machine) Target() string {
	return net.JoinHostPort(strings.TrimSpace(m.Address), strconv.Itoa(m.Port))
}

// Reach is the result of trying the port.
type Reach struct {
	Checked bool
	OK      bool
	Took    time.Duration
	Err     string
}

func (r Reach) Label() string {
	switch {
	case !r.Checked:
		return ""
	case r.OK && r.Took < time.Millisecond:
		return "up · <1 ms"
	case r.OK:
		return fmt.Sprintf("up · %d ms", r.Took.Milliseconds())
	}
	return "not answering"
}

// Reachability dials every machine's port at once with a short timeout. It is
// a TCP connect rather than a ping because ping needs privileges this binary
// should not ask for, and "is SSH answering" is the more useful question
// anyway. It is checked from the server, which is worth saying on the page:
// a machine the server cannot reach may be fine from your laptop.
func Reachability(ctx context.Context, machines []Machine, timeout time.Duration) {
	var wg sync.WaitGroup
	for i := range machines {
		if !machines[i].Checkable() {
			continue
		}
		wg.Add(1)
		go func(m *Machine) {
			defer wg.Done()
			start := time.Now()
			d := net.Dialer{Timeout: timeout}
			conn, err := d.DialContext(ctx, "tcp", m.Target())
			m.Up = Reach{Checked: true, Took: time.Since(start)}
			if err != nil {
				m.Up.Err = err.Error()
				return
			}
			conn.Close()
			m.Up.OK = true
		}(&machines[i])
	}
	wg.Wait()
}

// PowerBudget adds up what is recorded. Machines with no figure are counted
// separately rather than as zero, so the total does not look more complete
// than it is.
type PowerBudget struct {
	Watts   int
	Known   int
	Unknown int
}

func (p PowerBudget) KWhPerYear() float64 { return float64(p.Watts) * 24 * 365 / 1000 }

func BudgetPower(machines []Machine) PowerBudget {
	var b PowerBudget
	for _, m := range machines {
		if m.Watts > 0 {
			b.Watts += m.Watts
			b.Known++
		} else {
			b.Unknown++
		}
	}
	return b
}

// NormaliseSum tidies a pasted checksum: lowercase, no spaces, and with the
// "sha256:" prefix or trailing filename that sums files tend to carry removed.
// Anything that is not then 64 hex characters is returned as it was typed, so
// a mistake shows rather than being quietly emptied.
func NormaliseSum(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "sha256:")
	if f := strings.Fields(s); len(f) > 0 {
		s = f[0] // "abc123...  ubuntu.iso" -> the hash
	}
	if len(s) != 64 {
		return strings.TrimSpace(raw)
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return strings.TrimSpace(raw)
		}
	}
	return s
}

// ValidSum reports whether a checksum is a proper SHA-256.
func ValidSum(s string) bool { return len(s) == 64 && NormaliseSum(s) == s }

// --- storage ---------------------------------------------------------------------

const machineColumns = `m.id, m.name, m.role, m.address, m.port, m.mac, m.os, m.image,
	m.image_sum, m.hardware, m.location, m.watts, m.services, m.notes, m.item_id, m.parent_id,
	m.created_at, m.updated_at, COALESCE(i.name, ''), COALESCE(p.name, '')`

const machineFrom = ` FROM machines m
	LEFT JOIN items i ON i.id = m.item_id
	LEFT JOIN machines p ON p.id = m.parent_id`

func scanMachine(row rowScanner) (Machine, error) {
	var m Machine
	var item, parent sql.NullInt64
	var created, updated string
	err := row.Scan(&m.ID, &m.Name, &m.Role, &m.Address, &m.Port, &m.MAC, &m.OS, &m.Image,
		&m.ImageSum, &m.Hardware, &m.Location, &m.Watts, &m.Services, &m.Notes, &item, &parent,
		&created, &updated, &m.ItemName, &m.ParentName)
	if err != nil {
		return Machine{}, err
	}
	if item.Valid {
		v := item.Int64
		m.ItemID = &v
	}
	if parent.Valid {
		v := parent.Int64
		m.ParentID = &v
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, created)
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return m, nil
}

func (s *Store) ListMachines() ([]Machine, error) {
	rows, err := s.db.Query(`SELECT ` + machineColumns + machineFrom + ` ORDER BY m.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetMachine(id int64) (Machine, error) {
	return scanMachine(s.db.QueryRow(`SELECT `+machineColumns+machineFrom+` WHERE m.id = ?`, id))
}

// MachinesForItem is the other direction, for the part's own page: "this Pi
// is in service as the Home Assistant box".
func (s *Store) MachinesForItem(itemID int64) ([]Machine, error) {
	rows, err := s.db.Query(`SELECT `+machineColumns+machineFrom+` WHERE m.item_id = ?
		ORDER BY m.name COLLATE NOCASE`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) checkMachine(m *Machine) error {
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		return fmt.Errorf("a machine needs a name")
	}
	if m.Port < 0 || m.Port > 65535 {
		return fmt.Errorf("%d is not a port", m.Port)
	}
	m.ImageSum = NormaliseSum(m.ImageSum)
	if m.ImageSum != "" && !ValidSum(m.ImageSum) {
		return fmt.Errorf("that checksum is not a SHA-256 — it should be 64 characters of 0–9 and a–f")
	}
	return nil
}

func (s *Store) AddMachine(m Machine) (int64, error) {
	if err := s.checkMachine(&m); err != nil {
		return 0, err
	}
	now := nowRFC3339()
	res, err := s.db.Exec(`INSERT INTO machines (name, role, address, port, mac, os, image,
		image_sum, hardware, location, watts, services, notes, item_id, parent_id, created_at,
		updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Name, strings.TrimSpace(m.Role), strings.TrimSpace(m.Address), m.Port,
		strings.TrimSpace(m.MAC), strings.TrimSpace(m.OS), strings.TrimSpace(m.Image), m.ImageSum,
		strings.TrimSpace(m.Hardware), strings.TrimSpace(m.Location), m.Watts,
		strings.TrimSpace(m.Services), strings.TrimSpace(m.Notes), m.ItemID, m.ParentID, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateMachine(m Machine) error {
	if err := s.checkMachine(&m); err != nil {
		return err
	}
	// A machine cannot run on itself, or on something that runs on it. Walk up
	// from the proposed parent; meeting this machine on the way is a loop.
	if m.ParentID != nil {
		seen := map[int64]bool{}
		for at := *m.ParentID; at != 0; {
			if at == m.ID {
				return fmt.Errorf("that would make it run on itself")
			}
			if seen[at] {
				break
			}
			seen[at] = true
			var next sql.NullInt64
			if err := s.db.QueryRow(`SELECT parent_id FROM machines WHERE id = ?`, at).
				Scan(&next); err != nil {
				break
			}
			at = next.Int64
		}
	}
	_, err := s.db.Exec(`UPDATE machines SET name = ?, role = ?, address = ?, port = ?, mac = ?,
		os = ?, image = ?, image_sum = ?, hardware = ?, location = ?, watts = ?, services = ?,
		notes = ?, item_id = ?, parent_id = ?, updated_at = ? WHERE id = ?`,
		m.Name, strings.TrimSpace(m.Role), strings.TrimSpace(m.Address), m.Port,
		strings.TrimSpace(m.MAC), strings.TrimSpace(m.OS), strings.TrimSpace(m.Image), m.ImageSum,
		strings.TrimSpace(m.Hardware), strings.TrimSpace(m.Location), m.Watts,
		strings.TrimSpace(m.Services), strings.TrimSpace(m.Notes), m.ItemID, m.ParentID,
		nowRFC3339(), m.ID)
	return err
}

func (s *Store) DeleteMachine(id int64) error {
	_, err := s.db.Exec(`DELETE FROM machines WHERE id = ?`, id)
	return err
}
