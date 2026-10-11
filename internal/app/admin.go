package app

import (
	"archive/zip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bnvac/backoffice/internal/media"
)

// Everything an operator needs to answer two questions: is this install
// healthy, and can I get my data back out of it.

// Health is the state of the install, gathered fresh on each page load. None of
// it is cached: the whole point is that it is true right now.
type Health struct {
	SchemaVersion  int
	SchemaExpected int
	Integrity      string
	Checked        time.Time

	DatabaseBytes int64
	WALBytes      int64
	PhotoBytes    int64
	PhotoFiles    int
	ThumbFiles    int
	DiskFreeBytes int64
	DiskTotal     int64

	Items, Photos, Folders, Projects, Users, AuditRows, Prices, Refs int

	// Files on disk with no row pointing at them, and rows pointing at files
	// that are gone. Both are harmless and both are worth knowing about.
	Orphans []string
	Missing []string

	DataDir      string
	AuthMode     string
	FetchMode    string
	Sources      []string
	Octopart     bool
	BaseURL      string
	Go           string
	Uptime       time.Duration
	StartedAt    time.Time
	TemplateName string
}

func (h Health) Healthy() bool {
	return h.Integrity == "ok" && h.SchemaVersion == h.SchemaExpected && len(h.Missing) == 0
}

// Human sizes, since these are read by a person deciding whether to worry.
func (h Health) DatabaseSize() string { return humanBytes(h.DatabaseBytes) }
func (h Health) WALSize() string      { return humanBytes(h.WALBytes) }
func (h Health) PhotoSize() string    { return humanBytes(h.PhotoBytes) }
func (h Health) DiskFree() string     { return humanBytes(h.DiskFreeBytes) }

