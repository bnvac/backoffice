package app

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Named accounts are optional. With no users at all the single shared password
// still works exactly as before, which is what keeps a one-person install at
// zero configuration; adding the first account is what turns the audit trail
// from "someone" into "who".

const (
	pbkdf2Iterations = 210_000 // OWASP's 2023 floor for PBKDF2-HMAC-SHA256
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
	minPasswordLen   = 8
)

type User struct {
	ID        int64
	Username  string
	Admin     bool
	CreatedAt time.Time
	LastSeen  time.Time
}

// hashPassword produces a self-describing string, so the iteration count can be
// raised later without invalidating everyone's existing password.
func hashPassword(plain string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, plain, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword compares in constant time and refuses anything it does not
// recognise rather than falling back to a weaker check.
func verifyPassword(encoded, attempt string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 || iter > 10_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, attempt, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, admin, created_at, last_seen
		FROM users ORDER BY username COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, seen string
	if err := sc.Scan(&u.ID, &u.Username, &u.Admin, &created, &seen); err != nil {
		return u, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339, created)
	u.LastSeen, _ = time.Parse(time.RFC3339, seen)
	return u, nil
}

func (s *Store) GetUser(id int64) (User, error) {
	row := s.db.QueryRow(`SELECT id, username, admin, created_at, last_seen FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// CreateUser adds an account. The first one is always an administrator: an
// install with only non-admins could never manage itself.
func (s *Store) CreateUser(username, password string, admin bool) (int64, error) {
	username = strings.TrimSpace(username)
	if err := validUsername(username); err != nil {
		return 0, err
	}
	if len(password) < minPasswordLen {
		return 0, fmt.Errorf("the password needs at least %d characters", minPasswordLen)
	}
	n, err := s.CountUsers()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		admin = true
	}
	hash, err := hashPassword(password)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO users (username, password, admin, created_at)
		VALUES (?,?,?,?)`, username, hash, admin, nowRFC3339())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("there is already an account called %q", username)
		}
		return 0, err
	}
	return res.LastInsertId()
}

func validUsername(u string) error {
	if len(u) < 2 || len(u) > 32 {
		return errors.New("a username is between 2 and 32 characters")
	}
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return errors.New("a username can hold letters, digits, dot, dash and underscore")
		}
	}
	return nil
}

func (s *Store) SetPassword(id int64, password string) error {
	if len(password) < minPasswordLen {
		return fmt.Errorf("the password needs at least %d characters", minPasswordLen)
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET password = ? WHERE id = ?`, hash, id)
	return err
}

// DeleteUser refuses to remove the last administrator, which would lock
// everyone out of the settings page permanently.
func (s *Store) DeleteUser(id int64) error {
	var admin bool
	if err := s.db.QueryRow(`SELECT admin FROM users WHERE id = ?`, id).Scan(&admin); err != nil {
		return err
	}
	if admin {
		var others int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE admin = 1 AND id <> ?`, id).Scan(&others)
		if err != nil {
			return err
		}
		if others == 0 {
			return errors.New("that is the only administrator — make someone else an administrator first")
		}
	}
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

func (s *Store) SetAdmin(id int64, admin bool) error {
	if !admin {
		var others int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE admin = 1 AND id <> ?`, id).Scan(&others)
		if err != nil {
			return err
		}
		if others == 0 {
			return errors.New("that is the only administrator")
		}
	}
	_, err := s.db.Exec(`UPDATE users SET admin = ? WHERE id = ?`, admin, id)
	return err
}

// Authenticate checks a username and password, and returns the account.
func (s *Store) Authenticate(username, password string) (User, bool) {
	var id int64
	var hash string
	err := s.db.QueryRow(`SELECT id, password FROM users WHERE username = ?`,
		strings.TrimSpace(username)).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Still spend the time hashing, so a missing account and a wrong
		// password take the same length of time to reject.
		verifyPassword("pbkdf2-sha256$"+strconv.Itoa(pbkdf2Iterations)+"$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		return User{}, false
	}
	if err != nil || !verifyPassword(hash, password) {
		return User{}, false
	}
	u, err := s.GetUser(id)
	if err != nil {
		return User{}, false
	}
	return u, true
}

func (s *Store) TouchUser(id int64) {
	_, _ = s.db.Exec(`UPDATE users SET last_seen = ? WHERE id = ?`, nowRFC3339(), id)
}

// --- audit trail ------------------------------------------------------------

// AuditEntry is one recorded change.
type AuditEntry struct {
	ID       int64
	At       time.Time
	Actor    string
	Action   string
	Entity   string
	EntityID int64
	Detail   string
}

// Record appends to the audit trail. It deliberately never returns an error to
// its caller: failing to write a log line must not fail the change itself, so
// a problem here is logged and swallowed.
func (s *Store) Record(actor, action, entity string, id int64, detail string) {
	if actor == "" {
		actor = "someone"
	}
	_, err := s.db.Exec(`INSERT INTO audit (at, actor, action, entity, entity_id, detail)
		VALUES (?,?,?,?,?,?)`, nowRFC3339(), actor, action, entity, id, detail)
	if err != nil {
		log.Printf("audit: %v", err)
	}
}

func (s *Store) AuditTrail(limit int, entity string, entityID int64) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where, args := "1=1", []any{}
	if entity != "" {
		where, args = "entity = ?", append(args, entity)
		if entityID > 0 {
			where += " AND entity_id = ?"
			args = append(args, entityID)
		}
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT id, at, actor, action, entity, entity_id, detail
		FROM audit WHERE `+where+` ORDER BY at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Entity, &e.EntityID, &e.Detail); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Link is where the audited thing lives, or "" when it no longer has a page.
func (e AuditEntry) Link() string {
	if e.EntityID == 0 {
		return ""
	}
	switch e.Entity {
	case "item":
		return fmt.Sprintf("/items/%d", e.EntityID)
	case "project":
		return fmt.Sprintf("/projects/%d", e.EntityID)
	case "folder":
		return fmt.Sprintf("/folders/%d", e.EntityID)
	}
	return ""
}

// PruneAudit drops everything older than the given number of days.
func (s *Store) PruneAudit(days int) (int64, error) {
	if days < 0 {
		days = 0
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	res, err := s.db.Exec(`DELETE FROM audit WHERE at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
