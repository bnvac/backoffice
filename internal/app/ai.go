package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Somewhere to plug a model in, if you want one.
//
// Everything else in this app works without one and goes on working without
// one: the idea board has a suggester that reads your shelf directly, and the
// tool list is interpreted by rules. A model makes the suggestions better, not
// possible. That is why this is a setting rather than a requirement.
//
// Three request shapes cover everything worth plugging in -- Ollama's native
// API, the OpenAI chat-completions shape that half the industry now speaks,
// and Anthropic's Messages API -- so they are spoken directly over net/http
// rather than pulling in an SDK per provider. This app is one static binary
// with two dependencies, and a uniform three-way client is also the only way
// the calling code can stay ignorant of which one is configured.

// AIProvider is one configured endpoint.
type AIProvider struct {
	ID        int64
	Name      string
	Kind      string // ollama | openai | anthropic
	Endpoint  string
	Model     string
	APIKey    string // never rendered back into a page
	Active    bool
	Status    string
	CheckedAt time.Time
	CreatedAt time.Time
}

// HasKey is what the settings page shows instead of the key itself.
func (p AIProvider) HasKey() bool { return p.APIKey != "" }

func (p AIProvider) KindLabel() string {
	switch p.Kind {
	case "ollama":
		return "Ollama (local)"
	case "openai":
		return "OpenAI-compatible"
	case "anthropic":
		return "Anthropic"
	}
	return p.Kind
}

// NeedsKey says whether leaving the key blank is a mistake. A local model does
// not need one; a hosted one does.
func (p AIProvider) NeedsKey() bool { return p.Kind != "ollama" }

func (p AIProvider) Working() bool { return strings.HasPrefix(p.Status, "ok") }

// aiPreset is one of the ready-made choices on the settings page, so nobody has
// to go and find a base URL to try this out.
type aiPreset struct {
	Label    string
	Kind     string
	Endpoint string
	Model    string
	Note     string
}

// AIPresets are starting points, all editable afterwards. The model names are
// the ones each service documents as its general-purpose default; they change
// often, so the field is a text box rather than a list.
var AIPresets = []aiPreset{
	{"Ollama on this machine", "ollama", "http://localhost:11434", "llama3.2",
		"No key needed. Whatever you have pulled locally."},
	{"LM Studio", "openai", "http://localhost:1234/v1", "",
		"No key needed. Start the local server in LM Studio first."},
	{"Anthropic", "anthropic", "https://api.anthropic.com", "claude-opus-5",
		"Needs an API key from console.anthropic.com."},
	{"OpenAI", "openai", "https://api.openai.com/v1", "gpt-4o-mini",
		"Needs an API key from platform.openai.com."},
	{"Moonshot / Kimi", "openai", "https://api.moonshot.ai/v1", "kimi-k2-0905-preview",
		"OpenAI-shaped. Needs a Moonshot key."},
	{"OpenRouter", "openai", "https://openrouter.ai/api/v1", "",
		"OpenAI-shaped, many models behind one key."},
}

// --- the client -------------------------------------------------------------

// aiClient is deliberately not the app's Fetcher.
//
// The Fetcher refuses to dial private addresses, because it follows links that
// arrived from a web page and must not be talked into probing the LAN. An AI
// endpoint is the opposite case: somebody typed it into a settings form on
// purpose, and the most common thing to type is a model on this very machine.
// Blocking that would make local models -- the whole point of the Ollama
// option -- impossible. Only an operator can set this, and an operator can
// already read the database.
var aiClient = &http.Client{Timeout: 3 * time.Minute}

// Ask sends one prompt and returns the reply as text.
//
// wantJSON asks the provider for JSON where it can enforce that, which matters
// most for small local models: they will happily wrap JSON in prose otherwise.
func (p AIProvider) Ask(ctx context.Context, system, prompt string, wantJSON bool) (string, error) {
	if strings.TrimSpace(p.Model) == "" {
		return "", fmt.Errorf("%s has no model set", p.Name)
	}
	if p.NeedsKey() && p.APIKey == "" {
		return "", fmt.Errorf("%s needs an API key", p.Name)
	}
	switch p.Kind {
	case "ollama":
		return p.askOllama(ctx, system, prompt, wantJSON)
	case "openai":
		return p.askOpenAI(ctx, system, prompt, wantJSON)
	case "anthropic":
		return p.askAnthropic(ctx, system, prompt, wantJSON)
	}
	return "", fmt.Errorf("unknown provider kind %q", p.Kind)
}

