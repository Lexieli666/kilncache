// Package repair implements the bounded background worker pool that audits
// locally held objects, detects retained objects whose other replica is
// missing, and recreates the missing copy.
//
// Repair is what makes the replication factor hold over time rather than only
// at the moment of a write. Three things break it after the fact, and none of
// them is detectable at write time:
//
//   - a node was down when the object was written, so the write was refused,
//     but the coordinator's own copy is still there;
//   - a peer acknowledged a write it did not persist (see
//     cluster.TestTruncatingPeerIsAcceptedThenDetectedByRepair);
//   - a holder evicted its copy under quota pressure while the other holder
//     kept theirs.
package repair

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lexieli666/kilncache/internal/cluster"
	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// Task is one object that may need a copy recreated.
type Task struct {
	Namespace storage.Namespace
	Key       string
	Size      int64
}

// Stats counts repair work. Every number the chaos report publishes about
// convergence comes from here.
type Stats struct {
	AuditRuns        atomic.Int64
	ObjectsAudited   atomic.Int64
	Enqueued         atomic.Int64
	DroppedFullQueue atomic.Int64
	Repaired         atomic.Int64
	AlreadyPresent   atomic.Int64
	Failed           atomic.Int64
	BytesRepaired    atomic.Int64
	NotOurs          atomic.Int64

	// DeclinedOverQuota counts copies a holder refused because it is already
	// above its own high-water mark. It is not a failure: it is what stops
	// repair and eviction chasing each other under a tight quota.
	DeclinedOverQuota atomic.Int64
}

// isOverQuota reports whether a peer declined a repair write for lack of room.
//
// The peer answers 507 Insufficient Storage, which the peer client surfaces as
// a PeerError carrying that status. Matching on the status rather than on
// message text keeps this from breaking when the wording changes.
func isOverQuota(err error) bool {
	var pe *cluster.PeerError
	if errors.As(err, &pe) {
		return pe.Status == http.StatusInsufficientStorage
	}
	return errors.Is(err, protocol.ErrOverQuota)
}

// Snapshot is a JSON-friendly copy.
type Snapshot struct {
	AuditRuns         int64 `json:"audit_runs"`
	ObjectsAudited    int64 `json:"objects_audited"`
	Enqueued          int64 `json:"enqueued"`
	DroppedFullQueue  int64 `json:"dropped_full_queue"`
	Repaired          int64 `json:"repaired"`
	AlreadyPresent    int64 `json:"already_present"`
	Failed            int64 `json:"failed"`
	BytesRepaired     int64 `json:"bytes_repaired"`
	NotOurs           int64 `json:"not_ours"`
	DeclinedOverQuota int64 `json:"declined_over_quota"`
	QueueDepth        int   `json:"queue_depth"`
	QueueCapacity     int   `json:"queue_capacity"`
}

// Worker audits and repairs replicas.
type Worker struct {
	coord    *cluster.Coordinator
	log      *slog.Logger
	queue    chan Task
	nWorkers int

	// auditBudget bounds how many objects one audit pass examines.
	//
	// An audit HEADs a peer per object. On a million-object cache an unbounded
	// pass would issue a million requests to each peer and starve real traffic
	// for minutes -- repair that takes the cluster down is worse than the
	// missing replica it was fixing. Passes are bounded and resume from where
	// the last one stopped.
	auditBudget int

	// cursorNS and cursorKey are where the next audit pass resumes.
	cursorMu  sync.Mutex
	cursorNS  storage.Namespace
	cursorKey string

	stats Stats

	started atomic.Bool
	wg      sync.WaitGroup
	done    chan struct{}
	once    sync.Once
}

// Options configures a Worker.
type Options struct {
	Coordinator *cluster.Coordinator
	Logger      *slog.Logger
	Workers     int
	QueueSize   int
	AuditBudget int
}

const (
	defaultWorkers     = 4
	defaultQueueSize   = 1024
	defaultAuditBudget = 5000
)

