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
	ArtPath    string // absolute path of a sidecar/folder poster image, "" if none
	DateAdded  int64  // unix seconds
	Probed     ProbeState
	SeenGen    int64 // scan generation, used to prune vanished files
}

// ProbeState tracks whether metadata extraction has run for an item. It is
// deliberately tri-state: a probe that *failed* must be distinguishable from one
// that succeeded, or a transient ffprobe outage (or ffprobe simply not being
// installed yet) is recorded as a permanent empty result — `Put` only clears the
// flag when a file's size or mtime changes, so the item would never be retried.
type ProbeState int

const (
	ProbePending ProbeState = 0 // never attempted
	ProbeDone    ProbeState = 1 // attempted, result recorded
	ProbeFailed  ProbeState = 2 // attempted and failed; retry when ffprobe is available
)

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
    art_path    TEXT NOT NULL DEFAULT '',
    date_added  INTEGER NOT NULL DEFAULT 0,
    probed      INTEGER NOT NULL DEFAULT 0,
    seen_gen    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent);
CREATE INDEX IF NOT EXISTS idx_items_root ON items(root_name);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// additiveMigrations bring an older database schema up to date. Each ALTER is
// ignored if the column already exists (fresh DBs get them from `schema`).
var additiveMigrations = []string{
	`ALTER TABLE items ADD COLUMN sub_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE items ADD COLUMN probed INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE items ADD COLUMN art_path TEXT NOT NULL DEFAULT ''`,
}