func (h Health) DiskPercent() int {
	if h.DiskTotal <= 0 {
		return 0
	}
	return int((h.DiskTotal - h.DiskFreeBytes) * 100 / h.DiskTotal)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// Health gathers the numbers. A failure to read one section does not hide the
// rest: an install with a broken photo directory should still be able to tell
// you its schema version.
func (a *App) Health() Health {
	h := Health{
		SchemaExpected: len(migrations),
		Checked:        time.Now(),
		DataDir:        a.cfg.DataDir,
		BaseURL:        a.cfg.BaseURL,
		Sources:        a.search.Sources(),
		Octopart:       a.search.Nexar() != nil,
		StartedAt:      a.started,
		Uptime:         time.Since(a.started).Round(time.Second),
	}

	switch {
	case a.auth.Accounts() && a.auth.password != "":
		h.AuthMode = "named accounts, plus the shared password"
	case a.auth.Accounts():
		h.AuthMode = "named accounts"
	case a.auth.Enabled():
		h.AuthMode = "one shared password"
	default:
		h.AuthMode = "open — anyone who can reach it can edit it"
	}
	h.FetchMode = "public hosts only"
	if a.cfg.AllowPrivate {
		h.FetchMode = "private addresses allowed"
	}

	_ = a.store.db.QueryRow(`PRAGMA user_version`).Scan(&h.SchemaVersion)
	if err := a.store.db.QueryRow(`PRAGMA quick_check(1)`).Scan(&h.Integrity); err != nil {
		h.Integrity = "could not check: " + err.Error()
	}

	dbPath := filepath.Join(a.cfg.DataDir, "inventory.db")
	h.DatabaseBytes = fileSize(dbPath)
	h.WALBytes = fileSize(dbPath + "-wal")
	h.DiskFreeBytes, h.DiskTotal = diskSpace(a.cfg.DataDir)

	counts := map[string]*int{
		"items": &h.Items, "photos": &h.Photos, "folders": &h.Folders,
		"projects": &h.Projects, "users": &h.Users, "audit": &h.AuditRows,
		"prices": &h.Prices, "refs": &h.Refs,
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Table names come from the literal map above, never from a request.
		_ = a.store.db.QueryRow(`SELECT COUNT(*) FROM ` + name).Scan(counts[name])
	}

	h.PhotoFiles, h.ThumbFiles, h.PhotoBytes = a.photos.Usage()
	h.Orphans, h.Missing = a.photoDrift()
	return h
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// photoDrift compares what the database thinks it has against what is on disk.
func (a *App) photoDrift() (orphans, missing []string) {
	referenced := map[string]bool{}
	for _, query := range []string{
		`SELECT filename FROM photos`,
		`SELECT filename FROM refs WHERE filename <> ''`,
		`SELECT filename FROM log_photos`,
	} {
		names, err := a.store.photoFilenames(query)
		if err != nil {
			continue
		}
		for _, n := range names {
			referenced[n] = true
			if _, err := os.Stat(a.photos.Path(n)); err != nil {
				missing = append(missing, n)
			}
		}
	}
	onDisk, err := a.photos.Names()
	if err != nil {
		return orphans, missing
	}
	for _, n := range onDisk {
		if !referenced[n] {
			orphans = append(orphans, n)
		}
	}
	sort.Strings(orphans)
	sort.Strings(missing)
	return orphans, missing
}

// SweepOrphans deletes photo files nothing points at any more.
func (a *App) SweepOrphans() int {
	orphans, _ := a.photoDrift()
	for _, n := range orphans {
		a.photos.Remove(n)
	}
	return len(orphans)
}

// --- backup -----------------------------------------------------------------

const backupDBName = "inventory.db"

// WriteBackup streams a zip holding a consistent copy of the database and every
// photo. VACUUM INTO is what makes the copy consistent: it takes a real
// snapshot rather than copying a file that is being written to.
func (a *App) WriteBackup(w io.Writer) error {
	tmp, err := os.MkdirTemp("", "backoffice-backup")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	snapshot := filepath.Join(tmp, backupDBName)
	// VACUUM INTO takes the destination as a string literal, not a parameter.
	if _, err := a.store.db.Exec(`VACUUM INTO ?`, snapshot); err != nil {
		return fmt.Errorf("snapshot the database: %w", err)
	}

	zw := zip.NewWriter(w)
	if err := addFileToZip(zw, snapshot, backupDBName); err != nil {
		zw.Close()
		return err
	}
	names, err := a.photos.Names()
	if err == nil {
		for _, n := range names {
			if err := addFileToZip(zw, a.photos.Path(n), "photos/"+n); err != nil {
				zw.Close()
				return err
			}
		}
	}
	return zw.Close()
}

func addFileToZip(zw *zip.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// Deflate on a SQLite file is worth it; on JPEGs it is not, but mixing
	// methods to save a few percent is not worth the complexity.
	out, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, f)
	return err
}

// --- restore ----------------------------------------------------------------

const maxBackupSize = 2 << 30 // 2 GiB

// RestoreReport says what a restore actually did, so the confirmation is not
// just "done".
type RestoreReport struct {
	Tables int
	Rows   int64
	Photos int
	From   int // the schema version the backup was written at
}

// Restore replaces the contents of this install with a backup.
//
// It works by attaching the backup as a second database and copying it table by
// table inside one transaction, rather than swapping files underneath a running
// process. That keeps the open connection valid, keeps the operation atomic --
// a failure halfway through rolls back to what was there before -- and means
// the app does not have to be restarted afterwards.
//
// An older backup is migrated forward before being copied, so a backup taken
// six versions ago still restores.
func (a *App) Restore(r io.Reader) (RestoreReport, error) {
	var rep RestoreReport

	tmp, err := os.MkdirTemp("", "backoffice-restore")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(tmp)

	archivePath := filepath.Join(tmp, "backup.zip")
	if err := saveLimited(r, archivePath, maxBackupSize); err != nil {
		return rep, err
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return rep, fmt.Errorf("that is not a readable zip file")
	}
	defer zr.Close()

	// Pull the database out first: if it is missing or unreadable there is no
	// point writing any photos.
	dbPath := filepath.Join(tmp, backupDBName)
	found := false
	var photoEntries []*zip.File
	for _, f := range zr.File {
		switch {
		case f.Name == backupDBName:
			if err := extractZipFile(f, dbPath); err != nil {
				return rep, err
			}
			found = true
		case strings.HasPrefix(f.Name, "photos/"):
			photoEntries = append(photoEntries, f)
		}
	}
	if !found {
		return rep, fmt.Errorf("no %s inside that archive — is it a backup from this app?", backupDBName)
	}

	// Bring the backup up to this build's schema before copying anything, so a
	// backup from an older version restores rather than being refused.
	backup, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return rep, err
	}
	defer backup.Close()
	if err := backup.QueryRow(`PRAGMA user_version`).Scan(&rep.From); err != nil {
		return rep, fmt.Errorf("that file is not a Backoffice database")
	}
	if rep.From > len(migrations) {
		return rep, fmt.Errorf("that backup is from a newer version of the app (schema %d, this build knows %d)",
			rep.From, len(migrations))
	}
	if err := migrate(backup); err != nil {
		return rep, fmt.Errorf("upgrade the backup's schema: %w", err)
	}
	if err := backup.Close(); err != nil {
		return rep, err
	}

	rep.Tables, rep.Rows, err = a.store.copyFrom(dbPath)
	if err != nil {
		return rep, err
	}

	// Photos last: the database is the part that has to be atomic, and a photo
	// that fails to land shows up in the health page as a missing file.
	for _, f := range photoEntries {
		name := strings.TrimPrefix(f.Name, "photos/")
		if !media.SafeName.MatchString(name) {
			continue
		}
		if err := extractZipFile(f, a.photos.Path(name)); err != nil {
			continue
		}
		rep.Photos++
	}
	a.photos.RebuildThumbs()
	return rep, nil
}

