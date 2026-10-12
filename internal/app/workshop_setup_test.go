package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// --- reading a pasted tool list ---------------------------------------------

func TestToolListSurvivesHowPeopleActuallyWriteOne(t *testing.T) {
	raw := `3D printing:
- Prusa MK4 (250x210x220)
* Ender 3 V2

Soldering:
1. soldering iron — hakko fx888d
2) hot air rework station

digital calipers
Rigol DS1054Z oscilloscope - 50MHz
bench power supply 30V 5A
2x wire strippers
label printer
some gizmo nobody has heard of`

	tools := ParseToolList(raw)
	byName := map[string]Tool{}
	for _, tool := range tools {
		byName[strings.ToLower(tool.Name)] = tool
	}

	// The bullets, the numbering and the blank lines all come off.
	for name, wantKind := range map[string]string{
		"prusa mk4":                     "3d-printer",
		"ender 3 v2":                    "3d-printer",
		"soldering iron — hakko fx888d": "soldering",
		"hot air rework station":        "smd",
		"digital calipers":              "measurement",
		"rigol ds1054z oscilloscope":    "test",
		"bench power supply 30v 5a":     "power",
		"wire strippers":                "hand",
		"label printer":                 "labelling",
	} {
		got, ok := byName[name]
		if !ok {
			t.Errorf("%q did not come out of the list at all; got %v", name, names(tools))
			continue
		}
		if got.Kind != wantKind {
			t.Errorf("%q was filed as %q, want %q", name, got.Kind, wantKind)
		}
	}

	// A parenthesised size is kept as detail rather than becoming the name.
	if got := byName["prusa mk4"].Detail; got != "250x210x220" {
		t.Errorf("bed size = %q, want it kept as detail", got)
	}
	// A dashed-off spec is detail; a dashed-off model number is not.
	if got := byName["rigol ds1054z oscilloscope"].Detail; got != "50MHz" {
		t.Errorf("scope detail = %q, want 50MHz", got)
	}
	// A model number is part of the name; only something that reads as a
	// measurement is peeled off into detail.
	if got := byName["soldering iron — hakko fx888d"].Detail; got != "" {
		t.Errorf("the iron's model number was treated as a note: detail = %q", got)
	}
	// "2x" is recorded as a note rather than a second identical row: two irons
	// do not let you do anything one cannot, and the importer dedupes by name.
	strippers, ok := byName["wire strippers"]
	if !ok {
		t.Fatal("the wire strippers did not survive the count prefix")
	}
	if !strings.Contains(strippers.Detail, "2 of them") {
		t.Errorf("wire strippers detail = %q, want the count kept as a note", strippers.Detail)
	}
	if n := len(byName); n != len(tools) {
		t.Errorf("the list contains duplicate names: %v", names(tools))
	}
	// Nothing is silently dropped: an unrecognised line is still a tool.
	if got, ok := byName["some gizmo nobody has heard of"]; !ok || got.Kind != "other" {
		t.Errorf("an unrecognised line was dropped instead of kept as 'other'")
	}
}

func TestOneLineListSplitsOnCommasAndAMultiLineOneDoesNot(t *testing.T) {
	// A single line has nothing but commas to go on.
	one := ParseToolList("wire strippers, dupont crimper, tweezers")
	if len(one) != 3 {
		t.Errorf("single line gave %d tools, want 3: %v", len(one), names(one))
	}
	// But a real list must not have its commas split, or a model number that
	// contains one gets torn in half.
	many := ParseToolList("Rigol DS1054Z, 50MHz, 4 channel\ndigital calipers")
	if len(many) != 2 {
		t.Fatalf("multi-line list gave %d tools, want 2: %v", len(many), names(many))
	}
	if !strings.Contains(many[0].Name, "Rigol") || !strings.Contains(many[0].Name, "4 channel") {
		t.Errorf("the scope line was split at its commas: %q", many[0].Name)
	}
}