// postMigrations run after the columns above exist (an index over a migrated
// column must not be created before the ALTER that adds it).
var postMigrations = []string{
	`CREATE INDEX IF NOT EXISTS idx_items_probed ON items(probed, is_dir)`,
	// Covers the browse query's full ORDER BY, so listing a large directory is an
	// index scan instead of a temporary b-tree sort.
	`CREATE INDEX IF NOT EXISTS idx_items_browse ON items(parent, is_dir DESC, name COLLATE NOCASE)`,
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
		"&_pragma=synchronous(NORMAL)" +
		// Pin the page cache instead of inheriting SQLite's default per
		// connection: negative means KiB, so this is 2 MiB per connection and
		// ~8 MiB total, budgeted rather than incidental on a 1 GB NAS.
		"&_pragma=cache_size(-2000)" +
		// Memory-map reads so browse queries hit the page cache without copying
		// through a heap buffer. Capped at 64 MiB — this is virtual address
		// space, not resident memory.
		"&_pragma=mmap_size(67108864)"
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
	s := &Store{db: db}
	if err := s.repairProbeStates(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// repairProbeStates is a one-time fix for databases written before `probed`
// became tri-state.
//
// Back then a failed probe — most commonly "ffprobe was not installed yet" —
// was stored as probed=1, indistinguishable from success. Because Put only
// re-queues an item when its size or mtime changes, those items would stay
// blank forever. Video rows marked probed with no duration are exactly that
// case, so re-queue them once.
//
// Gated on a marker row: without it, genuinely unprobeable videos would be
// re-queued on every startup.
func (s *Store) repairProbeStates() error {
	const marker = "probe_state_repair_v1"
	var done string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, marker).Scan(&done)
	if err == nil {
		return nil // already applied
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := s.db.Exec(
		`UPDATE items SET probed=? WHERE is_dir=0 AND probed=? AND duration='' AND class LIKE '%videoItem%'`,
		int(ProbePending), int(ProbeDone)); err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, '1')`, marker)
	return err
}

// isDuplicateColumn reports whether err is SQLite's "duplicate column name"
// error, which we expect when a migration column already exists.
func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// putSQL upserts an item by path.
//
// On update it intentionally preserves duration/resolution/sub_path/art_path
// (filled by the metadata pass) and only resets `probed` to pending when the
// file's size or mtime actually changed — so a genuine edit triggers a re-probe,
// but a no-op reconcile scan does not.
const putSQL = `
INSERT INTO items (path, parent, name, root_name, is_dir, class, mime, size, mtime, duration, resolution, sub_path, art_path, date_added, probed, seen_gen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(path) DO UPDATE SET
    parent=excluded.parent, name=excluded.name, root_name=excluded.root_name,
    is_dir=excluded.is_dir, class=excluded.class, mime=excluded.mime,
    size=excluded.size, mtime=excluded.mtime,
    probed=CASE WHEN items.size<>excluded.size OR items.mtime<>excluded.mtime THEN 0 ELSE items.probed END,
    seen_gen=excluded.seen_gen`

func putArgs(it Item) []any {
	return []any{
		it.Path, it.Parent, it.Name, it.RootName, b2i(it.IsDir), it.Class, it.Mime,
		it.Size, it.MTime, it.Duration, it.Resolution, it.SubPath, it.ArtPath,
		it.DateAdded, int(it.Probed), it.SeenGen,
	}
}

// Put inserts or updates a single item by path.
func (s *Store) Put(it Item) error {
	_, err := s.db.Exec(putSQL, putArgs(it)...)
	return err
}

// Metadata is one item's enrichment result.
type Metadata struct {
	Duration   string
	Resolution string
	SubPath    string // sidecar subtitle, "" if none
	ArtPath    string // sidecar/folder poster, "" if none
	State      ProbeState
}

// PutBatch upserts many items in a single transaction.
//
// A full scan of 50k files issued 50k autocommits, each a separate round trip
// through database/sql and its own WAL frame. Batching makes a cold start on a
// large library dramatically cheaper without holding the whole tree in memory.
func (s *Store) PutBatch(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once committed

	stmt, err := tx.Prepare(putSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, it := range items {
		if _, err := stmt.Exec(putArgs(it)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateMetadata records the results of a metadata probe for one item. Empty
// values are stored as-is (e.g. images have no duration). State distinguishes a
// genuine empty result from a failed probe, so failures can be retried later.
func (s *Store) UpdateMetadata(path string, m Metadata) error {
	_, err := s.db.Exec(
		`UPDATE items SET duration=?, resolution=?, sub_path=?, art_path=?, probed=? WHERE path=?`,
		m.Duration, m.Resolution, m.SubPath, m.ArtPath, int(m.State), path)
	return err
}

// ResetFailedProbes re-queues every item whose probe previously failed. Called at
// startup once ffprobe is known to be available, so a library indexed before
// ffmpeg was installed recovers on the next run instead of staying blank forever.
func (s *Store) ResetFailedProbes() (int64, error) {
	res, err := s.db.Exec(`UPDATE items SET probed=? WHERE probed=?`, int(ProbePending), int(ProbeFailed))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ItemsNeedingMetadata returns up to limit media files that have not yet been
// probed. Items whose probe failed are excluded until ResetFailedProbes re-queues
// them, so a permanently unprobeable file cannot spin the enricher.
func (s *Store) ItemsNeedingMetadata(limit int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+columns+` FROM items WHERE is_dir=0 AND probed=? LIMIT ?`, int(ProbePending), limit)
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

// ChildRow is one browse result: an item plus, for containers, how many children
// it has. The count comes from the same query, so listing a folder of 500
// subfolders costs one round trip instead of 501.
type ChildRow struct {
	Item
	ChildCount int
}

// Children returns one page of a parent's direct children, containers first then
// items, alphabetically (case-insensitive; desc reverses the name order).
//
// Pagination happens in SQL rather than by slicing a fully-loaded folder: a
// 20k-item directory used to be materialised in full for every Browse, even one
// asking for 50 rows. limit <= 0 means no limit.
func (s *Store) Children(parent string, offset, limit int, desc bool) ([]ChildRow, error) {
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	q := `SELECT ` + prefixedColumns + `,
	    (SELECT COUNT(*) FROM items c WHERE c.parent = i.path) AS child_count
	  FROM items i WHERE i.parent = ?
	  ORDER BY i.is_dir DESC, i.name COLLATE NOCASE ` + dir
	args := []any{parent}
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, max(offset, 0))
	} else if offset > 0 {
		q += ` LIMIT -1 OFFSET ?`
		args = append(args, offset)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ChildRow, 0, min(limit, 256))
	for rows.Next() {
		var r ChildRow
		if r.Item, r.ChildCount, err = scanChildRow(rows); err != nil {
			return nil, err
		}
		out = append(out, r)
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
	// Match the node itself and any descendant, as a range over the primary key.
	//
	// This was a `LIKE ? ESCAPE '\'`, but SQLite disables its LIKE-to-index
	// optimisation whenever an ESCAPE clause is present — so despite `path` being
	// the PRIMARY KEY, every delete event scanned the whole table. Deleting a
	// 1000-file folder meant 1000 full scans. A range predicate is an index seek
	// and needs no escaping, so odd characters in names are handled naturally.
	lo, hi := subtreeRange(path)
	res, err := s.db.Exec(
		`DELETE FROM items WHERE path=? OR (path>=? AND path<?)`, path, lo, hi)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// subtreeRange returns the half-open key range covering everything beneath path.
// "\x7f" sorts above every printable character, bounding the "dir/" prefix.
func subtreeRange(path string) (lo, hi string) {
	lo = path + string(os.PathSeparator)
	return lo, lo + "\x7f"
}

const columns = `path, parent, name, root_name, is_dir, class, mime, size, mtime, duration, resolution, sub_path, art_path, date_added, probed, seen_gen`

// prefixedColumns is `columns` qualified with the `i` alias, for queries that
// join items against itself.
const prefixedColumns = `i.path, i.parent, i.name, i.root_name, i.is_dir, i.class, i.mime, i.size, i.mtime, ` +
	`i.duration, i.resolution, i.sub_path, i.art_path, i.date_added, i.probed, i.seen_gen`

// scanChildRow scans the Children query's row: the item columns plus child_count.
func scanChildRow(sc rowScanner) (Item, int, error) {
	var it Item
	var isDir, probed, childCount int
	err := sc.Scan(&it.Path, &it.Parent, &it.Name, &it.RootName, &isDir, &it.Class,
		&it.Mime, &it.Size, &it.MTime, &it.Duration, &it.Resolution, &it.SubPath,
		&it.ArtPath, &it.DateAdded, &probed, &it.SeenGen, &childCount)
	if err != nil {
		return Item{}, 0, err
	}
	it.IsDir = isDir != 0
	it.Probed = ProbeState(probed)
	return it, childCount, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(sc rowScanner) (Item, error) {
	var it Item
	var isDir, probed int
	err := sc.Scan(&it.Path, &it.Parent, &it.Name, &it.RootName, &isDir, &it.Class,
		&it.Mime, &it.Size, &it.MTime, &it.Duration, &it.Resolution, &it.SubPath,
		&it.ArtPath, &it.DateAdded, &probed, &it.SeenGen)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, err
	}
	it.IsDir = isDir != 0
	it.Probed = ProbeState(probed)
	return it, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
