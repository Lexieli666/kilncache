package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Store.
type Options struct {
	// Root is the directory that holds the cas/ and ac/ trees and tmp/.
	Root string

	// MaxObjectBytes rejects a single object larger than this. Zero means no
	// limit, which is only appropriate in tests.
	MaxObjectBytes int64

	// VerifyReads re-hashes CAS objects as they are served and refuses to
	// return bytes that do not match the key.
	//
	// This costs a SHA-256 pass over every byte read and, more importantly,
	// prevents the kernel's sendfile path from being used, so it is a real
	// throughput trade-off rather than a free safety net. It is measured both
	// ways in docs/perf-notes.md. Default on: a build cache that returns wrong
	// bytes fast is worse than one that returns right bytes slowly.
	VerifyReads bool

	Logger *slog.Logger

	// Quota bounds disk usage. A zero MaxBytes disables eviction, which is
	// what the unit tests want and what a node with a dedicated disk may want.
	Quota Quota

	// Policy ranks eviction candidates. Zero means DefaultEvictionPolicy.
	Policy EvictionPolicy

	// DisableIndex skips the metadata index entirely. Only useful in tests
	// that are about the object tree and nothing else.
	DisableIndex bool

	// now is injectable so eviction and repair tests can control time.
	now func() time.Time
}

// Store is the on-disk object store for one node.
//
// It is safe for concurrent use. Concurrent PUTs of the same key are safe
// because each writes to a uniquely named temp file and publishes with rename:
// the losers of the race overwrite the winner with byte-identical content, and
// a reader that has the file open keeps reading the inode it opened.
type Store struct {
	root           string
	tmpDir         string
	maxObjectBytes int64
	verifyReads    bool
	log            *slog.Logger
	now            func() time.Time

	dirs *dirCache

	// index is the metadata layer: sizes, access times, and the totals that
	// quota enforcement needs. It is nil when DisableIndex is set, and every
	// call site tolerates that rather than branching on a mode flag.
	index *Index

	// evictor is set by the owner (internal/node) after Open, because it needs
	// the store that is being constructed here.
	evictor atomic.Pointer[Evictor]

	closed atomic.Bool

	// dirSyncUnsupported is set the first time a directory fsync is rejected by
	// the filesystem, so the warning is logged once rather than per write.
	dirSyncUnsupported atomic.Bool
	dirSyncWarnOnce    sync.Once

	stats Stats
}

// Stats are the store's cheap always-on counters. They are the Phase 1 version
// of the Prometheus metrics: the same numbers, without a dependency.
type Stats struct {
	PutsOK            atomic.Int64
	PutsRejected      atomic.Int64
	PutsAlreadyStored atomic.Int64
	Gets              atomic.Int64
	GetMisses         atomic.Int64
	Deletes           atomic.Int64
	BytesIn           atomic.Int64
	BytesOut          atomic.Int64
	ObjectsStored     atomic.Int64
	BytesStored       atomic.Int64
	VerifyFailures    atomic.Int64
}

// StatsSnapshot is a point-in-time copy of Stats, suitable for JSON.
type StatsSnapshot struct {
	PutsOK            int64 `json:"puts_ok"`
	PutsRejected      int64 `json:"puts_rejected"`
	PutsAlreadyStored int64 `json:"puts_already_stored"`
	Gets              int64 `json:"gets"`
	GetMisses         int64 `json:"get_misses"`
	Deletes           int64 `json:"deletes"`
	BytesIn           int64 `json:"bytes_in"`
	BytesOut          int64 `json:"bytes_out"`
	ObjectsStored     int64 `json:"objects_stored"`
	BytesStored       int64 `json:"bytes_stored"`
	VerifyFailures    int64 `json:"verify_failures"`
}

