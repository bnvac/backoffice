package app

import (
	"html/template"
	"io/fs"
	"path"
	"strings"
)

// The illustrations, one per part of the app, inlined into pages rather than
// linked as images so they draw in the theme's own ink and take its colours in
// both light and dark. They are the app's own files, read once at start-up, so
// marking them safe HTML is marking our own markup safe.
var artSet = loadArt()

func loadArt() map[string]template.HTML {
	out := map[string]template.HTML{}
	entries, err := fs.ReadDir(staticFS, "static/art")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".svg" {
			continue
		}
		b, err := fs.ReadFile(staticFS, "static/art/"+e.Name())
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(e.Name(), ".svg")] = template.HTML(b)
	}
	return out
}

// art returns a drawing by name, or nothing for a name it does not know — a
// missing illustration should leave a gap, not break the page.
func art(name string) template.HTML { return artSet[name] }

// ArtNames is the set, for the contact sheet in the tests.
func ArtNames() []string {
	names := make([]string, 0, len(artSet))
	for k := range artSet {
		names = append(names, k)
	}
	return names
}
