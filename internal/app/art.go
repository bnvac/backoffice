package app

import (
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"strings"
)

// The pictures, one per part of the app. They are Microsoft's Fluent Emoji in
// the flat colour style (MIT licence, notice in static/art), renamed after the
// section they stand for so a template asks for "spool", not "thread".
//
// They are linked as images rather than inlined: each file defines its own
// gradient ids, and two inlined on one page would fight over them.
var artSet = loadArt()

func loadArt() map[string]bool {
	out := map[string]bool{}
	entries, err := fs.ReadDir(staticFS, "static/art")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ".svg" {
			out[strings.TrimSuffix(e.Name(), ".svg")] = true
		}
	}
	return out
}

func artImg(name, class string) string {
	return fmt.Sprintf(`<img class="%s" src="/static/art/%s.svg" alt="" width="96" height="96" decoding="async">`, class, name)
}

// art returns a picture by name, or nothing for a name it does not know — a
// missing picture should leave a gap, not break the page. "bench" is the
// cover: a few of the others arranged as a little workbench scene.
func art(name string) template.HTML {
	if name == "bench" {
		var b strings.Builder
		b.WriteString(`<span class="art art-bench">`)
		for i, n := range []string{"spool", "idea", "toolbox", "printer", "chip"} {
			b.WriteString(strings.Replace(artImg(n, "art"), `class="art"`,
				fmt.Sprintf(`class="art" style="--n:%d"`, i), 1))
		}
		b.WriteString(`</span>`)
		return template.HTML(b.String())
	}
	if !artSet[name] {
		return ""
	}
	return template.HTML(artImg(name, "art"))
}

// ArtNames is the set, for the contact sheet in the tests.
func ArtNames() []string {
	names := make([]string, 0, len(artSet))
	for k := range artSet {
		names = append(names, k)
	}
	return names
}