// New builds a repair worker. Call Run to start it.
func New(opts Options) (*Worker, error) {
	if opts.Coordinator == nil {
		return nil, errors.New("repair: coordinator is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if opts.AuditBudget <= 0 {
		opts.AuditBudget = defaultAuditBudget
	}
	return &Worker{
		coord:       opts.Coordinator,
		log:         opts.Logger,
		queue:       make(chan Task, opts.QueueSize),
		nWorkers:    opts.Workers,
		auditBudget: opts.AuditBudget,
		done:        make(chan struct{}),
	}, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Snapshot copies the counters.
func (w *Worker) Snapshot() Snapshot {
	return Snapshot{
		AuditRuns:         w.stats.AuditRuns.Load(),
		ObjectsAudited:    w.stats.ObjectsAudited.Load(),
		Enqueued:          w.stats.Enqueued.Load(),
		DroppedFullQueue:  w.stats.DroppedFullQueue.Load(),
		Repaired:          w.stats.Repaired.Load(),
		AlreadyPresent:    w.stats.AlreadyPresent.Load(),
		Failed:            w.stats.Failed.Load(),
		BytesRepaired:     w.stats.BytesRepaired.Load(),
		NotOurs:           w.stats.NotOurs.Load(),
		DeclinedOverQuota: w.stats.DeclinedOverQuota.Load(),
		QueueDepth:        len(w.queue),
		QueueCapacity:     cap(w.queue),
	}
}

// Run starts the worker pool and the audit loop, and blocks until ctx is
// cancelled and every worker has finished its current task.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if w.started.Swap(true) {
		return
	}
	defer close(w.done)

	if interval <= 0 {
		interval = 60 * time.Second
	}

	for i := 0; i < w.nWorkers; i++ {
		w.wg.Add(1)
		go func(id int) {
			defer w.wg.Done()
			w.workerLoop(ctx, id)
		}(i)
	}

	w.log.Info("repair worker started",
		slog.Int("workers", w.nWorkers),
		slog.Int("queue", cap(w.queue)),
		slog.Duration("interval", interval),
		slog.Int("audit_budget", w.auditBudget))

	// The wait between passes is adaptive.
	//
	// A fixed interval makes convergence time a function of the wrong thing.
	// A pass is bounded by the audit budget and by the queue, so after a node
	// returns there is far more to do than one pass can carry -- and sleeping
	// the full interval between passes makes repair rate = queue depth per
	// interval, which on a cache of any size is minutes to converge.
	//
	// So: when a pass ends with work still pending, the next one starts almost
	// immediately; when a pass finds nothing left to do, the full interval
	// applies. Repair is then fast when it matters and idle when it does not.
	next := time.NewTimer(interval)
	defer next.Stop()

	for {
		select {
		case <-ctx.Done():
			// Close the queue so workers drain what is already accepted and
			// then exit. Cancelling without closing would leave a worker
			// blocked on a receive and the pool never shutting down cleanly.
			close(w.queue)
			w.wg.Wait()
			w.log.Info("repair worker stopped", slog.Any("stats", w.Snapshot()))
			return
		case <-next.C:
			pending, err := w.auditPass(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				w.log.Error("repair audit failed", slog.String("err", err.Error()))
			}
			wait := interval
			if pending {
				wait = backlogInterval
			}
			next.Reset(wait)
		}
	}
}

// backlogInterval is how long the auditor waits before the next pass when it
// knows there is more to examine. Long enough that the worker pool drains the
// queue it just filled, short enough that convergence is measured in seconds.
const backlogInterval = 250 * time.Millisecond

// Wait blocks until Run has returned.
func (w *Worker) Wait() { <-w.done }

func (w *Worker) workerLoop(ctx context.Context, id int) {
	for task := range w.queue {
		if ctx.Err() != nil {
			// Drain without working: the node is shutting down and a repair
			// started now would be cancelled mid-transfer anyway.
			continue
		}
		w.repair(ctx, task, id)
	}
}

