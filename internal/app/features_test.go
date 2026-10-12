package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnvac/backoffice/internal/media"
)

// --- tags -------------------------------------------------------------------

func TestTagsAreStoredNormalisedAndCaseFolded(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "ESP32", Tags: tags("WiFi", "3v3")})[0]

	// A second item reusing the same tag in different case must not create a
	// duplicate row, or the tag filter shows the same tag twice.
	seed(t, app.store, Item{Name: "ESP8266", Tags: tags("wifi")})

	facets, err := app.store.TagFacets()
	if err != nil {
		t.Fatalf("TagFacets: %v", err)
	}
	counts := map[string]int{}
	for _, f := range facets {
		counts[strings.ToLower(f.Value)] = f.Count
	}
	if len(facets) != 2 {
		t.Errorf("tag facets = %+v, want 2 distinct tags", facets)
	}
	if counts["wifi"] != 2 {
		t.Errorf("wifi tag count = %d, want 2 (case-insensitive)", counts["wifi"])
	}

	it, _ := app.store.GetItem(id)
	if got := it.TagString(); got != "3v3, WiFi" {
		t.Errorf("tags = %q, want them sorted and preserved as first written", got)
	}
}

func TestFilteringBySeveralTagsNarrows(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "SMD resistor", Tags: tags("smd", "passive")},
		Item{Name: "SMD LED", Tags: tags("smd", "optical")},
		Item{Name: "Through-hole resistor", Tags: tags("tht", "passive")},
	)

	one, _ := app.store.ListItems(Query{Tags: []string{"smd"}})
	if len(one) != 2 {
		t.Errorf("one tag matched %d items, want 2", len(one))
	}
	both, _ := app.store.ListItems(Query{Tags: []string{"smd", "passive"}})
	if len(both) != 1 || both[0].Name != "SMD resistor" {
		t.Errorf("two tags matched %d items, want only the SMD resistor", len(both))
	}
	none, _ := app.store.ListItems(Query{Tags: []string{"smd", "tht"}})
	if len(none) != 0 {
		t.Errorf("contradictory tags matched %d items, want 0", len(none))
	}
}

func TestUpdatingTagsRemovesOrphans(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Board", Tags: tags("temporary")})[0]

	it, _ := app.store.GetItem(id)
	it.Tags = tags("kept")
	if err := app.store.UpdateItem(it); err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	facets, _ := app.store.TagFacets()
	if len(facets) != 1 || facets[0].Value != "kept" {
		t.Errorf("tag facets = %+v, want only \"kept\" -- unused tags should be dropped", facets)
	}
}

// --- folders ----------------------------------------------------------------

func TestFolderTreeRollsUpDescendantCounts(t *testing.T) {
	app := newTestApp(t)
	lab, _ := app.store.CreateFolder("Lab", nil)
	bench, _ := app.store.CreateFolder("Bench", &lab)
	drawer, _ := app.store.CreateFolder("Drawer", &bench)
	other, _ := app.store.CreateFolder("Storage", nil)

	seed(t, app.store,
		Item{Name: "Scope", Quantity: 1, FolderID: &lab},
		Item{Name: "Iron", Quantity: 2, FolderID: &bench},
		Item{Name: "Resistors", Quantity: 500, FolderID: &drawer},
		Item{Name: "Boxes", Quantity: 4, FolderID: &other},
		Item{Name: "Loose part", Quantity: 9},
	)

	tree, err := app.store.FolderTree()
	if err != nil {
		t.Fatalf("FolderTree: %v", err)
	}
	if len(tree) != 2 {
		t.Fatalf("got %d root folders, want 2", len(tree))
	}

	byName := map[string]*Folder{}
	for _, f := range FlattenFolders(tree) {
		byName[f.Name] = f
	}
	// Lab holds one item directly but three counting Bench and Drawer.
	if got := byName["Lab"].ItemCount; got != 1 {
		t.Errorf("Lab direct items = %d, want 1", got)
	}
	if got := byName["Lab"].TotalItems; got != 3 {
		t.Errorf("Lab total items = %d, want 3 including subfolders", got)
	}
	if got := byName["Lab"].TotalPieces; got != 503 {
		t.Errorf("Lab total pieces = %d, want 503", got)
	}
	if got := byName["Drawer"].Depth; got != 2 {
		t.Errorf("Drawer depth = %d, want 2", got)
	}
}

func TestFolderScopedListing(t *testing.T) {
	app := newTestApp(t)
	parent, _ := app.store.CreateFolder("Parent", nil)
	child, _ := app.store.CreateFolder("Child", &parent)

	seed(t, app.store,
		Item{Name: "In parent", FolderID: &parent},
		Item{Name: "In child", FolderID: &child},
		Item{Name: "Unfiled thing"},
	)

	direct, _ := app.store.ListItems(Query{FolderID: &parent})
	if len(direct) != 1 || direct[0].Name != "In parent" {
		t.Errorf("folder listing = %d items, want only the folder's own", len(direct))
	}
	deep, _ := app.store.ListItems(Query{FolderID: &parent, Recursive: true})
	if len(deep) != 2 {
		t.Errorf("recursive folder listing = %d items, want 2", len(deep))
	}
	unfiled, _ := app.store.ListItems(Query{Unfiled: true})
	if len(unfiled) != 1 || unfiled[0].Name != "Unfiled thing" {
		t.Errorf("unfiled listing = %+v, want just the unfiled item", unfiled)
	}
}

func TestDeletingFolderKeepsItemsAndUnfilesThem(t *testing.T) {
	app := newTestApp(t)
	parent, _ := app.store.CreateFolder("Parent", nil)
	child, _ := app.store.CreateFolder("Child", &parent)
	ids := seed(t, app.store,
		Item{Name: "Top item", FolderID: &parent},
		Item{Name: "Nested item", FolderID: &child},
	)

	if err := app.store.DeleteFolder(parent); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	for _, id := range ids {
		it, err := app.store.GetItem(id)
		if err != nil {
			t.Fatalf("item %d was deleted along with its folder: %v", id, err)
		}
		if it.FolderID != nil {
			t.Errorf("%s still points at folder %d after the folder was deleted", it.Name, *it.FolderID)
		}
	}
	if _, err := app.store.GetFolder(child); err != sql.ErrNoRows {
		t.Error("subfolder outlived its parent")
	}
}

func TestFolderCannotBeMovedIntoItsOwnSubtree(t *testing.T) {
	app := newTestApp(t)
	root, _ := app.store.CreateFolder("Root", nil)
	mid, _ := app.store.CreateFolder("Mid", &root)
	leaf, _ := app.store.CreateFolder("Leaf", &mid)

	if err := app.store.RenameFolder(root, "Root", &leaf); err == nil {
		t.Error("moving a folder under its own descendant was allowed, which would orphan the branch")
	}
	if err := app.store.RenameFolder(root, "Root", &root); err == nil {
		t.Error("a folder was allowed to become its own parent")
	}
	// A legitimate move still works.
	if err := app.store.RenameFolder(leaf, "Leaf", &root); err != nil {
		t.Errorf("valid reparent rejected: %v", err)
	}
}

func TestAncestorsGivesBreadcrumbPath(t *testing.T) {
	app := newTestApp(t)
	a, _ := app.store.CreateFolder("A", nil)
	b, _ := app.store.CreateFolder("B", &a)
	c, _ := app.store.CreateFolder("C", &b)

	path, err := app.store.Ancestors(c)
	if err != nil {
		t.Fatalf("Ancestors: %v", err)
	}
	var names []string
	for _, f := range path {
		names = append(names, f.Name)
	}
	if strings.Join(names, "/") != "A/B/C" {
		t.Errorf("breadcrumb = %v, want A/B/C (root first)", names)
	}
}

// --- SSRF guards ------------------------------------------------------------

