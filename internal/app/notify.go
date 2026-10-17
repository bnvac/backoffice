package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Telling your phone. Two kinds of address cover it without an app of our own:
//
//   - ntfy (https://ntfy.sh/some-long-topic, or your own ntfy server): install
//     the ntfy app, subscribe to the topic, done. Free, no account.
//   - a Discord webhook (https://discord.com/api/webhooks/…), for people who
//     already live in Discord.
//
// Either one gets the slicer's preview of the print attached, which is most of
// what a camera would have told you. The address is a secret — anyone with it
// can post to your phone — so, like the API keys, it is never shown back in
// full.

type NotifySettings struct {
	URL      string
	Finished bool
	Failed   bool
	Sensors  bool
	LastErr  string
	LastAt   time.Time
}

func (n NotifySettings) On() bool { return n.URL != "" }

func (n NotifySettings) Kind() string {
	if isDiscordWebhook(n.URL) {
		return "Discord"
	}
	return "ntfy"
}

// Masked is the address with the secret part hidden: enough to recognise it.
func (n NotifySettings) Masked() string {
	u, err := url.Parse(n.URL)
	if err != nil || u.Host == "" {
		return "set"
	}
	tail := strings.TrimRight(u.Path, "/")
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return u.Scheme + "://" + u.Host + "/…" + tail
}

func isDiscordWebhook(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return (host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com")) &&
		strings.HasPrefix(u.Path, "/api/webhooks/")
}

// checkNotifyURL accepts an ntfy topic URL or a Discord webhook.
func checkNotifyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("paste the whole address, starting https://")
	}
	if !isDiscordWebhook(raw) && strings.Trim(u.Path, "/") == "" {
		return "", fmt.Errorf("that is the ntfy server, not a topic — add /your-topic-name to the end")
	}
	u.Fragment = ""
	return u.String(), nil
}

// --- storage ---------------------------------------------------------------------

func (s *Store) Setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) NotifySettings() NotifySettings {
	n := NotifySettings{
		URL:      s.Setting("notify.url"),
		Finished: s.Setting("notify.finished") != "off",
		Failed:   s.Setting("notify.failed") != "off",
		Sensors:  s.Setting("notify.sensors") != "off",
		LastErr:  s.Setting("notify.last_error"),
	}
	n.LastAt, _ = time.Parse(time.RFC3339, s.Setting("notify.last_at"))
	return n
}

// --- sending ---------------------------------------------------------------------

var notifyClient = &http.Client{Timeout: 15 * time.Second}

// Note is one message.
type Note struct {
	Title  string
	Body   string
	Urgent bool
	Tag    string // an ntfy emoji tag: white_check_mark, x, droplet…
	Image  []byte // PNG, optional
	Link   string // where tapping it should go, optional
}

// notify sends a note if notifications are set up, and keeps the outcome for
// the settings card. It never fails the thing that caused it.
func (a *App) notify(ctx context.Context, n Note) error {
	cfg := a.store.NotifySettings()
	if !cfg.On() {
		return nil
	}
	err := sendNote(ctx, cfg.URL, n)
	a.store.SetSetting("notify.last_at", nowRFC3339())
	if err != nil {
		a.store.SetSetting("notify.last_error", err.Error())
	} else {
		a.store.SetSetting("notify.last_error", "")
	}
	return err
}

func sendNote(ctx context.Context, dest string, n Note) error {
	var req *http.Request
	if isDiscordWebhook(dest) {
		content := "**" + n.Title + "**\n" + n.Body
		if n.Link != "" {
			content += "\n" + n.Link
		}
		payload, _ := json.Marshal(map[string]any{"content": content})
		if n.Image != nil {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			mw.WriteField("payload_json", string(payload))
			fw, _ := mw.CreateFormFile("files[0]", "print.png")
			fw.Write(n.Image)
			mw.Close()
			req, _ = http.NewRequestWithContext(ctx, http.MethodPost, dest, &buf)
			req.Header.Set("Content-Type", mw.FormDataContentType())
		} else {
			req, _ = http.NewRequestWithContext(ctx, http.MethodPost, dest, bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		// ntfy takes the title and the rest as query parameters, which keeps
		// them UTF-8 safe; the body is either the message or the picture.
		u, _ := url.Parse(dest)
		q := u.Query()
		q.Set("title", n.Title)
		if n.Tag != "" {
			q.Set("tags", n.Tag)
		}
		if n.Urgent {
			q.Set("priority", "high")
		}
		if n.Link != "" {
			q.Set("click", n.Link)
		}
		if n.Image != nil {
			q.Set("message", n.Body)
			q.Set("filename", "print.png")
			u.RawQuery = q.Encode()
			req, _ = http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(n.Image))
		} else {
			u.RawQuery = q.Encode()
			req, _ = http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(n.Body))
		}
	}
	res, err := notifyClient.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s", req.URL.Host)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("%s answered %d: %s", req.URL.Host, res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// --- what gets sent ----------------------------------------------------------------

func (a *App) notifyPrint(ctx context.Context, r SyncResult, link string) {
	cfg := a.store.NotifySettings()
	ok := r.Job.Outcome == "completed"
	if (ok && !cfg.Finished) || (!ok && !cfg.Failed) {
		return
	}
	tag := "white_check_mark"
	if !ok {
		tag = "x"
	}
	a.notify(ctx, Note{Title: r.Headline(), Body: r.Body(), Urgent: r.Job.Outcome == "failed",
		Tag: tag, Image: r.Thumb, Link: link})
}
