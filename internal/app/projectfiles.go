package app

import (
	"database/sql"
	"path"
	"sort"
	"strings"
	"time"
)

// A project's files: the CAD it was drawn in, the meshes, the G-code that was
// actually printed, the PCB, the firmware, the datasheet you annotated. Kept
// next to the parts list and the build log, so a build can be made again a
// year later from one page.
//
// The same name uploaded again is the next revision. Older revisions stay —
// "the version that fitted" is exactly the one you want back.

const maxProjectFile = 512 << 20

type ProjectFile struct {
	ID        int64
	ProjectID int64
	Name      string
	Kind      string
	Rev       int
	Blob      string
	SHA256    string
	Size      int64
	Note      string
	Author    string
	CreatedAt time.Time
}

func (f ProjectFile) SizeLabel() string  { return humanBytes(f.Size) }
func (f ProjectFile) KindInfo() FileKind { return fileKindByKey(f.Kind) }
func (f ProjectFile) Printable() bool    { return f.Kind == "gcode" }
func (f ProjectFile) ShortSum() string {
	if len(f.SHA256) < 12 {
		return f.SHA256
	}
	return f.SHA256[:12]
}

// FileKind is what sort of file it is, which decides its picture and what can
// be done with it.
type FileKind struct {
	Key, Label, Art string
	Exts            []string
}

var FileKinds = []FileKind{
	{"cad", "CAD", "ruler", []string{".step", ".stp", ".f3d", ".fcstd", ".scad", ".iges", ".igs", ".dxf", ".sldprt", ".skp", ".blend"}},
	{"mesh", "Model", "prints", []string{".stl", ".3mf", ".obj", ".amf", ".ply"}},
	{"gcode", "G-code", "printer", []string{".gcode", ".gco", ".g", ".bgcode"}},
	{"pcb", "PCB", "power", []string{".kicad_pcb", ".kicad_sch", ".kicad_pro", ".brd", ".sch", ".gbr", ".drl", ".fzz", ".easyeda"}},
	{"firmware", "Firmware", "chip", []string{".bin", ".uf2", ".hex", ".elf"}},
	{"code", "Code", "machine", []string{".ino", ".py", ".c", ".cpp", ".h", ".hpp", ".rs", ".go", ".js", ".ts", ".yaml", ".yml", ".json", ".toml", ".sh", ".lua"}},
	{"doc", "Document", "activity", []string{".pdf", ".md", ".txt", ".csv", ".xlsx", ".ods", ".docx", ".odt"}},
	{"image", "Picture", "scan", []string{".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg", ".heic"}},
	{"archive", "Archive", "orders", []string{".zip", ".7z", ".tar", ".gz", ".tgz", ".rar"}},
	{"other", "File", "export", nil},
}

func fileKindByKey(k string) FileKind {
	for _, fk := range FileKinds {
		if fk.Key == k {
			return fk
		}
	}
	return FileKinds[len(FileKinds)-1]
}

// KindOf reads a file's sort from its name. KiCad's extensions have an
// underscore in them, which is why this is a list and not a mime lookup.
func KindOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	// Gerbers come with a dozen layer extensions (.gtl, .gbl, .gto…).
	if len(ext) == 4 && strings.HasPrefix(ext, ".g") && strings.ContainsRune("tbkmp", rune(ext[2])) {
		return "pcb"
	}
	for _, fk := range FileKinds {
		for _, e := range fk.Exts {
			if e == ext {
				return fk.Key
			}
		}
	}
	return "other"
}

// FileGroup is one name: its newest revision and the ones before it.
type FileGroup struct {
	Latest ProjectFile
	Older  []ProjectFile
}

// GroupFiles turns the flat list into one entry per name, grouped by kind
// order (CAD first, archives last) and then by name.
func GroupFiles(files []ProjectFile) []FileGroup {
	by := map[string]*FileGroup{}
	var order []string
	for _, f := range files {
		key := strings.ToLower(f.Name)
		g, ok := by[key]
		if !ok {
			g = &FileGroup{Latest: f}
			by[key] = g
			order = append(order, key)
			continue
		}
		if f.Rev > g.Latest.Rev {
			g.Older = append(g.Older, g.Latest)
			g.Latest = f
		} else {
			g.Older = append(g.Older, f)
		}
	}
	rank := map[string]int{}
	for i, fk := range FileKinds {
		rank[fk.Key] = i
	}
	out := make([]FileGroup, 0, len(order))
	for _, k := range order {
		g := by[k]
		sort.Slice(g.Older, func(i, j int) bool { return g.Older[i].Rev > g.Older[j].Rev })
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Latest, out[j].Latest
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out
}

// --- storage ---------------------------------------------------------------------

// AddProjectFile stores a file record, numbering it after any earlier upload
// with the same name. The same bytes uploaded again under the same name are
// not a new revision; the existing one is returned instead.
func (s *Store) AddProjectFile(f ProjectFile) (ProjectFile, bool, error) {
	f.Name = strings.TrimSpace(path.Base(strings.ReplaceAll(f.Name, "\\", "/")))
	if f.Name == "" || f.Name == "." || f.Name == "/" {
		f.Name = "file"
	}
	if f.Kind == "" {
		f.Kind = KindOf(f.Name)
	}
	var latestID int64
	var latestRev int
	var latestSum string
	err := s.db.QueryRow(`SELECT id, rev, sha256 FROM project_files
		WHERE project_id = ? AND name = ? COLLATE NOCASE ORDER BY rev DESC LIMIT 1`,
		f.ProjectID, f.Name).Scan(&latestID, &latestRev, &latestSum)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return f, false, err
	case latestSum == f.SHA256:
		existing, err := s.GetProjectFile(latestID)
		return existing, false, err
	}
	f.Rev = latestRev + 1
	res, err := s.db.Exec(`INSERT INTO project_files (project_id, name, kind, rev, blob, sha256,
		size, note, author, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		f.ProjectID, f.Name, f.Kind, f.Rev, f.Blob, f.SHA256, f.Size, strings.TrimSpace(f.Note),
		f.Author, nowRFC3339())
	if err != nil {
		return f, false, err
	}
	f.ID, _ = res.LastInsertId()
	f.CreatedAt = time.Now()
	s.db.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, nowRFC3339(), f.ProjectID)
	return f, true, nil
}

const projectFileCols = `id, project_id, name, kind, rev, blob, sha256, size, note, author, created_at`

func scanProjectFile(row rowScanner) (ProjectFile, error) {
	var f ProjectFile
	var created string
	err := row.Scan(&f.ID, &f.ProjectID, &f.Name, &f.Kind, &f.Rev, &f.Blob, &f.SHA256, &f.Size,
		&f.Note, &f.Author, &created)
	f.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return f, err
}

func (s *Store) GetProjectFile(id int64) (ProjectFile, error) {
	return scanProjectFile(s.db.QueryRow(`SELECT `+projectFileCols+` FROM project_files WHERE id = ?`, id))
}

func (s *Store) ProjectFiles(projectID int64) ([]ProjectFile, error) {
	rows, err := s.db.Query(`SELECT `+projectFileCols+` FROM project_files
		WHERE project_id = ? ORDER BY name COLLATE NOCASE, rev DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectFile
	for rows.Next() {
		f, err := scanProjectFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) DeleteProjectFile(id int64) error {
	_, err := s.db.Exec(`DELETE FROM project_files WHERE id = ?`, id)
	return err
}
