package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCanonicalManufacturerFoldsAbbreviations(t *testing.T) {
	cases := map[string]string{
		"TI":                "Texas Instruments",
		"ti":                "Texas Instruments",
		"Texas Instruments": "Texas Instruments",
		"ST":                "STMicroelectronics",
		"  Seeed  ":         "Seeed Studio",
		"Espressif Systems": "Espressif",
		"Atmel":             "Microchip",
		// Case alone must not split a maker in two: every name in the built-in
		// table is spelled the way the maker spells it, however you type it.
		"raspberry pi":       "Raspberry Pi",
		"RASPBERRY PI":       "Raspberry Pi",
		"espressif":          "Espressif",
		"sparkfun":           "SparkFun",
		"stmicroelectronics": "STMicroelectronics",
		"on semiconductor":   "ON Semiconductor",
		"rohm":               "ROHM",
		// Anything not in the table is kept exactly as typed, only tidied.
		"Some   Small  Maker": "Some Small Maker",
		"":                    "",
	}
	for in, want := range cases {
		if got := CanonicalManufacturer(in); got != want {
			t.Errorf("CanonicalManufacturer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMonogramAndHueAreStable(t *testing.T) {
	cases := map[string]string{
		"Espressif": "ES", "Raspberry Pi": "RP", "Texas Instruments": "TI",
		"X": "X", "": "?",
	}
	for in, want := range cases {
		if got := monogram(in); got != want {
			t.Errorf("monogram(%q) = %q, want %q", in, got, want)
		}
	}
	// The colour must not move between page loads, or a maker changes colour
	// every time you look at it.
	first := Manufacturer{Name: "Espressif"}.Hue()
	for i := 0; i < 5; i++ {
		if got := (Manufacturer{Name: "ESPRESSIF"}).Hue(); got != first {
			t.Fatalf("hue moved between calls: %d then %d", first, got)
		}
	}
	if first == (Manufacturer{Name: "Adafruit"}).Hue() {
		t.Error("two makers landed on the same hue; the colours would not distinguish them")
	}
}

func TestManufacturersGroupTheShelf(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", Manufacturer: "Espressif", Quantity: 3},
		Item{Name: "ESP32-C3", Manufacturer: "Espressif", Quantity: 2},
		Item{Name: "Pi 5", Manufacturer: "Raspberry Pi", Quantity: 1},
		Item{Name: "Mystery board", Quantity: 1},
	)
	app.store.SetPrice(ids[0], Price{Source: "Adafruit", Amount: 10})
	app.store.SetPrice(ids[2], Price{Source: "Pimoroni", Amount: 60})

	makers, err := app.store.Manufacturers()
	if err != nil {
		t.Fatalf("Manufacturers: %v", err)
	}
	if len(makers) != 2 {
		t.Fatalf("got %d makers, want 2 — the item with none should not become one", len(makers))
	}
	// Most items first.
	if makers[0].Name != "Espressif" || makers[0].Items != 2 || makers[0].Pieces != 5 {
		t.Errorf("first maker = %+v, want Espressif with 2 items and 5 pieces", makers[0])
	}
	if makers[0].Value != 30 {
		t.Errorf("Espressif value = %v, want 3 x 10 for the only priced item", makers[0].Value)
	}
	if makers[1].Name != "Raspberry Pi" || makers[1].Value != 60 {
		t.Errorf("second maker = %+v", makers[1])
	}

	// And filtering by one really narrows the grid.
	got, err := app.store.ListItems(Query{Manufacturer: "espressif"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("filtering by manufacturer gave %d items, want 2 (and it should ignore case)", len(got))
	}
}

func TestMergeManufacturerMovesEverything(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "A", Manufacturer: "Espressif", Quantity: 1},
		Item{Name: "B", Manufacturer: "espressif systems", Quantity: 1},
	)
	if err := app.store.SaveManufacturer(Manufacturer{
		Name: "espressif systems", Domain: "espressif.com",
	}); err != nil {
		t.Fatalf("SaveManufacturer: %v", err)
	}

	n, err := app.store.RenameManufacturer("espressif systems", "Espressif")
	if err != nil {
		t.Fatalf("RenameManufacturer: %v", err)
	}
	if n != 1 {
		t.Errorf("moved %d items, want 1", n)
	}
	makers, _ := app.store.Manufacturers()
	if len(makers) != 1 || makers[0].Items != 2 {
		t.Fatalf("after merging, makers = %+v, want one with both items", makers)
	}
	// The old details row goes with it rather than lingering unreachable.
	var rows int
	app.store.db.QueryRow(`SELECT COUNT(*) FROM manufacturers WHERE name = 'espressif systems'`).Scan(&rows)
	if rows != 0 {
		t.Error("the merged-away manufacturer left its details row behind")
	}
}

func TestLogoIsFoundFromTheSiteAndFallsBackToAFavicon(t *testing.T) {
	app := newTestApp(t)

	// A site that declares a decent apple-touch icon plus a useless .ico.
	var site *httptest.Server
	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apple-touch-icon.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(testPNG(t))
		case "/favicon.ico":
			w.Header().Set("Content-Type", "image/vnd.microsoft.icon")
			w.Write([]byte("not a decodable image"))
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><head>
				<link rel="icon" href="%s/favicon.ico" type="image/vnd.microsoft.icon">
				<link rel="apple-touch-icon" sizes="180x180" href="%s/apple-touch-icon.png">
			</head><body>hello</body></html>`, site.URL, site.URL)
		}
	}))
	defer site.Close()

	icons := app.siteIcons(context.Background(), site.URL)
	if len(icons) != 1 || !strings.HasSuffix(icons[0], "apple-touch-icon.png") {
		t.Fatalf("icons = %v, want only the PNG — an .ico cannot be decoded and is worse than nothing", icons)
	}
}

func TestSiteIconsSkipFormatsNothingCanDecode(t *testing.T) {
	for _, bad := range []string{"/favicon.ico", "/logo.svg", "/x.ICO?v=2"} {
		if usableIconFormat(bad) {
			t.Errorf("usableIconFormat(%q) = true, but nothing here decodes it", bad)
		}
	}
	for _, good := range []string{"/apple-touch-icon.png", "/logo.jpg", "/i/logo?size=180", "/x.webp"} {
		if !usableIconFormat(good) {
			t.Errorf("usableIconFormat(%q) = false, want it tried", good)
		}
	}
	// The biggest declared icon is the one worth having.
	if iconSize("180x180", "apple-touch-icon") <= iconSize("16x16", "icon") {
		t.Error("a 180px icon should outrank a 16px one")
	}
	if iconSize("", "apple-touch-icon") <= iconSize("", "icon") {
		t.Error("an apple-touch icon with no declared size should still outrank a plain one")
	}
}

func TestFetchLogoRefusesWithNothingToGoOn(t *testing.T) {
	app := newTestApp(t)
	_, err := app.FetchLogo(context.Background(), Manufacturer{Name: "Some Tiny Maker"})
	if err == nil {
		t.Fatal("a maker with no known website should not silently succeed")
	}
	if !strings.Contains(err.Error(), "no website known") {
		t.Errorf("error = %v, want it to say what is missing", err)
	}
	// One of the makers in the built-in table needs no help finding its domain.
	if got := DomainForManufacturer("Espressif"); got != "espressif.com" {
		t.Errorf("DomainForManufacturer(Espressif) = %q", got)
	}
	if got := DomainForManufacturer("  raspberry pi  "); got != "raspberrypi.com" {
		t.Errorf("DomainForManufacturer with padding = %q", got)
	}
}

func TestManufacturerFlowsThroughTheWeb(t *testing.T) {
	app := newTestApp(t)

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	// The form canonicalises on the way in, so two spellings never diverge.
	for _, maker := range []string{"TI", "Texas Instruments"} {
		res, err := http.PostForm(srv.URL+"/items", map[string][]string{
			"name": {"Op-amp " + maker}, "quantity": {"5"}, "manufacturer": {maker},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		res.Body.Close()
	}
	makers, err := app.store.Manufacturers()
	if err != nil {
		t.Fatalf("Manufacturers: %v", err)
	}
	if len(makers) != 1 || makers[0].Name != "Texas Instruments" || makers[0].Items != 2 {
		t.Fatalf("makers = %+v, want TI folded into Texas Instruments", makers)
	}

	for _, path := range []string{"/manufacturers", "/items?manufacturer=Texas+Instruments", "/items/1"} {
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
		if !strings.Contains(body, "Texas Instruments") {
			t.Errorf("GET %s does not mention the manufacturer", path)
		}
	}

	// The monogram stands in until a logo exists, so a maker is never blank.
	res, _ := http.Get(srv.URL + "/manufacturers")
	body := readAll(t, res.Body)
	res.Body.Close()
	if !strings.Contains(body, "maker-mark") || !strings.Contains(body, ">TI<") {
		t.Error("the manufacturers page does not show a monogram for a maker with no logo")
	}
}

func TestAnUnknownMakerKeepsTheSpellingTheShelfAlreadyUses(t *testing.T) {
	app := newTestApp(t)

	// Nobody has heard of this one, so there is no table to fold it against --
	// but the shelf itself remembers how it was spelled the first time.
	seed(t, app.store, Item{Name: "Odd relay", Manufacturer: "Bitsy Relay Werks", Quantity: 1})

	if got := app.store.SettleManufacturer("bitsy relay werks"); got != "Bitsy Relay Werks" {
		t.Errorf("SettleManufacturer = %q, want the spelling already on the shelf", got)
	}
	// A maker in the built-in table answers from the table, shelf or no shelf.
	if got := app.store.SettleManufacturer("RASPBERRY PI"); got != "Raspberry Pi" {
		t.Errorf("SettleManufacturer(RASPBERRY PI) = %q", got)
	}
	// And an unknown maker nobody has typed before is kept as typed.
	if got := app.store.SettleManufacturer("Brand New Supplier"); got != "Brand New Supplier" {
		t.Errorf("SettleManufacturer of a first-time maker = %q", got)
	}

	// Through the form, the two spellings must land on one maker.
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	res, err := http.PostForm(srv.URL+"/items", map[string][]string{
		"name": {"Another relay"}, "quantity": {"2"},
		"manufacturer": {"BITSY RELAY WERKS"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	res.Body.Close()

	makers, err := app.store.Manufacturers()
	if err != nil {
		t.Fatalf("Manufacturers: %v", err)
	}
	if len(makers) != 1 || makers[0].Name != "Bitsy Relay Werks" || makers[0].Items != 2 {
		t.Fatalf("makers = %+v, want one maker holding both relays", makers)
	}
	// The filter chips come from the stored spelling, so one pile means one chip.
	var spellings int
	app.store.db.QueryRow(`SELECT COUNT(DISTINCT manufacturer) FROM items
		WHERE manufacturer <> ''`).Scan(&spellings)
	if spellings != 1 {
		t.Errorf("the shelf holds %d spellings of one maker, want 1", spellings)
	}
}

func TestOneLogoFetchPerMakerAtATime(t *testing.T) {
	app := newTestApp(t)

	// Ten parts from the same maker arriving at once should send one person to
	// fetch the logo, not ten -- otherwise nine downloads are wasted and the
	// nine losers leave their files orphaned on disk.
	var got int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if app.claimFetch("Espressif") {
				atomic.AddInt64(&got, 1)
			}
		}()
	}
	wg.Wait()
	if got != 1 {
		t.Errorf("%d goroutines started a fetch, want 1", got)
	}

	// And once that one finishes, the next save may try again -- a failed
	// fetch must not lock the maker out forever.
	app.releaseFetch("Espressif")
	if !app.claimFetch("Espressif") {
		t.Error("the maker stayed claimed after its fetch finished")
	}
	if !app.claimFetch("Raspberry Pi") {
		t.Error("claiming one maker blocked a different one")
	}
}

func TestUploadedLogoWinsAndReplacesTheOldOne(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "ESP32", Manufacturer: "Espressif", Quantity: 1})

	first, err := app.photos.Save(strings.NewReader(string(testPNG(t))), "old.png")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := app.store.SaveManufacturer(Manufacturer{Name: "Espressif", Logo: first}); err != nil {
		t.Fatalf("SaveManufacturer: %v", err)
	}

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	body, contentType := multipartForm(t, map[string]string{"name": "Espressif"},
		"logo", "new.png", testPNG(t))
	res, err := http.Post(srv.URL+"/manufacturers/logo", contentType, body)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	res.Body.Close()

	m, err := app.store.GetManufacturer("Espressif")
	if err != nil {
		t.Fatalf("GetManufacturer: %v", err)
	}
	if m.Logo == "" || m.Logo == first {
		t.Fatalf("logo = %q, want the newly uploaded one", m.Logo)
	}
	// The replaced logo must not be left orphaned on disk.
	orphans, _ := app.photoDrift()
	if contains(orphans, first) {
		t.Error("the old logo was left behind on disk")
	}
}
