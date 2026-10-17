package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The assistant: a chat with Neb, kept per person, shown as a button in the
// corner of every page while Neb is running.
//
// It is only switched on when this install asks people to sign in. Neb can run
// shell commands, and with no password every route here is open to anyone on
// the network — so on an open install the button is never drawn and the
// endpoint refuses, whatever Neb itself would allow.

// assistantOn is the one switch: Neb configured, Neb answering, sign-in on.
func (a *App) assistantOn() bool {
	return a.neb != nil && a.auth.Enabled() && a.neb.Up()
}

// assistantSession is Neb's thread for this person. Named accounts each get
// their own; a shared password is one shared thread, because there is no way
// to tell the people behind it apart.
func assistantSession(u *User) string {
	if u == nil {
		return "backoffice-shared"
	}
	return fmt.Sprintf("backoffice-u%d", u.ID)
}

// sameOrigin is a second line behind the session cookie, which is SameSite=Lax
// and so not sent on a cross-site POST anyway. A request that names an origin
// must name this one.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // older browsers omit it on same-origin requests
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

type chatMessage struct {
	Role    string `json:"role"` // you | neb | note
	Text    string `json:"text"`
	Pending string `json:"pending,omitempty"`
	At      string `json:"at"`
}

// replyJSON writes v with a status; the assistant reports errors as JSON too.
func replyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleAssistantHistory is the thread so far, for when the panel opens.
func (a *App) handleAssistantHistory(w http.ResponseWriter, r *http.Request) {
	if !a.assistantOn() {
		replyJSON(w, http.StatusForbidden, map[string]string{"error": a.assistantOffReason()})
		return
	}
	msgs, err := a.store.ChatHistory(assistantSession(CurrentUser(r)), 60)
	if err != nil {
		replyJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	replyJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// handleAssistant passes one message to Neb and hands back what it said.
func (a *App) handleAssistant(w http.ResponseWriter, r *http.Request) {
	if !a.assistantOn() {
		replyJSON(w, http.StatusForbidden, map[string]string{"error": a.assistantOffReason()})
		return
	}
	if !sameOrigin(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		replyJSON(w, http.StatusForbidden, map[string]string{"error": "the assistant only takes requests from this site"})
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		replyJSON(w, http.StatusBadRequest, map[string]string{"error": "that message could not be read"})
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		replyJSON(w, http.StatusBadRequest, map[string]string{"error": "say something first"})
		return
	}
	if len(text) > 4000 {
		text = text[:4000]
	}

	session := assistantSession(CurrentUser(r))
	first, err := a.store.ChatIsNew(session)
	if err != nil {
		replyJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.store.AddChat(session, chatMessage{Role: "you", Text: text})

	// Neb has no slot for a system prompt, so the first message of a thread
	// carries a short brief about where it is and who it is talking to. The
	// person never sees it; their own words are what the panel shows.
	sent := text
	if first {
		sent = a.onboardingBrief(r) + "\n\n" + text
	}

	reply, err := a.neb.Ask(r.Context(), sent, session)
	if err != nil {
		a.store.AddChat(session, chatMessage{Role: "note", Text: err.Error()})
		replyJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if reply.Error != "" && reply.Reply == "" {
		a.store.AddChat(session, chatMessage{Role: "note", Text: reply.Error})
		replyJSON(w, http.StatusOK, map[string]any{"error": reply.Error})
		return
	}
	msg := chatMessage{Role: "neb", Text: reply.Reply, Pending: reply.PendingText()}
	a.store.AddChat(session, msg)
	replyJSON(w, http.StatusOK, map[string]any{"message": msg, "pending": reply.HasPending()})
}

// handleAssistantForget clears the thread shown here. Neb keeps its own memory
// of the session, which this cannot reach — the panel says so.
func (a *App) handleAssistantForget(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		replyJSON(w, http.StatusForbidden, map[string]string{"error": "the assistant only takes requests from this site"})
		return
	}
	a.store.ClearChat(assistantSession(CurrentUser(r)))
	replyJSON(w, http.StatusOK, map[string]string{"ok": "cleared"})
}

func (a *App) assistantOffReason() string {
	switch {
	case a.neb == nil:
		return "the assistant is not configured"
	case !a.auth.Enabled():
		return "the assistant needs sign-in switched on, because Neb can run commands on this machine"
	default:
		return "Neb is not answering"
	}
}

// onboardingBrief is the context Neb gets once per thread: what this app is,
// who it is talking to, what is set up and what is not. Counts only — no
// part names, no keys, nothing that is not already on the person's screen.
func (a *App) onboardingBrief(r *http.Request) string {
	p, _ := a.store.GetProfile()
	items, _ := a.store.CountItems()
	tools, _ := a.store.ListTools()
	printers, _ := a.store.ListPrinters()
	spools, _ := a.store.ListSpools(false)
	_, hasModel := a.store.ActiveProvider()

	who := "the person who runs it"
	if p.Name != "" {
		who = p.Name
	}
	var missing []string
	if items == 0 {
		missing = append(missing, "no parts yet (Add, or paste a shop link)")
	}
	if len(tools) == 0 {
		missing = append(missing, "no tools in the workshop")
	}
	if len(printers) == 0 {
		missing = append(missing, "no printer recorded (Printing)")
	}
	if len(spools) == 0 {
		missing = append(missing, "no filament (Filament)")
	}
	if !hasModel {
		missing = append(missing, "no model for the idea board (Settings → Model; optional)")
	}
	state := "Everything basic is set up."
	if len(missing) > 0 {
		state = "Not set up yet: " + strings.Join(missing, "; ") + "."
	}
	return fmt.Sprintf(`[backoffice onboarding] You are the assistant inside Backoffice, a self-hosted inventory and workshop app at %s, talking to %s.
Its sections: Items (parts, photos, drawers), Projects (parts lists, step checklists, wiring), Ideas (projects buildable from the shelf), Printing (printers, which filaments each can take, sending G-code), Filament (spools in grams, by colour), Firmware (what is flashed on which board), Machines (homelab boxes), Workshop (tools), Labels (QR codes).
Right now: %d parts, %d tools, %d printers, %d spools. %s
Greet them briefly, then help with what they ask; if they are new, suggest the next useful step from what is missing. Ask before changing anything.`,
		a.BaseURL(r), who, items, len(tools), len(printers), len(spools), state)
}

// --- storage ---------------------------------------------------------------------

func (s *Store) AddChat(session string, m chatMessage) error {
	_, err := s.db.Exec(`INSERT INTO assistant_messages (session, role, text, pending, created_at)
		VALUES (?,?,?,?,?)`, session, m.Role, m.Text, m.Pending, nowRFC3339())
	return err
}

func (s *Store) ChatIsNew(session string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM assistant_messages WHERE session = ?`, session).Scan(&n)
	return n == 0, err
}

// ChatHistory is the last few messages, oldest first.
func (s *Store) ChatHistory(session string, limit int) ([]chatMessage, error) {
	rows, err := s.db.Query(`SELECT role, text, pending, created_at FROM (
		SELECT id, role, text, pending, created_at FROM assistant_messages
		WHERE session = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []chatMessage{}
	for rows.Next() {
		var m chatMessage
		var at string
		if err := rows.Scan(&m.Role, &m.Text, &m.Pending, &at); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			m.At = t.Local().Format("15:04")
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ClearChat(session string) error {
	_, err := s.db.Exec(`DELETE FROM assistant_messages WHERE session = ?`, session)
	return err
}