// Audit runs one bounded audit pass.
func (w *Worker) Audit(ctx context.Context) error {
	_, err := w.auditPass(ctx)
	return err
}

// auditPass examines a bounded slice of the locally indexed objects and
// enqueues them for checking. It reports whether work remains.
//
// The index is read in pages rather than as one streamed cursor. A single
// iterator over the whole table would be held open for the duration of the
// pass, pinning a database connection while requests are trying to write to the
// same index -- which is precisely how the auditor came to starve eviction
// badly enough that the quota stopped being enforced (docs/bugs.md, entry 7).
// Each page is a short keyset query that releases its connection immediately.
//
// The cursor resumes where the previous pass stopped, so a cache larger than
// the budget is covered over successive passes instead of re-checking the same
// prefix forever.
func (w *Worker) auditPass(ctx context.Context) (pending bool, _ error) {
	idx := w.coord.Local().Index()
	if idx == nil {
		return false, nil
	}
	ring := w.coord.Ring()
	rf := w.coord.ReplicaCount()
	if ring.Size() < 2 || rf < 2 {
		// Nothing to repair in a single-node cluster, and nothing to compare
		// against with replica count 1.
		return false, nil
	}

	w.stats.AuditRuns.Add(1)

	w.cursorMu.Lock()
	curNS, curKey := w.cursorNS, w.cursorKey
	w.cursorMu.Unlock()

	examined := 0
	for examined < w.auditBudget {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		page, err := idx.PageAfter(ctx, curNS, curKey, auditPageSize)
		if err != nil {
			return false, err
		}
		if len(page) == 0 {
			// End of the index: the next pass starts from the beginning. A
			// sweep that enqueued work is worth following up promptly, because
			// the objects it queued may have siblings behind them.
			w.setCursor("", "")
			return w.pendingAfterFullSweep(), nil
		}

		for _, e := range page {
			examined++
			w.stats.ObjectsAudited.Add(1)

			// Only repair what this node is supposed to hold. An object that is
			// here because placement changed, or because an old repair put it
			// here, is served happily but is not this node's responsibility.
			if !ring.SelfHolds(e.Key, rf) {
				w.stats.NotOurs.Add(1)
				curNS, curKey = e.Namespace, e.Key
				continue
			}
			if !w.enqueue(Task{Namespace: e.Namespace, Key: e.Key, Size: e.Size}) {
				// The queue is full. Stop here and leave the cursor on the last
				// object that was actually accepted, so the next pass resumes
				// at this one rather than skipping past it.
				//
				// Advancing past dropped objects is the subtler bug: the pass
				// still appears to make progress, while the objects behind the
				// full queue are not examined again until the cursor wraps all
				// the way round. Convergence time then depends on total cache
				// size rather than on how much is actually broken.
				w.setCursor(curNS, curKey)
				return true, nil
			}
			curNS, curKey = e.Namespace, e.Key
		}
	}

	w.setCursor(curNS, curKey)
	return true, nil
}

// auditPageSize is how many index rows one keyset query returns. Small enough
// that a connection is never held for long, large enough that a full sweep is
// not dominated by query overhead.
const auditPageSize = 512

func (w *Worker) setCursor(ns storage.Namespace, key string) {
	w.cursorMu.Lock()
	w.cursorNS, w.cursorKey = ns, key
	w.cursorMu.Unlock()
}

// pendingAfterFullSweep reports whether a completed sweep left work worth
// following up promptly.
func (w *Worker) pendingAfterFullSweep() bool {
	return len(w.queue) > 0
}