// Snapshot copies the counters.
func (s *Store) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		PutsOK:            s.stats.PutsOK.Load(),
		PutsRejected:      s.stats.PutsRejected.Load(),
		PutsAlreadyStored: s.stats.PutsAlreadyStored.Load(),
		Gets:              s.stats.Gets.Load(),
		GetMisses:         s.stats.GetMisses.Load(),
		Deletes:           s.stats.Deletes.Load(),
		BytesIn:           s.stats.BytesIn.Load(),
		BytesOut:          s.stats.BytesOut.Load(),
		ObjectsStored:     s.stats.ObjectsStored.Load(),
		BytesStored:       s.stats.BytesStored.Load(),
		VerifyFailures:    s.stats.VerifyFailures.Load(),
	}
}

// tmpPrefix marks in-progress uploads. Nothing with this prefix is ever
// reachable by a GET, because a valid key is 64 hex characters and this prefix
// is not hex. Startup deletes anything left over from a crash.
const tmpPrefix = "incoming-"

// Open prepares the directory tree and returns a Store.
func Open(opts Options) (*Store, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return nil, errors.New("storage: root must not be empty")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve root: %w", err)
	}

	s := &Store{
		root:           root,
		tmpDir:         filepath.Join(root, "tmp"),
		maxObjectBytes: opts.MaxObjectBytes,
		verifyReads:    opts.VerifyReads,
		log:            opts.Logger,
		now:            opts.now,
		dirs:           newDirCache(),
	}

	for _, d := range []string{root, s.tmpDir, filepath.Join(root, string(NamespaceCAS)), filepath.Join(root, string(NamespaceAC))} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("storage: create %s: %w", d, err)
		}
	}

	if err := s.sweepTemps(); err != nil {
		return nil, err
	}

	if !opts.DisableIndex {
		idx, err := OpenIndex(IndexOptions{
			Path:   indexPath(root),
			Logger: opts.Logger,
			now:    opts.now,
		})
		if err != nil {
			return nil, err
		}
		s.index = idx

		// Reconcile before the store is handed to anyone. The index is derived
		// state and the disk is the truth; a node that served requests against
		// an unreconciled index could report an object it does not have.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := s.Reconcile(ctx); err != nil {
			_ = idx.Close()
			return nil, err
		}
	}
	return s, nil
}

// Index exposes the metadata index, or nil when it is disabled.
func (s *Store) Index() *Index { return s.index }

// AttachEvictor wires an evictor so that writes can wake it.
//
// It is set after construction rather than in Open because the evictor needs a
// reference to the store being constructed. The alternative -- a two-phase
// constructor -- would push that awkwardness onto every caller.
func (s *Store) AttachEvictor(e *Evictor) { s.evictor.Store(e) }

// ReconcileResult reports what startup reconciliation found.
type ReconcileResult struct {
	FilesOnDisk    int64         `json:"files_on_disk"`
	BytesOnDisk    int64         `json:"bytes_on_disk"`
	IndexedBefore  int64         `json:"indexed_before"`
	AddedToIndex   int64         `json:"added_to_index"`
	RemovedStale   int64         `json:"removed_stale_index_rows"`
	SizeCorrected  int64         `json:"size_corrected"`
	Duration       time.Duration `json:"-"`
	DurationMillis int64         `json:"duration_ms"`
}

