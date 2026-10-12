package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The related-row loaders used to read their whole table and throw away what did
// not match, so opening one item read every photo, tag, price, specification,
// reference and model in the database. These tests pin the fix: work must scale
// with what is on the page, not with what is in the shelf.

// seedShelf fills a database with enough parts, each with its own attachments,
// that a whole-table read is measurably different from a scoped one.
func seedShelf(t *testing.T, app *App, n int) []int64 {
	t.Helper()
	var ids []int64
	for i := 0; i < n; i++ {
		id, err := app.store.CreateItem(Item{
			Name:       fmt.Sprintf("Part %04d", i),
			Quantity:   i % 7,
			Category:   "module",
			Location:   fmt.Sprintf("Drawer %d", i%12),
			PartNumber: fmt.Sprintf("PN-%04d", i),
			Tags:       []Tag{{Name: fmt.Sprintf("tag%d", i%9)}},
			Interfaces: []string{"I2C", "3V3 logic"},
			Specs:      []Spec{{Name: "GPIO", Value: "24"}, {Name: "Flash", Value: "8MB"}},
		})
		if err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
		if err := app.store.AddPhoto(id, fmt.Sprintf("%016x.jpg", i)); err != nil {
			t.Fatalf("AddPhoto: %v", err)
		}
		if err := app.store.SetPrice(id, Price{Source: "LCSC", Amount: float64(i%40) + 1}); err != nil {
			t.Fatalf("SetPrice: %v", err)
		}
		if _, err := app.store.AddReference(Reference{ItemID: id, Kind: "datasheet", Title: "DS", URL: "https://example.com/d.pdf"}); err != nil {
			t.Fatalf("AddReference: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestOneItemDoesNotReadTheWholeShelf(t *testing.T) {
	app := newTestApp(t)
	ids := seedShelf(t, app, 400)

	// Reading one item must not get slower as the shelf grows. Timing is a
	// blunt instrument, so the bar is deliberately generous: the point is to
	// catch a return to reading every row, which is hundreds of times worse.
	start := time.Now()
	for i := 0; i < 40; i++ {
		it, err := app.store.GetItem(ids[i])
		if err != nil {
			t.Fatalf("GetItem: %v", err)
		}
		// And it must still actually load everything that belongs to it.
		if len(it.Photos) != 1 || len(it.Tags) != 1 || len(it.Prices) != 1 ||
			len(it.Specs) != 2 || len(it.Refs) != 1 || len(it.Interfaces) != 2 {
			t.Fatalf("item %d came back short: %d photos, %d tags, %d prices, %d specs, %d refs, %d interfaces",
				it.ID, len(it.Photos), len(it.Tags), len(it.Prices), len(it.Specs), len(it.Refs), len(it.Interfaces))
		}
	}
	each := time.Since(start) / 40
	if each > 12*time.Millisecond {
		t.Errorf("one item off a 400-part shelf took %v; it should not depend on the size of the shelf", each)
	}
}

func TestAttachmentsLandOnTheRightItems(t *testing.T) {
	// Chunking is where a scoped loader goes wrong: rows from one batch landing
	// on items from another. This uses more items than fit in one chunk.
	app := newTestApp(t)
	ids := seedShelf(t, app, idChunk+25)

	items, err := app.store.ListItems(Query{Sort: "name"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != len(ids) {
		t.Fatalf("got %d items, want %d", len(items), len(ids))
	}
	for _, it := range items {
		var want int
		if _, err := fmt.Sscanf(it.Name, "Part %04d", &want); err != nil {
			t.Fatalf("unexpected name %q", it.Name)
		}
		if len(it.Photos) != 1 || it.Photos[0].Filename != fmt.Sprintf("%016x.jpg", want) {
			t.Fatalf("%s got photos %+v — a row landed on the wrong item across a chunk boundary",
				it.Name, it.Photos)
		}
		if len(it.Tags) != 1 || it.Tags[0].Name != fmt.Sprintf("tag%d", want%9) {
			t.Fatalf("%s got tags %+v", it.Name, it.Tags)
		}
		// The grid deliberately does not load specs or references, so this
		// checks only what it does load. The detail page is covered below.
		if len(it.Prices) != 1 || len(it.Interfaces) != 2 {
			t.Fatalf("%s got %d prices and %d interfaces", it.Name, len(it.Prices), len(it.Interfaces))
		}
	}

	// The detail page loads the rest, and must get them right at both ends of
	// the shelf -- either side of a chunk boundary.
	for _, id := range []int64{ids[0], ids[idChunk-1], ids[idChunk], ids[len(ids)-1]} {
		it, err := app.store.GetItem(id)
		if err != nil {
			t.Fatalf("GetItem(%d): %v", id, err)
		}
		if len(it.Specs) != 2 || len(it.Refs) != 1 {
			t.Errorf("item %d got %d specs and %d refs, want 2 and 1", id, len(it.Specs), len(it.Refs))
		}
	}
}

func TestPagesStayQuickOnAFullShelf(t *testing.T) {
	app := newTestApp(t)
	ids := seedShelf(t, app, 400)

	projectID, _ := app.store.CreateProject("Big build", "")
	for i := 0; i < 20; i++ {
		if err := app.store.AddProjectPart(projectID, &ids[i], "", 2, ""); err != nil {
			t.Fatalf("AddProjectPart: %v", err)
		}
	}

	srv := httptest.NewServer(app.routes())
	defer srv.Close()

	for _, path := range []string{"/", "/items", "/items/1", "/projects/1", "/manufacturers", "/orders"} {
		start := time.Now()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()
		took := time.Since(start)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.StatusCode)
		}
		if took > 2*time.Second {
			t.Errorf("GET %s took %v on a 400-part shelf", path, took)
		}
	}
}

func TestScanFindsAPartWithoutReadingEverything(t *testing.T) {
	app := newTestApp(t)
	seedShelf(t, app, 400)

	start := time.Now()
	for i := 0; i < 25; i++ {
		it, problem, ok := app.resolveCode(fmt.Sprintf("PN-%04d", i))
		if !ok {
			t.Fatalf("PN-%04d did not resolve: %s", i, problem)
		}
		if it.PartNumber != fmt.Sprintf("PN-%04d", i) {
			t.Fatalf("resolved to the wrong part: %s", it.PartNumber)
		}
	}
	each := time.Since(start) / 25
	if each > 12*time.Millisecond {
		t.Errorf("one scan against a 400-part shelf took %v; a scan should be a lookup, not a scan", each)
	}
}
