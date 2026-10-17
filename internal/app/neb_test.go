package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeNeb answers /ask the way the brief describes Neb: it insists on its own
// Host and Origin, remembers sessions, and holds a tool action for a yes.
type fakeNeb struct {
	mu       sync.Mutex
	host     string
	got      []map[string]string
	badHosts int
}

func (f *fakeNeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/ask" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if r.Host != f.host || r.Header.Get("Origin") != "http://"+f.host ||
		r.Header.Get("Content-Type") != "application/json" {
		f.badHosts++
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in map[string]string
	json.NewDecoder(r.Body).Decode(&in)
	f.got = append(f.got, in)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(in["text"], "delete"):
		io.WriteString(w, `{"reply":"I can do that.","pending":{"tool":"parts.delete","summary":"Delete the TMP36 from the shelf?"},"error":null}`)
	case in["text"] == "yes":
		io.WriteString(w, `{"reply":"Done — it's gone.","pending":null,"error":null}`)
	default:
		io.WriteString(w, `{"reply":"You have 6 parts.","pending":null,"error":null}`)
	}
}

// signedInApp is an app with a password set, a running server, and a cookie.
func signedInApp(t *testing.T, nebURL string) (*App, *httptest.Server, *http.Cookie) {
	t.Helper()
	app := newTestApp(t)
	auth, err := NewAuth("hunter2", filepath.Join(app.cfg.DataDir, "session.key"), app.store)
	if err != nil {
		t.Fatal(err)
	}
	app.auth = auth
	app.neb = NewNeb(nebURL)
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.PostForm(srv.URL+"/login", url.Values{"password": {"hunter2"}})
	if err != nil || len(res.Cookies()) == 0 {
		t.Fatalf("login: %v", err)
	}
	return app, srv, res.Cookies()[0]
}

func say(t *testing.T, srv *httptest.Server, cookie *http.Cookie, text string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/assistant", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestAssistantTalksToNebTheWayNebInsists(t *testing.T) {
	fake := &fakeNeb{}
	neb := httptest.NewServer(fake)
	defer neb.Close()
	fake.host = strings.TrimPrefix(neb.URL, "http://")
	app, srv, cookie := signedInApp(t, neb.URL)
	app.store.AddSpool(Spool{Material: "pla", NetGrams: 1000})

	// Signed in with Neb up, every page carries the chat button.
	home, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	home.AddCookie(cookie)
	if res, err := http.DefaultClient.Do(home); err == nil {
		page, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(page), "data-chat") {
			t.Error("the chat button should be drawn when signed in and Neb is up")
		}
	}

	code, out := say(t, srv, cookie, "how many parts do I have?", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, out)
	}
	if fake.badHosts != 0 {
		t.Fatal("Neb refused the Host or Origin it was sent")
	}
	msg := out["message"].(map[string]any)
	if msg["text"] != "You have 6 parts." {
		t.Errorf("reply = %v", msg["text"])
	}

	first := fake.got[0]
	if first["session"] != "backoffice-shared" || first["origin"] != "api" {
		t.Errorf("sent %v", first)
	}
	// The first message of a thread carries the brief, ending with what was asked.
	if !strings.HasPrefix(first["text"], "[backoffice onboarding]") ||
		!strings.HasSuffix(first["text"], "how many parts do I have?") ||
		!strings.Contains(first["text"], "1 spools") {
		t.Errorf("first message should carry the onboarding brief: %q", first["text"])
	}

	// The second does not.
	say(t, srv, cookie, "and printers?", nil)
	if fake.got[1]["text"] != "and printers?" {
		t.Errorf("only the first message gets the brief: %q", fake.got[1]["text"])
	}

	// And the panel can show the thread, without the brief in it.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/assistant", nil)
	req.AddCookie(cookie)
	res, _ := http.DefaultClient.Do(req)
	var hist struct{ Messages []chatMessage }
	json.NewDecoder(res.Body).Decode(&hist)
	if len(hist.Messages) != 4 || hist.Messages[0].Text != "how many parts do I have?" {
		t.Errorf("history = %+v", hist.Messages)
	}
}