// base tidies the endpoint into something joinable.
func (p AIProvider) base() string { return strings.TrimRight(strings.TrimSpace(p.Endpoint), "/") }

func (p AIProvider) post(ctx context.Context, path string, body any, headers map[string]string) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := p.base() + path
	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("bad endpoint %q", p.Endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := aiClient.Do(req)
	if err != nil {
		return nil, tidyAIError(err)
	}
	defer res.Body.Close()

	// Cap the read: a misconfigured endpoint can be anything at all.
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s said %s: %s", p.Name, res.Status, apiErrorMessage(data))
	}
	return data, nil
}

// apiErrorMessage digs the human part out of an error body, whatever shape it
// arrived in, and falls back to the first line of the raw response.
func apiErrorMessage(data []byte) string {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && len(body.Error) > 0 {
		var msg string
		if json.Unmarshal(body.Error, &msg) == nil && msg != "" {
			return msg
		}
		var obj struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		if json.Unmarshal(body.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
	}
	text := strings.TrimSpace(string(data))
	if i := strings.IndexByte(text, '\n'); i > 0 {
		text = text[:i]
	}
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		return "no message"
	}
	return text
}

// tidyAIError turns Go's transport errors into something a person can act on.
func tidyAIError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("nothing is listening there — is the server running?")
	case strings.Contains(msg, "no such host"):
		return fmt.Errorf("that hostname does not resolve")
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "Timeout"):
		return fmt.Errorf("the model took too long to answer")
	case strings.Contains(msg, "certificate"):
		return fmt.Errorf("the TLS certificate was rejected")
	}
	return err
}

// --- Ollama -----------------------------------------------------------------

func (p AIProvider) askOllama(ctx context.Context, system, prompt string, wantJSON bool) (string, error) {
	body := map[string]any{
		"model":  p.Model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
	}
	if wantJSON {
		// Ollama constrains generation to valid JSON, which is the difference
		// between a small local model being usable here and not.
		body["format"] = "json"
	}
	data, err := p.post(ctx, "/api/chat", body, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("could not read the reply: %w", err)
	}
	return strings.TrimSpace(out.Message.Content), nil
}

// --- OpenAI-compatible ------------------------------------------------------

