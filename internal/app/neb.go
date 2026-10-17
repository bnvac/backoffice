package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Neb is a separate assistant that runs on the same machine and answers over
// HTTP on loopback. This app talks to it from the server only, never from the
// browser: Neb runs shell tools and holds API keys, so it accepts nothing but
// loopback and refuses any request whose Host or Origin is not its own. That
// also means it must be on this machine — an address elsewhere is refused here
// before Neb gets the chance.
//
// The rest of the app's AI (the idea board, the structured suggestions) stays
// on the provider configured in Settings, because those calls need JSON mode
// and Neb's /ask is a conversation. Neb is the one you talk to.

type Neb struct {
	base   string // "http://127.0.0.1:7000", no trailing slash
	host   string // "127.0.0.1:7000", sent as Host and in Origin
	client *http.Client

	mu      sync.Mutex
	up      bool
	checked time.Time
	busy    bool // a background check is running
}

// NebReply is what /ask answers with. Pending is non-nil when Neb is holding
// a tool action until it is told yes or no in the same session.
type NebReply struct {
	Reply   string          `json:"reply"`
	Pending json.RawMessage `json:"pending"`
	Error   string          `json:"error"`
}

// HasPending reports whether Neb is waiting for a yes or a no.
func (r NebReply) HasPending() bool {
	p := strings.TrimSpace(string(r.Pending))
	return p != "" && p != "null" && p != "false" && p != "{}" && p != `""`
}

// PendingText describes what Neb is waiting to do, from whatever shape it
// sent. Neb's pending object is its own business, so this reads the fields a
// description is likely to be in and falls back to a plain question rather
// than showing raw JSON to a person deciding whether to allow something.
func (r NebReply) PendingText() string {
	if !r.HasPending() {
		return ""
	}
	var s string
	if json.Unmarshal(r.Pending, &s) == nil && strings.TrimSpace(s) != "" {
		return s
	}
	var m map[string]any
	if json.Unmarshal(r.Pending, &m) == nil {
		for _, k := range []string{"prompt", "question", "summary", "description", "message", "text"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
		for _, k := range []string{"tool", "action", "name", "command"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return "Neb wants to run " + v + "."
			}
		}
	}
	return "Neb wants to run a tool to do that."
}

// NewNeb sets up the client, or returns nil — with the reason logged — when
// the address is switched off, unreadable, or not on this machine.
func NewNeb(raw string) *Neb {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "", "off", "none", "false", "0":
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		log.Printf("assistant: NEB_URL %q is not an address; the assistant is off", raw)
		return nil
	}
	if !loopbackHost(u.Hostname()) {
		log.Printf("assistant: NEB_URL %q is not on this machine; Neb only answers on loopback, so the assistant is off", raw)
		return nil
	}
	return &Neb{
		base: strings.TrimRight(u.Scheme+"://"+u.Host, "/"),
		host: u.Host,
		// A model can take a while to answer, and tools longer.
		client: &http.Client{Timeout: 3 * time.Minute},
	}
}

func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Up reports whether Neb is answering. It is asked on every page render, so it
// never waits: the first call checks (a loopback connect is immediate either
// way), and after that a stale answer is refreshed in the background.
func (n *Neb) Up() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	first := n.checked.IsZero()
	stale := time.Since(n.checked) > 15*time.Second
	if stale && !first && !n.busy {
		n.busy = true
		go n.check()
	}
	up := n.up
	n.mu.Unlock()
	if first {
		return n.check()
	}
	return up
}

func (n *Neb) check() bool {
	conn, err := net.DialTimeout("tcp", n.host, 400*time.Millisecond)
	up := err == nil
	if conn != nil {
		conn.Close()
	}
	n.mu.Lock()
	n.up, n.checked, n.busy = up, time.Now(), false
	n.mu.Unlock()
	return up
}

// Ask sends one message in a session. A stable session per person is what
// lets Neb remember the thread, so onboarding and the questions after it flow
// as one conversation.
func (n *Neb) Ask(ctx context.Context, text, session string) (NebReply, error) {
	body, _ := json.Marshal(map[string]string{"text": text, "session": session, "origin": "api"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base+"/ask", bytes.NewReader(body))
	if err != nil {
		return NebReply{}, err
	}
	// Neb checks both, and refuses anything that is not itself.
	req.Host = n.host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", n.base)
	req.Header.Set("Accept", "application/json")

	res, err := n.client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return NebReply{}, errors.New("Neb took too long to answer")
		}
		n.mu.Lock()
		n.up, n.checked = false, time.Now()
		n.mu.Unlock()
		return NebReply{}, errors.New("Neb is not answering — is it running?")
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	switch {
	case res.StatusCode == http.StatusForbidden:
		return NebReply{}, errors.New("Neb refused the request — it only accepts calls from this machine")
	case res.StatusCode >= 400:
		return NebReply{}, fmt.Errorf("Neb answered with HTTP %d", res.StatusCode)
	}
	var out NebReply
	if err := json.Unmarshal(raw, &out); err != nil {
		return NebReply{}, errors.New("Neb's answer was not readable")
	}
	return out, nil
}