func names(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// --- what the tools add up to -----------------------------------------------

func TestCapabilitiesDecideHowTheBoxGetsMade(t *testing.T) {
	printer := SummariseTools(ParseToolList("Prusa MK4 (250x210x220)\ndigital calipers"))
	if !printer.Print3D {
		t.Fatal("a printer in the list did not make Print3D true")
	}
	if !printer.BuildVolume.Known() || printer.BuildVolume.X != 250 {
		t.Errorf("build volume = %v, want it read from the name", printer.BuildVolume)
	}
	if !printer.Fits(200, 150, 60) {
		t.Error("a part well inside the bed was reported as not fitting")
	}
	if printer.Fits(240, 240, 60) {
		t.Error("a part wider than the bed in both axes was reported as fitting")
	}
	// Turning it ninety degrees is what anybody would do, so it counts.
	if !printer.BuildVolume.Fits(200, 240, 60) {
		t.Error("a part that fits when rotated was reported as not fitting")
	}
	if advice := enclosureAdvice(printer, "a small box"); !strings.Contains(advice, "Print it") ||
		!strings.Contains(advice, "calipers") {
		t.Errorf("advice with a printer and calipers = %q", advice)
	}

	// The same question with no printer must get a different answer.
	drill := SummariseTools(ParseToolList("cordless drill\nhacksaw"))
	if drill.Print3D {
		t.Error("no printer was listed but Print3D is true")
	}
	advice := enclosureAdvice(drill, "a small box")
	if strings.Contains(advice, "Print it") {
		t.Errorf("told somebody with no printer to print: %q", advice)
	}
	if !strings.Contains(advice, "project box") {
		t.Errorf("no-printer advice = %q, want it to suggest buying a box", advice)
	}

	// And with nothing at all, it should say so rather than assume.
	bare := SummariseTools(nil)
	if a := enclosureAdvice(bare, "x"); !strings.Contains(a, "No printer and no cutting tools") {
		t.Errorf("empty-workshop advice = %q", a)
	}
}

// --- the shelf reader -------------------------------------------------------

func TestShelfSuggestionsOnlyNamePartsYouActuallyHave(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "ESP32 devkit", Category: "Microcontroller", Quantity: 3},
		Item{Name: "BME280 breakout", Category: "Sensor", Quantity: 2},
		Item{Name: "SSD1306 OLED", Category: "Display", Quantity: 1},
		Item{Name: "A drawer of M3 screws", Category: "Hardware", Quantity: 200},
	)
	items, err := app.store.ListItems(Query{})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	caps := SummariseTools(ParseToolList("Prusa MK4 (250x210x220)"))

	ideas := SuggestFromShelf(items, caps, 8)
	if len(ideas) == 0 {
		t.Fatal("a controller, a sensor and a display produced no ideas at all")
	}
	have := map[string]bool{}
	for _, it := range items {
		have[it.Name] = true
	}
	for _, idea := range ideas {
		if len(idea.Parts) == 0 {
			t.Errorf("%q was suggested with no parts", idea.Title)
		}
		for _, p := range idea.Parts {
			if !p.Have || !have[p.Name] {
				t.Errorf("%q lists %q, which is not on the shelf — the shelf reader must not invent parts",
					idea.Title, p.Name)
			}
		}
		if !idea.Buildable() {
			t.Errorf("%q is not buildable, but the shelf reader only emits complete recipes", idea.Title)
		}
		if !strings.Contains(idea.Enclosure, "Print") {
			t.Errorf("%q ignored the printer in the workshop: %q", idea.Title, idea.Enclosure)
		}
	}

	// An empty shelf gets no suggestions rather than generic ones.
	if got := SuggestFromShelf(nil, caps, 8); len(got) != 0 {
		t.Errorf("an empty shelf produced %d ideas", len(got))
	}
}

