package storage

import (
	"container/heap"
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// EvictionPolicy scores objects for eviction.
//
// Plain LRU is the obvious choice and is subtly wrong for a build cache. Object
// sizes here span five orders of magnitude -- a symbol file is a few hundred
// bytes, a static library is tens of megabytes -- and under LRU a single
// 64 MiB object occupies the space of ten thousand small ones while costing
// exactly as much to keep. When the quota binds, that trade is almost always
// the wrong way round: the ten thousand small objects represent ten thousand
// actions that would otherwise be re-executed.
//
// So the score is recency with a size penalty:
//
//	score = lastAccess - SizeWeight * log2(1 + size/SizeUnit)
//
// expressed in time. A 64 MiB object with SizeUnit 64 KiB and SizeWeight one
// hour is treated as about ten hours colder than it really is; a 64 KiB object
// as one hour colder. The lowest score is evicted first.
//
// log2 rather than a linear penalty on purpose: linear weighting makes large
// objects effectively uncacheable, and the point is to break ties among objects
// of *similar* age, not to refuse to store big things.
type EvictionPolicy struct {
	// SizeWeight is how much older one doubling of size makes an object look.
	SizeWeight time.Duration
	// SizeUnit is the size at which the penalty starts counting.
	SizeUnit int64
}

// DefaultEvictionPolicy is one hour per doubling above 64 KiB.
//
// The value is a judgement call, not a measurement, and is stated as such: it
// says that between two objects an hour apart in age, the larger by 2x should
// go first. `make quota-report` measures the hit-rate consequence against a
// recorded Bazel workload so the number can be revised against evidence rather
// than taste.
func DefaultEvictionPolicy() EvictionPolicy {
	return EvictionPolicy{SizeWeight: time.Hour, SizeUnit: 64 << 10}
}

// Score returns the eviction score; lower is evicted sooner.
func (p EvictionPolicy) Score(e Entry) int64 {
	unit := p.SizeUnit
	if unit <= 0 {
		unit = 64 << 10
	}
	ratio := float64(e.Size) / float64(unit)
	penalty := math.Log2(1+ratio) * float64(p.SizeWeight)
	return e.LastAccess.UnixNano() - int64(penalty)
}

// candidate pairs an entry with its score.
type candidate struct {
	entry Entry
	score int64
}

// candidateHeap is a min-heap on score: Pop yields the best victim.
type candidateHeap []candidate

func (h candidateHeap) Len() int           { return len(h) }
func (h candidateHeap) Less(i, j int) bool { return h[i].score < h[j].score }
func (h candidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(x any) {
	c, ok := x.(candidate)
	if !ok {
		return
	}
	*h = append(*h, c)
}
func (h *candidateHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// Quota bounds how much disk this node uses.
type Quota struct {
	// MaxBytes is the ceiling.
	MaxBytes int64
	// HighWater is the fraction of MaxBytes at which eviction starts.
	HighWater float64
	// LowWater is the fraction eviction drains down to.
	//
	// Two marks, not one. With a single threshold, every write past the limit
	// triggers an eviction of exactly one object, so a full cache does a
	// database transaction and an unlink on the write path forever. Draining to
	// a low-water mark amortises that: eviction runs rarely and does real work
	// when it does.
	LowWater float64
}

// Valid checks the quota is usable.
func (q Quota) Valid() error {
	if q.MaxBytes <= 0 {
		return fmt.Errorf("quota: max bytes must be positive, got %d", q.MaxBytes)
	}
	if q.HighWater <= 0 || q.HighWater > 1 {
		return fmt.Errorf("quota: high water must be in (0,1], got %v", q.HighWater)
	}
	if q.LowWater <= 0 || q.LowWater >= q.HighWater {
		return fmt.Errorf("quota: low water must be in (0,high), got low=%v high=%v", q.LowWater, q.HighWater)
	}
	return nil
}

// HighBytes is the byte count at which eviction begins.
func (q Quota) HighBytes() int64 { return int64(float64(q.MaxBytes) * q.HighWater) }

// LowBytes is the byte count eviction drains to.
func (q Quota) LowBytes() int64 { return int64(float64(q.MaxBytes) * q.LowWater) }

// Evictor enforces the quota.
//
// It runs as a single goroutine woken by writes, never inline on the write
// path. Evicting synchronously would make the latency of an unlucky PUT include
// a scan, a batch of unlinks and a transaction -- and would make that PUT's
// latency depend on how full the disk happens to be, which is the least
// predictable tail latency a cache can have.
type Evictor struct {
	store  *Store
	index  *Index
	quota  Quota
	policy EvictionPolicy
	log    *slog.Logger

	wake   chan struct{}
	done   chan struct{}
	closed atomic.Bool
	once   sync.Once

	// batchSize is how many candidates to rank at once. Larger batches make
	// better decisions and cost more memory per sweep.
	batchSize int

	stats EvictorStats
}

// EvictorStats counts eviction work.
type EvictorStats struct {
	Sweeps         atomic.Int64
	Evicted        atomic.Int64
	BytesReclaimed atomic.Int64
	Failures       atomic.Int64
	Overshoot      atomic.Int64
}

// EvictorSnapshot is a JSON-friendly copy.
type EvictorSnapshot struct {
	Sweeps         int64 `json:"sweeps"`
	Evicted        int64 `json:"evicted"`
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	Failures       int64 `json:"failures"`
	Overshoot      int64 `json:"overshoot"`
}

// Snapshot copies the counters.
func (e *Evictor) Snapshot() EvictorSnapshot {
	return EvictorSnapshot{
		Sweeps:         e.stats.Sweeps.Load(),
		Evicted:        e.stats.Evicted.Load(),
		BytesReclaimed: e.stats.BytesReclaimed.Load(),
		Failures:       e.stats.Failures.Load(),
		Overshoot:      e.stats.Overshoot.Load(),
	}
}

// EvictorOptions configures an Evictor.
type EvictorOptions struct {
	Store     *Store
	Index     *Index
	Quota     Quota
	Policy    EvictionPolicy
	Logger    *slog.Logger
	BatchSize int
}

const defaultEvictBatch = 512

// NewEvictor builds an Evictor. Call Run to start it.
func NewEvictor(opts EvictorOptions) (*Evictor, error) {
	if opts.Store == nil || opts.Index == nil {
		return nil, fmt.Errorf("evictor: store and index are required")
	}
	if err := opts.Quota.Valid(); err != nil {
		return nil, err
	}
	if opts.Policy.SizeUnit == 0 && opts.Policy.SizeWeight == 0 {
		opts.Policy = DefaultEvictionPolicy()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultEvictBatch
	}
	return &Evictor{
		store:     opts.Store,
		index:     opts.Index,
		quota:     opts.Quota,
		policy:    opts.Policy,
		log:       opts.Logger,
		batchSize: opts.BatchSize,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}, nil
}

// Quota returns the quota this evictor enforces.
func (e *Evictor) Quota() Quota { return e.quota }

// Wake asks for a sweep. It never blocks: the channel has room for one pending
// request, and a second request while one is queued is redundant.
func (e *Evictor) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run sweeps on wake-up and on a ticker until ctx is cancelled.
//
// The ticker matters even though writes wake the evictor: a node that is only
// read from still has to drain after a restart that raised its usage, and a
// node whose wake-up was coalesced away must not stay over quota indefinitely.
func (e *Evictor) Run(ctx context.Context, interval time.Duration) {
	defer close(e.done)
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Sweep once at startup: usage after reconciliation may already be over.
	e.sweepLogged(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
			e.sweepLogged(ctx)
		case <-ticker.C:
			e.sweepLogged(ctx)
		}
	}
}

// Wait blocks until Run has returned.
func (e *Evictor) Wait() { <-e.done }

func (e *Evictor) sweepLogged(ctx context.Context) {
	n, freed, err := e.Sweep(ctx)
	if err != nil {
		e.stats.Failures.Add(1)
		e.log.Error("eviction sweep failed", slog.String("err", err.Error()))
		return
	}
	if n > 0 {
		e.log.Info("evicted",
			slog.Int("objects", n),
			slog.Int64("bytes", freed),
			slog.Int64("high_water_bytes", e.quota.HighBytes()),
			slog.Int64("low_water_bytes", e.quota.LowBytes()))
	}
}

// Sweep evicts until usage is at or below the low-water mark. It returns the
// number of objects removed and the bytes reclaimed.
func (e *Evictor) Sweep(ctx context.Context) (int, int64, error) {
	if e.closed.Load() {
		return 0, 0, ErrClosed
	}
	// Queued reads carry the recency information eviction is about to act on.
	// Sweeping without flushing them would evict objects that were read
	// seconds ago as though they were cold.
	if err := e.index.FlushTouches(ctx); err != nil {
		return 0, 0, fmt.Errorf("flush touches before sweep: %w", err)
	}

	_, used, err := e.index.Totals(ctx)
	if err != nil {
		return 0, 0, err
	}
	if used <= e.quota.HighBytes() {
		return 0, 0, nil
	}
	e.stats.Sweeps.Add(1)

	target := e.quota.LowBytes()
	mustFree := used - target

	var evicted int
	var freed int64

	for freed < mustFree {
		if err := ctx.Err(); err != nil {
			return evicted, freed, err
		}
		batch, err := e.index.ColdestN(ctx, e.batchSize)
		if err != nil {
			return evicted, freed, err
		}
		if len(batch) == 0 {
			// Nothing left to evict and still over target: the quota is smaller
			// than the live set. Report it rather than spinning.
			e.stats.Overshoot.Add(1)
			e.log.Warn("cannot reach the low-water mark; the index is empty but usage is still above target",
				slog.Int64("used", used-freed), slog.Int64("target", target))
			break
		}

		h := make(candidateHeap, 0, len(batch))
		for _, entry := range batch {
			h = append(h, candidate{entry: entry, score: e.policy.Score(entry)})
		}
		heap.Init(&h)

		// Unlink the files first, then remove all their index rows in one
		// transaction. A per-object commit makes the index, not the disk, the
		// limit on how fast a sweep can run.
		refs := make([]Ref, 0, h.Len())
		progressed := false
		for h.Len() > 0 && freed < mustFree {
			c, ok := heap.Pop(&h).(candidate)
			if !ok {
				break
			}
			if _, err := e.store.DeleteFile(c.entry.Namespace, c.entry.Key); err != nil {
				e.stats.Failures.Add(1)
				e.log.Warn("could not evict an object",
					slog.String("ns", c.entry.Namespace.String()),
					slog.String("key", c.entry.Key),
					slog.String("err", err.Error()))
				continue
			}
			refs = append(refs, Ref{Namespace: c.entry.Namespace, Key: c.entry.Key})
			evicted++
			freed += c.entry.Size
			progressed = true
		}
		if len(refs) > 0 {
			if err := e.index.RemoveBatch(ctx, refs); err != nil {
				// The files are gone; the rows are not. Reconciliation clears
				// them at the next startup, and until then the quota
				// over-counts, which is the safe direction to be wrong in.
				e.stats.Failures.Add(1)
				e.log.Error("evicted files but could not clear their index rows",
					slog.Int("rows", len(refs)), slog.String("err", err.Error()))
			}
		}
		if !progressed {
			// Every candidate in the batch failed to evict. Retrying the same
			// batch would spin.
			e.stats.Overshoot.Add(1)
			break
		}
	}

	e.stats.Evicted.Add(int64(evicted))
	e.stats.BytesReclaimed.Add(freed)
	return evicted, freed, nil
}

// Close stops accepting sweeps.
func (e *Evictor) Close() {
	e.once.Do(func() { e.closed.Store(true) })
}