func TestPrivateAddressesAreRejected(t *testing.T) {
	blocked := []string{
		"127.0.0.1",        // loopback
		"10.1.2.3",         // RFC1918
		"192.168.1.1",      // home router
		"172.16.0.1",       // RFC1918
		"169.254.169.254",  // cloud metadata
		"100.64.0.1",       // carrier NAT / Tailscale
		"0.0.0.0",          // unspecified
		"::1",              // IPv6 loopback
		"fd00::1",          // IPv6 unique-local
		"fe80::1",          // IPv6 link-local
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"255.255.255.255",  // broadcast
	}
	for _, s := range blocked {
		ip := netip.MustParseAddr(s)
		if isPublicIP(ip) {
			t.Errorf("isPublicIP(%s) = true, want false -- this address is reachable only from inside the network", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700::1111"} {
		if !isPublicIP(netip.MustParseAddr(s)) {
			t.Errorf("isPublicIP(%s) = false, want true", s)
		}
	}
}

func TestFetcherRefusesPrivateHostsByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><head><title>Router admin</title></head></html>"))
	}))
	defer srv.Close()

	// httptest listens on loopback, which is exactly what must be blocked.
	guarded := NewFetcher(false)
	if _, err := guarded.FetchPage(context.Background(), srv.URL); err == nil {
		t.Fatal("fetcher reached a loopback address with the guard on")
	} else if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error = %v, want it to explain the address was refused", err)
	}

	// And the escape hatch works, otherwise nobody could use a LAN wiki.
	open := NewFetcher(true)
	meta, err := open.FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ALLOW_PRIVATE_FETCH did not permit a LAN fetch: %v", err)
	}
	if meta.Title != "Router admin" {
		t.Errorf("title = %q, want %q", meta.Title, "Router admin")
	}
}

func TestFetcherRejectsNonHTTPSchemes(t *testing.T) {
	f := NewFetcher(true)
	for _, raw := range []string{"file:///etc/passwd", "gopher://x/1", "ftp://example.com/f"} {
		if _, err := f.FetchPage(context.Background(), raw); err == nil {
			t.Errorf("FetchPage(%q) succeeded, want a scheme rejection", raw)
		}
	}
}

// --- metadata import --------------------------------------------------------

const productPage = `<!doctype html><html><head>
<title>Ignored when OpenGraph is present</title>
<meta property="og:title" content="ESP32-WROOM-32 Development Board">
<meta property="og:description" content="  38-pin devkit   with   USB-C. ">
<meta property="og:site_name" content="Example Parts">
<meta property="og:image" content="/img/board.jpg">
<meta property="product:price:amount" content="7.50">
<meta property="product:price:currency" content="USD">
<meta property="mpn" content="ESP32-WROOM-32E">
</head><body>irrelevant</body></html>`

func TestFetchPageReadsOpenGraphMetadata(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(productPage))
	}))
	defer srv.Close()

	meta, err := NewFetcher(true).FetchPage(context.Background(), srv.URL+"/products/esp32")
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if meta.Title != "ESP32-WROOM-32 Development Board" {
		t.Errorf("title = %q", meta.Title)
	}
	if meta.Description != "38-pin devkit with USB-C." {
		t.Errorf("description = %q, want whitespace collapsed", meta.Description)
	}
	if meta.Price != 7.50 || meta.Currency != "USD" {
		t.Errorf("price = %v %s, want 7.50 USD", meta.Price, meta.Currency)
	}
	if meta.PartNumber != "ESP32-WROOM-32E" {
		t.Errorf("part number = %q", meta.PartNumber)
	}
	// A root-relative og:image has to be resolved against the page URL or the
	// follow-up image fetch has nothing to work with.
	want := srv.URL + "/img/board.jpg"
	if meta.ImageURL != want {
		t.Errorf("image = %q, want it resolved to %q", meta.ImageURL, want)
	}
	if meta.SiteName != "Example Parts" {
		t.Errorf("site = %q", meta.SiteName)
	}
}

func TestFetchPageFallsBackToTitleTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>  Plain   Datasheet  </title></head></html>`))
	}))
	defer srv.Close()

	meta, err := NewFetcher(true).FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if meta.Title != "Plain Datasheet" {
		t.Errorf("title = %q, want the <title> used when OpenGraph is missing", meta.Title)
	}
}

func TestImportPreviewEndpointReturnsFields(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(productPage))
	}))
	defer page.Close()

	app := newTestApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/import/preview", url.Values{"url": {page.URL}})
	if err != nil {
		t.Fatalf("POST /import/preview: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body := readAll(t, res.Body)
	for _, want := range []string{"ESP32-WROOM-32 Development Board", "7.5", "ESP32-WROOM-32E", "/img/board.jpg"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview response missing %q\ngot: %s", want, body)
		}
	}
}

func TestImportPreviewReportsBadURLAsClientError(t *testing.T) {
	app := newTestApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/import/preview", url.Values{"url": {"not a url at all"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 -- a bad paste is the user's error, not a server fault", res.StatusCode)
	}
}

func TestPhotoImportFromURL(t *testing.T) {
	jpegBytes := testJPEG(t, 800, 600, 0)
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".jpg") {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpegBytes)
			return
		}
		// A product page, so the fallback path gets exercised too.
		fmt.Fprintf(w, `<html><head><meta property="og:image" content="/board.jpg"></head></html>`)
	}))
	defer imgSrv.Close()

	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Board"})[0]
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	// Direct image URL.
	if msg := app.savePhotoFromURL(r.Context(), id, imgSrv.URL+"/board.jpg"); msg != "" {
		t.Fatalf("direct image import failed: %s", msg)
	}
	// A page URL: the image should be found via its og:image.
	if msg := app.savePhotoFromURL(r.Context(), id, imgSrv.URL+"/product"); msg != "" {
		t.Fatalf("page import did not fall back to og:image: %s", msg)
	}

	it, _ := app.store.GetItem(id)
	if len(it.Photos) != 2 {
		t.Fatalf("item has %d photos, want 2", len(it.Photos))
	}
	for _, p := range it.Photos {
		if !media.SafeName.MatchString(p.Filename) {
			t.Errorf("stored filename %q is not servable", p.Filename)
		}
		if _, _, err := decodeFile(app.photos.ThumbPath(p.Filename)); err != nil {
			t.Errorf("no thumbnail generated for imported photo: %v", err)
		}
	}
}

func TestPhotoImportRejectsNonImagePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><head><title>No pictures here</title></head></html>"))
	}))
	defer srv.Close()

	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Board"})[0]
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	if msg := app.savePhotoFromURL(r.Context(), id, srv.URL); msg == "" {
		t.Error("importing a page with no image reported success")
	}
	it, _ := app.store.GetItem(id)
	if len(it.Photos) != 0 {
		t.Errorf("item gained %d photos from a failed import", len(it.Photos))
	}
}

// --- migrations -------------------------------------------------------------

func TestMigrationCarriesOldTagsIntoTheTagTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// Build a database at the original schema, as an existing deployment has.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := raw.Exec(migrations[0].sql); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	_, err = raw.Exec(`INSERT INTO items (name, category, quantity, location, part_number,
		value, tags, link, notes, created_at, updated_at)
		VALUES ('Old item','MCU',3,'Drawer 1','X','v','wifi, SMD , wifi','','',
			'2024-01-01T00:00:00Z','2024-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	raw.Close()

	// Opening it with the current build must upgrade it in place.
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB on an old database: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}

	items, err := store.ListItems(Query{})
	if err != nil {
		t.Fatalf("ListItems after migration: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items after migration, want the existing row preserved", len(items))
	}
	if got := items[0].TagString(); got != "SMD, wifi" {
		t.Errorf("migrated tags = %q, want the old comma list split and de-duplicated", got)
	}
	if items[0].Name != "Old item" || items[0].Quantity != 3 {
		t.Errorf("existing data changed during migration: %+v", items[0])
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.db")
	for i := 0; i < 3; i++ {
		db, err := openDB(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		db.Close()
	}
}

// --- pages ------------------------------------------------------------------

func TestDashboardAndFolderPagesRender(t *testing.T) {
	app := newTestApp(t)
	lab, _ := app.store.CreateFolder("Lab bench", nil)
	seed(t, app.store,
		Item{Name: "Soldering iron", Quantity: 1, FolderID: &lab, Tags: tags("tools")},
		Item{Name: "Empty reel", Quantity: 0},
	)

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{}

	dash := getBody(t, client, srv.URL+"/")
	for _, want := range []string{"Lab bench", "Backoffice", "tools", "Running low", "Unfiled"} {
		if !strings.Contains(dash, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}

	folder := getBody(t, client, srv.URL+fmt.Sprintf("/folders/%d", lab))
	if !strings.Contains(folder, "Soldering iron") {
		t.Error("folder page does not list its item")
	}
	if strings.Contains(folder, "Empty reel") {
		t.Error("folder page leaked an item from outside the folder")
	}

	// Tag filtering from the grid.
	tagged := getBody(t, client, srv.URL+"/items?tag=tools")
	if !strings.Contains(tagged, "Soldering iron") || strings.Contains(tagged, "Empty reel") {
		t.Error("tag filter did not narrow the grid")
	}
}

func TestCreateItemWithFolderAndTagsOverHTTP(t *testing.T) {
	app := newTestApp(t)
	folder, _ := app.store.CreateFolder("Sensors", nil)

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	form := url.Values{
		"name":      {"DS18B20"},
		"quantity":  {"9"},
		"folder_id": {fmt.Sprint(folder)},
		"tags":      {"1-wire, temperature, 1-wire"},
	}
	res, err := client.PostForm(srv.URL+"/items", form)
	if err != nil {
		t.Fatalf("POST /items: %v", err)
	}
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}

	items, _ := app.store.ListItems(Query{FolderID: &folder})
	if len(items) != 1 {
		t.Fatalf("folder holds %d items, want 1", len(items))
	}
	if got := items[0].TagString(); got != "1-wire, temperature" {
		t.Errorf("tags = %q, want duplicates dropped", got)
	}
}

func TestQueryURLTogglesAndPreservesFilters(t *testing.T) {
	q := Query{Search: "esp", Category: "MCU", Tags: []string{"wifi"}}

	got := q.URL("location", "Drawer 3")
	for _, want := range []string{"q=esp", "category=MCU", "tag=wifi", "location=Drawer+3"} {
		if !strings.Contains(got, want) {
			t.Errorf("URL = %q, missing %q -- filters must compose", got, want)
		}
	}
	// Re-selecting the active value clears it.
	if got := q.URL("category", "MCU"); strings.Contains(got, "category=") {
		t.Errorf("URL = %q, want the category cleared when re-selected", got)
	}
	if got := q.WithTagToggled("wifi"); strings.Contains(got, "tag=") {
		t.Errorf("URL = %q, want the tag removed when toggled off", got)
	}
	if got := q.WithTagToggled("smd"); !strings.Contains(got, "tag=wifi") || !strings.Contains(got, "tag=smd") {
		t.Errorf("URL = %q, want both tags kept", got)
	}

	// Inside a folder the filters stay on the folder's own page.
	id := int64(7)
	scoped := Query{FolderID: &id, Sort: "name"}
	if got := scoped.URL("sort", "low"); !strings.HasPrefix(got, "/folders/7?") {
		t.Errorf("URL = %q, want it to stay under /folders/7", got)
	}
}

func TestImportDecodesDoubleEncodedEntities(t *testing.T) {
	// Real shops (Adafruit among them) emit OpenGraph text whose entities are
	// encoded twice, so the parser yields a literal "&#39;" in the description.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head>
			<meta property="og:title" content="Adafruit HUZZAH32 &amp;#8211; ESP32 Feather">
			<meta property="og:description" content="Aww yeah, it&amp;#39;s the Feather you&amp;#39;ve waited for.">
			</head></html>`)
	}))
	defer srv.Close()

	meta, err := NewFetcher(true).FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if strings.Contains(meta.Title, "&#") || strings.Contains(meta.Description, "&#") {
		t.Errorf("entities survived into the form:\n title = %q\n notes = %q", meta.Title, meta.Description)
	}
	if meta.Description != "Aww yeah, it's the Feather you've waited for." {
		t.Errorf("description = %q", meta.Description)
	}
	if meta.Title != "Adafruit HUZZAH32 – ESP32 Feather" {
		t.Errorf("title = %q", meta.Title)
	}
}

func TestUndecodableButValidImageIsKept(t *testing.T) {
	// Go's image/jpeg rejects some perfectly valid JPEGs ("unsupported JPEG
	// feature: luma/chroma subsampling ratio"), which real product photos and
	// camera output do hit. Losing the photo over that is worse than having no
	// thumbnail, so the original is stored and served in the thumbnail's place.
	app := newTestApp(t)

	valid := testJPEG(t, 40, 30, 0)
	undecodable := append([]byte(nil), valid[:6]...) // keep the JPEG magic
	undecodable = append(undecodable, []byte("garbage that no decoder accepts")...)

	name, err := app.photos.Save(bytes.NewReader(undecodable), "weird.jpg")
	if err != nil {
		t.Fatalf("a JPEG the decoder cannot read was rejected outright: %v", err)
	}
	if _, err := os.Stat(app.photos.Path(name)); err != nil {
		t.Fatalf("original was not stored: %v", err)
	}

	// Serving its thumbnail must fall back to the original rather than 404.
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/media/thumb/" + name)
	if err != nil {
		t.Fatalf("GET thumb: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("thumbnail request = %d, want 200 by falling back to the original", res.StatusCode)
	}

	// Genuinely non-image uploads are still refused.
	if _, err := app.photos.Save(strings.NewReader("<html>not an image</html>"), "page.html"); err == nil {
		t.Error("a non-image was accepted")
	}
}

// --- search -----------------------------------------------------------------

// adafruitFixture mirrors the real catalogue's shape: every field is a JSON
// string, prices and ids included. Decoding these as numbers is exactly the
// mistake that made the first version return nothing.
const adafruitFixture = `[
 {"product_id":"3405","product_name":"Adafruit HUZZAH32 ESP32 Feather Board","product_price":"19.95",
  "product_image":"https://cdn-shop.adafruit.com/640x480/3405-08.jpg","product_mpn":"ADA3405",
  "product_stock":"in stock","product_url":"https://www.adafruit.com/product/3405"},
 {"product_id":"381","product_name":"Waterproof DS18B20 Digital temperature sensor","product_price":"9.95",
  "product_image":"https://cdn-shop.adafruit.com/640x480/381-00.jpg","product_mpn":"ADA381",
  "product_stock":"42","product_url":"https://www.adafruit.com/product/381"},
 {"product_id":"165","product_name":"TMP36 Temperature sensor","product_price":"2.75",
  "product_image":"","product_mpn":"ADA165","product_stock":"0","product_url":""}
]`

func adafruitTestProvider(t *testing.T, body string) (*AdafruitProvider, *httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	p := NewAdafruitProvider(NewFetcher(true))
	p.catalogURL = srv.URL
	return p, srv, &calls
}

func TestAdafruitSearchReadsStringFields(t *testing.T) {
	p, _, _ := adafruitTestProvider(t, adafruitFixture)

	got, err := p.Search(context.Background(), "ds18b20", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
	r := got[0]
	if r.Price != 9.95 {
		t.Errorf("price = %v, want 9.95 parsed from the string field", r.Price)
	}
	if r.PartNumber != "ADA381" || r.Source != "Adafruit" {
		t.Errorf("unexpected result %+v", r)
	}
	if r.Stock != "42 in stock" {
		t.Errorf("stock = %q, want a numeric count rendered readably", r.Stock)
	}
	if r.URL == "" || r.ImageURL == "" {
		t.Errorf("result is missing its link or image: %+v", r)
	}
}

func TestAdafruitSearchRequiresEveryWordAndRanks(t *testing.T) {
	p, _, _ := adafruitTestProvider(t, adafruitFixture)

	got, _ := p.Search(context.Background(), "temperature sensor", 10)
	if len(got) != 2 {
		t.Fatalf("got %d results, want both temperature sensors", len(got))
	}
	// Relevance leads: the concise title whose words appear early beats the
	// longer one, even though the longer one is the item in stock.
	if got[0].Title != "TMP36 Temperature sensor" {
		t.Errorf("ranked %q first; expected the closest title match", got[0].Title)
	}

	if none, _ := p.Search(context.Background(), "esp32 temperature", 10); len(none) != 0 {
		t.Errorf("got %d results for a contradictory query, want 0", len(none))
	}
}

func TestAdafruitStockBreaksTiesBetweenEqualMatches(t *testing.T) {
	// Two identical titles, so relevance cannot separate them and availability
	// is the only thing left to sort on.
	const fixture = `[
	 {"product_id":"1","product_name":"Widget board","product_price":"5.00","product_stock":"0","product_url":"u1"},
	 {"product_id":"2","product_name":"Widget board","product_price":"5.00","product_stock":"7","product_url":"u2"}
	]`
	p, _, _ := adafruitTestProvider(t, fixture)

	got, err := p.Search(context.Background(), "widget", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if got[0].Stock != "7 in stock" {
		t.Errorf("first result is %q; the in-stock one should win a tie", got[0].Stock)
	}
}

func TestAdafruitCatalogIsCached(t *testing.T) {
	p, _, calls := adafruitTestProvider(t, adafruitFixture)

	for i := 0; i < 3; i++ {
		if _, err := p.Search(context.Background(), "esp32", 5); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	if *calls != 1 {
		t.Errorf("catalogue was downloaded %d times for 3 searches, want 1", *calls)
	}
}

func TestSearchEndpointReportsUnreachableSourcesHonestly(t *testing.T) {
	app := newTestApp(t)
	// Point the provider at a dead address so the source fails.
	dead, _, _ := adafruitTestProvider(t, adafruitFixture)
	dead.catalogURL = "http://127.0.0.1:1/nope"
	app.search = &SearchHub{providers: []SearchProvider{dead}}

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	res, err := http.PostForm(srv.URL+"/import/search", url.Values{"q": {"esp32"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	body := readAll(t, res.Body)

	if !strings.Contains(body, "Adafruit:") {
		t.Errorf("a failed source produced no note, so the UI cannot say why:\n%s", body)
	}
	// Shops that cannot be queried server-side still offer a way through.
	for _, want := range []string{"Amazon", "AliExpress"} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not offer a %s link", want)
		}
	}
}

func TestParsePrice(t *testing.T) {
	ok := map[string]float64{
		"19.95": 19.95, "$19.95": 19.95, "USD 19.95": 19.95,
		"1,234.50": 1234.50, "12,50": 12.50, " 7 ": 7, "": 0,
	}
	for in, want := range ok {
		got, err := parsePrice(in)
		if err != nil {
			t.Errorf("parsePrice(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parsePrice(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"free", "-5", "abc"} {
		if _, err := parsePrice(bad); err == nil {
			t.Errorf("parsePrice(%q) was accepted", bad)
		}
	}
}

// --- prices -----------------------------------------------------------------

func TestPricesPerSourceAndShelfValue(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "ESP32 devkit", Quantity: 4})[0]

	for _, p := range []Price{
		{Source: "Adafruit", Amount: 19.95, Currency: "USD"},
		{Source: "AliExpress", Amount: 4.20, Currency: "USD"},
		{Source: "Amazon", Amount: 12.99, Currency: "USD"},
	} {
		if err := app.store.SetPrice(id, p); err != nil {
			t.Fatalf("SetPrice(%s): %v", p.Source, err)
		}
	}

	it, _ := app.store.GetItem(id)
	if len(it.Prices) != 3 {
		t.Fatalf("got %d prices, want one per source", len(it.Prices))
	}
	best := it.Best()
	if best == nil || best.Source != "AliExpress" {
		t.Errorf("best price = %+v, want the cheapest (AliExpress)", best)
	}
	if got := it.LineValue(); got != 4*4.20 {
		t.Errorf("line value = %v, want quantity x cheapest", got)
	}

	// Re-recording a source replaces it rather than stacking up.
	if err := app.store.SetPrice(id, Price{Source: "AliExpress", Amount: 5.50}); err != nil {
		t.Fatalf("SetPrice again: %v", err)
	}
	it, _ = app.store.GetItem(id)
	if len(it.Prices) != 3 {
		t.Errorf("re-pricing a source added a row; got %d", len(it.Prices))
	}

	stats, err := app.store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Value != 4*5.50 {
		t.Errorf("shelf value = %v, want 22 (4 x cheapest 5.50)", stats.Value)
	}

	if err := app.store.DeletePrice(id, "Amazon"); err != nil {
		t.Fatalf("DeletePrice: %v", err)
	}
	it, _ = app.store.GetItem(id)
	if len(it.Prices) != 2 {
		t.Errorf("after deleting one source there are %d prices, want 2", len(it.Prices))
	}
}

func TestItemWithNoPriceHasNoBest(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Mystery part", Quantity: 3})[0]
	it, _ := app.store.GetItem(id)
	if it.Best() != nil {
		t.Error("an item with no prices reported a best price")
	}
	if it.LineValue() != 0 {
		t.Error("an item with no prices contributed value")
	}
}

// --- tag icons --------------------------------------------------------------

func TestTagIconsAreAssignedAndOverridable(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store, Item{Name: "Board", Tags: tags("wifi", "zzz-unknown-tag")})

	facets, err := app.store.TagFacets()
	if err != nil {
		t.Fatalf("TagFacets: %v", err)
	}
	icons := map[string]string{}
	for _, f := range facets {
		icons[f.Value] = f.Icon
	}
	if icons["wifi"] != "📶" {
		t.Errorf("wifi icon = %q, want the keyword match", icons["wifi"])
	}
	if icons["zzz-unknown-tag"] == "" {
		t.Error("an unrecognised tag got no icon; every tag should get one")
	}

	if err := app.store.SetTagIcon("wifi", "🛜"); err != nil {
		t.Fatalf("SetTagIcon: %v", err)
	}
	facets, _ = app.store.TagFacets()
	for _, f := range facets {
		if f.Value == "wifi" && f.Icon != "🛜" {
			t.Errorf("icon override did not stick: %q", f.Icon)
		}
	}
}

func TestIconForTagIsStable(t *testing.T) {
	// Same input, same icon -- otherwise tags would flicker between renders.
	for _, name := range []string{"wibble", "another-odd-tag", "x"} {
		if IconForTag(name) != IconForTag(name) {
			t.Errorf("IconForTag(%q) is not deterministic", name)
		}
	}
	if IconForTag("ESP32-S3") != IconForTag("esp32-s3") {
		t.Error("icon lookup should be case-insensitive")
	}
	if IconForTag("") != "" {
		t.Error("an empty tag should get no icon")
	}
}

// --- interfaces, specs and references ---------------------------------------

func TestInterfacesAreVocabularyControlledAndFilterable(t *testing.T) {
	app := newTestApp(t)
	seed(t, app.store,
		Item{Name: "OLED", Quantity: 1, Interfaces: []string{"I2C", "3V3 logic"}},
		Item{Name: "SD breakout", Quantity: 1, Interfaces: []string{"SPI", "3V3 logic"}},
		Item{Name: "Resistor", Quantity: 1},
	)

	one, _ := app.store.ListItems(Query{Interfaces: []string{"3V3 logic"}})
	if len(one) != 2 {
		t.Errorf("one interface matched %d items, want 2", len(one))
	}
	both, _ := app.store.ListItems(Query{Interfaces: []string{"I2C", "3V3 logic"}})
	if len(both) != 1 || both[0].Name != "OLED" {
		t.Errorf("two interfaces matched %+v, want just the OLED", both)
	}

	// Anything outside the vocabulary is dropped rather than stored, so the
	// filter row cannot fill up with typos.
	id := seed(t, app.store, Item{Name: "Odd", Interfaces: []string{"I2C", "Telepathy"}})[0]
	it, _ := app.store.GetItem(id)
	if len(it.Interfaces) != 1 || it.Interfaces[0] != "I2C" {
		t.Errorf("interfaces = %v, want the invalid one discarded", it.Interfaces)
	}

	facets, err := app.store.InterfaceFacets()
	if err != nil {
		t.Fatalf("InterfaceFacets: %v", err)
	}
	for _, f := range facets {
		if f.Icon == "" {
			t.Errorf("interface %q has no icon", f.Value)
		}
	}
}

func TestSpecsRoundTripThroughTheForm(t *testing.T) {
	specs := ParseSpecs("Logic level: 3.3V\n\nFlash: 8MB\nNo colon line\nLogic level: duplicate\n")
	if len(specs) != 3 {
		t.Fatalf("parsed %d specs, want 3 (blank skipped, duplicate dropped): %+v", len(specs), specs)
	}
	if specs[0].Name != "Logic level" || specs[0].Value != "3.3V" {
		t.Errorf("first spec = %+v", specs[0])
	}
	if specs[2].Name != "No colon line" || specs[2].Value != "" {
		t.Errorf("a line without a colon should be kept as a bare name: %+v", specs[2])
	}

	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Board", Specs: specs})[0]
	it, _ := app.store.GetItem(id)
	if len(it.Specs) != 3 {
		t.Fatalf("stored %d specs, want 3", len(it.Specs))
	}
	if !strings.HasPrefix(it.SpecText(), "Logic level: 3.3V\n") {
		t.Errorf("SpecText did not round-trip:\n%s", it.SpecText())
	}
}

func TestReferencesSeparateImagesFromLinks(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "ESP32"})[0]

	app.store.AddReference(Reference{ItemID: id, Kind: "datasheet", Title: "Datasheet", URL: "https://example.com/d.pdf"})
	app.store.AddReference(Reference{ItemID: id, Kind: "pinout", Title: "Pinout", Filename: "aaaaaaaaaaaaaaaa.jpg"})

	it, _ := app.store.GetItem(id)
	if len(it.Pinouts()) != 1 || it.Pinouts()[0].Title != "Pinout" {
		t.Errorf("pinouts = %+v, want the stored image", it.Pinouts())
	}
	if len(it.Docs()) != 1 || it.Docs()[0].Title != "Datasheet" {
		t.Errorf("docs = %+v, want the link", it.Docs())
	}

	itemID, filename, err := app.store.DeleteReference(it.Pinouts()[0].ID)
	if err != nil {
		t.Fatalf("DeleteReference: %v", err)
	}
	if itemID != id || filename != "aaaaaaaaaaaaaaaa.jpg" {
		t.Errorf("delete reported item %d file %q; the file must come back so it can be unlinked", itemID, filename)
	}
}

// --- price history ----------------------------------------------------------

func TestPriceHistoryOnlyRecordsChanges(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Board", Quantity: 1})[0]

	for _, amount := range []float64{19.95, 19.95, 17.50, 17.50, 22.00} {
		if err := app.store.SetPrice(id, Price{Source: "Adafruit", Amount: amount}); err != nil {
			t.Fatalf("SetPrice(%v): %v", amount, err)
		}
	}
	points, err := app.store.PriceHistory(id)
	if err != nil {
		t.Fatalf("PriceHistory: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("recorded %d points, want 3 — repeats of the same price are not news", len(points))
	}

	changes := PriceChanges(points)
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1 source", len(changes))
	}
	c := changes[0]
	if c.First != 19.95 || c.Latest != 22.00 || c.Direction() != "up" {
		t.Errorf("change = %+v, direction %q", c, c.Direction())
	}
}

func TestPriceChangesIgnoresSingleObservations(t *testing.T) {
	// One reading is not a trend and should not be drawn as one.
	points := []PricePoint{{Source: "Amazon", Amount: 5}}
	if got := PriceChanges(points); len(got) != 0 {
		t.Errorf("got %+v, want nothing for a single observation", got)
	}
}

// --- projects ---------------------------------------------------------------

func TestProjectShortfallAndCost(t *testing.T) {
	app := newTestApp(t)
	esp := seed(t, app.store, Item{Name: "ESP32", Quantity: 1, Interfaces: []string{"I2C", "WiFi"}})[0]
	oled := seed(t, app.store, Item{Name: "OLED", Quantity: 0, Interfaces: []string{"I2C"}})[0]
	app.store.SetPrice(oled, Price{Source: "Adafruit", Amount: 9.95})

	pid, err := app.store.CreateProject("Greenhouse", "monitors humidity")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Two ESP32s wanted but only one owned; two OLEDs wanted and none owned.
	app.store.AddProjectPart(pid, &esp, "", 2, "")
	app.store.AddProjectPart(pid, &oled, "", 2, "")
	// And something not in the inventory at all.
	app.store.AddProjectPart(pid, nil, "Enclosure", 1, "3D print")

	p, err := app.store.GetProject(pid)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(p.Parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(p.Parts))
	}

	short := p.Shortfall()
	if len(short) != 3 {
		t.Fatalf("shortfall = %d lines, want all three unmet: %+v", len(short), short)
	}
	byName := map[string]ProjectPart{}
	for _, s := range short {
		byName[s.Label()] = s
	}
	if got := byName["ESP32"].Short(); got != 1 {
		t.Errorf("ESP32 short by %d, want 1 (own 1 of 2)", got)
	}
	if got := byName["Enclosure"].Have(); got != 0 {
		t.Errorf("a part not in the inventory reported %d in stock", got)
	}
	if p.Ready() {
		t.Error("project reported ready while parts are missing")
	}
	// Only the missing OLEDs have a known price: 2 x 9.95.
	if got := p.EstimatedCost(); got != 2*9.95 {
		t.Errorf("estimated cost = %v, want 19.90", got)
	}

	// Stocking up clears the line.
	app.store.AdjustQuantity(esp, 5)
	p, _ = app.store.GetProject(pid)
	for _, s := range p.Shortfall() {
		if s.Label() == "ESP32" {
			t.Error("ESP32 still short after restocking")
		}
	}
}

func TestProjectSuggestsOwnedPartsSharingInterfaces(t *testing.T) {
	app := newTestApp(t)
	esp := seed(t, app.store, Item{Name: "ESP32", Quantity: 1, Interfaces: []string{"I2C", "WiFi"}})[0]
	seed(t, app.store,
		Item{Name: "BME280", Quantity: 2, Interfaces: []string{"I2C"}},                // shares I2C
		Item{Name: "Level shifter", Quantity: 5, Interfaces: []string{"I2C", "WiFi"}}, // shares both
		Item{Name: "Stepper motor", Quantity: 1, Interfaces: []string{"CAN"}},         // unrelated
		Item{Name: "Out of stock sensor", Quantity: 0, Interfaces: []string{"I2C"}},
	)

	pid, _ := app.store.CreateProject("Weather", "")
	app.store.AddProjectPart(pid, &esp, "", 1, "")
	p, _ := app.store.GetProject(pid)

	got, err := app.store.SuggestForProject(p, 10)
	if err != nil {
		t.Fatalf("SuggestForProject: %v", err)
	}
	names := make([]string, len(got))
	for i, it := range got {
		names[i] = it.Name
	}
	if len(got) != 2 {
		t.Fatalf("suggested %v, want the two in-stock I2C parts", names)
	}
	// The part matching both interfaces should lead.
	if names[0] != "Level shifter" {
		t.Errorf("suggested %v; the closest interface match should come first", names)
	}
	for _, n := range names {
		if n == "ESP32" {
			t.Error("suggested a part already in the parts list")
		}
		if n == "Out of stock sensor" {
			t.Error("suggested something with no stock")
		}
	}
}

func TestProjectPagesRender(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "ESP32", Quantity: 1, Interfaces: []string{"I2C"}})[0]
	pid, _ := app.store.CreateProject("Greenhouse", "notes here")
	app.store.AddProjectPart(pid, &id, "", 3, "")

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{}

	list := getBody(t, client, srv.URL+"/projects")
	if !strings.Contains(list, "Greenhouse") {
		t.Error("project list does not show the project")
	}

	page := getBody(t, client, srv.URL+fmt.Sprintf("/projects/%d", pid))
	for _, want := range []string{"Greenhouse", "ESP32", "What you still need"} {
		if !strings.Contains(page, want) {
			t.Errorf("project page missing %q", want)
		}
	}

	// The dashboard leads with recently updated items now.
	dash := getBody(t, client, srv.URL+"/")
	recent := strings.Index(dash, "Recently updated")
	folders := strings.Index(dash, ">Folders<")
	if recent < 0 || folders < 0 {
		t.Fatalf("dashboard sections missing (recent=%d folders=%d)", recent, folders)
	}
	if recent > folders {
		t.Error("Recently updated should come before Folders on the dashboard")
	}
}

func TestNewItemFormPrefillsFromQuery(t *testing.T) {
	// The project shortfall links here with a name, so a missing part can be
	// added without retyping it.
	app := newTestApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	body := getBody(t, &http.Client{}, srv.URL+"/items/new?name=Weatherproof+enclosure&location=Drawer+9")
	if !strings.Contains(body, `value="Weatherproof enclosure"`) {
		t.Error("the name from the query string was not prefilled")
	}
	if !strings.Contains(body, `value="Drawer 9"`) {
		t.Error("the location from the query string was not prefilled")
	}
}

func TestPinoutImageIsStoredOrExplainedNotSilentlyDowngraded(t *testing.T) {
	jpegBytes := testJPEG(t, 400, 300, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "good") {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpegBytes)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "ESP32"})[0]
	appSrv := httptest.NewServer(app.routes())
	defer appSrv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// A reachable image is downloaded so the pinout is visible on the page.
	res, err := client.PostForm(appSrv.URL+fmt.Sprintf("/items/%d/refs", id),
		url.Values{"kind": {"pinout"}, "title": {"Good"}, "url": {srv.URL + "/good.jpg"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if strings.Contains(res.Header.Get("Location"), "error=") {
		t.Fatalf("storing a reachable pinout reported an error: %s", res.Header.Get("Location"))
	}

	// A dead link is still kept, but the response explains why it is a link.
	res, err = client.PostForm(appSrv.URL+fmt.Sprintf("/items/%d/refs", id),
		url.Values{"kind": {"pinout"}, "title": {"Dead"}, "url": {srv.URL + "/missing.jpg"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if !strings.Contains(res.Header.Get("Location"), "error=") {
		t.Error("a pinout whose image could not be fetched was downgraded silently")
	}

	it, _ := app.store.GetItem(id)
	if len(it.Pinouts()) != 1 || it.Pinouts()[0].Title != "Good" {
		t.Errorf("pinouts = %+v, want only the one that downloaded", it.Pinouts())
	}
	if len(it.Docs()) != 1 || it.Docs()[0].Title != "Dead" {
		t.Errorf("docs = %+v, want the dead link kept as a link", it.Docs())
	}
}

// --- shipping lead times -----------------------------------------------------

func TestCheapestAndFastestAreTrackedSeparately(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "ESP32", Quantity: 1})[0]

	app.store.SetPrice(id, Price{Source: "AliExpress", Amount: 4.20, LeadDays: 30})
	app.store.SetPrice(id, Price{Source: "Amazon", Amount: 12.99, LeadDays: 2})
	app.store.SetPrice(id, Price{Source: "Adafruit", Amount: 19.95, LeadDays: 5})

	it, _ := app.store.GetItem(id)
	best, fastest := it.Best(), it.Fastest()
	if best == nil || best.Source != "AliExpress" {
		t.Errorf("cheapest = %+v, want AliExpress", best)
	}
	if fastest == nil || fastest.Source != "Amazon" {
		t.Errorf("fastest = %+v, want Amazon", fastest)
	}
	if fastest.Lead() != "~2 days" {
		t.Errorf("lead label = %q", fastest.Lead())
	}

	// A source with no known wait must not be treated as instant.
	id2 := seed(t, app.store, Item{Name: "Mystery"})[0]
	app.store.SetPrice(id2, Price{Source: "Market stall", Amount: 1})
	it2, _ := app.store.GetItem(id2)
	if it2.Fastest() != nil {
		t.Error("a price with no lead time was reported as the fastest")
	}
	if it2.Prices[0].Lead() != "" {
		t.Errorf("unknown lead rendered as %q, want blank", it2.Prices[0].Lead())
	}
}

func TestDefaultLeadDaysFillsBlanksOnly(t *testing.T) {
	if DefaultLeadDays("AliExpress") != 30 || DefaultLeadDays("amazon") != 2 {
		t.Error("well-known shops should have a default wait")
	}
	if DefaultLeadDays("Some local shop") != 0 {
		t.Error("an unknown shop should not get an invented lead time")
	}

	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Board"})[0]
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	// Blank falls back to the shop's usual wait.
	client.PostForm(srv.URL+fmt.Sprintf("/items/%d/price", id),
		url.Values{"source": {"AliExpress"}, "amount": {"4.20"}, "lead_days": {""}})
	// An explicit figure wins over the default.
	client.PostForm(srv.URL+fmt.Sprintf("/items/%d/price", id),
		url.Values{"source": {"Amazon"}, "amount": {"12.99"}, "lead_days": {"9"}})

	it, _ := app.store.GetItem(id)
	got := map[string]int{}
	for _, p := range it.Prices {
		got[p.Source] = p.LeadDays
	}
	if got["AliExpress"] != 30 {
		t.Errorf("AliExpress lead = %d, want the 30-day default", got["AliExpress"])
	}
	if got["Amazon"] != 9 {
		t.Errorf("Amazon lead = %d, want the 9 that was typed", got["Amazon"])
	}
}

// --- labels ------------------------------------------------------------------

func TestQRAndBarcodeRenderAsPNG(t *testing.T) {
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "ESP32 devkit", PartNumber: "ESP32-WROOM-32E"})[0]

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	for _, path := range []string{"qr.png", "barcode.png"} {
		res, err := http.Get(srv.URL + fmt.Sprintf("/items/%d/%s", id, path))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("%s content type = %q", path, ct)
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
		if err != nil {
			t.Errorf("%s is not a decodable image: %v", path, err)
			continue
		}
		if format != "png" || cfg.Width < 100 {
			t.Errorf("%s decoded as %s %dx%d", path, format, cfg.Width, cfg.Height)
		}
	}
}

func TestBarcodePayloadFallsBackWhenUnencodable(t *testing.T) {
	cases := []struct {
		item Item
		want string
	}{
		{Item{ID: 7, PartNumber: "ESP32-WROOM-32E"}, "ESP32-WROOM-32E"},
		{Item{ID: 7}, "BO-7"},                                       // no part number
		{Item{ID: 7, PartNumber: "10kΩ 1%"}, "BO-7"},                // Code 128 is ASCII only
		{Item{ID: 7, PartNumber: strings.Repeat("X", 200)}, "BO-7"}, // unreadably wide
	}
	for _, tc := range cases {
		if got := barcodePayload(tc.item); got != tc.want {
			t.Errorf("barcodePayload(%q) = %q, want %q", tc.item.PartNumber, got, tc.want)
		}
	}

	// Whatever it picks must actually encode, or the label 500s.
	app := newTestApp(t)
	id := seed(t, app.store, Item{Name: "Ohm", PartNumber: "10kΩ 1%"})[0]
	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + fmt.Sprintf("/items/%d/barcode.png", id))
	if err != nil {
		t.Fatalf("GET barcode: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("a part number with a non-ASCII character produced %d", res.StatusCode)
	}
}

func TestLabelSheetUsesGridFiltersAndBaseURL(t *testing.T) {
	app := newTestApp(t)
	app.cfg.BaseURL = "https://parts.example.lan"
	seed(t, app.store,
		Item{Name: "In drawer", Location: "Drawer 3"},
		Item{Name: "Elsewhere", Location: "Shelf B"},
	)

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{}

	all := getBody(t, client, srv.URL+"/labels")
	if !strings.Contains(all, "In drawer") || !strings.Contains(all, "Elsewhere") {
		t.Error("the unfiltered sheet should cover everything")
	}
	if !strings.Contains(all, "parts.example.lan") {
		t.Error("BASE_URL should be shown so the codes can be checked before printing")
	}

	scoped := getBody(t, client, srv.URL+"/labels?location=Drawer+3")
	if !strings.Contains(scoped, "In drawer") || strings.Contains(scoped, "Elsewhere") {
		t.Error("the sheet did not honour the grid's location filter")
	}

	barcodes := getBody(t, client, srv.URL+"/labels?code=barcode")
	if !strings.Contains(barcodes, "barcode.png") {
		t.Error("the barcode sheet still rendered QR codes")
	}
}

func TestBaseURLPrefersConfigThenForwardedHeaders(t *testing.T) {
	app := newTestApp(t)

	r := httptest.NewRequest(http.MethodGet, "http://internal:8080/labels", nil)
	if got := app.BaseURL(r); got != "http://internal:8080" {
		t.Errorf("BaseURL = %q, want the request host", got)
	}

	// Behind a reverse proxy the server cannot see its own public name.
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "parts.example.com")
	if got := app.BaseURL(r); got != "https://parts.example.com" {
		t.Errorf("BaseURL = %q, want the forwarded host", got)
	}

	// An explicit setting beats both, and a trailing slash is not doubled.
	app.cfg.BaseURL = "https://configured.example/"
	if got := app.BaseURL(r); got != "https://configured.example" {
		t.Errorf("BaseURL = %q, want the configured value without a trailing slash", got)
	}
}

// --- image and document discovery --------------------------------------------

func TestImageDiscoveryFallsBackThroughConventions(t *testing.T) {
	pages := map[string]string{
		// OpenGraph is the best source and must win.
		"/og": `<html><head>
			<meta property="og:image" content="/og.jpg">
			<link rel="image_src" href="/link.jpg">
			</head><body><img src="/body.jpg" width="900" height="900"></body></html>`,
		// No OpenGraph: fall through to <link rel="image_src">.
		"/link": `<html><head><link rel="image_src" href="/link.jpg"></head>
			<body><img src="/body.jpg" width="900" height="900"></body></html>`,
		// Then schema.org.
		"/jsonld": `<html><head>
			<script type="application/ld+json">{"@type":"Product","image":["/ld.jpg"]}</script>
			</head><body><img src="/body.jpg" width="900" height="900"></body></html>`,
		// Last resort: the biggest picture on the page, skipping furniture.
		"/body": `<html><head><title>Plain</title></head><body>
			<img src="/logo.png" width="2000" height="2000">
			<img src="/small.jpg" width="40" height="40">
			<img src="/big.jpg" width="800" height="600">
			</body></html>`,
		// Protocol-relative and relative URLs both have to resolve.
		"/relative": `<html><head><meta property="og:image" content="//cdn.example.com/x.jpg"></head></html>`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	f := NewFetcher(true)
	cases := map[string]string{
		"/og":       srv.URL + "/og.jpg",
		"/link":     srv.URL + "/link.jpg",
		"/jsonld":   srv.URL + "/ld.jpg",
		"/body":     srv.URL + "/big.jpg",
		"/relative": "http://cdn.example.com/x.jpg",
	}
	for path, want := range cases {
		meta, err := f.FetchPage(context.Background(), srv.URL+path)
		if err != nil {
			t.Errorf("FetchPage(%s): %v", path, err)
			continue
		}
		if meta.ImageURL != want {
			t.Errorf("%s image = %q, want %q", path, meta.ImageURL, want)
		}
	}
}

func TestDocumentDiscoveryFindsDatasheetsNotNavigation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><title>Board</title></head><body>
			<a href="/files/esp32_datasheet.pdf">Datasheet</a>
			<a href="/files/board_schematic.pdf">Schematic</a>
			<a href="/img/pinout.png">Pinout diagram</a>
			<a href="/category/datasheets">All datasheets</a>
			<a href="/help">Manual returns policy</a>
			<a href="/files/esp32_datasheet.pdf">Datasheet (again)</a>
			</body></html>`)
	}))
	defer srv.Close()

	meta, err := NewFetcher(true).FetchPage(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}

	got := map[string]string{}
	for _, d := range meta.Documents {
		got[d.Kind] = d.URL
	}
	if len(meta.Documents) != 3 {
		t.Fatalf("found %d documents, want 3 (duplicates and category pages excluded): %+v", len(meta.Documents), meta.Documents)
	}
	for kind, wantSuffix := range map[string]string{
		"datasheet": "/files/esp32_datasheet.pdf",
		"schematic": "/files/board_schematic.pdf",
		"pinout":    "/img/pinout.png",
	} {
		if !strings.HasSuffix(got[kind], wantSuffix) {
			t.Errorf("%s = %q, want it to end with %q", kind, got[kind], wantSuffix)
		}
	}
	// "/category/datasheets" is a page, not a file, and must not be attached.
	for _, d := range meta.Documents {
		if strings.Contains(d.URL, "/category/") || strings.Contains(d.URL, "/help") {
			t.Errorf("attached a navigation link: %+v", d)
		}
	}
}