// A shelf of Raspberry Pis used to produce no ideas at all: every recipe asked
// for a "controller" and a Pi is a "computer", so nothing ever matched.
func TestASingleBoardComputerCountsAsABrain(t *testing.T) {
	pi := []Item{
		{ID: 1, Name: "Raspberry Pi", Quantity: 1},
		{ID: 2, Name: "TMP35", Quantity: 5},
	}
	ideas := SuggestFromShelf(pi, Capabilities{}, 8)
	if len(ideas) == 0 {
		t.Fatal("a Raspberry Pi and a temperature sensor produced no ideas")
	}

	// Both parts have to be recognised in the first place.
	for _, name := range []string{"Raspberry Pi", "Raspberry Pi 4", "Raspberry Pi 5 8GB"} {
		if got := roleOf(Item{Name: name}); got != "computer" {
			t.Errorf("roleOf(%q) = %q, want computer", name, got)
		}
	}
	// ...but a Pico is a microcontroller, not a computer.
	if got := roleOf(Item{Name: "Raspberry Pi Pico W"}); got != "controller" {
		t.Errorf("roleOf(Pico) = %q, want controller", got)
	}
	for _, name := range []string{"TMP35", "TMP36", "LM35", "10k thermistor", "MAX6675"} {
		if got := roleOf(Item{Name: name}); got != "environment" {
			t.Errorf("roleOf(%q) = %q, want environment", name, got)
		}
	}
}

func TestAPiReadingAnAnalogueSensorIsToldItNeedsAConverter(t *testing.T) {
	pi := []Item{
		{ID: 1, Name: "Raspberry Pi", Quantity: 1},
		{ID: 2, Name: "TMP35", Quantity: 5},
	}
	ideas := SuggestFromShelf(pi, Capabilities{}, 8)
	if len(ideas) == 0 {
		t.Fatal("no ideas")
	}
	idea := ideas[0]

	var adc *IdeaPart
	for i := range idea.Parts {
		if idea.Parts[i].Role == "adc" {
			adc = &idea.Parts[i]
		}
	}
	if adc == nil {
		t.Fatal("a Pi was told to read an analogue sensor with no converter — it has no ADC")
	}
	if adc.Have {
		t.Error("a converter nobody owns was marked as being on the shelf")
	}
	if idea.Buildable() {
		t.Error("the idea claims to be buildable while a part is missing")
	}
	// The reasoning must not claim you own the thing you have to buy.
	if strings.Contains(idea.Detail, "already have "+adc.Name) ||
		strings.Contains(idea.Detail, "and "+adc.Name+".") {
		t.Errorf("the shopping list was described as things you own: %q", idea.Detail)
	}
	if !strings.Contains(idea.Detail, "no analogue input") {
		t.Errorf("the reason the converter is needed was not explained: %q", idea.Detail)
	}

	// With a converter already on the shelf it is ticked off, not bought --
	// even though "ADS1115 16-bit ADC" matches no role of its own.
	withADC := append(pi, Item{ID: 3, Name: "ADS1115 16-bit ADC", Quantity: 2})
	got := SuggestFromShelf(withADC, Capabilities{}, 8)
	if len(got) == 0 {
		t.Fatal("no ideas with the converter present")
	}
	if !got[0].Buildable() {
		t.Errorf("the converter on the shelf was not found: %+v", got[0].Parts)
	}

	// A microcontroller reads it straight, so no converter should appear.
	esp := []Item{
		{ID: 1, Name: "ESP32 devkit", Quantity: 1},
		{ID: 2, Name: "TMP35", Quantity: 5},
	}
	for _, i := range SuggestFromShelf(esp, Capabilities{}, 8) {
		for _, p := range i.Parts {
			if p.Role == "adc" {
				t.Error("an ESP32 was told it needs an external ADC; it has one built in")
			}
		}
	}
}

func TestOutOfStockPartsDoNotCountTowardsAnIdea(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "ESP32 devkit", Category: "Microcontroller", Quantity: 2},
		Item{Name: "BME280 breakout", Category: "Sensor", Quantity: 0},
	)
	items, _ := app.store.ListItems(Query{})
	shelf := ReadShelf(items)
	if _, ok := shelf.Has("environment"); ok {
		t.Error("a part with none in stock was counted as being on the shelf")
	}
	if _, ok := shelf.Brain(); !ok {
		t.Error("the controller that is in stock was not found")
	}
}

