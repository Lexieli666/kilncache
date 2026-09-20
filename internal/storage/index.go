package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps the runtime image distroless/static
)

// Index is the per-node metadata index: what this node holds, how big it is,
// and when it was last read.
//
// SQLite rather than a Go map plus a snapshot file. The index has to survive a
// crash, has to answer "what is the total size" without a full scan, and has to
// answer "give me the coldest N objects" cheaply. A map gives none of those
// without reimplementing a B-tree and a write-ahead log, which is a worse
// version of what SQLite already is.
//
// The pure-Go driver (modernc.org/sqlite) rather than mattn/go-sqlite3 for one
// concrete reason: CGO_ENABLED=0 keeps the runtime image
// distroless/static:nonroot, with no libc and no shell. That is a real security
// property, not a preference.
type Index struct {
	// Two pools over the same file, which is the standard shape for SQLite in
	// Go and the fix for a real starvation bug (docs/bugs.md, entry 7).
	//
	// SQLite allows exactly one writer at a time, so the write pool is capped
	// at one connection: letting database/sql open several turns that
	// serialisation into SQLITE_BUSY errors instead of an orderly queue. WAL
	// mode, though, allows any number of concurrent readers alongside that
	// writer -- so capping *reads* at one connection as well throws away the
	// main reason WAL was chosen. With a single shared connection, the repair
	// auditor's index scan held it for the duration of the scan and every
	// eviction sweep, access-time flush and metadata write queued behind it.
	// Under load the quota stopped being enforced at all.
	w    *sql.DB
	r    *sql.DB
	log  *slog.Logger
	path string
	now  func() time.Time

	// Touches are batched. Recording a read means writing to the index, so a
	// naive implementation turns every cache hit -- the operation this whole
	// system exists to make fast -- into a database write and an fsync. Reads
	// accumulate here and are flushed periodically; the cost of losing a few on
	// a crash is that eviction picks slightly the wrong victim, which is not a
	// correctness property.
	touchMu      sync.Mutex
	pendingTouch map[objRef]time.Time
	touchLimit   int

	closed   atomic.Bool
	flushing sync.Mutex

	stats IndexStats
}

type objRef struct {
	NS  Namespace
	Key string
}

// IndexStats counts index work, so the cost of the metadata layer is visible
// rather than inferred.
type IndexStats struct {
	Inserts        atomic.Int64
	Deletes        atomic.Int64
	TouchesQueued  atomic.Int64
	TouchFlushes   atomic.Int64
	TouchesWritten atomic.Int64
	TouchesDropped atomic.Int64
}

// IndexOptions configures an Index.
type IndexOptions struct {
	Path       string
	Logger     *slog.Logger
	TouchLimit int

	// Readers caps the concurrent read connections. Zero means the default.
	Readers int

	now func() time.Time
}

const (
	defaultTouchLimit = 4096
	// defaultReaders is small on purpose: the read workload is a handful of
	// background scans and point lookups, not a query engine.
	defaultReaders = 4
)