// --- quick add ----------------------------------------------------------------

func TestQuickAddBuildsAnItemFromOnePastedLink(t *testing.T) {
	jpegBytes := testJPEG(t, 500, 400, 0)
	shop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".jpg") {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpegBytes)
			return
		}
		fmt.Fprint(w, `<html><head>
			<meta property="og:title" content="ESP32 Feather Board">
			<meta property="og:description" content="A small board.">
			<meta property="og:image" content="/board.jpg">
			<meta property="og:site_name" content="Adafruit">
			<meta property="product:price:amount" content="19.95">
			<meta property="product:price:currency" content="USD">
			</head><body>
			<a href="/files/esp32.pdf">Datasheet</a>
			</body></html>`)
	}))
	defer shop.Close()

	app := newTestApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := client.PostForm(srv.URL+"/items/quick", url.Values{"url": {shop.URL + "/product"}})
	if err != nil {
		t.Fatalf("POST /items/quick: %v", err)
	}
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	if q := res.Header.Get("Location"); strings.Contains(q, "error=") {
		t.Fatalf("quick add reported an error: %s", q)
	}

	items, _ := app.store.ListItems(Query{})
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	it, _ := app.store.GetItem(items[0].ID)

	if it.Name != "ESP32 Feather Board" {
		t.Errorf("name = %q", it.Name)
	}
	if len(it.Photos) != 1 {
		t.Errorf("got %d photos, want the og:image downloaded", len(it.Photos))
	}
	best := it.Best()
	if best == nil || best.Amount != 19.95 || best.Source != "Adafruit" {
		t.Errorf("price = %+v, want 19.95 from Adafruit", best)
	}
	if best != nil && best.LeadDays != 5 {
		t.Errorf("lead days = %d, want Adafruit's default", best.LeadDays)
	}
	if len(it.Docs()) != 1 || it.Docs()[0].Kind != "datasheet" {
		t.Errorf("references = %+v, want the linked datasheet attached", it.Refs)
	}
}