func TestRolesComeFromWhateverTheItemRecords(t *testing.T) {
	cases := []struct {
		item Item
		want string
	}{
		{Item{Name: "ESP32-C3 SuperMini"}, "controller"},
		{Item{Name: "Raspberry Pi 5 8GB"}, "computer"},
		{Item{Name: "DS18B20 temperature probe"}, "environment"},
		{Item{Name: "5V relay module"}, "actuator"},
		{Item{Name: "HC-SR04"}, "distance"},
		{Item{Name: "Capacitive soil moisture sensor"}, "soil"},
		// Nothing in the name, but the category says it.
		{Item{Name: "Mystery board", Category: "Display"}, "display"},
		// Nothing in the name or category, but it speaks WiFi.
		{Item{Name: "Unmarked module", Interfaces: []string{"WiFi"}}, "controller"},
		// Genuinely unclassifiable stays unclassified rather than guessing.
		{Item{Name: "Box of M3 screws"}, ""},
	}
	for _, c := range cases {
		if got := roleOf(c.item); got != c.want {
			t.Errorf("roleOf(%q) = %q, want %q", c.item.Name, got, c.want)
		}
	}
}

// --- the model path ---------------------------------------------------------

// fakeModel stands in for a provider, so the parsing and the checking can be
// tested without anything leaving the machine.
func fakeModel(t *testing.T, reply string) (*httptest.Server, AIProvider) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("called %s, want the Ollama chat path", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != false {
			t.Error("the request did not ask for a non-streaming reply")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"content": reply},
		})
	}))
	return srv, AIProvider{Name: "test model", Kind: "ollama", Endpoint: srv.URL, Model: "test"}
}

func TestModelIdeasAreCheckedAgainstTheRealInventory(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "ESP32 devkit", Category: "Microcontroller", Quantity: 3},
		Item{Name: "BME280 breakout", Category: "Sensor", Quantity: 2},
	)
	items, _ := app.store.ListItems(Query{})

	// The model claims to have found a part that is not there, and marks it as
	// owned. The inventory is the authority, not the model.
	srv, provider := fakeModel(t, `{"ideas":[
		{"title":"Weather box","summary":"s","why":"w","effort":"an evening",
		 "parts":[{"name":"ESP32 devkit","have":true},
		          {"name":"Solar panel","have":true}]}]}`)
	defer srv.Close()

	ideas, err := SuggestWithModel(context.Background(), provider, items,
		SummariseTools(nil), Profile{}, "", 4)
	if err != nil {
		t.Fatalf("SuggestWithModel: %v", err)
	}
	if len(ideas) != 1 {
		t.Fatalf("got %d ideas, want 1", len(ideas))
	}
	byName := map[string]IdeaPart{}
	for _, p := range ideas[0].Parts {
		byName[p.Name] = p
	}
	if !byName["ESP32 devkit"].Have || byName["ESP32 devkit"].ItemID == nil {
		t.Error("a part that really is on the shelf was not linked to the item")
	}
	if byName["Solar panel"].Have {
		t.Error("the model said it had a solar panel and was believed; the shelf is the authority")
	}
	if ideas[0].Enclosure == "" {
		t.Error("no enclosure advice was filled in for an idea that gave none")
	}
}

func TestModelRepliesWrappedInProseStillParse(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "ESP32 devkit", Category: "Microcontroller", Quantity: 1})
	items, _ := app.store.ListItems(Query{})

	// Small models wrap JSON in fences and apologies constantly. Failing on
	// that would make half of them unusable for no good reason.
	srv, provider := fakeModel(t, "Sure! Here you go:\n```json\n"+
		`{"ideas":[{"title":"Blinky","summary":"s","parts":[{"name":"ESP32 devkit"}]}]}`+
		"\n```\nHope that helps.")
	defer srv.Close()

	ideas, err := SuggestWithModel(context.Background(), provider, items,
		SummariseTools(nil), Profile{}, "", 4)
	if err != nil {
		t.Fatalf("a fenced reply was rejected: %v", err)
	}
	if len(ideas) != 1 || ideas[0].Title != "Blinky" {
		t.Fatalf("ideas = %+v", ideas)
	}
}

func TestAModelThatSaysNothingUsableIsAnError(t *testing.T) {
	srv, provider := fakeModel(t, "I'm not sure what you mean.")
	defer srv.Close()
	_, err := SuggestWithModel(context.Background(), provider, nil, Capabilities{}, Profile{}, "", 4)
	if err == nil {
		t.Fatal("a reply with no JSON in it was accepted")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("error = %v, want it to say what went wrong", err)
	}
}

