// Package store persists links and their append-only audit history in SQLite.
//
// Callers validate input (package link) before calling the store; the store
// enforces integrity: one live link per key, optimistic concurrency on edits,
// soft deletes, and an audit log that the database itself refuses to rewrite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"byname/internal/link"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

// Errors.
var (
	ErrNotFound      = errors.New("link not found")
	ErrExists        = errors.New("a link with that name already exists")
	ErrConflict      = errors.New("the link was changed by someone else; reload and try again")
	ErrLimit         = errors.New("link limit reached")
	ErrNotRevertible = errors.New("that history entry has no version to restore")
	ErrEventNotFound = errors.New("history entry not found")
	errBadPath       = errors.New("database path must not contain '?' or '#'")
)

// Actor identifies who made a change. Login and Node come from Tailscale
// WhoIs; IP is always present.
type Actor struct {
	Login string `json:"login,omitempty"`
	Node  string `json:"node,omitempty"`
	IP    string `json:"ip"`
}

// Display is the short human form stored in created_by / updated_by.
func (a Actor) Display() string {
	switch {
	case a.Login != "" && a.Node != "":
		return a.Login + " (" + a.Node + ")"
	case a.Login != "":
		return a.Login
	case a.Node != "":
		return a.Node
	case a.IP != "":
		return a.IP
	}
	return "unknown"
}

// Fields are the user-editable parts of a link.
type Fields struct {
	Name        string `json:"name"`
	Target      string `json:"target"`
	Description string `json:"description"`
}

// Link is a stored link.
type Link struct {
	ID          int64
	Name        string
	Key         string
	Target      string
	Description string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedBy   string
	UpdatedAt   time.Time
	Rev         int64
	Clicks      int64
	LastClick   time.Time // zero if never clicked
	DeletedAt   time.Time // zero if live
}

// Deleted reports whether the link is soft-deleted.
func (l Link) Deleted() bool { return !l.DeletedAt.IsZero() }

// Fields returns the editable fields.
func (l Link) Fields() Fields {
	return Fields{Name: l.Name, Target: l.Target, Description: l.Description}
}

// Event is one audit-log entry.
type Event struct {
	ID       int64
	LinkID   int64
	LinkName string // the link's current name
	Action   string
	Before   *Fields
	After    *Fields
	Actor    Actor
	At       time.Time
}

// Store is a SQLite-backed link store. It is safe for concurrent use.
type Store struct {
	db       *sql.DB
	now      func() time.Time
	maxLinks int // live-link cap; 0 = unlimited
}

// SetMaxLinks caps the number of live links. Every path that makes a link
// live (create, import, restore, revert of a deleted link) enforces it inside
// its transaction. n <= 0 removes the cap.
func (s *Store) SetMaxLinks(n int) { s.maxLinks = n }

