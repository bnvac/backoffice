package app

import (
	"database/sql"
	"fmt"
	"strings"
)

// migration is one forward step. Most are plain SQL; a few need Go to reshape
// existing rows. Steps run in order inside a transaction and the database's
// user_version records how far it has got, so an old data directory upgrades
// itself on the next start and there is still no migration command to run.
type migration struct {
	name string
	sql  string
	fn   func(*sql.Tx) error
}

var migrations = []migration{
	{
		name: "initial schema",
		sql: `
CREATE TABLE IF NOT EXISTS items (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT    NOT NULL,
	category    TEXT    NOT NULL DEFAULT '',
	quantity    INTEGER NOT NULL DEFAULT 0,
	location    TEXT    NOT NULL DEFAULT '',
	part_number TEXT    NOT NULL DEFAULT '',
	value       TEXT    NOT NULL DEFAULT '',
	tags        TEXT    NOT NULL DEFAULT '',
	link        TEXT    NOT NULL DEFAULT '',
	notes       TEXT    NOT NULL DEFAULT '',
	created_at  TEXT    NOT NULL,
	updated_at  TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS photos (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	filename TEXT    NOT NULL,
	position INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_photos_item ON photos(item_id, position);
CREATE INDEX IF NOT EXISTS idx_items_category ON items(category);
CREATE INDEX IF NOT EXISTS idx_items_location ON items(location);
`,
	},
	{
		name: "folders",
		sql: `
CREATE TABLE folders (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL,
	parent_id  INTEGER REFERENCES folders(id) ON DELETE CASCADE,
	created_at TEXT    NOT NULL
);

CREATE INDEX idx_folders_parent ON folders(parent_id);

-- Deleting a folder leaves its items in place, just unfiled.
ALTER TABLE items ADD COLUMN folder_id INTEGER REFERENCES folders(id) ON DELETE SET NULL;
CREATE INDEX idx_items_folder ON items(folder_id);
`,
	},
	{
		name: "normalised tags",
		sql: `
CREATE TABLE tags (
	id   INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL COLLATE NOCASE UNIQUE
);

CREATE TABLE item_tags (
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	tag_id  INTEGER NOT NULL REFERENCES tags(id)  ON DELETE CASCADE,
	PRIMARY KEY (item_id, tag_id)
);

CREATE INDEX idx_item_tags_tag ON item_tags(tag_id);
`,
		// Split the old comma-separated items.tags column into the new tables so
		// tags people already typed survive the upgrade.
		fn: func(tx *sql.Tx) error {
			rows, err := tx.Query(`SELECT id, tags FROM items WHERE tags <> ''`)
			if err != nil {
				return err
			}
			existing := map[int64]string{}
			for rows.Next() {
				var id int64
				var tags string
				if err := rows.Scan(&id, &tags); err != nil {
					rows.Close()
					return err
				}
				existing[id] = tags
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}

			// This insert is deliberately written against the tags table as it
			// exists at *this* step, rather than calling the shared helper.
			// A migration has to keep working against the schema of its own
			// moment: the helper later learned to write an icon column that
			// does not exist until the next migration.
			link := func(itemID int64, name string) error {
				var id int64
				err := tx.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id)
				if err == sql.ErrNoRows {
					res, err := tx.Exec(`INSERT INTO tags (name) VALUES (?)`, name)
					if err != nil {
						return err
					}
					if id, err = res.LastInsertId(); err != nil {
						return err
					}
				} else if err != nil {
					return err
				}
				_, err = tx.Exec(`INSERT OR IGNORE INTO item_tags (item_id, tag_id) VALUES (?, ?)`, itemID, id)
				return err
			}

			for itemID, raw := range existing {
				for _, name := range splitTags(raw) {
					if err := link(itemID, name); err != nil {
						return err
					}
				}
			}
			// The column is now a stale duplicate of item_tags.
			_, err = tx.Exec(`ALTER TABLE items DROP COLUMN tags`)
			return err
		},
	},
	{
		name: "tag icons",
		sql:  `ALTER TABLE tags ADD COLUMN icon TEXT NOT NULL DEFAULT '';`,
		// Give the tags that already exist an icon, so the upgrade is visible
		// rather than leaving a wall of blank tags.
		fn: func(tx *sql.Tx) error {
			rows, err := tx.Query(`SELECT id, name FROM tags`)
			if err != nil {
				return err
			}
			icons := map[int64]string{}
			for rows.Next() {
				var id int64
				var name string
				if err := rows.Scan(&id, &name); err != nil {
					rows.Close()
					return err
				}
				icons[id] = IconForTag(name)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for id, icon := range icons {
				if _, err := tx.Exec(`UPDATE tags SET icon = ? WHERE id = ?`, icon, id); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		name: "prices per source",
		sql: `
CREATE TABLE prices (
	item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	source     TEXT    NOT NULL,
	amount     REAL    NOT NULL,
	currency   TEXT    NOT NULL DEFAULT 'USD',
	url        TEXT    NOT NULL DEFAULT '',
	updated_at TEXT    NOT NULL,
	PRIMARY KEY (item_id, source)
);
`,
	},
	{
		name: "interfaces, specs and references",
		sql: `
-- What a part speaks and needs: I2C, SPI, 3V3 logic, USB-C, and so on. A
-- controlled vocabulary rather than free tags, so a project can reason about it.
CREATE TABLE item_interfaces (
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	name    TEXT    NOT NULL,
	PRIMARY KEY (item_id, name)
);
CREATE INDEX idx_item_interfaces_name ON item_interfaces(name);

-- Free-form key/value specifications: "Logic level = 3.3V", "Flash = 8MB".
CREATE TABLE specs (
	item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	name     TEXT    NOT NULL,
	value    TEXT    NOT NULL,
	position INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (item_id, name)
);

-- Datasheets, pinout diagrams, manuals. A reference is a link, an stored
-- image, or both -- a pinout is only useful if you can actually look at it.
CREATE TABLE refs (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	kind     TEXT    NOT NULL DEFAULT 'reference',
	title    TEXT    NOT NULL,
	url      TEXT    NOT NULL DEFAULT '',
	filename TEXT    NOT NULL DEFAULT '',
	position INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_refs_item ON refs(item_id, position);
`,
	},
	{
		name: "price history",
		sql: `
CREATE TABLE price_history (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	source   TEXT    NOT NULL,
	amount   REAL    NOT NULL,
	currency TEXT    NOT NULL DEFAULT 'USD',
	seen_at  TEXT    NOT NULL
);
CREATE INDEX idx_price_history ON price_history(item_id, source, seen_at);
`,
		// Seed history from the prices already recorded, so the first chart is
		// not empty for anyone upgrading.
		fn: func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO price_history (item_id, source, amount, currency, seen_at)
				SELECT item_id, source, amount, currency, updated_at FROM prices`)
			return err
		},
	},
	{
		name: "projects",
		sql: `
CREATE TABLE projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL,
	notes      TEXT    NOT NULL DEFAULT '',
	status     TEXT    NOT NULL DEFAULT 'planning',
	created_at TEXT    NOT NULL,
	updated_at TEXT    NOT NULL
);

-- A line is either a part you own (item_id) or one you do not yet (name only),
-- which is what lets the shortfall list say "buy this".
CREATE TABLE project_parts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	item_id    INTEGER REFERENCES items(id) ON DELETE SET NULL,
	name       TEXT    NOT NULL DEFAULT '',
	quantity   INTEGER NOT NULL DEFAULT 1,
	note       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_project_parts ON project_parts(project_id);
`,
	},
	{
		name: "shipping lead times",
		// 0 means "unknown", which is different from "arrives today".
		sql: `ALTER TABLE prices ADD COLUMN lead_days INTEGER NOT NULL DEFAULT 0;`,
		// Seed the sources already recorded with the usual wait, so the column
		// is useful immediately instead of a wall of blanks.
		fn: func(tx *sql.Tx) error {
			rows, err := tx.Query(`SELECT DISTINCT source FROM prices`)
			if err != nil {
				return err
			}
			var sources []string
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					rows.Close()
					return err
				}
				sources = append(sources, s)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, src := range sources {
				if days := DefaultLeadDays(src); days > 0 {
					if _, err := tx.Exec(`UPDATE prices SET lead_days = ? WHERE source = ?`, days, src); err != nil {
						return err
					}
				}
			}
			return nil
		},
	},
	{
		name: "stock reservations and low-stock thresholds",
		sql: `
-- How few is "running low" depends on the part: two Raspberry Pis is plenty,
-- two 0805 resistors is nothing. 2 matches the figure that used to be hardcoded.
ALTER TABLE items ADD COLUMN low_stock INTEGER NOT NULL DEFAULT 2;

-- A project that has been built has taken its parts off the shelf. The moment
-- it did is recorded so the deduction happens exactly once and can be undone.
ALTER TABLE projects ADD COLUMN consumed_at TEXT NOT NULL DEFAULT '';

-- What each project actually took, so returning the parts puts back what was
-- taken rather than what the bill of materials says today.
CREATE TABLE project_consumption (
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	item_id    INTEGER NOT NULL REFERENCES items(id)    ON DELETE CASCADE,
	quantity   INTEGER NOT NULL,
	PRIMARY KEY (project_id, item_id)
);
`,
	},
	{
		name: "build log",
		sql: `
-- A dated notebook per project: what you did, what went wrong, photos of it.
CREATE TABLE log_entries (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	body       TEXT    NOT NULL DEFAULT '',
	author     TEXT    NOT NULL DEFAULT '',
	created_at TEXT    NOT NULL
);
CREATE INDEX idx_log_entries ON log_entries(project_id, created_at);

CREATE TABLE log_photos (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	entry_id INTEGER NOT NULL REFERENCES log_entries(id) ON DELETE CASCADE,
	filename TEXT    NOT NULL,
	position INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_log_photos ON log_photos(entry_id, position);
`,
	},
	{
		name: "project controller",
		// The board everything else hangs off, so pin demand has something to be
		// measured against. NULL default is what makes ADD COLUMN with a foreign
		// key legal in SQLite.
		sql: `ALTER TABLE projects ADD COLUMN controller_id INTEGER REFERENCES items(id) ON DELETE SET NULL;`,
	},
	{
		name: "users and audit trail",
		sql: `
-- Optional: with no rows here the single shared password still works, which
-- keeps a one-person install at zero configuration.
CREATE TABLE users (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	username   TEXT    NOT NULL COLLATE NOCASE UNIQUE,
	password   TEXT    NOT NULL,
	admin      INTEGER NOT NULL DEFAULT 0,
	created_at TEXT    NOT NULL,
	last_seen  TEXT    NOT NULL DEFAULT ''
);

-- Who changed what. Append-only: nothing in the app deletes from it except the
-- explicit prune on the admin page.
CREATE TABLE audit (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	at        TEXT    NOT NULL,
	actor     TEXT    NOT NULL DEFAULT '',
	action    TEXT    NOT NULL,
	entity    TEXT    NOT NULL DEFAULT '',
	entity_id INTEGER NOT NULL DEFAULT 0,
	detail    TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_at ON audit(at DESC, id DESC);
`,
	},
	{
		name: "orders and receiving",
		sql: `
-- What you bought, from whom, and whether it turned up. Receiving an order is
-- the inverse of building a project: one puts stock on the shelf, the other
-- takes it off, and both record enough to be undone.
CREATE TABLE orders (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	source      TEXT    NOT NULL DEFAULT '',
	reference   TEXT    NOT NULL DEFAULT '',
	status      TEXT    NOT NULL DEFAULT 'draft',
	placed_at   TEXT    NOT NULL DEFAULT '',
	expected_at TEXT    NOT NULL DEFAULT '',
	arrived_at  TEXT    NOT NULL DEFAULT '',
	tracking    TEXT    NOT NULL DEFAULT '',
	shipping    REAL    NOT NULL DEFAULT 0,
	currency    TEXT    NOT NULL DEFAULT 'USD',
	notes       TEXT    NOT NULL DEFAULT '',
	project_id  INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	created_at  TEXT    NOT NULL,
	updated_at  TEXT    NOT NULL
);
CREATE INDEX idx_orders_status ON orders(status, placed_at DESC);

CREATE TABLE order_lines (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	order_id   INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
	item_id    INTEGER REFERENCES items(id) ON DELETE SET NULL,
	name       TEXT    NOT NULL DEFAULT '',
	quantity   INTEGER NOT NULL DEFAULT 1,
	unit_price REAL    NOT NULL DEFAULT 0,
	currency   TEXT    NOT NULL DEFAULT 'USD',
	-- How many of this line have actually been put on the shelf, so a partial
	-- delivery can be received twice without double-counting.
	received   INTEGER NOT NULL DEFAULT 0,
	note       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_order_lines ON order_lines(order_id);
`,
	},
	{
		name: "pin assignments",
		sql: `
-- The budget counts pins; this records which pin went where, which is the only
-- way to catch the same one being used twice.
CREATE TABLE pin_assignments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	pin        TEXT    NOT NULL,
	item_id    INTEGER REFERENCES items(id) ON DELETE SET NULL,
	part       TEXT    NOT NULL DEFAULT '',
	signal     TEXT    NOT NULL DEFAULT '',
	note       TEXT    NOT NULL DEFAULT '',
	position   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_pin_assignments ON pin_assignments(project_id, position, id);
`,
	},
	{
		name: "sub-assemblies",
		// A parts list line can point at another project instead of an item: a
		// power supply module you build once and then use in three things.
		sql: `ALTER TABLE project_parts ADD COLUMN sub_project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE;`,
	},
	{
		name: "footprints",
		sql: `
-- The land pattern a part solders onto, by KiCad library name. The file itself
-- is cached so the drawing survives the library moving or the network going
-- away, and so a footprint is fetched once rather than on every page load.
ALTER TABLE items ADD COLUMN footprint TEXT NOT NULL DEFAULT '';

CREATE TABLE footprints (
	name       TEXT PRIMARY KEY,
	source     TEXT NOT NULL DEFAULT '',
	body       TEXT NOT NULL,
	fetched_at TEXT NOT NULL
);
`,
	},
	{
		name: "printable models",
		sql: `
-- Cases, brackets and mounts somebody has already drawn for a part.
--
-- The STL itself is deliberately not kept: the geometry is read once for its
-- measurements and its preview, and the file stays where it came from. A
-- hundred models at forty megabytes each is not something a homelab inventory
-- should be storing on your behalf.
CREATE TABLE models (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	source     TEXT    NOT NULL DEFAULT '',
	title      TEXT    NOT NULL DEFAULT '',
	url        TEXT    NOT NULL DEFAULT '',
	author     TEXT    NOT NULL DEFAULT '',
	licence    TEXT    NOT NULL DEFAULT '',
	downloads  INTEGER NOT NULL DEFAULT 0,
	likes      INTEGER NOT NULL DEFAULT 0,
	rating     REAL    NOT NULL DEFAULT 0,
	-- The site's own render, downloaded so it shows on the page.
	thumb      TEXT    NOT NULL DEFAULT '',
	-- Our render of the actual mesh, when an STL has been read.
	preview    TEXT    NOT NULL DEFAULT '',
	dimensions TEXT    NOT NULL DEFAULT '',
	triangles  INTEGER NOT NULL DEFAULT 0,
	note       TEXT    NOT NULL DEFAULT '',
	position   INTEGER NOT NULL DEFAULT 0,
	created_at TEXT    NOT NULL
);
CREATE INDEX idx_models ON models(item_id, position, id);
`,
	},
	{
		name: "manufacturers",
		sql: `
-- Who made the part. It sits on the item like category and location do, so it
-- becomes a filter and a facet for free; the table beside it exists only to
-- hang a logo and a website off the name.
ALTER TABLE items ADD COLUMN manufacturer TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_items_manufacturer ON items(manufacturer);

CREATE TABLE manufacturers (
	name       TEXT PRIMARY KEY COLLATE NOCASE,
	domain     TEXT NOT NULL DEFAULT '',
	url        TEXT NOT NULL DEFAULT '',
	logo       TEXT NOT NULL DEFAULT '',
	notes      TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
`,
	},
	{
		name: "workshop, models and the idea board",
		sql: `
-- Who is using this and how they work. One row, because it describes the
-- installation rather than a login -- accounts are the users table.
CREATE TABLE profile (
	id           INTEGER PRIMARY KEY CHECK (id = 1),
	name         TEXT NOT NULL DEFAULT '',
	bench        TEXT NOT NULL DEFAULT '',
	units        TEXT NOT NULL DEFAULT 'mm',
	skill        TEXT NOT NULL DEFAULT '',
	interests    TEXT NOT NULL DEFAULT '',
	onboarded_at TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL
);

-- What you can make things with, as opposed to what you can make things from.
-- kind is a slug from a fixed vocabulary so capabilities can be derived; raw
-- keeps the line it was interpreted from, so a bad guess can be traced back.
CREATE TABLE tools (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	kind       TEXT NOT NULL DEFAULT 'other',
	detail     TEXT NOT NULL DEFAULT '',
	notes      TEXT NOT NULL DEFAULT '',
	raw        TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE INDEX idx_tools_kind ON tools(kind);

-- Where to find a model, if you want one at all. api_key is written here
-- because the point is to configure it from the browser; it is never rendered
-- back, logged, or exported, and a backup of this file carries it.
CREATE TABLE ai_providers (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	kind       TEXT NOT NULL,
	endpoint   TEXT NOT NULL DEFAULT '',
	model      TEXT NOT NULL DEFAULT '',
	api_key    TEXT NOT NULL DEFAULT '',
	active     INTEGER NOT NULL DEFAULT 0,
	status     TEXT NOT NULL DEFAULT '',
	checked_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

-- The idea board. An idea becomes a project once you decide to build it, and
-- keeps the link so the board can show what came of it.
CREATE TABLE ideas (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	title      TEXT NOT NULL,
	summary    TEXT NOT NULL DEFAULT '',
	detail     TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'new',
	source     TEXT NOT NULL DEFAULT '',
	enclosure  TEXT NOT NULL DEFAULT '',
	effort     TEXT NOT NULL DEFAULT '',
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX idx_ideas_status ON ideas(status);

CREATE TABLE idea_parts (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	idea_id  INTEGER NOT NULL REFERENCES ideas(id) ON DELETE CASCADE,
	item_id  INTEGER REFERENCES items(id) ON DELETE SET NULL,
	name     TEXT NOT NULL DEFAULT '',
	quantity INTEGER NOT NULL DEFAULT 1,
	role     TEXT NOT NULL DEFAULT '',
	have     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_idea_parts ON idea_parts(idea_id);
`,
	},
	{
		name: "printers",
		sql: `
-- A printer is recorded as specifications rather than as a line of text,
-- because whether a filament will run is a question about all of them at once:
-- the hotend ceiling, the bed ceiling, the box around it, the nozzle, and
-- whether the spool can be dried. Zero means "not said", which is treated as
-- unknown rather than as a failure.
CREATE TABLE printers (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT    NOT NULL,
	bed_x        REAL    NOT NULL DEFAULT 0,
	bed_y        REAL    NOT NULL DEFAULT 0,
	bed_z        REAL    NOT NULL DEFAULT 0,
	nozzle       REAL    NOT NULL DEFAULT 0,
	nozzle_kind  TEXT    NOT NULL DEFAULT 'brass',
	max_hotend   INTEGER NOT NULL DEFAULT 0,
	max_bed      INTEGER NOT NULL DEFAULT 0,
	enclosed     INTEGER NOT NULL DEFAULT 0,
	direct_drive INTEGER NOT NULL DEFAULT 0,
	dryer        INTEGER NOT NULL DEFAULT 0,
	notes        TEXT    NOT NULL DEFAULT '',
	created_at   TEXT    NOT NULL
);
`,
	},
	{
		name: "project steps",
		sql: `
-- What is meant to happen, as opposed to the build log's record of what did.
-- position is sparse and only ever compared, never assumed contiguous, so
-- deleting out of the middle does not renumber the rest. done_at is a stamp
-- rather than a flag because when a step was ticked is worth keeping.
CREATE TABLE project_steps (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	position   INTEGER NOT NULL DEFAULT 0,
	body       TEXT    NOT NULL,
	done_at    TEXT    NOT NULL DEFAULT '',
	created_at TEXT    NOT NULL
);
CREATE INDEX idx_project_steps ON project_steps(project_id, position);
`,
	},
	{
		name: "printer connections and print jobs",
		sql: `
-- How to reach a printer on the network. link_key is written here for the
-- same reason ai_providers.api_key is: it is configured from the browser. It
-- is never rendered back, logged, or exported.
ALTER TABLE printers ADD COLUMN link_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE printers ADD COLUMN link_url  TEXT NOT NULL DEFAULT '';
ALTER TABLE printers ADD COLUMN link_key  TEXT NOT NULL DEFAULT '';

-- Every G-code file sent, kept by hash so it can be sent again. A failed send
-- is recorded too, with why, because "did that actually go?" is the question.
CREATE TABLE print_jobs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	printer_id INTEGER NOT NULL REFERENCES printers(id) ON DELETE CASCADE,
	item_id    INTEGER REFERENCES items(id) ON DELETE SET NULL,
	name       TEXT    NOT NULL,
	blob       TEXT    NOT NULL,
	sha256     TEXT    NOT NULL,
	size       INTEGER NOT NULL DEFAULT 0,
	started    INTEGER NOT NULL DEFAULT 0,
	error      TEXT    NOT NULL DEFAULT '',
	sent_at    TEXT    NOT NULL
);
CREATE INDEX idx_print_jobs_item ON print_jobs(item_id);
`,
	},
	{
		name: "firmware on boards",
		sql: `
-- One row per time a board was flashed. The shelf counts boards as a number;
-- this tells the physical ones apart by what is on them. unit is free text
-- because people identify boards by a sticker, a MAC, or where they are.
CREATE TABLE flashes (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	item_id    INTEGER REFERENCES items(id) ON DELETE SET NULL,
	unit       TEXT    NOT NULL DEFAULT '',
	firmware   TEXT    NOT NULL,
	version    TEXT    NOT NULL DEFAULT '',
	file       TEXT    NOT NULL DEFAULT '',
	blob       TEXT    NOT NULL DEFAULT '',
	sha256     TEXT    NOT NULL DEFAULT '',
	size       INTEGER NOT NULL DEFAULT 0,
	project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
	notes      TEXT    NOT NULL DEFAULT '',
	flashed_at TEXT    NOT NULL,
	created_at TEXT    NOT NULL
);
CREATE INDEX idx_flashes_item ON flashes(item_id);
CREATE INDEX idx_flashes_project ON flashes(project_id);
`,
	},
	{
		name: "machines",
		sql: `
-- The shelf, switched on. parent_id is what a machine runs on or plugs into:
-- a VM on its host, a Pi on a PoE switch. image_sum is the published SHA-256
-- of whatever it was installed from, so "which Debian is that?" has an answer.
CREATE TABLE machines (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL,
	role       TEXT    NOT NULL DEFAULT '',
	address    TEXT    NOT NULL DEFAULT '',
	port       INTEGER NOT NULL DEFAULT 22,
	mac        TEXT    NOT NULL DEFAULT '',
	os         TEXT    NOT NULL DEFAULT '',
	image      TEXT    NOT NULL DEFAULT '',
	image_sum  TEXT    NOT NULL DEFAULT '',
	hardware   TEXT    NOT NULL DEFAULT '',
	location   TEXT    NOT NULL DEFAULT '',
	watts      INTEGER NOT NULL DEFAULT 0,
	services   TEXT    NOT NULL DEFAULT '',
	notes      TEXT    NOT NULL DEFAULT '',
	item_id    INTEGER REFERENCES items(id) ON DELETE SET NULL,
	parent_id  INTEGER REFERENCES machines(id) ON DELETE SET NULL,
	created_at TEXT    NOT NULL,
	updated_at TEXT    NOT NULL
);
CREATE INDEX idx_machines_item ON machines(item_id);
`,
	},
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database is at schema version %d but this build only knows %d; "+
			"it was written by a newer version of the app", version, len(migrations))
	}

	for i := version; i < len(migrations); i++ {
		m := migrations[i]
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if strings.TrimSpace(m.sql) != "" {
			if _, err := tx.Exec(m.sql); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d (%s): %w", i+1, m.name, err)
			}
		}
		if m.fn != nil {
			if err := m.fn(tx); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d (%s): %w", i+1, m.name, err)
			}
		}
		// PRAGMA user_version does not accept a bound parameter.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (%s): record version: %w", i+1, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d (%s): commit: %w", i+1, m.name, err)
		}
	}
	return nil
}

// splitTags cleans a comma-separated tag string into distinct, trimmed names.
func splitTags(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t == "" || len(t) > 40 {
			continue
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
}

// attachTagTx links an item to a tag name, creating the tag if it is new.
// Tag names are compared case-insensitively, so "SMD" and "smd" are one tag.
func attachTagTx(tx *sql.Tx, itemID int64, name string) error {
	var id int64
	err := tx.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		res, err := tx.Exec(`INSERT INTO tags (name, icon) VALUES (?, ?)`, name, IconForTag(name))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO item_tags (item_id, tag_id) VALUES (?, ?)`, itemID, id)
	return err
}