func (p AIProvider) askOpenAI(ctx context.Context, system, prompt string, wantJSON bool) (string, error) {
	body := map[string]any{
		"model": p.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
	}
	if wantJSON {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	headers := map[string]string{}
	if p.APIKey != "" {
		headers["Authorization"] = "Bearer " + p.APIKey
	}
	data, err := p.post(ctx, "/chat/completions", body, headers)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("could not read the reply: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%s returned no answer", p.Name)
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// --- Anthropic --------------------------------------------------------------

// anthropicVersion is the dated API version every request must carry.
const anthropicVersion = "2023-06-01"

func (p AIProvider) askAnthropic(ctx context.Context, system, prompt string, wantJSON bool) (string, error) {
	if wantJSON {
		// There is no JSON mode on this shape; asking plainly is what works,
		// and readJSON below copes with a stray sentence either side anyway.
		system += "\n\nReply with JSON only. No prose, no code fences."
	}
	body := map[string]any{
		"model":      p.Model,
		"max_tokens": 8192,
		"system":     system,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	data, err := p.post(ctx, "/v1/messages", body, map[string]string{
		"x-api-key":         p.APIKey,
		"anthropic-version": anthropicVersion,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason  string                        `json:"stop_reason"`
		StopDetails *struct{ Explanation string } `json:"stop_details"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("could not read the reply: %w", err)
	}
	// A refusal comes back as a normal 200 with nothing useful in the content,
	// so it has to be checked before the text is read or it looks like a bug.
	if out.StopReason == "refusal" {
		why := "the model declined to answer"
		if out.StopDetails != nil && out.StopDetails.Explanation != "" {
			why += ": " + out.StopDetails.Explanation
		}
		return "", fmt.Errorf("%s", why)
	}
	var text strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("%s returned no text", p.Name)
	}
	return strings.TrimSpace(text.String()), nil
}

// --- reading what came back -------------------------------------------------

// readJSON pulls a JSON document out of a reply that may be wrapped in a code
// fence or padded with "Sure, here you go". Models do this constantly and it is
// not worth failing over.
func readJSON(reply string, into any) error {
	text := strings.TrimSpace(reply)
	if fence := strings.Index(text, "```"); fence >= 0 {
		rest := text[fence+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			rest = rest[:end]
		}
		text = strings.TrimSpace(rest)
	}
	if err := json.Unmarshal([]byte(text), into); err == nil {
		return nil
	}
	// Fall back to the outermost braces, which handles a stray sentence.
	start := strings.IndexAny(text, "{[")
	if start < 0 {
		return fmt.Errorf("the model did not return JSON")
	}
	closer := byte('}')
	if text[start] == '[' {
		closer = ']'
	}
	end := strings.LastIndexByte(text, closer)
	if end <= start {
		return fmt.Errorf("the model did not return JSON")
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), into); err != nil {
		return fmt.Errorf("the model did not return usable JSON")
	}
	return nil
}

// --- storage ----------------------------------------------------------------

func (s *Store) ListProviders() ([]AIProvider, error) {
	rows, err := s.db.Query(`SELECT id, name, kind, endpoint, model, api_key, active,
		status, checked_at, created_at FROM ai_providers ORDER BY active DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AIProvider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanProvider(row rowScanner) (AIProvider, error) {
	var p AIProvider
	var checked, created string
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.Endpoint, &p.Model, &p.APIKey,
		&p.Active, &p.Status, &checked, &created)
	if err != nil {
		return AIProvider{}, err
	}
	p.CheckedAt, _ = time.Parse(time.RFC3339, checked)
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return p, nil
}

func (s *Store) GetProvider(id int64) (AIProvider, error) {
	row := s.db.QueryRow(`SELECT id, name, kind, endpoint, model, api_key, active,
		status, checked_at, created_at FROM ai_providers WHERE id = ?`, id)
	return scanProvider(row)
}

// ActiveProvider is the one the app will use, or a zero value with ok false.
// Everything that calls a model goes through here, so "no model configured" is
// a normal answer rather than an error.
func (s *Store) ActiveProvider() (AIProvider, bool) {
	row := s.db.QueryRow(`SELECT id, name, kind, endpoint, model, api_key, active,
		status, checked_at, created_at FROM ai_providers WHERE active = 1 LIMIT 1`)
	p, err := scanProvider(row)
	if err != nil {
		return AIProvider{}, false
	}
	return p, true
}

func (s *Store) AddProvider(p AIProvider) (int64, error) {
	if strings.TrimSpace(p.Name) == "" {
		return 0, fmt.Errorf("give it a name")
	}
	if p.Endpoint == "" {
		return 0, fmt.Errorf("give it an endpoint")
	}
	res, err := s.db.Exec(`INSERT INTO ai_providers (name, kind, endpoint, model, api_key,
		active, status, checked_at, created_at) VALUES (?,?,?,?,?,0,'','',?)`,
		p.Name, p.Kind, p.Endpoint, p.Model, p.APIKey, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateProvider writes the editable fields. An empty key means "leave the one
// you have" rather than "erase it", because the form never shows the old one
// and would otherwise wipe it on every save.
func (s *Store) UpdateProvider(p AIProvider) error {
	if p.APIKey == "" {
		_, err := s.db.Exec(`UPDATE ai_providers SET name = ?, kind = ?, endpoint = ?, model = ?
			WHERE id = ?`, p.Name, p.Kind, p.Endpoint, p.Model, p.ID)
		return err
	}
	_, err := s.db.Exec(`UPDATE ai_providers SET name = ?, kind = ?, endpoint = ?, model = ?,
		api_key = ? WHERE id = ?`, p.Name, p.Kind, p.Endpoint, p.Model, p.APIKey, p.ID)
	return err
}

func (s *Store) ClearProviderKey(id int64) error {
	_, err := s.db.Exec(`UPDATE ai_providers SET api_key = '' WHERE id = ?`, id)
	return err
}

// ActivateProvider makes one the active provider and stands the others down.
func (s *Store) ActivateProvider(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE ai_providers SET active = 0`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE ai_providers SET active = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeactivateProviders() error {
	_, err := s.db.Exec(`UPDATE ai_providers SET active = 0`)
	return err
}

func (s *Store) DeleteProvider(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ai_providers WHERE id = ?`, id)
	return err
}

// RecordProviderStatus remembers how the last test went, so the settings page
// can say what happened without testing again on every page load.
func (s *Store) RecordProviderStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE ai_providers SET status = ?, checked_at = ? WHERE id = ?`,
		status, nowRFC3339(), id)
	return err
}
