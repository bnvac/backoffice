package app

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// Writes a contact sheet of every illustration, in both themes, for a person
// to look at. Only runs when asked: ART_SHEET=/path/out.html go test -run Sheet
func TestWriteArtSheet(t *testing.T) {
	out := os.Getenv("ART_SHEET")
	if out == "" {
		t.Skip("set ART_SHEET to write the contact sheet")
	}
	css, _ := staticFS.ReadFile("static/style.css")
	names := ArtNames()
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("<!doctype html><meta charset=utf-8><style>" + string(css) +
		`body{padding:20px} .row{display:grid;grid-template-columns:repeat(6,1fr);gap:14px;margin-bottom:28px}
		 .cell{background:var(--panel);border-radius:10px;padding:12px;text-align:center;font-size:12px;color:var(--muted)}
		 .cell .big{color:var(--text);margin-bottom:6px} .small{width:32px;margin:6px auto 0;color:var(--text);--art-w:1.6}</style>`)
	for _, theme := range []string{"dark", "light"} {
		b.WriteString(`<div data-theme-wrap="` + theme + `"><h2 style="color:var(--text)">` + theme + `</h2><div class="row">`)
		for _, n := range names {
			b.WriteString(`<div class="cell"><div class="big">` + string(art(n)) + `</div>` + n +
				`<div class="small">` + string(art(n)) + `</div></div>`)
		}
		b.WriteString("</div></div>")
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