// enqueue offers a task without blocking, and reports whether it was accepted.
//
// Never blocking is the point: a blocked audit loop holds the index iterator
// open across a transaction while workers are trying to write to the same
// database, and the deadlock that follows is not obvious from the outside. The
// caller handles a refusal by stopping the pass rather than by skipping the
// object, so nothing is lost -- the next pass resumes exactly here.
func (w *Worker) enqueue(task Task) bool {
	select {
	case w.queue <- task:
		w.stats.Enqueued.Add(1)
		return true
	default:
		w.stats.DroppedFullQueue.Add(1)
		return false
	}
}

// Enqueue offers a task from outside the audit loop, and reports whether the
// queue accepted it.
func (w *Worker) Enqueue(task Task) bool { return w.enqueue(task) }

// repair ensures every holder of an object has a copy.
func (w *Worker) repair(ctx context.Context, task Task, workerID int) {
	ring := w.coord.Ring()
	rf := w.coord.ReplicaCount()
	self := ring.Self()

	local := w.coord.Local()
	st, err := local.Stat(task.Namespace, task.Key)
	if err != nil {
		// Gone since the audit saw it -- evicted, most likely. Not a failure:
		// eviction racing repair is the normal state of a cache under quota
		// pressure, not a fault.
		return
	}

	missing := 0
	for _, m := range ring.Holders(task.Key, rf) {
		if m.Name == self {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}

		_, err := w.coord.Peers().Stat(ctx, m, task.Namespace, task.Key, protocol.HopRead)
		if err == nil {
			w.stats.AlreadyPresent.Add(1)
			continue
		}
		if !errors.Is(err, cluster.ErrPeerNotFound) {
			// Unreachable, not absent. Sending a copy to a node that is down
			// would fail anyway, and counting it as a repair failure would make
			// the metric say "repair is broken" when the truth is "a node is
			// down", which the node-down metric already says.
			w.log.Debug("repair: holder unreachable, will retry on the next pass",
				slog.String("key", short(task.Key)),
				slog.String("peer", m.Name),
				slog.String("err", err.Error()))
			continue
		}

		missing++
		if err := w.sendCopy(ctx, m, task, st.Size); err != nil {
			if isOverQuota(err) {
				// The target is above its own high-water mark and has declined
				// the copy. That is the system working, not a failure: it is
				// what stops repair and eviction chasing each other.
				w.stats.DeclinedOverQuota.Add(1)
				w.log.Debug("repair: holder declined a copy, it is over quota",
					slog.String("key", short(task.Key)),
					slog.String("peer", m.Name))
				continue
			}
			w.stats.Failed.Add(1)
			w.log.Warn("repair: could not recreate a missing copy",
				slog.String("ns", task.Namespace.String()),
				slog.String("key", short(task.Key)),
				slog.String("peer", m.Name),
				slog.Int("worker", workerID),
				slog.String("err", err.Error()))
			continue
		}
		w.stats.Repaired.Add(1)
		w.stats.BytesRepaired.Add(st.Size)
		w.log.Info("repair: recreated a missing copy",
			slog.String("ns", task.Namespace.String()),
			slog.String("key", short(task.Key)),
			slog.String("peer", m.Name),
			slog.Int64("size", st.Size))
	}
	_ = missing
}

func (w *Worker) sendCopy(ctx context.Context, target cluster.Member, task Task, size int64) error {
	obj, err := w.coord.Local().Get(task.Namespace, task.Key)
	if err != nil {
		return fmt.Errorf("reopen local copy: %w", err)
	}
	defer obj.Close()

	// HopRepair is terminal, like HopReplica: the receiving node stores the
	// copy and forwards nothing, so repair cannot start a forwarding chain. It
	// is a separate hop only so that a node already over its high-water mark
	// can decline it -- see protocol.HopRepair.
	return w.coord.Peers().Put(ctx, target, task.Namespace, task.Key, obj.File(), size, protocol.HopRepair)
}

func short(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

// Close is idempotent and safe to call without Run.
func (w *Worker) Close() {
	w.once.Do(func() {
		if !w.started.Load() {
			close(w.done)
		}
	})
}
