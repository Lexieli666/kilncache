// Package node assembles a complete KilnCache node from its subsystems.
//
// It exists so that there is exactly one definition of what a node *is*.
// Without it, cmd/kilncache would wire the store, ring and repair worker
// together one way and the integration tests would wire them together another,
// and the tests would be exercising an arrangement that is not shipped.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Lexieli666/kilncache/internal/buildinfo"
	"github.com/Lexieli666/kilncache/internal/cluster"
	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/httpapi"
	"github.com/Lexieli666/kilncache/internal/metrics"
	"github.com/Lexieli666/kilncache/internal/repair"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// Node owns every long-lived resource a running node holds, and the order they
// are torn down in.
type Node struct {
	cfg    config.Config
	log    *slog.Logger
	health *httpapi.Health

	store   *storage.Store
	ring    *cluster.Ring
	peers   cluster.PeerClient
	coord   *cluster.Coordinator
	evictor *storage.Evictor
	repair  *repair.Worker
	metrics *metrics.Metrics
	handler http.Handler
	server  *httpapi.Server

	// background is cancelled to stop the evictor, the repair worker and the
	// touch flusher. It is separate from the request context so that shutdown
	// stops accepting first and only then stops the background work that
	// in-flight requests may still depend on.
	bgCancel context.CancelFunc
	bgDone   sync.WaitGroup
}

// Options allows tests to substitute a peer client that can be made to fail,
// hang, or truncate on demand. Production passes nothing and gets the HTTP one.
type Options struct {
	PeerClient cluster.PeerClient
}

// New builds a node: opens storage, builds the ring, wires handlers, and binds
// the listener.
//
// Readiness is announced by Run, not here. Opening the store reconciles the
// temp directory against a possible crash, and a node that advertised itself
// before that finished would be offering a cache whose disk it has not checked.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, opts ...Options) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}

	health := httpapi.NewHealth()
	health.SetNotReady("opening object store")

	health.SetNotReady("reconciling the index against disk")

	store, err := storage.Open(storage.Options{
		Root:           cfg.AbsDataDir(),
		MaxObjectBytes: cfg.MaxObjectBytes,
		VerifyReads:    cfg.VerifyReads,
		Logger:         log,
	})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	n := &Node{cfg: cfg, log: log, health: health, store: store}

	ring, err := cluster.New(cfg.Peers, cfg.NodeName)
	if err != nil {
		_ = n.closeAll()
		return nil, fmt.Errorf("build ring: %w", err)
	}
	n.ring = ring

	peers := opt.PeerClient
	if peers == nil {
		peers = cluster.NewHTTPPeerClient(cfg.NodeName, cfg.PeerTimeout)
	}
	n.peers = peers

	coord, err := cluster.NewCoordinator(cluster.CoordinatorOptions{
		Ring:         ring,
		Local:        store,
		Peers:        peers,
		ReplicaCount: cfg.ReplicaCount,
		Logger:       log,
	})
	if err != nil {
		_ = n.closeAll()
		return nil, fmt.Errorf("build coordinator: %w", err)
	}
	n.coord = coord

	evictor, err := storage.NewEvictor(storage.EvictorOptions{
		Store: store,
		Index: store.Index(),
		Quota: storage.Quota{
			MaxBytes:  cfg.MaxBytes,
			HighWater: cfg.HighWater,
			LowWater:  cfg.LowWater,
		},
		Logger: log,
	})
	if err != nil {
		_ = n.closeAll()
		return nil, fmt.Errorf("build evictor: %w", err)
	}
	n.evictor = evictor
	store.AttachEvictor(evictor)

	// Let the coordinator decline repair writes when this node is already over
	// its high-water mark. Without it, eviction and repair chase each other:
	// this node evicts to get under quota, the other holder's auditor sees a
	// missing replica and sends it straight back, and the quota is never
	// enforced. See docs/bugs.md entry 7.
	idx := store.Index()
	coord.SetQuotaProbe(func(ctx context.Context) (bool, int64, int64) {
		if idx == nil {
			return false, 0, 0
		}
		_, used, err := idx.Totals(ctx)
		if err != nil {
			// Unknown usage: accept the write. Refusing on an unreadable index
			// would turn a metadata hiccup into a cluster-wide repair outage.
			return false, 0, 0
		}
		high := evictor.Quota().HighBytes()
		return used > high, used, high
	})

	if cfg.RepairWorkers > 0 {
		rw, err := repair.New(repair.Options{
			Coordinator: coord,
			Logger:      log,
			Workers:     cfg.RepairWorkers,
			QueueSize:   cfg.RepairQueue,
		})
		if err != nil {
			_ = n.closeAll()
			return nil, fmt.Errorf("build repair worker: %w", err)
		}
		n.repair = rw
	}

	log.Info("node assembled",
		slog.String("root", store.Root()),
		slog.Bool("verify_reads", cfg.VerifyReads),
		slog.Bool("dir_fsync_supported", store.DirSyncSupported()),
		slog.Int64("max_object_bytes", cfg.MaxObjectBytes),
		slog.Int("cluster_size", ring.Size()),
		slog.Int("replica_count", coord.ReplicaCount()),
		slog.Any("members", config.PeerNames(cfg.Peers)),
		slog.Int64("quota_bytes", cfg.MaxBytes),
		slog.Int64("high_water_bytes", evictor.Quota().HighBytes()),
		slog.Int64("low_water_bytes", evictor.Quota().LowBytes()),
		slog.Int("repair_workers", cfg.RepairWorkers),
	)

	n.metrics = metrics.New(cfg.NodeName)
	if err := n.metrics.RegisterSources(n.metricSources()); err != nil {
		_ = n.closeAll()
		return nil, fmt.Errorf("register metrics: %w", err)
	}

	router := httpapi.NewRouter(httpapi.RouterOptions{
		Node:    cfg.NodeName,
		Version: buildinfo.String(),
		Health:  health,
		Log:     log,
		DevMode: cfg.DevMode,
		Cache:   httpapi.NewCacheHandler(coord, log, cfg.NodeName, cfg.MaxObjectBytes, n.metrics),
		Metrics: n.metrics.Handler(),
		Stats:   n,
	})
	n.handler = router

	srv, err := httpapi.NewServer(cfg, log, health, router)
	if err != nil {
		_ = n.closeAll()
		return nil, err
	}
	n.server = srv

	_ = ctx
	return n, nil
}