func TestProviderErrorsSayWhatToDo(t *testing.T) {
	// Nothing listening: the most common mistake with a local model.
	dead := AIProvider{Name: "ollama", Kind: "ollama", Endpoint: "http://127.0.0.1:9", Model: "x"}
	_, err := dead.Ask(context.Background(), "s", "p", false)
	if err == nil {
		t.Fatal("a dead endpoint did not error")
	}
	if !strings.Contains(err.Error(), "listening") {
		t.Errorf("error = %v, want it to point at the server not running", err)
	}

	// A hosted provider with no key should say so before making a request.
	noKey := AIProvider{Name: "anthropic", Kind: "anthropic", Endpoint: "https://example.invalid", Model: "x"}
	if _, err := noKey.Ask(context.Background(), "s", "p", false); err == nil ||
		!strings.Contains(err.Error(), "API key") {
		t.Errorf("missing key error = %v", err)
	}

	// And no model set is caught in the same place.
	noModel := AIProvider{Name: "ollama", Kind: "ollama", Endpoint: "http://127.0.0.1:9"}
	if _, err := noModel.Ask(context.Background(), "s", "p", false); err == nil ||
		!strings.Contains(err.Error(), "no model") {
		t.Errorf("missing model error = %v", err)
	}
}

func TestAnthropicRefusalIsNotReadAsAnEmptyAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
		}
		if r.Header.Get("x-api-key") == "" {
			t.Error("the key was not sent")
		}
		if got := r.URL.Path; got != "/v1/messages" {
			t.Errorf("path = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"content":      []any{},
			"stop_reason":  "refusal",
			"stop_details": map[string]string{"explanation": "not going to"},
		})
	}))
	defer srv.Close()

	p := AIProvider{Name: "anthropic", Kind: "anthropic", Endpoint: srv.URL,
		Model: "claude-opus-5", APIKey: "test-key"}
	_, err := p.Ask(context.Background(), "s", "p", false)
	if err == nil {
		t.Fatal("a refusal was treated as a successful empty answer")
	}
	if !strings.Contains(err.Error(), "declined") {
		t.Errorf("error = %v, want it to say the model declined", err)
	}
}