func TestPendingToolActionAsksForAYes(t *testing.T) {
	fake := &fakeNeb{}
	neb := httptest.NewServer(fake)
	defer neb.Close()
	fake.host = strings.TrimPrefix(neb.URL, "http://")
	_, srv, cookie := signedInApp(t, neb.URL)

	_, out := say(t, srv, cookie, "delete the TMP36", nil)
	if out["pending"] != true {
		t.Fatalf("Neb is holding an action; the reply should say so: %v", out)
	}
	if p := out["message"].(map[string]any)["pending"]; p != "Delete the TMP36 from the shelf?" {
		t.Errorf("pending text = %v", p)
	}
	_, out = say(t, srv, cookie, "yes", nil)
	if out["message"].(map[string]any)["text"] != "Done — it's gone." || fake.got[1]["session"] != fake.got[0]["session"] {
		t.Error("the yes should go back in the same session")
	}
}

// The safety rule: no sign-in, no assistant — Neb can run commands, and an
// open install is open to the whole network.
func TestAssistantIsOffWithoutSignIn(t *testing.T) {
	fake := &fakeNeb{}
	neb := httptest.NewServer(fake)
	defer neb.Close()
	fake.host = strings.TrimPrefix(neb.URL, "http://")

	app := newTestApp(t) // no password, no accounts
	app.neb = NewNeb(neb.URL)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	code, out := say(t, srv, nil, "rm -rf something", nil)
	if code != http.StatusForbidden || !strings.Contains(out["error"].(string), "sign-in") {
		t.Errorf("open install: %d %v", code, out)
	}
	if len(fake.got) != 0 {
		t.Fatal("Neb was called from an install with no sign-in")
	}
	res, _ := http.Get(srv.URL + "/")
	page, _ := io.ReadAll(res.Body)
	if strings.Contains(string(page), "data-chat") {
		t.Error("the chat button should not be drawn on an open install")
	}
	// The setup page explains why it is off.
	res, _ = http.Get(srv.URL + "/welcome?step=ai")
	page, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(page), "AUTH_PASSWORD") {
		t.Error("the setup page should say how to switch the assistant on")
	}
}

func TestAssistantRefusesOtherSites(t *testing.T) {
	fake := &fakeNeb{}
	neb := httptest.NewServer(fake)
	defer neb.Close()
	fake.host = strings.TrimPrefix(neb.URL, "http://")
	_, srv, cookie := signedInApp(t, neb.URL)

	code, _ := say(t, srv, cookie, "hello", map[string]string{"Origin": "https://evil.example"})
	if code != http.StatusForbidden {
		t.Errorf("cross-origin post = %d", code)
	}
	code, _ = say(t, srv, cookie, "hello", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if code != http.StatusForbidden {
		t.Errorf("cross-site fetch = %d", code)
	}
	if len(fake.got) != 0 {
		t.Error("Neb was called for a cross-site request")
	}
}

func TestNebMustBeOnThisMachine(t *testing.T) {
	for _, raw := range []string{"http://192.168.1.20:7000", "http://neb.example.com", "ftp://127.0.0.1:7000", "off", ""} {
		if NewNeb(raw) != nil {
			t.Errorf("NewNeb(%q) should be off", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:7000", "localhost:7000", "http://[::1]:7000"} {
		if NewNeb(raw) == nil {
			t.Errorf("NewNeb(%q) should be on", raw)
		}
	}
}

func TestNebDownIsAPlainMessage(t *testing.T) {
	fake := &fakeNeb{}
	neb := httptest.NewServer(fake)
	fake.host = strings.TrimPrefix(neb.URL, "http://")
	app, srv, cookie := signedInApp(t, neb.URL)
	if !app.assistantOn() {
		t.Fatal("assistant should be on while Neb answers")
	}
	neb.Close()
	code, out := say(t, srv, cookie, "hello", nil)
	if code == http.StatusOK {
		t.Fatalf("Neb is down: %v", out)
	}
	if e, _ := out["error"].(string); e == "" || strings.Contains(e, "dial tcp") {
		t.Errorf("want a plain message, not a socket error: %q", e)
	}
}

func TestPendingTextFromWhateverShapeNebSends(t *testing.T) {
	for raw, want := range map[string]string{
		`"Run apt upgrade?"`:             "Run apt upgrade?",
		`{"summary":"Delete 3 photos?"}`: "Delete 3 photos?",
		`{"tool":"shell","args":["ls"]}`: "Neb wants to run shell.",
		`{"weird":1}`:                    "Neb wants to run a tool to do that.",
	} {
		r := NebReply{Pending: []byte(raw)}
		if !r.HasPending() || r.PendingText() != want {
			t.Errorf("%s -> %q, want %q", raw, r.PendingText(), want)
		}
	}
	for _, none := range []string{"", "null", "{}", "false"} {
		if (NebReply{Pending: []byte(none)}).HasPending() {
			t.Errorf("%q is not pending", none)
		}
	}
}