// Addr returns the bound listen address.
func (n *Node) Addr() string { return n.server.Addr() }

// BaseURL returns the http:// URL a client should use to reach this node.
func (n *Node) BaseURL() string { return "http://" + n.Addr() }

// Name returns the node's cluster identity.
func (n *Node) Name() string { return n.cfg.NodeName }

// Store exposes the local object store, for tests and for the repair worker.
func (n *Node) Store() *storage.Store { return n.store }

// Coordinator exposes the placement and replication layer.
func (n *Node) Coordinator() *cluster.Coordinator { return n.coord }

// Ring exposes the placement function.
func (n *Node) Ring() *cluster.Ring { return n.ring }

// Health exposes the readiness signal.
func (n *Node) Health() *httpapi.Health { return n.health }

// Handler exposes the full middleware stack, for httptest-based tests.
func (n *Node) Handler() http.Handler { return n.handler }

// metricSources maps the counters the subsystems already keep onto Prometheus
// collectors.
//
// Reading the existing atomics rather than incrementing a parallel set of
// Prometheus counters means /stats and /metrics cannot disagree. When they do
// disagree during an incident, the time goes into deciding which to believe
// rather than into the incident.
func (n *Node) metricSources() []metrics.Source {
	store := n.store
	coord := n.coord
	evictor := n.evictor
	rw := n.repair
	idx := store.Index()

	src := []metrics.Source{
		{Name: "bytes_in_total", Help: "Object bytes received.", Counter: true,
			Value: func() float64 { return float64(store.Snapshot().BytesIn) }},
		{Name: "bytes_out_total", Help: "Object bytes served.", Counter: true,
			Value: func() float64 { return float64(store.Snapshot().BytesOut) }},
		{Name: "puts_total", Help: "Objects written to local disk.", Counter: true,
			Value: func() float64 { return float64(store.Snapshot().PutsOK) }},
		{Name: "puts_rejected_total", Help: "Writes rejected before publication.", Counter: true,
			Value: func() float64 { return float64(store.Snapshot().PutsRejected) }},
		{Name: "puts_already_stored_total", Help: "Writes that were no-ops.", Counter: true,
			Value: func() float64 { return float64(store.Snapshot().PutsAlreadyStored) }},
		{Name: "verify_failures_total",
			Help:    "Objects that failed digest verification while being served. Should be zero; anything else is local corruption.",
			Counter: true,
			Value:   func() float64 { return float64(store.Snapshot().VerifyFailures) }},

		// One help string for both series: Prometheus requires every series of
		// a metric to share it, and it has to describe the metric rather than
		// the particular label value.
		{Name: "hits_total", Help: "Reads served, by where the bytes came from.", Counter: true,
			Labels: map[string]string{"source": "local"},
			Value:  func() float64 { return float64(coord.Snapshot().LocalHits) }},
		{Name: "hits_total", Help: "Reads served, by where the bytes came from.", Counter: true,
			Labels: map[string]string{"source": "peer"},
			Value:  func() float64 { return float64(coord.Snapshot().PeerHits) }},
		{Name: "misses_total", Help: "Reads that found nothing anywhere.", Counter: true,
			Value: func() float64 { return float64(coord.Snapshot().Misses) }},
		{Name: "read_fallbacks_total", Help: "Reads served by a peer after the local copy was absent.", Counter: true,
			Value: func() float64 { return float64(coord.Snapshot().ReadFallbacks) }},
		{Name: "replication_attempts_total", Help: "Replica writes attempted against peers.", Counter: true,
			Value: func() float64 { return float64(coord.Snapshot().ReplicaWrites + coord.Snapshot().ReplicaFailures) }},
		{Name: "replication_failures_total", Help: "Replica writes that failed.", Counter: true,
			Value: func() float64 { return float64(coord.Snapshot().ReplicaFailures) }},
		{Name: "insufficient_replicas_total",
			Help:    "PUTs refused because the required number of copies could not be written.",
			Counter: true,
			Value:   func() float64 { return float64(coord.Snapshot().InsufficientAcks) }},
		{Name: "proxied_puts_total", Help: "PUTs this node forwarded to an owner.", Counter: true,
			Value: func() float64 { return float64(coord.Snapshot().ForwardedPuts) }},
		{Name: "repair_declined_over_quota_total",
			Help:    "Repair copies this node declined because it is above its high-water mark.",
			Counter: true,
			Value:   func() float64 { return float64(coord.Snapshot().RepairDeclined) }},

		{Name: "quota_bytes", Help: "Configured per-node disk quota.",
			Value: func() float64 { return float64(evictor.Quota().MaxBytes) }},
		{Name: "high_water_bytes", Help: "Usage at which eviction begins.",
			Value: func() float64 { return float64(evictor.Quota().HighBytes()) }},
		{Name: "low_water_bytes", Help: "Usage that eviction drains down to.",
			Value: func() float64 { return float64(evictor.Quota().LowBytes()) }},
		{Name: "evictions_total", Help: "Objects evicted to stay within the quota.", Counter: true,
			Value: func() float64 { return float64(evictor.Snapshot().Evicted) }},
		{Name: "evicted_bytes_total", Help: "Bytes reclaimed by eviction.", Counter: true,
			Value: func() float64 { return float64(evictor.Snapshot().BytesReclaimed) }},
		{Name: "eviction_sweeps_total", Help: "Eviction sweeps that found work to do.", Counter: true,
			Value: func() float64 { return float64(evictor.Snapshot().Sweeps) }},
		{Name: "eviction_failures_total", Help: "Objects eviction could not remove.", Counter: true,
			Value: func() float64 { return float64(evictor.Snapshot().Failures) }},
	}

	if idx != nil {
		// Usage is read from the index rather than counted incrementally,
		// because reconciliation can correct it and an incremental counter
		// would drift away from the truth after every unclean restart.
		src = append(src,
			metrics.Source{Name: "stored_bytes", Help: "Object bytes currently held, from the metadata index.",
				Value: func() float64 { return float64(n.usageBytes()) }},
			metrics.Source{Name: "stored_objects", Help: "Objects currently held.",
				Value: func() float64 { return float64(n.usageObjects()) }},
			metrics.Source{Name: "index_touches_written_total", Help: "Access-time updates written to the index.", Counter: true,
				Value: func() float64 { return float64(idx.Snapshot().TouchesWritten) }},
			metrics.Source{Name: "index_touches_dropped_total",
				Help:    "Access-time updates dropped because the queue was full. Nonzero means eviction is ranking on slightly stale information.",
				Counter: true,
				Value:   func() float64 { return float64(idx.Snapshot().TouchesDropped) }},
		)
	}

	if rw != nil {
		src = append(src,
			metrics.Source{Name: "repair_queue_depth", Help: "Objects waiting to be checked by the repair worker.",
				Value: func() float64 { return float64(rw.Snapshot().QueueDepth) }},
			metrics.Source{Name: "repair_queue_capacity", Help: "Repair queue capacity.",
				Value: func() float64 { return float64(rw.Snapshot().QueueCapacity) }},
			metrics.Source{Name: "repair_completed_total", Help: "Missing replicas recreated.", Counter: true,
				Value: func() float64 { return float64(rw.Snapshot().Repaired) }},
			metrics.Source{Name: "repair_bytes_total", Help: "Bytes sent to recreate missing replicas.", Counter: true,
				Value: func() float64 { return float64(rw.Snapshot().BytesRepaired) }},
			metrics.Source{Name: "repair_failed_total",
				Help:    "Repair attempts that failed. Excludes copies declined by a holder that is over quota.",
				Counter: true,
				Value:   func() float64 { return float64(rw.Snapshot().Failed) }},
			metrics.Source{Name: "repair_audited_total", Help: "Objects examined by the repair auditor.", Counter: true,
				Value: func() float64 { return float64(rw.Snapshot().ObjectsAudited) }},
			metrics.Source{Name: "repair_dropped_full_queue_total",
				Help:    "Audit tasks not queued because the queue was full. The pass resumes at that object, so nothing is skipped.",
				Counter: true,
				Value:   func() float64 { return float64(rw.Snapshot().DroppedFullQueue) }},
		)
	}
	return src
}