func TestQuickAddReportsABadLinkWithoutCreatingAnItem(t *testing.T) {
	app := newTestApp(t)
	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := client.PostForm(srv.URL+"/items/quick", url.Values{"url": {"not a url"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if !strings.Contains(res.Header.Get("Location"), "error=") {
		t.Error("a bad link was accepted silently")
	}
	if items, _ := app.store.ListItems(Query{}); len(items) != 0 {
		t.Errorf("a half-built item was left behind: %+v", items)
	}
}

// --- type-ahead ----------------------------------------------------------------

func TestLiveSearchReturnsInventoryMatches(t *testing.T) {
	app := newTestApp(t)
	ids := seed(t, app.store,
		Item{Name: "ESP32 devkit", Quantity: 5, Location: "Drawer 3"},
		Item{Name: "ESP32-CAM", Quantity: 0},
		Item{Name: "Resistor 10k", Quantity: 400},
	)
	app.store.SetPrice(ids[0], Price{Source: "Adafruit", Amount: 19.95})

	srv := httptest.NewServer(app.routes())
	defer srv.Close()
	client := &http.Client{}

	body := getBody(t, client, srv.URL+"/items/search.json?q=esp32")
	var payload struct {
		Results []struct {
			Name     string `json:"name"`
			Location string `json:"location"`
			Quantity int    `json:"quantity"`
			Price    string `json:"price"`
		} `json:"results"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if payload.Total != 2 || len(payload.Results) != 2 {
		t.Fatalf("got %d results (total %d), want the two ESP32s", len(payload.Results), payload.Total)
	}
	byName := map[string]int{}
	for _, r := range payload.Results {
		byName[r.Name] = r.Quantity
	}
	if _, ok := byName["Resistor 10k"]; ok {
		t.Error("type-ahead returned a non-matching item")
	}
	if payload.Results[0].Price == "" && payload.Results[1].Price == "" {
		t.Error("no result carried its price, which the dropdown shows")
	}

	// An empty query must not dump the whole inventory into the dropdown.
	empty := getBody(t, client, srv.URL+"/items/search.json?q=")
	if err := json.Unmarshal([]byte(empty), &payload); err != nil {
		t.Fatalf("empty response is not JSON: %v", err)
	}
	if len(payload.Results) != 0 {
		t.Errorf("an empty query returned %d results", len(payload.Results))
	}
}

func TestShopifyProviderReadsSuggestions(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		fmt.Fprint(w, `{"resources":{"results":{"products":[
			{"title":"Pico 2","url":"/products/pico-2?_pos=1&_psq=pico","image":"//cdn.example/pico.jpg",
			 "price":"5.30","vendor":"Raspberry Pi","available":true},
			{"title":"Sold out board","url":"/products/gone","image":"","price":"9.00","available":false}
		]}}}`)
	}))
	defer srv.Close()

	p := NewShopifyProvider(NewFetcher(true), strings.TrimPrefix(srv.URL, "http://"))
	p.scheme = "http"

	got, err := p.Search(context.Background(), "pico", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "pico" {
		t.Errorf("query sent = %q", gotQuery)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if got[0].Price != 5.30 {
		t.Errorf("price = %v, want 5.30 parsed from the string", got[0].Price)
	}
	// Search-tracking parameters would be meaningless once saved.
	if strings.Contains(got[0].URL, "_pos=") || !strings.HasSuffix(got[0].URL, "/products/pico-2") {
		t.Errorf("url = %q, want it absolute and stripped of tracking", got[0].URL)
	}
	if got[0].ImageURL != "http://cdn.example/pico.jpg" {
		t.Errorf("image = %q, want the protocol-relative URL resolved", got[0].ImageURL)
	}
	if got[0].Stock != "in stock" || got[1].Stock != "out of stock" {
		t.Errorf("stock = %q / %q", got[0].Stock, got[1].Stock)
	}
}

func TestSearchHubInterleavesShopsSoOneCannotCrowdOut(t *testing.T) {
	make := func(name string, n int) SearchProvider { return &stubProvider{name: name, count: n} }
	hub := &SearchHub{providers: []SearchProvider{make("Big", 5), make("Small", 2)}}

	report := hub.Search(context.Background(), "esp32", 10)
	if len(report.Results) != 7 {
		t.Fatalf("got %d results, want all 7", len(report.Results))
	}
	// The first few must alternate rather than being five Bigs in a row.
	if report.Results[0].Source != "Big" || report.Results[1].Source != "Small" ||
		report.Results[2].Source != "Big" || report.Results[3].Source != "Small" {
		var order []string
		for _, r := range report.Results {
			order = append(order, r.Source)
		}
		t.Errorf("order = %v, want the shops interleaved", order)
	}
}

type stubProvider struct {
	name  string
	count int
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Search(context.Context, string, int) ([]SearchResult, error) {
	out := make([]SearchResult, s.count)
	for i := range out {
		out[i] = SearchResult{Source: s.name, Title: fmt.Sprintf("%s %d", s.name, i)}
	}
	return out, nil
}

// --- Octopart / Nexar ---------------------------------------------------------

// nexarStub stands in for api.nexar.com so the integration can be tested
// without a plan, a network, or a credential.
func nexarStub(t *testing.T, respond func(query string) string) (*NexarProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token") {
			fmt.Fprint(w, `{"access_token":"minted-token","expires_in":86400}`)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			http.Error(w, "no bearer token", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, respond(req.Query))
	}))
	t.Cleanup(srv.Close)

	p := NewNexarProvider(NewFetcher(true), "client", "secret", "")
	p.endpoint = srv.URL + "/graphql"
	p.tokenURL = srv.URL + "/token"
	return p, srv
}

const nexarPartJSON = `{"data":{"supSearchMpn":{"results":[{"part":{
  "mpn":"ESP32-WROOM-32E","manufacturer":{"name":"Espressif"},
  "shortDescription":"WiFi/BLE module","octopartUrl":"https://octopart.com/x",
  "bestImage":{"url":"https://img.example/esp32.jpg"},
  "bestDatasheet":{"name":"ESP32-WROOM-32E datasheet","url":"https://ex.com/ds.pdf"},
  "medianPrice1000":{"price":2.10,"currency":"USD"},
  "specs":[{"attribute":{"name":"Frequency"},"displayValue":"2.4 GHz"},
           {"attribute":{"name":"Flash"},"displayValue":"4 MB"}],
  "sellers":[
    {"company":{"name":"Mouser"},"offers":[{"clickUrl":"https://mouser/x","inventoryLevel":1200,
      "factoryLeadDays":21,"prices":[{"quantity":1,"price":3.50,"currency":"USD"},{"quantity":100,"price":2.40,"currency":"USD"}]}]},
    {"company":{"name":"Digi-Key"},"offers":[{"clickUrl":"https://dk/x","inventoryLevel":0,
      "factoryLeadDays":40,"prices":[{"quantity":1,"price":2.90,"currency":"USD"}]}]},
    {"company":{"name":"LCSC"},"offers":[{"clickUrl":"https://lcsc/x","inventoryLevel":500,
      "prices":[{"quantity":1,"price":3.10,"currency":"USD"}]}]}
  ]}}]}}}`

func TestNexarSearchReadsPartsAndPicksTheBestOffer(t *testing.T) {
	p, _ := nexarStub(t, func(string) string { return nexarPartJSON })

	got, err := p.Search(context.Background(), "ESP32-WROOM-32E", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	r := got[0]
	if r.PartNumber != "ESP32-WROOM-32E" || r.Source != "Octopart" {
		t.Errorf("unexpected result %+v", r)
	}
	// LCSC is cheapest among sellers with stock (3.10 vs Mouser 3.50);
	// Digi-Key is cheaper still at 2.90 but has none, so it must not win.
	if r.Price != 3.10 {
		t.Errorf("price = %v, want the cheapest in-stock single-unit price (3.10)", r.Price)
	}
	if !strings.Contains(r.Stock, "LCSC") {
		t.Errorf("stock = %q, want it to name the distributor", r.Stock)
	}
}

func TestNexarLookupMapsOntoTheItemModel(t *testing.T) {
	p, _ := nexarStub(t, func(string) string { return nexarPartJSON })

	detail, err := p.Lookup(context.Background(), "ESP32-WROOM-32E")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(detail.Specs) != 2 || detail.Specs[0].Name != "Frequency" {
		t.Errorf("specs = %+v", detail.Specs)
	}
	if detail.Datasheet == nil || detail.Datasheet.Kind != "datasheet" {
		t.Errorf("datasheet = %+v", detail.Datasheet)
	}
	if len(detail.Offers) != 3 {
		t.Fatalf("offers = %d, want one per seller", len(detail.Offers))
	}
	// In-stock sellers lead, cheapest first; the price break for quantity 1 is
	// the one a person buying a single part pays.
	if detail.Offers[0].Seller != "LCSC" || detail.Offers[0].Price != 3.10 {
		t.Errorf("first offer = %+v, want the cheapest in-stock seller", detail.Offers[0])
	}
	if detail.Offers[len(detail.Offers)-1].Seller != "Digi-Key" {
		t.Errorf("out-of-stock seller should sort last, got %+v", detail.Offers)
	}
	if detail.Offers[0].LeadDays != 0 {
		t.Errorf("LCSC quoted no lead time; got %d", detail.Offers[0].LeadDays)
	}
	for _, o := range detail.Offers {
		if o.Seller == "Mouser" && o.LeadDays != 21 {
			t.Errorf("Mouser lead days = %d, want 21", o.LeadDays)
		}
	}
}

func TestNexarSurfacesPlanAndQuotaErrors(t *testing.T) {
	// This is exactly what a free Nexar account answers: HTTP 200, with the
	// refusal inside the GraphQL errors array. Swallowing it would look like
	// "no results" and send someone hunting for a bug in their query.
	p, _ := nexarStub(t, func(string) string {
		return `{"errors":[{"message":"You have exceeded your part limit of 0. Please upgrade your plan."}]}`
	})

	_, err := p.Search(context.Background(), "esp32", 5)
	if err == nil {
		t.Fatal("a quota refusal was reported as success")
	}
	if !strings.Contains(err.Error(), "part limit") {
		t.Errorf("error = %q, want the API's own wording passed through", err)
	}
}

func TestNexarMintsAndReusesTokens(t *testing.T) {
	var tokenCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token") {
			tokenCalls++
			fmt.Fprint(w, `{"access_token":"minted","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer minted" {
			t.Errorf("request carried %q, want the minted token", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, nexarPartJSON)
	}))
	defer srv.Close()

	p := NewNexarProvider(NewFetcher(true), "client", "secret", "")
	p.endpoint, p.tokenURL = srv.URL+"/graphql", srv.URL+"/token"

	for i := 0; i < 3; i++ {
		if _, err := p.Search(context.Background(), "esp32", 3); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	if tokenCalls != 1 {
		t.Errorf("minted %d tokens for 3 searches, want 1 reused", tokenCalls)
	}
}

func TestNexarRefusesAnExpiredPastedToken(t *testing.T) {
	// A pasted token cannot be renewed, so when it lapses the app should say
	// that rather than let the API answer with a bare 401.
	expired := makeJWT(t, time.Now().Add(-time.Hour))
	p := NewNexarProvider(NewFetcher(true), "", "", expired)
	if !p.Configured() {
		t.Fatal("a pasted token should count as configured")
	}
	_, err := p.Search(context.Background(), "esp32", 3)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %v, want it to name the expiry and point at client credentials", err)
	}

	// A token still in date is used as-is.
	live := makeJWT(t, time.Now().Add(time.Hour))
	p2 := NewNexarProvider(NewFetcher(true), "", "", live)
	if _, err := p2.accessToken(context.Background()); err != nil {
		t.Errorf("a live pasted token was rejected: %v", err)
	}
}

func TestNexarStaysQuietWhenUnconfigured(t *testing.T) {
	p := NewNexarProvider(NewFetcher(true), "", "", "")
	if p.Configured() {
		t.Error("no credentials should not count as configured")
	}
	got, err := p.Search(context.Background(), "esp32", 5)
	if err != nil || len(got) != 0 {
		t.Errorf("unconfigured search = %v, %v; want silence rather than an error on every search", got, err)
	}
}

// makeJWT builds an unsigned token with just the exp claim, which is all the
// expiry check reads.
func makeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	enc := func(b []byte) string {
		return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
	}
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".signature"
}
