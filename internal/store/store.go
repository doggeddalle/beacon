// Package store is the SQLite persistence layer for the media library. It uses
// the pure-Go modernc.org/sqlite driver (no cgo), so the whole binary still
// cross-compiles to the NAS's ARM64 target with CGO_ENABLED=0.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// RootParent is the sentinel parent value for the top-level configured folders
// (their conceptual parent is the ContentDirectory root object "0").
const RootParent = "0"

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Item is one node in the library tree: a directory or a media file. Paths are
// absolute and used as the primary key.
type Item struct {
	Path       string
	Parent     string
	Name       string
	RootName   string
	IsDir      bool
	Class      string // UPnP class (media files only)
	Mime       string // media files only
	Size       int64
	MTime      int64 // unix seconds
	Duration   string
	Resolution string
	SubPath    string // absolute path of a sidecar subtitle file, "" if none
	DateAdded  int64  // unix seconds
	Probed     bool   // metadata extraction has run for this item
	SeenGen    int64  // scan generation, used to prune vanished files
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
    path        TEXT PRIMARY KEY,
    parent      TEXT NOT NULL,
    name        TEXT NOT NULL,
    root_name   TEXT NOT NULL,
    is_dir      INTEGER NOT NULL,
    class       TEXT NOT NULL DEFAULT '',
    mime        TEXT NOT NULL DEFAULT '',
    size        INTEGER NOT NULL DEFAULT 0,
    mtime       INTEGER NOT NULL DEFAULT 0,
    duration    TEXT NOT NULL DEFAULT '',
    resolution  TEXT NOT NULL DEFAULT '',
    sub_path    TEXT NOT NULL DEFAULT '',
    date_added  INTEGER NOT NULL DEFAULT 0,
    probed      INTEGER NOT NULL DEFAULT 0,
    seen_gen    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent);
CREATE INDEX IF NOT EXISTS idx_items_root ON items(root_name);
`

// additiveMigrations bring an older database schema up to date. Each ALTER is
// ignored if the column already exists (fresh DBs get them from `schema`).
var additiveMigrations = []string{
	`ALTER TABLE items ADD COLUMN sub_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN probed INTEGER NOT NULL DEFAULT 0`,
}

// postMigrations run after the columns above exist (an index over a migrated
// column must not be created before the ALTER that adds it).
var postMigrations = []string{
	`CREATE INDEX IF NOT EXISTS idx_items_probed ON items(probed, is_dir)`,
}

// Open opens (creating if needed) the SQLite database at path and runs
// migrations. WAL mode lets browse reads proceed concurrently with scan writes.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Modest connection pool: SQLite serialises writes anyway, and we are
	// RAM-constrained.
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	for _, stmt := range additiveMigrations {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			db.Close()
			return nil, fmt.Errorf("store: migrate: %w", err)
		}
	}
	for _, stmt := range postMigrations {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: migrate: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// isDuplicateColumn reports whether err is SQLite's "duplicate column name"
// error, which we expect when a migration column already exists.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Put inserts or updates an item by path.
func (s *Store) Put(it Item) error {
	// On update we intentionally preserve duration/resolution/sub_path (filled
	// by the metadata pass) and only reset `probed` to 0 when the file's size
	// or mtime actually changed — so a genuine edit triggers a re-probe, but a
	// no-op reconcile scan does not.
	_, err := s.db.Exec(`
INSERT INTO items (path, parent, name, root_name, is_dir, class, mime, size, mtime, duration, resolution, sub_path, date_added, probed, seen_gen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(path) DO UPDATE SET
    parent=excluded.parent, name=excluded.name, root_name=excluded.root_name,
    is_dir=excluded.is_dir, class=excluded.class, mime=excluded.mime,
    size=excluded.size, mtime=excluded.mtime,
    probed=CASE WHEN items.size<>excluded.size OR items.mtime<>excluded.mtime THEN 0 ELSE items.probed END,
    seen_gen=excluded.seen_gen`,
		it.Path, it.Parent, it.Name, it.RootName, b2i(it.IsDir), it.Class, it.Mime,
		it.Size, it.MTime, it.Duration, it.Resolution, it.SubPath, it.DateAdded, b2i(it.Probed), it.SeenGen)
	return err
}

// UpdateMetadata records the results of a metadata probe for one item and marks
// it probed. Empty values are stored as-is (e.g. images have no duration).
func (s *Store) UpdateMetadata(path, duration, resolution, subPath string) error {
	_, err := s.db.Exec(
		`UPDATE items SET duration=?, resolution=?, sub_path=?, probed=1 WHERE path=?`,
		duration, resolution, subPath, path)
	return err
}

// ItemsNeedingMetadata returns up to limit media files that have not yet been
// probed.
func (s *Store) ItemsNeedingMetadata(limit int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+columns+` FROM items WHERE is_dir=0 AND probed=0 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Get returns a single item by path.
func (s *Store) Get(path string) (Item, error) {
	row := s.db.QueryRow(`SELECT `+columns+` FROM items WHERE path=?`, path)
	return scanItem(row)
}

// Children returns the direct children of a parent path, containers first then
// items, each alphabetically (case-insensitive).
func (s *Store) Children(parent string) ([]Item, error) {
	rows, err := s.db.Query(`SELECT `+columns+` FROM items WHERE parent=? ORDER BY is_dir DESC, name COLLATE NOCASE ASC`, parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountChildren returns how many direct children a container has.
func (s *Store) CountChildren(parent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE parent=?`, parent).Scan(&n)
	return n, err
}

// Count returns the total number of indexed items.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n)
	return n, err
}

// DeleteByRoot removes every row belonging to a named root (used when a folder
// is removed from the configuration). Returns the number of rows deleted.
func (s *Store) DeleteByRoot(rootName string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM items WHERE root_name=?`, rootName)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteStale removes rows for a root whose seen_gen differs from gen — i.e.
// files/folders that were not seen in the latest scan of that root. Returns the
// number of rows deleted.
func (s *Store) DeleteStale(rootName string, gen int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM items WHERE root_name=? AND seen_gen<>?`, rootName, gen)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Delete removes a single item by path.
func (s *Store) Delete(path string) error {
	_, err := s.db.Exec(`DELETE FROM items WHERE path=?`, path)
	return err
}

// DeleteSubtree removes an item and everything beneath it (used when a
// directory is deleted or moved away). Returns the number of rows removed.
func (s *Store) DeleteSubtree(path string) (int64, error) {
	// Match the node itself and any descendant whose path starts with "path/".
	// LIKE wildcards in the prefix are escaped so odd names can't broaden it.
	prefix := escapeLike(path+string(os.PathSeparator)) + "%"
	res, err := s.db.Exec(
		`DELETE FROM items WHERE path=? OR path LIKE ? ESCAPE '\'`, path, prefix)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// escapeLike escapes the LIKE metacharacters %, _ and the escape char itself so
// a path can be used as a literal prefix.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const columns = `path, parent, name, root_name, is_dir, class, mime, size, mtime, duration, resolution, sub_path, date_added, probed, seen_gen`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(sc rowScanner) (Item, error) {
	var it Item
	var isDir, probed int
	err := sc.Scan(&it.Path, &it.Parent, &it.Name, &it.RootName, &isDir, &it.Class,
		&it.Mime, &it.Size, &it.MTime, &it.Duration, &it.Resolution, &it.SubPath,
		&it.DateAdded, &probed, &it.SeenGen)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	it.IsDir = isDir != 0
	it.Probed = probed != 0
	return it, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