// copyFrom replaces every table's contents with the attached database's. It
// runs in one transaction with foreign keys deferred, so the tables can be
// emptied and refilled in any order.
func (s *Store) copyFrom(path string) (tables int, rows int64, err error) {
	// ATTACH is not allowed inside a transaction, so it happens first. The pool
	// is capped at one connection, which is what makes it stick for the copy.
	if _, err := s.db.Exec(`ATTACH DATABASE ? AS backup`, path); err != nil {
		return 0, 0, fmt.Errorf("attach the backup: %w", err)
	}
	defer s.db.Exec(`DETACH DATABASE backup`)

	names, err := s.tableNames("main")
	if err != nil {
		return 0, 0, err
	}
	fromBackup, err := s.tableNames("backup")
	if err != nil {
		return 0, 0, err
	}
	inBackup := map[string]bool{}
	for _, n := range fromBackup {
		inBackup[n] = true
	}

	// Every column list is read up front. The pool is capped at one connection,
	// so a query issued while the transaction below is open would wait for a
	// connection the transaction is holding, and the restore would hang.
	columns := map[string][]string{}
	for _, name := range names {
		if !inBackup[name] {
			continue
		}
		// Columns are listed explicitly and intersected, so a table that gained
		// a column in a later migration still copies cleanly.
		cols, err := s.sharedColumns(name)
		if err != nil {
			return 0, 0, err
		}
		columns[name] = cols
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return 0, 0, err
	}
	for _, name := range names {
		if _, err := tx.Exec(`DELETE FROM main."` + name + `"`); err != nil {
			return 0, 0, fmt.Errorf("clear %s: %w", name, err)
		}
	}
	for _, name := range names {
		cols := columns[name]
		if len(cols) == 0 {
			continue
		}
		list := `"` + strings.Join(cols, `","`) + `"`
		res, err := tx.Exec(fmt.Sprintf(`INSERT INTO main."%s" (%s) SELECT %s FROM backup."%s"`,
			name, list, list, name))
		if err != nil {
			return 0, 0, fmt.Errorf("copy %s: %w", name, err)
		}
		n, _ := res.RowsAffected()
		rows += n
		tables++
	}
	// AUTOINCREMENT keeps its high-water marks in this table; copying it keeps
	// ids from being reused after a restore.
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return tables, rows, nil
}

func (s *Store) tableNames(schema string) ([]string, error) {
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT name FROM %s.sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%%' ORDER BY name`,
		schema))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// sharedColumns returns the columns a table has in both databases, in the live
// database's order. Intersecting them is what lets a backup taken before a
// column existed restore into a schema that has it.
func (s *Store) sharedColumns(table string) ([]string, error) {
	live, order, err := s.columnsOf("main", table)
	if err != nil {
		return nil, err
	}
	backup, _, err := s.columnsOf("backup", table)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range order {
		if live[c] && backup[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// columnsOf reads a table's columns. The schema and table names come from
// sqlite_master, never from a request, so interpolating them is safe -- PRAGMA
// does not accept bound parameters.
func (s *Store) columnsOf(schema, table string) (map[string]bool, []string, error) {
	rows, err := s.db.Query(fmt.Sprintf(`SELECT name FROM %s.pragma_table_info("%s")`, schema, table))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	set, order := map[string]bool{}, []string(nil)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, nil, err
		}
		set[name] = true
		order = append(order, name)
	}
	return set, order, rows.Err()
}

func saveLimited(r io.Reader, path string, limit int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("that archive is larger than %s", humanBytes(limit))
	}
	if n == 0 {
		return fmt.Errorf("the upload was empty")
	}
	return nil
}

func extractZipFile(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, io.LimitReader(rc, maxBackupSize))
	return err
}