func TestAKeyIsNeverRenderedBack(t *testing.T) {
	app := newTestApp(t)
	const secret = "sk-do-not-show-this-anywhere"
	id, err := app.store.AddProvider(AIProvider{
		Name: "hosted", Kind: "openai", Endpoint: "https://example.com/v1",
		Model: "m", APIKey: secret,
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	for _, path := range []string{"/settings/ai", "/profile", "/welcome?step=ai"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readAll(t, res.Body)
		res.Body.Close()
		if strings.Contains(body, secret) {
			t.Errorf("GET %s put the API key in the page", path)
		}
		if strings.Contains(body, "<no value>") {
			t.Errorf("GET %s rendered a template error:\n%s", path, firstLines(body, 20))
		}
	}

	// Saving with the key box left empty must not wipe the stored key.
	res, err := http.PostForm(srv.URL+fmt.Sprintf("/settings/ai/%d", id), map[string][]string{
		"name": {"hosted"}, "kind": {"openai"},
		"endpoint": {"https://example.com/v1"}, "model": {"m2"}, "api_key": {""},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	res.Body.Close()
	after, err := app.store.GetProvider(id)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if after.APIKey != secret {
		t.Error("saving the form with an empty key box erased the stored key")
	}
	if after.Model != "m2" {
		t.Errorf("the rest of the form did not save: model = %q", after.Model)
	}
}

// --- the whole flow through the web -----------------------------------------

func TestOnboardingAndTheIdeaBoardThroughTheWeb(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "ESP32 devkit", Category: "Microcontroller", Quantity: 3},
		Item{Name: "BME280 breakout", Category: "Sensor", Quantity: 2},
	)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	// A fresh install offers the wizard on the dashboard.
	body := get(t, srv.URL+"/")
	if !strings.Contains(body, "/welcome") {
		t.Error("a fresh install did not offer to set itself up")
	}

	// Step one: who you are.
	res, err := http.PostForm(srv.URL+"/profile", map[string][]string{
		"name": {"Ben"}, "bench": {"garage"}, "skill": {"comfortable"},
		"interests": {"home automation"}, "units": {"mm"}, "next": {"/welcome?step=tools"},
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	res.Body.Close()

	// Step two: paste the tools, and check the review step shows them without
	// having saved anything yet.
	res, err = http.PostForm(srv.URL+"/workshop/interpret", map[string][]string{
		"raw":    {"Prusa MK4 (250x210x220)\nsoldering iron\ndigital calipers"},
		"wizard": {"1"},
	})
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	review := readAll(t, res.Body)
	res.Body.Close()
	if !strings.Contains(review, "Prusa MK4") || !strings.Contains(review, "3D printing") {
		t.Errorf("the review step did not show the interpretation:\n%s", firstLines(review, 30))
	}
	if tools, _ := app.store.ListTools(); len(tools) != 0 {
		t.Errorf("the review step saved %d tools; it must not save until confirmed", len(tools))
	}

	// Step two, confirmed.
	res, err = http.PostForm(srv.URL+"/workshop/confirm", map[string][]string{
		"keep":   {"0", "1", "2"},
		"name":   {"Prusa MK4", "Soldering iron", "Digital calipers"},
		"kind":   {"3d-printer", "soldering", "measurement"},
		"detail": {"250x210x220", "", ""},
		"wizard": {"1"},
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	res.Body.Close()

	tools, _ := app.store.ListTools()
	if len(tools) != 3 {
		t.Fatalf("after confirming, %d tools were saved, want 3", len(tools))
	}
	caps, _ := app.store.Capabilities()
	if !caps.Print3D || !caps.Measure {
		t.Errorf("capabilities did not follow from the tools: %+v", caps)
	}

	// Finish, and the banner goes away for good.
	res, err = http.PostForm(srv.URL+"/welcome/finish", nil)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	res.Body.Close()
	if body := get(t, srv.URL+"/"); strings.Contains(body, "First time here?") {
		t.Error("the setup banner came back after finishing")
	}

	// The idea board, with no model configured at all.
	res, err = http.PostForm(srv.URL+"/ideas/generate", map[string][]string{"source": {"shelf"}})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	res.Body.Close()

	ideas, err := app.store.ListIdeas("")
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	if len(ideas) == 0 {
		t.Fatal("generating from the shelf produced nothing")
	}
	board := get(t, srv.URL+"/ideas")
	if !strings.Contains(board, ideas[0].Title) {
		t.Error("the board does not show the idea that was just generated")
	}

	// Generating again must not double the board.
	before := len(ideas)
	res, _ = http.PostForm(srv.URL+"/ideas/generate", map[string][]string{"source": {"shelf"}})
	res.Body.Close()
	again, _ := app.store.ListIdeas("")
	if len(again) != before {
		t.Errorf("generating twice gave %d ideas, want %d — duplicates were not skipped",
			len(again), before)
	}

	// One idea, its enclosure plan, and turning it into a project.
	page := get(t, fmt.Sprintf("%s/ideas/%d", srv.URL, ideas[0].ID))
	if !strings.Contains(page, "Print it") {
		t.Errorf("the idea page did not use the printer in the plan:\n%s", firstLines(page, 40))
	}
	res, err = http.PostForm(fmt.Sprintf("%s/ideas/%d/plan", srv.URL, ideas[0].ID), nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	res.Body.Close()

	planned, err := app.store.GetIdea(ideas[0].ID)
	if err != nil {
		t.Fatalf("GetIdea: %v", err)
	}
	if !planned.Planned() {
		t.Fatal("planning an idea did not link it to a project")
	}
	project, err := app.store.GetProject(planned.ProjectRef())
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(project.Parts) != len(planned.Parts) {
		t.Errorf("the project got %d parts, the idea had %d", len(project.Parts), len(planned.Parts))
	}
	if project.Name != planned.Title {
		t.Errorf("project name = %q, want %q", project.Name, planned.Title)
	}
}

func TestEveryNewPageRenders(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "ESP32 devkit", Category: "Microcontroller", Quantity: 1})
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	for _, path := range []string{
		"/welcome", "/welcome?step=tools", "/welcome?step=ai", "/welcome?step=done",
		"/profile", "/workshop", "/settings/ai", "/ideas", "/ideas?status=all",
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readAll(t, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.StatusCode)
			continue
		}
		if strings.Contains(body, "<no value>") || strings.Contains(body, "ERROR:") {
			t.Errorf("GET %s rendered a template error:\n%s", path, firstLines(body, 30))
		}
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	return readAll(t, res.Body)
}

// --- the wiring drawing -----------------------------------------------------

func TestWiringDiagramDrawsWhatTheTableSays(t *testing.T) {
	wiring := []PinAssignment{
		{Pin: "3V3", Part: "BME280", Signal: "VCC"},
		{Pin: "GND", Part: "BME280", Signal: "GND"},
		{Pin: "GPIO21", Part: "BME280", Signal: "SDA"},
		{Pin: "GPIO22", Part: "BME280", Signal: "SCL"},
		{Pin: "GPIO21", Part: "OLED", Signal: "SDA", Clashes: []string{"BME280"}},
	}
	svg := WiringDiagram("ESP32 devkit", wiring)
	if svg == "" {
		t.Fatal("nothing was drawn")
	}
	for _, want := range []string{"ESP32 devkit", "BME280", "OLED", "GPIO21", "SDA", "used twice"} {
		if !strings.Contains(svg, want) {
			t.Errorf("the diagram does not mention %q", want)
		}
	}
	// Bench convention: power red, ground black.
	if wireColour("3V3", "VCC") != "#d23b3b" {
		t.Error("power is not drawn red")
	}
	if wireColour("GND", "GND") != "#3b3b3b" {
		t.Error("ground is not drawn black")
	}
	if wireColourName("GND", "GND") != "black" {
		t.Error("the spreadsheet does not name the ground wire colour")
	}
	// Every wire gets a line of its own.
	if got := strings.Count(svg, "<path"); got != len(wiring) {
		t.Errorf("drew %d wires, want %d", got, len(wiring))
	}
	// A device with several wires is one box, not several.
	if got := strings.Count(svg, "<rect"); got != 3 {
		t.Errorf("drew %d boxes, want 3 (controller + two devices)", got)
	}
	// Nothing to draw is an empty string, not an empty picture.
	if WiringDiagram("x", nil) != "" {
		t.Error("an empty pin map still drew something")
	}

	legend := WireLegend(wiring)
	if len(legend) == 0 {
		t.Fatal("no legend was produced")
	}
	for _, e := range legend {
		if e.Signal == "" || e.Colour == "" {
			t.Errorf("incomplete legend entry %+v", e)
		}
	}
}

func TestDiagramEscapesWhatComesFromTheDatabase(t *testing.T) {
	// Part names are free text, and an SVG is markup.
	svg := WiringDiagram(`<script>alert(1)</script>`, []PinAssignment{
		{Pin: `"><script>`, Part: `<img src=x onerror=y>`, Signal: "SDA"},
	})
	if strings.Contains(svg, "<script>") || strings.Contains(svg, "<img") {
		t.Errorf("a part name was written into the drawing unescaped:\n%s", svg)
	}
	if !strings.Contains(svg, "&lt;script&gt;") {
		t.Error("the name was dropped rather than escaped")
	}
}

// --- the header: who you are, and how the page looks ------------------------

func TestAccountMenuSaysWhoYouAreWithNoAccounts(t *testing.T) {
	app := newTestApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	// With no accounts and no profile, the button still has to say something.
	if body := get(t, srv.URL+"/items"); !strings.Contains(body, ">You<") {
		t.Error("with nothing set up, the account button has no name on it")
	}

	// Once a name is given, that is the name.
	if err := app.store.SaveProfile(Profile{Name: "Ben", Units: "mm"}); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	body := get(t, srv.URL+"/items")
	if !strings.Contains(body, "Ben") {
		t.Error("the profile name did not reach the account button")
	}
	// Nobody to switch to, so no switcher.
	if strings.Contains(body, "Switch to") {
		t.Error("a switcher was offered with no other accounts to switch to")
	}
}

func TestAccountMenuOffersTheOtherAccountsButStillAsksForThePassword(t *testing.T) {
	app := newTestApp(t)
	for _, name := range []string{"ada", "bob"} {
		if _, err := app.store.CreateUser(name, "correct horse battery", name == "ada"); err != nil {
			t.Fatalf("CreateUser %s: %v", name, err)
		}
	}
	// Rebuilding auth is what picks up accounts written straight to the table:
	// the handler path also mints the session key, which sessions need.
	auth, err := NewAuth("", filepath.Join(app.cfg.DataDir, "session.key"), app.store)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	app.auth = auth

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}

	res, err := client.PostForm(srv.URL+"/login", map[string][]string{
		"username": {"ada"}, "password": {"correct horse battery"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	res.Body.Close()

	res, err = client.Get(srv.URL + "/items")
	if err != nil {
		t.Fatalf("GET /items: %v", err)
	}
	body := readAll(t, res.Body)
	res.Body.Close()

	if !strings.Contains(body, ">ada<") {
		t.Errorf("the signed-in account is not on the button:\n%s", firstLines(body, 6))
	}
	if !strings.Contains(body, "Switch to") || !strings.Contains(body, "/login?as=bob") {
		t.Error("the other account is not offered in the menu")
	}
	if strings.Contains(body, "/login?as=ada") {
		t.Error("the menu offers switching to the account already signed in")
	}

	// Switching goes through the sign-in form, with the name filled in and the
	// password still required. Anything else would be a hole, not a feature.
	res, err = client.Get(srv.URL + "/login?as=bob")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	login := readAll(t, res.Body)
	res.Body.Close()
	if !strings.Contains(login, `value="bob"`) {
		t.Errorf("the sign-in form did not prefill the account being switched to:\n%s",
			firstLines(login, 24))
	}
	if !strings.Contains(login, `name="password"`) {
		t.Error("the sign-in form skipped the password")
	}
	// And it really is still ada until the password is given.
	res, err = client.Get(srv.URL + "/items")
	if err != nil {
		t.Fatalf("GET /items: %v", err)
	}
	after := readAll(t, res.Body)
	res.Body.Close()
	if !strings.Contains(after, ">ada<") {
		t.Error("visiting the switch link changed who is signed in without a password")
	}
}

func TestThemeChoiceCanBeatTheSystemInBothDirections(t *testing.T) {
	css, err := staticFS.ReadFile("static/style.css")
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	sheet := string(css)

	// Light defined only inside the media query would make the toggle one-way:
	// somebody on a light laptop could never choose dark.
	if !strings.Contains(sheet, `:root[data-theme="light"]`) {
		t.Error("no explicit light theme, so Light cannot override a dark system")
	}
	if !strings.Contains(sheet, `:root:not([data-theme="dark"])`) {
		t.Error("the light media query is not guarded, so Dark cannot override a light system")
	}

	// The choice has to be applied before the stylesheet paints or every load
	// flashes the wrong colours.
	layout, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout: %v", err)
	}
	head := string(layout)
	script := strings.Index(head, "localStorage.getItem('theme')")
	sheetLink := strings.Index(head, `href="/static/style.css"`)
	if script < 0 || sheetLink < 0 {
		t.Fatal("the theme script or the stylesheet link is missing from the layout")
	}
	if script > sheetLink {
		t.Error("the theme is applied after the stylesheet loads, which flashes on every page")
	}
}

func TestEveryIconOnlyButtonHasAName(t *testing.T) {
	// A button whose only content is a glyph reads as "times" or nothing at all
	// to a screen reader unless it is given a label.
	names, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, f := range names {
		body, err := templateFS.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatalf("read %s: %v", f.Name(), err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "<button") {
				continue
			}
			for _, glyph := range []string{">×<", ">✕<", ">＋<"} {
				if strings.Contains(line, glyph) && !strings.Contains(line, "aria-label") {
					t.Errorf("%s: icon-only button with no aria-label:\n  %s",
						f.Name(), strings.TrimSpace(line))
				}
			}
		}
	}
}