// Reconcile rebuilds the index from what is actually on disk.
//
// The disk is authoritative and the index is derived, in that order and never
// the other way round. This is the rule that makes the Phase 3 acceptance
// criterion -- "metadata never marks a missing file valid" -- structurally true
// rather than a behaviour to be maintained: reconciliation walks the object
// tree, and an index row with no file simply does not survive the walk.
//
// The cost is a full directory walk at startup, which is proportional to the
// number of objects rather than to how long the node was down. On a
// million-object cache that is seconds, not milliseconds, and it happens before
// the node reports ready. The alternative -- trusting the index and repairing
// lazily -- would mean a node coming back from an unclean shutdown could answer
// HEAD with 200 for an object it lost, which is the one answer a cache must
// never give.
func (s *Store) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult
	if s.index == nil {
		return res, nil
	}
	start := s.now()

	before, _, err := s.index.Totals(ctx)
	if err != nil {
		return res, err
	}
	res.IndexedBefore = before

	// Keep whatever access history the old index has: a restart should not make
	// every object look equally hot and defeat eviction's ranking.
	known := make(map[objRef]Entry, before)
	if err := s.index.All(ctx, func(e Entry) error {
		known[objRef{NS: e.Namespace, Key: e.Key}] = e
		return nil
	}); err != nil {
		return res, err
	}

	entries := make([]Entry, 0, len(known))
	for _, ns := range []Namespace{NamespaceCAS, NamespaceAC} {
		if err := s.Walk(ns, func(st Stat) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			res.FilesOnDisk++
			res.BytesOnDisk += st.Size

			ref := objRef{NS: ns, Key: st.Key}
			if prev, ok := known[ref]; ok {
				delete(known, ref)
				if prev.Size != st.Size {
					// The file on disk wins. A size mismatch means the index is
					// describing something that is no longer there.
					res.SizeCorrected++
					prev.Size = st.Size
				}
				entries = append(entries, prev)
				return nil
			}
			res.AddedToIndex++
			entries = append(entries, Entry{
				Namespace:   ns,
				Key:         st.Key,
				Size:        st.Size,
				CreatedAt:   st.ModTime,
				LastAccess:  st.ModTime,
				AccessCount: 1,
			})
			return nil
		}); err != nil {
			return res, fmt.Errorf("storage: reconcile walk %s: %w", ns, err)
		}
	}
	// Anything left in `known` is an index row with no file.
	res.RemovedStale = int64(len(known))

	if err := s.index.ReplaceAll(ctx, entries); err != nil {
		return res, err
	}

	s.stats.ObjectsStored.Store(res.FilesOnDisk)
	s.stats.BytesStored.Store(res.BytesOnDisk)

	res.Duration = s.now().Sub(start)
	res.DurationMillis = res.Duration.Milliseconds()

	if err := s.index.SetMeta(ctx, "reconciled_at", s.now().UTC().Format(time.RFC3339Nano)); err != nil {
		s.log.Warn("could not record reconciliation time", slog.String("err", err.Error()))
	}

	level := slog.LevelInfo
	if res.RemovedStale > 0 || res.SizeCorrected > 0 {
		// Either of these means the previous shutdown was unclean, or something
		// touched the data directory. Worth noticing.
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, "reconciled index against disk",
		slog.Int64("files_on_disk", res.FilesOnDisk),
		slog.Int64("bytes_on_disk", res.BytesOnDisk),
		slog.Int64("indexed_before", res.IndexedBefore),
		slog.Int64("added", res.AddedToIndex),
		slog.Int64("removed_stale_rows", res.RemovedStale),
		slog.Int64("size_corrected", res.SizeCorrected),
		slog.Int64("duration_ms", res.DurationMillis),
	)
	return res, nil
}

// Root returns the absolute store root.
func (s *Store) Root() string { return s.root }

// Close marks the store closed and shuts the index down.
//
// Open file handles held by in-flight readers remain valid; the kernel, not
// this type, owns their lifetime.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.index != nil {
		return s.index.Close()
	}
	return nil
}

// sweepTemps removes partial uploads left by a crash.
//
// This is the startup half of the "a partial upload is never visible" claim.
// The runtime half is that a temp file is never named like a key; this half is
// that a crash does not leave them accumulating until the disk fills. It runs
// before the node reports ready.
func (s *Store) sweepTemps() error {
	entries, err := os.ReadDir(s.tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("storage: scan tmp dir: %w", err)
	}
	var removed, bytes int64
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tmpPrefix) {
			continue
		}
		p := filepath.Join(s.tmpDir, e.Name())
		if info, statErr := e.Info(); statErr == nil {
			bytes += info.Size()
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("storage: remove stale temp %s: %w", p, err)
		}
		removed++
	}
	if removed > 0 {
		s.log.Warn("removed partial uploads left by a previous run",
			slog.Int64("count", removed),
			slog.Int64("bytes", bytes),
		)
	}
	return nil
}