// OpenIndex opens or creates the metadata database.
//
// The pragmas are chosen deliberately:
//
//   - journal_mode=WAL: readers do not block the writer, which matters because
//     eviction scans while requests are being served.
//   - synchronous=NORMAL: in WAL mode this fsyncs at checkpoints rather than
//     every commit. The index is *derived* state -- startup reconciliation
//     rebuilds it from the disk -- so paying a full fsync per metadata write to
//     protect data that can be recomputed would be the wrong trade. The object
//     files themselves are still fsynced individually (ADR-0003); that is where
//     durability actually lives.
//   - busy_timeout: the eviction sweep and a request can collide; waiting is
//     correct, returning SQLITE_BUSY to a client is not.
//   - foreign_keys: no foreign keys here, but on by default costs nothing and
//     prevents a future schema from silently not enforcing them.
func OpenIndex(opts IndexOptions) (*Index, error) {
	if opts.Path == "" {
		return nil, errors.New("storage: index path must not be empty")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	if opts.TouchLimit <= 0 {
		opts.TouchLimit = defaultTouchLimit
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	base := "file:" + url.PathEscape(opts.Path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(ON)"

	w, err := sql.Open("sqlite", base)
	if err != nil {
		return nil, fmt.Errorf("storage: open index for writing: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := w.PingContext(ctx); err != nil {
		w.Close()
		return nil, fmt.Errorf("storage: ping index: %w", err)
	}
	// Schema first: the read pool opens the same file, and a reader that
	// arrives before the tables exist would fail its first query.
	if err := migrate(ctx, w); err != nil {
		w.Close()
		return nil, err
	}

	readers := opts.Readers
	if readers <= 0 {
		readers = defaultReaders
	}
	r, err := sql.Open("sqlite", base+"&_pragma=query_only(ON)")
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("storage: open index for reading: %w", err)
	}
	r.SetMaxOpenConns(readers)
	r.SetMaxIdleConns(readers)
	r.SetConnMaxLifetime(0)
	if err := r.PingContext(ctx); err != nil {
		w.Close()
		r.Close()
		return nil, fmt.Errorf("storage: ping index readers: %w", err)
	}

	idx := &Index{
		w:            w,
		r:            r,
		log:          opts.Logger,
		path:         opts.Path,
		now:          opts.now,
		pendingTouch: make(map[objRef]time.Time, opts.TouchLimit),
		touchLimit:   opts.TouchLimit,
	}
	return idx, nil
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

const schema = `
CREATE TABLE IF NOT EXISTS objects (
    ns           TEXT    NOT NULL,
    key          TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    last_access  INTEGER NOT NULL,
    access_count INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (ns, key)
) WITHOUT ROWID;

-- Eviction reads objects coldest-first. Without this index that is a full
-- table scan on every sweep, which on a million-object cache is the difference
-- between a sweep that finishes and one that does not.
CREATE INDEX IF NOT EXISTS objects_by_access ON objects(last_access);

CREATE TABLE IF NOT EXISTS meta (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL
) WITHOUT ROWID;
`

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("storage: create index schema: %w", err)
	}
	return nil
}

// Path returns the index file path.
func (i *Index) Path() string { return i.path }

// Snapshot copies the index counters.
func (i *Index) Snapshot() IndexSnapshot {
	return IndexSnapshot{
		Inserts:        i.stats.Inserts.Load(),
		Deletes:        i.stats.Deletes.Load(),
		TouchesQueued:  i.stats.TouchesQueued.Load(),
		TouchFlushes:   i.stats.TouchFlushes.Load(),
		TouchesWritten: i.stats.TouchesWritten.Load(),
		TouchesDropped: i.stats.TouchesDropped.Load(),
	}
}

// IndexSnapshot is a JSON-friendly copy of IndexStats.
type IndexSnapshot struct {
	Inserts        int64 `json:"inserts"`
	Deletes        int64 `json:"deletes"`
	TouchesQueued  int64 `json:"touches_queued"`
	TouchFlushes   int64 `json:"touch_flushes"`
	TouchesWritten int64 `json:"touches_written"`
	TouchesDropped int64 `json:"touches_dropped"`
}

// Entry is one indexed object.
type Entry struct {
	Namespace   Namespace `json:"namespace"`
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	LastAccess  time.Time `json:"last_access"`
	AccessCount int64     `json:"access_count"`
}

// Upsert records an object, preserving its access history if it is already
// known. A duplicate PUT must not look like a fresh, cold object to eviction.
func (i *Index) Upsert(ctx context.Context, ns Namespace, key string, size int64) error {
	if i.closed.Load() {
		return ErrClosed
	}
	now := i.now().UnixNano()
	_, err := i.w.ExecContext(ctx, `
        INSERT INTO objects (ns, key, size, created_at, last_access, access_count)
        VALUES (?, ?, ?, ?, ?, 1)
        ON CONFLICT(ns, key) DO UPDATE SET
            size        = excluded.size,
            last_access = excluded.last_access`,
		string(ns), key, size, now, now)
	if err != nil {
		return fmt.Errorf("storage: index upsert: %w", err)
	}
	i.stats.Inserts.Add(1)
	return nil
}

// Remove deletes an object from the index.
func (i *Index) Remove(ctx context.Context, ns Namespace, key string) error {
	if i.closed.Load() {
		return ErrClosed
	}
	// Drop any pending touch first, so a flush cannot resurrect a row that was
	// just deleted. Without this, evicting an object that was read moments
	// earlier leaves a phantom row claiming space that no file occupies.
	i.touchMu.Lock()
	delete(i.pendingTouch, objRef{NS: ns, Key: key})
	i.touchMu.Unlock()

	if _, err := i.w.ExecContext(ctx, `DELETE FROM objects WHERE ns = ? AND key = ?`, string(ns), key); err != nil {
		return fmt.Errorf("storage: index remove: %w", err)
	}
	i.stats.Deletes.Add(1)
	return nil
}

// Ref names one object, for batch operations.
type Ref struct {
	Namespace Namespace
	Key       string
}

// RemoveBatch deletes many objects from the index in one transaction.
//
// One transaction rather than one per object, because eviction removes
// thousands at a time and a per-object commit makes the index the bottleneck in
// a sweep -- which is what kept the quota from being enforced under load
// (docs/bugs.md, entry 8).
func (i *Index) RemoveBatch(ctx context.Context, refs []Ref) error {
	if i.closed.Load() {
		return ErrClosed
	}
	if len(refs) == 0 {
		return nil
	}

	// Drop pending touches first, so a later flush cannot resurrect a row that
	// is about to be deleted.
	i.touchMu.Lock()
	for _, r := range refs {
		delete(i.pendingTouch, objRef{NS: r.Namespace, Key: r.Key})
	}
	i.touchMu.Unlock()

	tx, err := i.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin batch remove: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `DELETE FROM objects WHERE ns = ? AND key = ?`)
	if err != nil {
		return fmt.Errorf("storage: prepare batch remove: %w", err)
	}
	defer stmt.Close()

	for _, r := range refs {
		if _, err := stmt.ExecContext(ctx, string(r.Namespace), r.Key); err != nil {
			return fmt.Errorf("storage: batch remove: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit batch remove: %w", err)
	}
	i.stats.Deletes.Add(int64(len(refs)))
	return nil
}

// Touch records a read. It is queued, not written: see the Index doc comment.
func (i *Index) Touch(ns Namespace, key string) {
	if i.closed.Load() {
		return
	}
	now := i.now()

	i.touchMu.Lock()
	if len(i.pendingTouch) >= i.touchLimit {
		if _, known := i.pendingTouch[objRef{NS: ns, Key: key}]; !known {
			// Full, and this is a new key. Dropping the touch loses recency
			// information for one object, which shifts eviction order slightly.
			// The alternative -- blocking a cache hit on a database write --
			// would make a burst of reads slower than a cold miss.
			i.touchMu.Unlock()
			i.stats.TouchesDropped.Add(1)
			return
		}
	}
	i.pendingTouch[objRef{NS: ns, Key: key}] = now
	i.touchMu.Unlock()
	i.stats.TouchesQueued.Add(1)
}

// FlushTouches writes queued reads to the index.
func (i *Index) FlushTouches(ctx context.Context) error {
	i.flushing.Lock()
	defer i.flushing.Unlock()

	i.touchMu.Lock()
	if len(i.pendingTouch) == 0 {
		i.touchMu.Unlock()
		return nil
	}
	batch := i.pendingTouch
	i.pendingTouch = make(map[objRef]time.Time, i.touchLimit)
	i.touchMu.Unlock()

	tx, err := i.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin touch flush: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
        UPDATE objects
           SET last_access = ?, access_count = access_count + 1
         WHERE ns = ? AND key = ?`)
	if err != nil {
		return fmt.Errorf("storage: prepare touch flush: %w", err)
	}
	defer stmt.Close()

	var written int64
	for ref, at := range batch {
		if _, err := stmt.ExecContext(ctx, at.UnixNano(), string(ref.NS), ref.Key); err != nil {
			return fmt.Errorf("storage: touch flush: %w", err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit touch flush: %w", err)
	}

	i.stats.TouchFlushes.Add(1)
	i.stats.TouchesWritten.Add(written)
	return nil
}

// Totals returns the number of indexed objects and their total size.
func (i *Index) Totals(ctx context.Context) (objects, bytes int64, err error) {
	row := i.r.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM objects`)
	if err := row.Scan(&objects, &bytes); err != nil {
		return 0, 0, fmt.Errorf("storage: index totals: %w", err)
	}
	return objects, bytes, nil
}

// Get returns one entry.
func (i *Index) Get(ctx context.Context, ns Namespace, key string) (Entry, error) {
	row := i.r.QueryRowContext(ctx, `
        SELECT size, created_at, last_access, access_count
          FROM objects WHERE ns = ? AND key = ?`, string(ns), key)
	var size, created, access, count int64
	if err := row.Scan(&size, &created, &access, &count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("storage: index get: %w", err)
	}
	return Entry{
		Namespace:   ns,
		Key:         key,
		Size:        size,
		CreatedAt:   time.Unix(0, created),
		LastAccess:  time.Unix(0, access),
		AccessCount: count,
	}, nil
}

// ColdestN returns the n least-recently-accessed entries.
//
// It returns a batch rather than a single victim so that the eviction pass can
// rank candidates by a size-weighted score (see evict.go) instead of blindly
// taking the oldest. Ranking needs more candidates than it will use.
func (i *Index) ColdestN(ctx context.Context, n int) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := i.r.QueryContext(ctx, `
        SELECT ns, key, size, created_at, last_access, access_count
          FROM objects
      ORDER BY last_access ASC
         LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("storage: index coldest: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var ns, key string
		var size, created, access, count int64
		if err := rows.Scan(&ns, &key, &size, &created, &access, &count); err != nil {
			return nil, fmt.Errorf("storage: scan coldest: %w", err)
		}
		out = append(out, Entry{
			Namespace:   Namespace(ns),
			Key:         key,
			Size:        size,
			CreatedAt:   time.Unix(0, created),
			LastAccess:  time.Unix(0, access),
			AccessCount: count,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate coldest: %w", err)
	}
	return out, nil
}

// PageAfter returns up to limit entries ordered by (namespace, key), starting
// strictly after the given position.
//
// The repair auditor uses this instead of streaming the whole table. Holding a
// single long-lived iterator open across a table that requests are actively
// mutating is a bad idea in any database; in SQLite it also pins a connection
// for the duration, which is what let the auditor starve eviction badly enough
// that the quota stopped being enforced (docs/bugs.md, entry 7).
//
// Keyset pagination rather than LIMIT/OFFSET: the table is WITHOUT ROWID with
// (ns, key) as its primary key, so this is a direct index seek regardless of
// how far through the table the cursor is. OFFSET would re-scan everything
// before the cursor on every page, making a full sweep quadratic.
func (i *Index) PageAfter(ctx context.Context, lastNS Namespace, lastKey string, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := i.r.QueryContext(ctx, `
        SELECT ns, key, size, created_at, last_access, access_count
          FROM objects
         WHERE (ns > ?) OR (ns = ? AND key > ?)
      ORDER BY ns ASC, key ASC
         LIMIT ?`, string(lastNS), string(lastNS), lastKey, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: index page: %w", err)
	}
	defer rows.Close()

	out := make([]Entry, 0, limit)
	for rows.Next() {
		var ns, key string
		var size, created, access, count int64
		if err := rows.Scan(&ns, &key, &size, &created, &access, &count); err != nil {
			return nil, fmt.Errorf("storage: scan page: %w", err)
		}
		out = append(out, Entry{
			Namespace:   Namespace(ns),
			Key:         key,
			Size:        size,
			CreatedAt:   time.Unix(0, created),
			LastAccess:  time.Unix(0, access),
			AccessCount: count,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate page: %w", err)
	}
	return out, nil
}

// All streams every entry, for reconciliation.
func (i *Index) All(ctx context.Context, fn func(Entry) error) error {
	rows, err := i.r.QueryContext(ctx, `
        SELECT ns, key, size, created_at, last_access, access_count FROM objects`)
	if err != nil {
		return fmt.Errorf("storage: index scan: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ns, key string
		var size, created, access, count int64
		if err := rows.Scan(&ns, &key, &size, &created, &access, &count); err != nil {
			return fmt.Errorf("storage: scan entry: %w", err)
		}
		if err := fn(Entry{
			Namespace:   Namespace(ns),
			Key:         key,
			Size:        size,
			CreatedAt:   time.Unix(0, created),
			LastAccess:  time.Unix(0, access),
			AccessCount: count,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ReplaceAll rewrites the index from an authoritative list, in one transaction.
// Used by startup reconciliation.
func (i *Index) ReplaceAll(ctx context.Context, entries []Entry) error {
	tx, err := i.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM objects`); err != nil {
		return fmt.Errorf("storage: clear index: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO objects (ns, key, size, created_at, last_access, access_count)
        VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("storage: prepare reconcile insert: %w", err)
	}
	defer stmt.Close()

	for _, e := range entries {
		count := e.AccessCount
		if count < 1 {
			count = 1
		}
		if _, err := stmt.ExecContext(ctx, string(e.Namespace), e.Key, e.Size,
			e.CreatedAt.UnixNano(), e.LastAccess.UnixNano(), count); err != nil {
			return fmt.Errorf("storage: reconcile insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit reconcile: %w", err)
	}
	return nil
}

// SetMeta stores a small key/value, used to record when reconciliation last ran.
func (i *Index) SetMeta(ctx context.Context, k, v string) error {
	_, err := i.w.ExecContext(ctx,
		`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	if err != nil {
		return fmt.Errorf("storage: set meta: %w", err)
	}
	return nil
}

// GetMeta reads a small key/value.
func (i *Index) GetMeta(ctx context.Context, k string) (string, error) {
	var v string
	err := i.r.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("storage: get meta: %w", err)
	}
	return v, nil
}

// Close flushes queued touches and closes the database.
//
// Flushing on the way out is what makes touch batching acceptable: a clean
// shutdown loses nothing, and only a crash costs some recency information.
func (i *Index) Close() error {
	if i.closed.Swap(true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// closed is already set, so Touch will not add more; flush what is queued.
	i.touchMu.Lock()
	pending := len(i.pendingTouch)
	i.touchMu.Unlock()
	if pending > 0 {
		i.closed.Store(false)
		err := i.FlushTouches(ctx)
		i.closed.Store(true)
		if err != nil {
			i.log.Warn("could not flush queued access times on shutdown; eviction order may be slightly stale",
				slog.Int("pending", pending), slog.String("err", err.Error()))
		}
	}
	var firstErr error
	if err := i.r.Close(); err != nil {
		firstErr = err
	}
	if err := i.w.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// indexPath returns the index file location inside a store root.
func indexPath(root string) string { return filepath.Join(root, "index.db") }
