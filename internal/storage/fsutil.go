package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
)

// bufSize is the fixed per-request copy buffer.
//
// This constant is the entire memory bound on the write and read paths: an
// object of any size moves through a buffer of exactly this many bytes, so
// resident memory is a function of concurrency, not of object size. The
// benchmark that would falsify that claim measures RSS while streaming 8 MiB
// and 64 MiB objects and asserts the two are within noise of each other.
//
// 256 KiB rather than 32 KiB (io.Copy's default) because at 32 KiB an 8 MiB
// object costs 256 read/write syscall pairs; at 256 KiB it costs 32.
const bufSize = 256 << 10

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, bufSize)
		return &b
	},
}

func getBuf() *[]byte {
	b, _ := bufPool.Get().(*[]byte)
	return b
}
func putBuf(b *[]byte) { bufPool.Put(b) }

// syncDir fsyncs a directory so that a rename into it is durable.
//
// This is the call that ADR-0003 is about. A rename() is atomic with respect to
// concurrent readers the moment it returns, but it is not *durable* until the
// directory's own metadata reaches stable storage. Without this, a power loss
// between the rename and the filesystem's next journal commit can leave the
// object's data on disk with no directory entry pointing at it: the file is
// gone, while anything that recorded the write believes it exists.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Some filesystems (and some container storage drivers) return EINVAL
		// for fsync on a directory. That is a property of the filesystem, not a
		// bug here, and failing the write would make the cache unusable there.
		// Report it once, at the call site's discretion, rather than pretending
		// the durability guarantee holds.
		if isNotSupported(err) {
			return errDirSyncUnsupported
		}
		return fmt.Errorf("fsync dir: %w", err)
	}
	return nil
}

// errDirSyncUnsupported signals that the filesystem does not support fsync on a
// directory. Callers downgrade the durability claim rather than fail the write.
var errDirSyncUnsupported = errors.New("directory fsync is not supported by this filesystem")

// isNotSupported distinguishes "this filesystem cannot fsync a directory" from
// "this fsync failed". The first is a capability gap to be reported once; the
// second is an I/O error that must fail the write.
func isNotSupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, fs.ErrInvalid)
}

// mkdirAllCached creates a shard directory, remembering the ones it has already
// created.
//
// Under a saturated PUT workload the same few hundred shard directories are
// touched over and over. MkdirAll on an existing path still costs a stat per
// level; the cache turns that into a map lookup. It is a plain map behind a
// mutex rather than a sync.Map because the read path is not the hot one here —
// the write already costs an open, a stream, and an fsync.
type dirCache struct {
	mu   sync.RWMutex
	seen map[string]struct{}
}

func newDirCache() *dirCache {
	return &dirCache{seen: make(map[string]struct{}, 512)}
}

func (c *dirCache) ensure(dir string) error {
	c.mu.RLock()
	_, ok := c.seen[dir]
	c.mu.RUnlock()
	if ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create shard dir: %w", err)
	}
	c.mu.Lock()
	c.seen[dir] = struct{}{}
	c.mu.Unlock()
	return nil
}