// PutResult describes what a Put did.
type PutResult struct {
	Key           string    `json:"key"`
	Namespace     Namespace `json:"namespace"`
	Size          int64     `json:"size"`
	Digest        string    `json:"digest"`
	AlreadyStored bool      `json:"already_stored"`
	Durable       bool      `json:"durable"`
}

// Put streams r into the store under key.
//
// The sequence is the whole durability argument, and its order is not
// negotiable:
//
//  1. stream into a uniquely named temp file, hashing as we go;
//  2. verify the digest (CAS only) and reject before anything is published;
//  3. fsync the file, so its data is on stable media;
//  4. rename into the shard directory, which is atomic for readers;
//  5. fsync the shard directory, so the name itself is durable.
//
// Step 5 is the one that is usually missing. See docs/adr/0003-durability.md.
//
// declaredSize is the client's Content-Length, or -1 when unknown. When it is
// known, a body that ends early is rejected: a truncated upload that hashes to
// something else would already fail the CAS digest check, but an AC entry has
// no such protection, and a short write there would publish a truncated
// ActionResult.
func (s *Store) Put(ctx context.Context, ns Namespace, key string, r io.Reader, declaredSize int64) (PutResult, error) {
	if s.closed.Load() {
		return PutResult{}, ErrClosed
	}
	if !ns.Valid() {
		return PutResult{}, fmt.Errorf("%w: %q", ErrBadNamespace, ns)
	}
	if err := ValidateKey(key); err != nil {
		return PutResult{}, err
	}
	if s.maxObjectBytes > 0 && declaredSize > s.maxObjectBytes {
		return PutResult{}, &TooLargeError{Size: declaredSize, Limit: s.maxObjectBytes}
	}

	dir, finalPath := s.ObjectPath(ns, key)
	if err := s.dirs.ensure(dir); err != nil {
		return PutResult{}, err
	}

	// A CAS object that is already present is already correct: its key is its
	// content hash, so there is nothing a second copy could improve. Skipping
	// the write saves a stream, an fsync and a directory fsync on the hottest
	// path in a warm cache. AC entries are not skipped, because their value can
	// legitimately change.
	if ns.ContentAddressed() {
		if info, err := os.Stat(finalPath); err == nil && info.Mode().IsRegular() {
			// Drain the body so the connection can be reused; a client that
			// gets a 200 while its request body sits unread will see the next
			// request on that connection fail.
			n, _ := s.drain(r)
			s.stats.PutsAlreadyStored.Add(1)
			s.stats.BytesIn.Add(n)
			// A duplicate PUT is a use. Recording it keeps an object that
			// several builds keep re-uploading from being evicted as cold.
			if s.index != nil {
				s.index.Touch(ns, key)
			}
			return PutResult{
				Key: key, Namespace: ns, Size: info.Size(),
				Digest: key, AlreadyStored: true, Durable: true,
			}, nil
		}
	}

	tmp, err := os.CreateTemp(s.tmpDir, tmpPrefix)
	if err != nil {
		return PutResult{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Anything that returns before the rename must leave nothing behind.
	published := false
	defer func() {
		if !published {
			_ = tmp.Close()
			if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
				s.log.Error("could not remove abandoned temp file",
					slog.String("path", tmpPath), slog.String("err", err.Error()))
			}
		}
	}()

	hasher := sha256.New()
	var limited io.Reader = r
	if s.maxObjectBytes > 0 {
		// +1 so that hitting the limit exactly is allowed and exceeding it by
		// one byte is detected.
		limited = io.LimitReader(r, s.maxObjectBytes+1)
	}

	buf := getBuf()
	written, copyErr := io.CopyBuffer(io.MultiWriter(tmp, hasher), limited, *buf)
	putBuf(buf)

	if copyErr != nil {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, fmt.Errorf("stream body: %w", copyErr)
	}
	if err := ctx.Err(); err != nil {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, err
	}
	if s.maxObjectBytes > 0 && written > s.maxObjectBytes {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, &TooLargeError{Size: written, Limit: s.maxObjectBytes}
	}
	if declaredSize >= 0 && written != declaredSize {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, fmt.Errorf("%w: declared %d, received %d", ErrShortWrite, declaredSize, written)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	if ns.ContentAddressed() && digest != key {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, &DigestMismatchError{Want: key, Got: digest, Size: written}
	}

	if err := tmp.Sync(); err != nil {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, fmt.Errorf("fsync object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, fmt.Errorf("close object: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		s.stats.PutsRejected.Add(1)
		return PutResult{}, fmt.Errorf("publish object: %w", err)
	}
	published = true

	durable := true
	if err := syncDir(dir); err != nil {
		if errors.Is(err, errDirSyncUnsupported) {
			durable = false
			s.noteDirSyncUnsupported(dir)
		} else {
			// The object is visible but its name may not survive a power loss.
			// Report the write as succeeded-but-not-durable rather than failing
			// it: the bytes are there and a reader will get them.
			durable = false
			s.log.Error("directory fsync failed; object is visible but its name is not durable",
				slog.String("dir", dir), slog.String("err", err.Error()))
		}
	}

	s.stats.PutsOK.Add(1)
	s.stats.BytesIn.Add(written)
	s.stats.ObjectsStored.Add(1)
	s.stats.BytesStored.Add(written)

	if s.index != nil {
		if err := s.index.Upsert(ctx, ns, key, written); err != nil {
			// The object is on disk and readable. An index that has not caught
			// up costs accurate quota accounting until the next
			// reconciliation, which is a worse outcome than losing the object
			// but not a reason to fail a write that succeeded.
			s.log.Error("object stored but not indexed; quota accounting will be short until the next reconcile",
				slog.String("ns", ns.String()), slog.String("key", key),
				slog.String("err", err.Error()))
		}
		if e := s.evictor.Load(); e != nil {
			e.Wake()
		}
	}

	return PutResult{
		Key: key, Namespace: ns, Size: written,
		Digest: digest, AlreadyStored: false, Durable: durable,
	}, nil
}

func (s *Store) noteDirSyncUnsupported(dir string) {
	s.dirSyncUnsupported.Store(true)
	s.dirSyncWarnOnce.Do(func() {
		s.log.Warn("this filesystem does not support fsync on a directory; publication is atomic but not crash-durable",
			slog.String("dir", dir))
	})
}

// DirSyncSupported reports whether directory fsync worked. The benchmark and
// chaos reports record it, because a durability claim on a filesystem that
// cannot fsync a directory is not a durability claim.
func (s *Store) DirSyncSupported() bool { return !s.dirSyncUnsupported.Load() }

func (s *Store) drain(r io.Reader) (int64, error) {
	buf := getBuf()
	defer putBuf(buf)
	return io.CopyBuffer(io.Discard, r, *buf)
}

// Stat describes a stored object without opening it.
type Stat struct {
	Key       string    `json:"key"`
	Namespace Namespace `json:"namespace"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
}

// Stat returns metadata for a stored object.
func (s *Store) Stat(ns Namespace, key string) (Stat, error) {
	if !ns.Valid() {
		return Stat{}, fmt.Errorf("%w: %q", ErrBadNamespace, ns)
	}
	if err := ValidateKey(key); err != nil {
		return Stat{}, err
	}
	_, path := s.ObjectPath(ns, key)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Stat{}, ErrNotFound
		}
		return Stat{}, fmt.Errorf("stat object: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Stat{}, ErrNotFound
	}
	// A HEAD is how Bazel asks "do you have this"; an object it keeps asking
	// about is in use even if the bytes are already in the client's own cache.
	if s.index != nil {
		s.index.Touch(ns, key)
	}
	return Stat{Key: key, Namespace: ns, Size: info.Size(), ModTime: info.ModTime()}, nil
}

// Object is an open handle to a stored object. The caller must Close it.
type Object struct {
	Stat
	f           *os.File
	verify      bool
	hasher      interface{ Write([]byte) (int, error) }
	store       *Store
	closeOnce   sync.Once
	bytesServed int64
}

// Read implements io.Reader, hashing along the way when verification is on.
func (o *Object) Read(p []byte) (int, error) {
	n, err := o.f.Read(p)
	if n > 0 {
		o.bytesServed += int64(n)
		if o.verify {
			_, _ = o.hasher.Write(p[:n])
		}
	}
	return n, err
}

// WriteTo streams the object to w.
//
// With verification off, io.Copy is allowed to find the destination's ReadFrom
// and reach the kernel's sendfile path: the bytes never enter user space at
// all, which is what makes a large-object GET cheap.
//
// With verification on the bytes must pass through the hasher, so sendfile is
// out of reach and the copy is an explicit loop over a pooled buffer.
//
// The explicit loop matters, and the reason is not obvious. The natural
// spelling is io.CopyBuffer(w, reader, pooledBuf) -- but io.CopyBuffer *ignores
// the buffer it is handed* whenever the destination implements io.ReaderFrom,
// and delegates to that instead. The chain here ends at net/http's response
// writer, which then allocates its own 32 KiB buffer per call. A CPU profile of
// a saturated 64 KiB GET workload showed that allocation at 3.6% of total CPU:
// the pooled buffer was taken, never used, and returned, while every request
// allocated afresh. See docs/perf-notes.md for what that did and did not buy.
func (o *Object) WriteTo(w io.Writer) (int64, error) {
	if o.verify {
		buf := getBuf()
		defer putBuf(buf)
		return o.copyVerified(w, *buf)
	}
	n, err := io.Copy(w, o.f)
	o.bytesServed += n
	return n, err
}

// copyVerified is io.Copy's loop written out, so that the pooled buffer is
// actually the buffer used.
func (o *Object) copyVerified(w io.Writer, buf []byte) (int64, error) {
	var written int64
	for {
		nr, rerr := o.f.Read(buf)
		if nr > 0 {
			if _, herr := o.hasher.Write(buf[:nr]); herr != nil {
				return written, herr
			}
			nw, werr := w.Write(buf[:nr])
			written += int64(nw)
			o.bytesServed += int64(nw)
			if werr != nil {
				return written, werr
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return written, nil
			}
			return written, rerr
		}
	}
}

// File exposes the underlying handle for http.ServeContent, which needs a
// ReadSeeker. Callers must not close it; Close on the Object does that.
func (o *Object) File() *os.File { return o.f }

// Verify checks the digest accumulated during reading. It is only meaningful
// after the whole object has been read, and only when verification was on.
func (o *Object) Verify() error {
	if !o.verify {
		return nil
	}
	sum, ok := o.hasher.(interface{ Sum([]byte) []byte })
	if !ok {
		return nil
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if got != o.Key {
		o.store.stats.VerifyFailures.Add(1)
		return &DigestMismatchError{Want: o.Key, Got: got, Size: o.bytesServed}
	}
	return nil
}

// Close releases the file handle and records bytes served.
func (o *Object) Close() error {
	var err error
	o.closeOnce.Do(func() {
		o.store.stats.BytesOut.Add(o.bytesServed)
		err = o.f.Close()
	})
	return err
}

// Get opens a stored object.
//
// The size recorded at open time is what the caller declares as Content-Length.
// If the file is truncated between the stat and the read the copy ends short,
// and the HTTP layer's own accounting catches it: a response that promised N
// bytes and delivered fewer is a protocol error the client will see, rather
// than silently short data.
func (s *Store) Get(ns Namespace, key string) (*Object, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if !ns.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrBadNamespace, ns)
	}
	if err := ValidateKey(key); err != nil {
		return nil, err
	}

	_, path := s.ObjectPath(ns, key)
	f, err := os.Open(path)
	if err != nil {
		s.stats.Gets.Add(1)
		s.stats.GetMisses.Add(1)
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		s.stats.Gets.Add(1)
		return nil, fmt.Errorf("stat open object: %w", err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		s.stats.Gets.Add(1)
		s.stats.GetMisses.Add(1)
		return nil, ErrNotFound
	}

	s.stats.Gets.Add(1)
	if s.index != nil {
		s.index.Touch(ns, key)
	}
	// Only CAS objects can be verified on read: an AC entry's key is a hash of
	// the action, not of the bytes, so there is nothing to compare against.
	verify := s.verifyReads && ns.ContentAddressed()
	o := &Object{
		Stat:   Stat{Key: key, Namespace: ns, Size: info.Size(), ModTime: info.ModTime()},
		f:      f,
		verify: verify,
		store:  s,
	}
	if verify {
		o.hasher = sha256.New()
	}
	return o, nil
}

// Delete removes an object.
//
// Deleting something that is not there is not an error: eviction and repair
// race with each other and with clients, and a double delete is the normal
// outcome of that race, not a fault.
//
// There is deliberately no directory fsync here, and the asymmetry with Put is
// the point. Publication has to be durable because the client was told the
// object exists (ADR-0003). A *deletion* does not: if the machine loses power
// after the unlink but before the directory entry reaches stable storage, the
// file reappears, startup reconciliation re-adopts it, and the evictor removes
// it again. Nothing is lost and nothing is wrong -- the only cost is a little
// disk for a little while.
//
// Paying for that guarantee anyway cost an order of magnitude. On the
// development host an fsync costs about 1.1 ms
// (bench/results/2026-09-20-yutongzhao/device-baseline.json), and one per
// deletion held eviction to roughly 190 objects per second -- below the ingest
// rate, so the quota simply stopped being enforced under load. See
// docs/perf-notes.md and docs/bugs.md entry 8.
func (s *Store) Delete(ns Namespace, key string) error {
	if !ns.Valid() {
		return fmt.Errorf("%w: %q", ErrBadNamespace, ns)
	}
	if err := ValidateKey(key); err != nil {
		return err
	}
	dir, path := s.ObjectPath(ns, key)

	info, statErr := os.Stat(path)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("delete object: %w", err)
	}
	if statErr == nil {
		s.stats.ObjectsStored.Add(-1)
		s.stats.BytesStored.Add(-info.Size())
	}
	s.stats.Deletes.Add(1)
	if s.index != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.index.Remove(ctx, ns, key); err != nil && !errors.Is(err, ErrClosed) {
			s.log.Warn("file deleted but index row remains; reconciliation will clear it",
				slog.String("ns", ns.String()), slog.String("key", key),
				slog.String("err", err.Error()))
		}
	}
	_ = dir
	return nil
}

// DeleteFile unlinks an object without touching the index.
//
// The evictor uses it so that a batch of deletions can share one index
// transaction instead of paying for one per object. Anything else should use
// Delete, which keeps the two in step.
func (s *Store) DeleteFile(ns Namespace, key string) (int64, error) {
	if !ns.Valid() {
		return 0, fmt.Errorf("%w: %q", ErrBadNamespace, ns)
	}
	if err := ValidateKey(key); err != nil {
		return 0, err
	}
	_, path := s.ObjectPath(ns, key)
	info, statErr := os.Stat(path)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("delete object: %w", err)
	}
	var size int64
	if statErr == nil {
		size = info.Size()
		s.stats.ObjectsStored.Add(-1)
		s.stats.BytesStored.Add(-size)
	}
	s.stats.Deletes.Add(1)
	return size, nil
}

// Walk visits every stored object. It is used by the startup reconciliation and
// by the repair auditor, both of which must see the disk rather than an index
// that may disagree with it.
func (s *Store) Walk(ns Namespace, fn func(Stat) error) error {
	if !ns.Valid() {
		return fmt.Errorf("%w: %q", ErrBadNamespace, ns)
	}
	nsRoot := filepath.Join(s.root, string(ns))
	return filepath.WalkDir(nsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if ValidateKey(name) != nil {
			// Not an object: a stray temp file, an editor backup, anything.
			// Walk reports objects, so skip silently rather than fail the sweep.
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return fn(Stat{Key: name, Namespace: ns, Size: info.Size(), ModTime: info.ModTime()})
	})
}