func (s *Store) checkCap(ctx context.Context, q querier) error {
	if s.maxLinks <= 0 {
		return nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM links WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n >= s.maxLinks {
		return ErrLimit
	}
	return nil
}

//go:embed migrations/*.sql
var migrationFS embed.FS

// Open opens (creating if needed) the database at path and applies
// migrations.
func Open(path string) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, errBadPath
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: household load is tiny, and a single writer means no
	// SQLITE_BUSY surprises. Readers in other processes (byname export, backups)
	// still work thanks to WAL.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (s *Store) migrate(ctx context.Context) error {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(files) {
		return fmt.Errorf("database schema version %d is newer than this binary (%d); refusing to run", version, len(files))
	}
	for i := version; i < len(files); i++ {
		body, err := migrationFS.ReadFile(files[i])
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("%s: %w", files[i], err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SchemaVersion returns the applied migration count.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

const linkCols = `id, name, key, target, description, created_by, created_at,
	updated_by, updated_at, rev, clicks, last_click, deleted_at`

type rowScanner interface{ Scan(...any) error }

func scanLink(r rowScanner) (Link, error) {
	var (
		l                    Link
		created, updated     int64
		lastClick, deletedAt sql.NullInt64
	)
	err := r.Scan(&l.ID, &l.Name, &l.Key, &l.Target, &l.Description, &l.CreatedBy, &created,
		&l.UpdatedBy, &updated, &l.Rev, &l.Clicks, &lastClick, &deletedAt)
	if err != nil {
		return Link{}, err
	}
	l.CreatedAt = time.UnixMilli(created)
	l.UpdatedAt = time.UnixMilli(updated)
	if lastClick.Valid {
		l.LastClick = time.UnixMilli(lastClick.Int64)
	}
	if deletedAt.Valid {
		l.DeletedAt = time.UnixMilli(deletedAt.Int64)
	}
	return l, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func getByKey(ctx context.Context, q querier, key string) (Link, error) {
	l, err := scanLink(q.QueryRowContext(ctx,
		`SELECT `+linkCols+` FROM links WHERE key = ? AND deleted_at IS NULL`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

func getByID(ctx context.Context, q querier, id int64) (Link, error) {
	l, err := scanLink(q.QueryRowContext(ctx, `SELECT `+linkCols+` FROM links WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	return l, err
}

// Get returns the live link with the given key (see link.Key).
func (s *Store) Get(ctx context.Context, key string) (Link, error) {
	return getByKey(ctx, s.db, key)
}

// GetByID returns a link by id, live or deleted.
func (s *Store) GetByID(ctx context.Context, id int64) (Link, error) {
	return getByID(ctx, s.db, id)
}

func marshalFields(f *Fields) any {
	if f == nil {
		return nil
	}
	b, _ := json.Marshal(f)
	return string(b)
}

func addEvent(ctx context.Context, q querier, linkID int64, action string, before, after *Fields, a Actor, at time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO link_events
		(link_id, action, before_json, after_json, actor_login, actor_node, actor_ip, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		linkID, action, marshalFields(before), marshalFields(after), a.Login, a.Node, a.IP, at.UnixMilli())
	return err
}

func liveKeyTaken(ctx context.Context, q querier, key string, exceptID int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM links WHERE key = ? AND deleted_at IS NULL AND id != ?`, key, exceptID).Scan(&n)
	return n > 0, err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Create adds a link.
func (s *Store) Create(ctx context.Context, f Fields, a Actor) (Link, error) {
	return s.create(ctx, f, a, "create")
}

func (s *Store) create(ctx context.Context, f Fields, a Actor, action string) (Link, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	l, err := s.createTx(ctx, tx, f, a, action)
	if err != nil {
		return Link{}, err
	}
	return l, tx.Commit()
}

func (s *Store) createTx(ctx context.Context, tx *sql.Tx, f Fields, a Actor, action string) (Link, error) {
	if err := s.checkCap(ctx, tx); err != nil {
		return Link{}, err
	}
	key := link.Key(f.Name)
	if taken, err := liveKeyTaken(ctx, tx, key, 0); err != nil {
		return Link{}, err
	} else if taken {
		return Link{}, ErrExists
	}
	now := s.now()
	who := a.Display()
	res, err := tx.ExecContext(ctx, `INSERT INTO links
		(name, key, target, description, created_by, created_at, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Name, key, f.Target, f.Description, who, now.UnixMilli(), who, now.UnixMilli())
	if isUniqueViolation(err) {
		return Link{}, ErrExists
	}
	if err != nil {
		return Link{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Link{}, err
	}
	if err := addEvent(ctx, tx, id, action, nil, &f, a, now); err != nil {
		return Link{}, err
	}
	return getByID(ctx, tx, id)
}

// Update changes a live link's fields. rev must match the link's current
// revision, so two people editing at once cannot silently overwrite each other.
func (s *Store) Update(ctx context.Context, id, rev int64, f Fields, a Actor) (Link, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	cur, err := getByID(ctx, tx, id)
	if err != nil {
		return Link{}, err
	}
	if cur.Deleted() {
		return Link{}, ErrNotFound
	}
	if rev != cur.Rev {
		return Link{}, ErrConflict
	}
	if cur.Fields() == f {
		return cur, nil
	}
	l, err := s.apply(ctx, tx, cur, f, a, "update", false)
	if err != nil {
		return Link{}, err
	}
	return l, tx.Commit()
}

// apply writes f onto cur (optionally undeleting it) and records an event.
func (s *Store) apply(ctx context.Context, tx *sql.Tx, cur Link, f Fields, a Actor, action string, undelete bool) (Link, error) {
	if undelete {
		if err := s.checkCap(ctx, tx); err != nil {
			return Link{}, err
		}
	}
	key := link.Key(f.Name)
	if key != cur.Key || undelete {
		if taken, err := liveKeyTaken(ctx, tx, key, cur.ID); err != nil {
			return Link{}, err
		} else if taken {
			return Link{}, ErrExists
		}
	}
	now := s.now()
	q := `UPDATE links SET name = ?, key = ?, target = ?, description = ?,
		updated_by = ?, updated_at = ?, rev = rev + 1`
	if undelete {
		q += `, deleted_at = NULL`
	}
	_, err := tx.ExecContext(ctx, q+` WHERE id = ?`,
		f.Name, key, f.Target, f.Description, a.Display(), now.UnixMilli(), cur.ID)
	if isUniqueViolation(err) {
		return Link{}, ErrExists
	}
	if err != nil {
		return Link{}, err
	}
	before := cur.Fields()
	if err := addEvent(ctx, tx, cur.ID, action, &before, &f, a, now); err != nil {
		return Link{}, err
	}
	return getByID(ctx, tx, cur.ID)
}

// Delete soft-deletes a live link.
func (s *Store) Delete(ctx context.Context, id int64, a Actor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cur, err := getByID(ctx, tx, id)
	if err != nil {
		return err
	}
	if cur.Deleted() {
		return ErrNotFound
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE links SET deleted_at = ?, updated_by = ?, updated_at = ?, rev = rev + 1 WHERE id = ?`,
		now.UnixMilli(), a.Display(), now.UnixMilli(), id); err != nil {
		return err
	}
	before := cur.Fields()
	if err := addEvent(ctx, tx, id, "delete", &before, nil, a, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Restore undeletes a link with its last fields.
func (s *Store) Restore(ctx context.Context, id int64, a Actor) (Link, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	cur, err := getByID(ctx, tx, id)
	if err != nil {
		return Link{}, err
	}
	if !cur.Deleted() {
		return cur, nil
	}
	l, err := s.apply(ctx, tx, cur, cur.Fields(), a, "restore", true)
	if err != nil {
		return Link{}, err
	}
	return l, tx.Commit()
}

// RevertTo sets a link back to the version recorded by an event (its "after"
// fields), undeleting it if necessary.
func (s *Store) RevertTo(ctx context.Context, eventID int64, a Actor) (Link, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	ev, err := getEvent(ctx, tx, eventID)
	if err != nil {
		return Link{}, err
	}
	if ev.After == nil {
		return Link{}, ErrNotRevertible
	}
	cur, err := getByID(ctx, tx, ev.LinkID)
	if err != nil {
		return Link{}, err
	}
	if !cur.Deleted() && cur.Fields() == *ev.After {
		return cur, nil
	}
	l, err := s.apply(ctx, tx, cur, *ev.After, a, "revert", cur.Deleted())
	if err != nil {
		return Link{}, err
	}
	return l, tx.Commit()
}

// ListOptions controls List.
type ListOptions struct {
	Query   string // substring match on name, target, description
	Sort    string // "name" (default), "recent", "popular", "created"
	Limit   int    // 0 = no limit
	Deleted bool   // list deleted links instead of live ones
}

// List returns links matching opts.
func (s *Store) List(ctx context.Context, opts ListOptions) ([]Link, error) {
	var (
		where = []string{"deleted_at IS NULL"}
		args  []any
	)
	if opts.Deleted {
		where[0] = "deleted_at IS NOT NULL"
	}
	if q := strings.TrimSpace(opts.Query); q != "" {
		like := "%" + escapeLike(strings.ToLower(q)) + "%"
		clause := `(lower(name) LIKE ? ESCAPE '\' OR lower(target) LIKE ? ESCAPE '\' OR lower(description) LIKE ? ESCAPE '\'`
		args = append(args, like, like, like)
		if k := link.Key(q); k != "" {
			clause += ` OR key LIKE ? ESCAPE '\'`
			args = append(args, "%"+escapeLike(k)+"%")
		}
		where = append(where, clause+")")
	}
	order := "key ASC"
	switch opts.Sort {
	case "recent":
		order = "updated_at DESC, key ASC"
	case "popular":
		order = "clicks DESC, key ASC"
	case "created":
		order = "created_at DESC, key ASC"
	}
	q := `SELECT ` + linkCols + ` FROM links WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + order
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// CountLive returns the number of live links.
func (s *Store) CountLive(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM links WHERE deleted_at IS NULL`).Scan(&n)
	return n, err
}

const eventCols = `e.id, e.link_id, l.name, e.action, e.before_json, e.after_json,
	e.actor_login, e.actor_node, e.actor_ip, e.at`

func scanEvent(r rowScanner) (Event, error) {
	var (
		e             Event
		before, after sql.NullString
		at            int64
	)
	if err := r.Scan(&e.ID, &e.LinkID, &e.LinkName, &e.Action, &before, &after,
		&e.Actor.Login, &e.Actor.Node, &e.Actor.IP, &at); err != nil {
		return Event{}, err
	}
	e.At = time.UnixMilli(at)
	var err error
	if e.Before, err = unmarshalFields(before); err != nil {
		return Event{}, err
	}
	if e.After, err = unmarshalFields(after); err != nil {
		return Event{}, err
	}
	return e, nil
}

func unmarshalFields(ns sql.NullString) (*Fields, error) {
	if !ns.Valid {
		return nil, nil
	}
	var f Fields
	if err := json.Unmarshal([]byte(ns.String), &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func getEvent(ctx context.Context, q querier, id int64) (Event, error) {
	e, err := scanEvent(q.QueryRowContext(ctx,
		`SELECT `+eventCols+` FROM link_events e JOIN links l ON l.id = e.link_id WHERE e.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrEventNotFound
	}
	return e, err
}

// GetEvent returns one audit entry.
func (s *Store) GetEvent(ctx context.Context, id int64) (Event, error) {
	return getEvent(ctx, s.db, id)
}

func (s *Store) events(ctx context.Context, where string, limit int, args ...any) ([]Event, error) {
	q := `SELECT ` + eventCols + ` FROM link_events e JOIN links l ON l.id = e.link_id ` +
		where + ` ORDER BY e.id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// History returns a link's audit entries, newest first.
func (s *Store) History(ctx context.Context, linkID int64) ([]Event, error) {
	return s.events(ctx, `WHERE e.link_id = ?`, 0, linkID)
}

// RecentEvents returns the newest audit entries across all links.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	return s.events(ctx, ``, limit)
}

// AddClicks adds batched click counts.
func (s *Store) AddClicks(ctx context.Context, counts map[int64]int64, at time.Time) error {
	if len(counts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, n := range counts {
		if _, err := tx.ExecContext(ctx,
			`UPDATE links SET clicks = clicks + ?, last_click = max(coalesce(last_click, 0), ?) WHERE id = ?`,
			n, at.UnixMilli(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Snapshot writes a consistent copy of the database to dest, which must not
// exist. It does not block readers and takes milliseconds at household scale.
//
// The copy is written under a hidden temporary name next to dest (see
// SnapshotTempName), flushed to disk, and only then renamed to dest, so dest
// is either absent or complete: an interrupted snapshot (a restart, a full
// disk, a power cut) never leaves an empty or partial file under a name that
// listings, backups and restores would take for a good one.
func (s *Store) Snapshot(ctx context.Context, dest string) (err error) {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("snapshot %s already exists", dest)
	}
	tmp := SnapshotTempName(dest)
	// A leftover from a crash would make VACUUM INTO fail.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if beforeSnapshotRename != nil {
		if err := beforeSnapshotRename(tmp); err != nil {
			return err
		}
	}
	if err := CheckSnapshot(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("sync snapshot directory: %w", err)
	}
	return nil
}

// beforeSnapshotRename, when set by a test, runs between writing a snapshot
// and renaming it into place, standing in for a failure at that point.
var beforeSnapshotRename func(tmp string) error

// SnapshotTempName is where Snapshot writes dest before renaming it: the same
// directory, with a leading "." and a ".tmp" suffix, so it never matches a
// snapshot name.
func SnapshotTempName(dest string) string {
	return filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp")
}

func syncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename in dir durable. File systems that can't sync a
// directory say so with EINVAL or ENOTSUP; there is nothing more to do there.
func syncDir(dir string) error {
	err := syncFile(dir)
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

// sqliteMagic starts every SQLite database file.
const sqliteMagic = "SQLite format 3\x00"

// ErrBadSnapshot means a file is not a complete SQLite database.
var ErrBadSnapshot = errors.New("not a complete SQLite database")

// CheckSnapshot reports whether the file at p looks like a complete SQLite
// database: it has the SQLite header, a valid page size, and (when the header
// records it, as every SQLite since 2010 does) exactly as many pages as the
// header says. It catches empty, truncated and foreign files without opening
// them; it is not an integrity check.
func CheckSnapshot(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: %w (not a regular file)", p, ErrBadSnapshot)
	}
	var h [100]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return fmt.Errorf("%s: %w (%d bytes)", p, ErrBadSnapshot, fi.Size())
	}
	if string(h[:16]) != sqliteMagic {
		return fmt.Errorf("%s: %w (no SQLite header)", p, ErrBadSnapshot)
	}
	pageSize := int64(binary.BigEndian.Uint16(h[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return fmt.Errorf("%s: %w (bad page size %d)", p, ErrBadSnapshot, pageSize)
	}
	size := fi.Size()
	pages := int64(binary.BigEndian.Uint32(h[28:32]))
	changes, validFor := binary.BigEndian.Uint32(h[24:28]), binary.BigEndian.Uint32(h[92:96])
	switch {
	case pages > 0 && changes == validFor:
		if size != pages*pageSize {
			return fmt.Errorf("%s: %w (%d bytes, header says %d pages of %d)", p, ErrBadSnapshot, size, pages, pageSize)
		}
	case size < pageSize || size%pageSize != 0:
		return fmt.Errorf("%s: %w (%d bytes is not a whole number of %d-byte pages)", p, ErrBadSnapshot, size, pageSize)
	}
	return nil
}

// ImportResult summarises an import.
type ImportResult struct {
	Created, Updated, Skipped int
	Errors                    []string // items that failed; nothing was imported
	Committed                 bool     // the changes were saved
}

// ImportOptions controls Import.
type ImportOptions struct {
	Overwrite bool // update existing live links instead of skipping them
	DryRun    bool // do everything, then roll it back
}

// Import creates links from items in one transaction; an existing live link
// with the same key is skipped, or updated when Overwrite is set. Fields must
// already be valid.
//
// It is all or nothing: when any item fails (the link cap, for example),
// every item is still tried so that res.Errors lists all the failures, and
// then nothing is saved. A dry run saves nothing either, but reports exactly
// what the import would do. The returned error is for failures of the
// database itself.
func (s *Store) Import(ctx context.Context, items []Fields, a Actor, opt ImportOptions) (ImportResult, error) {
	var res ImportResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, f := range items {
		cur, err := getByKey(ctx, tx, link.Key(f.Name))
		switch {
		case errors.Is(err, ErrNotFound):
			_, err = s.createTx(ctx, tx, f, a, "import")
			if err == nil {
				res.Created++
			}
		case err != nil:
			return res, err
		case !opt.Overwrite || cur.Fields() == f:
			res.Skipped++
		default:
			_, err = s.apply(ctx, tx, cur, f, a, "update", false)
			if err == nil {
				res.Updated++
			}
		}
		switch {
		case errors.Is(err, ErrLimit), errors.Is(err, ErrExists):
			res.Errors = append(res.Errors, f.Name+": "+err.Error())
		case err != nil:
			return res, err
		}
	}
	if len(res.Errors) > 0 || opt.DryRun {
		return res, nil // rolled back
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	res.Committed = true
	return res, nil
}
