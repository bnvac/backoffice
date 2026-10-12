package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	sessionCookie = "parts_session"
	sessionMaxAge = 30 * 24 * time.Hour
)

// Auth starts as an intentionally small gate: one shared password, set with
// AUTH_PASSWORD, and leaving it unset disables auth entirely -- the right
// default for something already behind a LAN or a tunnel.
//
// Named accounts sit on top of that rather than replacing it. Add one and
// sign-in wants a username, sessions carry who you are, and the audit trail
// stops saying "someone". Add none and nothing changes.
type Auth struct {
	password string
	key      []byte
	store    *Store
	// Kept as a counter rather than queried per request: this is on the path of
	// every single page load, including images.
	users atomic.Int32
}

func NewAuth(password, keyPath string, store *Store) (*Auth, error) {
	a := &Auth{password: password, store: store}
	if store != nil {
		if err := a.refreshUserCount(); err != nil {
			return nil, err
		}
	}
	// A key is needed whenever a session can be issued at all, which now
	// includes an install with accounts but no shared password.
	if password == "" && a.users.Load() == 0 {
		return a, nil
	}
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	a.key = key
	return a, nil
}

// refreshUserCount is called after any change to the accounts table, and also
// lazily mints the session key the first account needs.
func (a *Auth) refreshUserCount() error {
	n, err := a.store.CountUsers()
	if err != nil {
		return err
	}
	a.users.Store(int32(n))
	return nil
}

// ensureKey mints the signing key on demand, for the case where the first
// account is created on an install that had no password at all.
func (a *Auth) ensureKey(path string) error {
	if len(a.key) > 0 {
		return nil
	}
	key, err := loadOrCreateKey(path)
	if err != nil {
		return err
	}
	a.key = key
	return nil
}

// Enabled reports whether anything is being asked for at the door.
func (a *Auth) Enabled() bool { return a.password != "" || a.users.Load() > 0 }

// Accounts reports whether named accounts exist, which is what decides whether
// the sign-in form asks for a username.
func (a *Auth) Accounts() bool { return a.users.Load() > 0 }

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// 0600: this key is what keeps existing sessions valid across restarts.
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write session key: %w", err)
	}
	return key, nil
}

// session is who the cookie says you are.
type session struct {
	UserID  int64 // 0 when signed in with the shared password
	Expires time.Time
}

type ctxKey int

const sessionKey ctxKey = 0

// CurrentUser reads the account attached to this request, if there is one.
func CurrentUser(r *http.Request) *User {
	u, _ := r.Context().Value(sessionKey).(*User)
	return u
}

func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		s, ok := a.parse(r)
		if !ok {
			if r.Method != http.MethodGet {
				http.Error(w, "session expired, reload and sign in again", http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if s.UserID > 0 {
			u, err := a.store.GetUser(s.UserID)
			if err != nil {
				// The account was deleted while the session was still valid.
				a.clear(w)
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), sessionKey, &u))
		}
		next.ServeHTTP(w, r)
	})
}

// parse validates the cookie. The value is "expiry" or "expiry:userID", both
// signed; the shorter form is what sessions issued before accounts existed look
// like, and they keep working.
func (a *Auth) parse(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	value, sig, ok := strings.Cut(c.Value, ".")
	if !ok || len(a.key) == 0 {
		return session{}, false
	}
	if !hmac.Equal([]byte(sig), []byte(a.sign(value))) {
		return session{}, false
	}
	expiry, rest, _ := strings.Cut(value, ":")
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || time.Now().Unix() > unix {
		return session{}, false
	}
	s := session{Expires: time.Unix(unix, 0)}
	if rest != "" {
		if id, err := strconv.ParseInt(rest, 10, 64); err == nil {
			s.UserID = id
		}
	}
	// Once accounts exist, an anonymous shared-password session is only good
	// enough if the shared password is still configured.
	if s.UserID == 0 && a.password == "" {
		return session{}, false
	}
	return s, true
}

func (a *Auth) valid(r *http.Request) bool {
	_, ok := a.parse(r)
	return ok
}

func (a *Auth) sign(value string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

// issue writes the session cookie. userID 0 means the shared password.
func (a *Auth) issue(w http.ResponseWriter, r *http.Request, userID int64) {
	value := strconv.FormatInt(time.Now().Add(sessionMaxAge).Unix(), 10)
	if userID > 0 {
		value += ":" + strconv.FormatInt(userID, 10)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value + "." + a.sign(value),
		Path:     "/",
		HttpOnly: true,
		// Lax is what keeps a third-party page from POSTing edits through your
		// browser; the app has no cross-site flows that need None.
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   int(sessionMaxAge / time.Second),
	})
}

func (a *Auth) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

// Check compares in constant time so a wrong password leaks no timing signal.
func (a *Auth) Check(attempt string) bool {
	if a.password == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(attempt), []byte(a.password)) == 1
}