// usageBytes and usageObjects read the index, with a short deadline so a scrape
// can never block on a stalled database.
func (n *Node) usageBytes() int64 {
	_, b := n.usage()
	return b
}

func (n *Node) usageObjects() int64 {
	o, _ := n.usage()
	return o
}

func (n *Node) usage() (objects, bytes int64) {
	idx := n.store.Index()
	if idx == nil {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o, b, err := idx.Totals(ctx)
	if err != nil {
		return 0, 0
	}
	return o, b
}

// Stats returns every subsystem's counters, for /stats.
//
// It is assembled here rather than in each subsystem because this is the only
// place that knows which subsystems exist. Counters only: no keys, no paths.
func (n *Node) Stats() map[string]any {
	out := map[string]any{
		"cluster": map[string]any{
			"self":          n.cfg.NodeName,
			"members":       config.PeerNames(n.cfg.Peers),
			"replica_count": n.coord.ReplicaCount(),
		},
	}
	if n.store != nil {
		out["storage"] = n.store.Snapshot()
		out["dir_fsync_supported"] = n.store.DirSyncSupported()
		if idx := n.store.Index(); idx != nil {
			out["index"] = idx.Snapshot()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if objects, bytes, err := idx.Totals(ctx); err == nil {
				out["usage"] = map[string]any{
					"objects":          objects,
					"bytes":            bytes,
					"quota_bytes":      n.evictor.Quota().MaxBytes,
					"high_water_bytes": n.evictor.Quota().HighBytes(),
					"low_water_bytes":  n.evictor.Quota().LowBytes(),
					"over_high_water":  bytes > n.evictor.Quota().HighBytes(),
				}
			}
			cancel()
		}
	}
	if n.coord != nil {
		out["coordinator"] = n.coord.Snapshot()
	}
	if n.evictor != nil {
		out["eviction"] = n.evictor.Snapshot()
	}
	if n.repair != nil {
		out["repair"] = n.repair.Snapshot()
	}
	return out
}

// Evictor exposes the quota enforcer.
func (n *Node) Evictor() *storage.Evictor { return n.evictor }

// Repair exposes the repair worker, or nil when repair is disabled.
func (n *Node) Repair() *repair.Worker { return n.repair }

// Run starts the background workers, serves until ctx is cancelled, then drains
// requests and stops the workers.
//
// Order matters on the way down: stop accepting and drain first, then stop the
// background workers. Stopping the evictor first would be harmless; stopping
// the repair worker first would abort transfers that a request is waiting on.
func (n *Node) Run(ctx context.Context) error {
	bgCtx, cancel := context.WithCancel(context.Background())
	n.bgCancel = cancel

	n.bgDone.Add(1)
	go func() {
		defer n.bgDone.Done()
		n.evictor.Run(bgCtx, evictInterval)
	}()

	n.bgDone.Add(1)
	go func() {
		defer n.bgDone.Done()
		n.flushTouches(bgCtx)
	}()

	if n.repair != nil {
		n.bgDone.Add(1)
		go func() {
			defer n.bgDone.Done()
			n.repair.Run(bgCtx, n.cfg.RepairInterval)
		}()
	}

	n.health.SetReady()
	n.log.Info("ready", slog.String("addr", n.Addr()))

	err := n.server.Run(ctx)

	n.stopBackground()
	return err
}

// evictInterval is how often the evictor sweeps even without a write to wake
// it. A node that is only read from still has to drain after a restart that
// left it over quota.
const evictInterval = 30 * time.Second

// touchFlushInterval is how often queued reads reach the index. Short enough
// that eviction ranks on roughly current information, long enough that a burst
// of hits collapses into one transaction.
const touchFlushInterval = 5 * time.Second

// flushTouches writes queued access times to the index.
//
// Recording a read has to happen somewhere, and doing it inline would turn
// every cache hit -- the operation the whole system exists to make fast -- into
// a database write. Here it costs one transaction every few seconds regardless
// of hit rate.
func (n *Node) flushTouches(ctx context.Context) {
	idx := n.store.Index()
	if idx == nil {
		return
	}
	ticker := time.NewTicker(touchFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := idx.FlushTouches(flushCtx); err != nil {
				n.log.Warn("final access-time flush failed", slog.String("err", err.Error()))
			}
			cancel()
			return
		case <-ticker.C:
			if err := idx.FlushTouches(ctx); err != nil && !errors.Is(err, context.Canceled) {
				n.log.Warn("access-time flush failed", slog.String("err", err.Error()))
			}
		}
	}
}

func (n *Node) stopBackground() {
	if n.bgCancel == nil {
		return
	}
	n.bgCancel()
	n.bgCancel = nil

	stopped := make(chan struct{})
	go func() {
		n.bgDone.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		// Say so rather than hang. A worker that will not stop is a bug worth
		// seeing in the logs, and blocking shutdown forever hides it.
		n.log.Error("background workers did not stop within 30s")
	}
	if n.evictor != nil {
		n.evictor.Close()
	}
	if n.repair != nil {
		n.repair.Close()
	}
}

// Close releases resources in reverse dependency order: stop accepting, drop
// peer connections, then close storage. Reversing that would let a request in
// flight touch a store that has already been closed.
func (n *Node) Close() error {
	var firstErr error
	n.stopBackground()
	if n.server != nil {
		if err := n.server.Close(); err != nil {
			firstErr = err
		}
	}
	if err := n.closeAll(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (n *Node) closeAll() error {
	if n.peers != nil {
		n.peers.Close()
		n.peers = nil
	}
	if n.store == nil {
		return nil
	}
	err := n.store.Close()
	n.store = nil
	return err
}
