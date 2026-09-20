package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// ErrInsufficientReplicas means the required number of copies could not be
// written; it is defined in internal/protocol because the HTTP layer matches on
// it to choose a status code.
//
// This is the error that makes the replication factor mean something. A cache
// that returns 200 when only one of two copies was written has not replicated
// the object; it has replicated the *belief* that it did, which is worse than
// no replication at all, because the repair worker will not know to fix it.
var ErrInsufficientReplicas = protocol.ErrInsufficientReplicas

// InsufficientReplicasError reports how far short the write fell and why.
type InsufficientReplicasError struct {
	Key     string
	Wanted  int
	Got     int
	Holders []string
	Causes  []error
}

func (e *InsufficientReplicasError) Error() string {
	return fmt.Sprintf("%v: wrote %d of %d copies of %s (holders %v): %v",
		ErrInsufficientReplicas, e.Got, e.Wanted, short(e.Key), e.Holders, errors.Join(e.Causes...))
}

func (e *InsufficientReplicasError) Unwrap() error { return ErrInsufficientReplicas }

func short(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

// Coordinator is the Backend: it places objects using the ring, replicates
// synchronously, and falls back to peers on read.
//
// A single-node deployment uses the same type with a one-member ring and
// ReplicaCount 1, so there is no separate "local only" code path that the
// cluster tests never exercise.
type Coordinator struct {
	ring         *Ring
	local        *storage.Store
	peers        PeerClient
	replicaCount int
	log          *slog.Logger

	stats CoordinatorStats
}

// CoordinatorStats are the cluster-level counters. Phase 4 exposes them as
// Prometheus metrics; they exist from Phase 2 so the integration tests can
// assert on forwarding behaviour rather than infer it from logs.
type CoordinatorStats struct {
	LocalHits        atomic.Int64
	PeerHits         atomic.Int64
	Misses           atomic.Int64
	ForwardedPuts    atomic.Int64
	ReplicaWrites    atomic.Int64
	ReplicaFailures  atomic.Int64
	CoordinatedPuts  atomic.Int64
	InsufficientAcks atomic.Int64
	ReadFallbacks    atomic.Int64
}

// CoordinatorSnapshot is a JSON-friendly copy.
type CoordinatorSnapshot struct {
	LocalHits        int64 `json:"local_hits"`
	PeerHits         int64 `json:"peer_hits"`
	Misses           int64 `json:"misses"`
	ForwardedPuts    int64 `json:"forwarded_puts"`
	ReplicaWrites    int64 `json:"replica_writes"`
	ReplicaFailures  int64 `json:"replica_failures"`
	CoordinatedPuts  int64 `json:"coordinated_puts"`
	InsufficientAcks int64 `json:"insufficient_acks"`
	ReadFallbacks    int64 `json:"read_fallbacks"`
}

// Snapshot copies the counters.
func (c *Coordinator) Snapshot() CoordinatorSnapshot {
	return CoordinatorSnapshot{
		LocalHits:        c.stats.LocalHits.Load(),
		PeerHits:         c.stats.PeerHits.Load(),
		Misses:           c.stats.Misses.Load(),
		ForwardedPuts:    c.stats.ForwardedPuts.Load(),
		ReplicaWrites:    c.stats.ReplicaWrites.Load(),
		ReplicaFailures:  c.stats.ReplicaFailures.Load(),
		CoordinatedPuts:  c.stats.CoordinatedPuts.Load(),
		InsufficientAcks: c.stats.InsufficientAcks.Load(),
		ReadFallbacks:    c.stats.ReadFallbacks.Load(),
	}
}

// CoordinatorOptions configures a Coordinator.
type CoordinatorOptions struct {
	Ring         *Ring
	Local        *storage.Store
	Peers        PeerClient
	ReplicaCount int
	Logger       *slog.Logger
}

// NewCoordinator builds the Backend.
func NewCoordinator(opts CoordinatorOptions) (*Coordinator, error) {
	if opts.Ring == nil {
		return nil, errors.New("cluster: ring is required")
	}
	if opts.Local == nil {
		return nil, errors.New("cluster: local store is required")
	}
	rf := opts.ReplicaCount
	if rf < 1 {
		rf = 1
	}
	if rf > opts.Ring.Size() {
		rf = opts.Ring.Size()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Coordinator{
		ring:         opts.Ring,
		local:        opts.Local,
		peers:        opts.Peers,
		replicaCount: rf,
		log:          opts.Logger,
	}, nil
}

// Ring exposes the placement function, for repair and for tests.
func (c *Coordinator) Ring() *Ring { return c.ring }

// ReplicaCount is the number of copies a PUT must place.
func (c *Coordinator) ReplicaCount() int { return c.replicaCount }

// Local exposes the local store.
func (c *Coordinator) Local() *storage.Store { return c.local }

// Put places an object according to its hop role.
//
//	HopReplica     store locally, nothing else. Terminal.
//	HopCoordinator store locally (this node is an owner) and replicate to the
//	               other owners. Terminal for forwarding: only replica writes.
//	HopClient      if this node is an owner, act as coordinator; otherwise
//	               proxy the body to an owner and let it coordinate.
//
// A client's PUT therefore crosses at most two network hops, and a forwarded
// one at most one. See protocol.Hop for why the bound matters.
func (c *Coordinator) Put(ctx context.Context, ns storage.Namespace, key string, body io.Reader, declaredSize int64, hop protocol.Hop) (protocol.PutOutcome, error) {
	switch hop {
	case protocol.HopReplica:
		return c.putLocalOnly(ctx, ns, key, body, declaredSize)
	case protocol.HopCoordinator:
		return c.coordinate(ctx, ns, key, body, declaredSize)
	case protocol.HopRead:
		// A read hop has no business carrying a body; treat it as a replica
		// write rather than inventing a fourth behaviour.
		return c.putLocalOnly(ctx, ns, key, body, declaredSize)
	default:
		holders := c.ring.Holders(key, c.replicaCount)
		if c.selfIn(holders) {
			return c.coordinate(ctx, ns, key, body, declaredSize)
		}
		return c.proxyPut(ctx, ns, key, body, declaredSize, holders)
	}
}

func (c *Coordinator) selfIn(members []Member) bool {
	for _, m := range members {
		if m.Name == c.ring.Self() {
			return true
		}
	}
	return false
}

func (c *Coordinator) putLocalOnly(ctx context.Context, ns storage.Namespace, key string, body io.Reader, size int64) (protocol.PutOutcome, error) {
	res, err := c.local.Put(ctx, ns, key, body, size)
	if err != nil {
		return protocol.PutOutcome{}, err
	}
	c.stats.ReplicaWrites.Add(1)
	return protocol.PutOutcome{
		Size:          res.Size,
		AlreadyStored: res.AlreadyStored,
		Copies:        1,
		Wanted:        1,
		Holders:       []string{c.ring.Self()},
		Durable:       res.Durable,
	}, nil
}

// coordinate writes the local copy first, then fans out to the other holders.
//
// Local first, rather than streaming to every holder at once: the local write
// verifies the digest and gives a stable source to replicate from, so a peer
// that is slow or dies mid-transfer can be retried without the client's body,
// which has already been consumed. Streaming to all holders in parallel through
// io.Pipe would cut PUT latency to max(local, remote) instead of
// local + remote, and it is the obvious next optimisation; it is not done here
// because a mid-stream peer failure would then have no source to retry from.
// The trade-off is measured in docs/perf-notes.md rather than assumed.
func (c *Coordinator) coordinate(ctx context.Context, ns storage.Namespace, key string, body io.Reader, size int64) (protocol.PutOutcome, error) {
	holders := c.ring.Holders(key, c.replicaCount)
	wanted := len(holders)

	res, err := c.local.Put(ctx, ns, key, body, size)
	if err != nil {
		return protocol.PutOutcome{}, err
	}
	c.stats.CoordinatedPuts.Add(1)

	got := 1
	acked := []string{c.ring.Self()}
	var causes []error

	targets := make([]Member, 0, wanted)
	for _, m := range holders {
		if m.Name != c.ring.Self() {
			targets = append(targets, m)
		}
	}

	if len(targets) > 0 && c.peers != nil {
		results := c.fanOut(ctx, ns, key, res.Size, targets)
		for _, r := range results {
			if r.err == nil {
				got++
				acked = append(acked, r.peer)
				continue
			}
			causes = append(causes, r.err)
			c.stats.ReplicaFailures.Add(1)
			c.log.Warn("replica write failed",
				slog.String("key", short(key)),
				slog.String("ns", ns.String()),
				slog.String("peer", r.peer),
				slog.String("err", r.err.Error()))
		}
	}

	outcome := protocol.PutOutcome{
		Size:          res.Size,
		AlreadyStored: res.AlreadyStored,
		Copies:        got,
		Wanted:        wanted,
		Holders:       acked,
		Durable:       res.Durable,
	}

	if got < wanted {
		c.stats.InsufficientAcks.Add(1)
		return outcome, &InsufficientReplicasError{
			Key: key, Wanted: wanted, Got: got, Holders: acked, Causes: causes,
		}
	}
	return outcome, nil
}

type fanOutResult struct {
	peer string
	err  error
}

// fanOut replicates to every target concurrently, each reading the object from
// local disk independently.
//
// Concurrent rather than sequential because with three nodes and RF=3 a
// sequential fan-out would make PUT latency the sum of every hop. Each target
// opens its own file handle: sharing one reader would force them to be
// sequential anyway, and the file is in page cache after the write that just
// happened.
func (c *Coordinator) fanOut(ctx context.Context, ns storage.Namespace, key string, size int64, targets []Member) []fanOutResult {
	results := make([]fanOutResult, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target Member) {
			defer wg.Done()
			results[i] = fanOutResult{peer: target.Name, err: c.replicateOne(ctx, ns, key, size, target)}
		}(i, target)
	}
	wg.Wait()
	return results
}

func (c *Coordinator) replicateOne(ctx context.Context, ns storage.Namespace, key string, size int64, target Member) error {
	obj, err := c.local.Get(ns, key)
	if err != nil {
		return fmt.Errorf("reopen local copy of %s for replication: %w", short(key), err)
	}
	defer obj.Close()

	// Replication reads from a file, so the peer gets a known Content-Length
	// and can enforce it. A chunked replica write would lose the truncation
	// check on the AC namespace, where there is no digest to fall back on.
	if err := c.peers.Put(ctx, target, ns, key, obj.File(), size, protocol.HopReplica); err != nil {
		return err
	}
	return nil
}

// proxyPut streams a client's body to an owner, which coordinates the rest.
//
// The front door does not stage a copy on its own disk. It is not an owner, so
// that copy would be written, replicated from, and then be a fourth copy nobody
// asked for — on a 2 GiB build, two gigabytes of pointless writes on whichever
// node the client happened to address.
func (c *Coordinator) proxyPut(ctx context.Context, ns storage.Namespace, key string, body io.Reader, size int64, holders []Member) (protocol.PutOutcome, error) {
	if c.peers == nil {
		return protocol.PutOutcome{}, errors.New("cluster: no peer client configured")
	}
	c.stats.ForwardedPuts.Add(1)

	target := holders[0]
	err := c.peers.Put(ctx, target, ns, key, body, size, protocol.HopCoordinator)
	if err == nil {
		return protocol.PutOutcome{
			Size:    size,
			Copies:  len(holders),
			Wanted:  len(holders),
			Holders: memberNames(holders),
			Durable: true,
		}, nil
	}

	// The body has been consumed by the failed attempt, so there is nothing
	// left to send to a different owner. Reporting the failure honestly is the
	// only correct option: a client that retries will send the body again,
	// which is exactly what should happen.
	c.stats.InsufficientAcks.Add(1)
	return protocol.PutOutcome{Wanted: len(holders)}, &InsufficientReplicasError{
		Key: key, Wanted: len(holders), Got: 0, Causes: []error{err},
	}
}

func memberNames(ms []Member) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

// Open finds an object: local copy first, then each holder in preference order,
// then any remaining node.
//
// Local first even when this node is not a holder. Placement is advisory, not
// authoritative: an object can be here because this node used to be a holder
// before the membership changed, or because repair put it here. Serving a local
// copy costs nothing and skips a network hop, and the copy is verified on read
// when verification is enabled.
func (c *Coordinator) Open(ctx context.Context, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectReader, error) {
	obj, err := c.local.Get(ns, key)
	if err == nil {
		c.stats.LocalHits.Add(1)
		return &localObject{Object: obj}, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}

	// A forwarded read is terminal: the node that forwarded it already knows
	// the preference list, so a second hop would be re-deriving a decision that
	// has already been made — and would be the edge that closes a forwarding
	// loop when two nodes disagree about placement.
	if hop.Terminal() || c.peers == nil {
		c.stats.Misses.Add(1)
		return nil, storage.ErrNotFound
	}

	candidates := c.readOrder(key)
	var lastErr error
	for _, m := range candidates {
		reader, err := c.peers.Get(ctx, m, ns, key, protocol.HopRead)
		if err == nil {
			c.stats.PeerHits.Add(1)
			c.stats.ReadFallbacks.Add(1)
			return reader, nil
		}
		if errors.Is(err, ErrPeerNotFound) {
			continue
		}
		lastErr = err
		c.log.Debug("peer read failed, trying the next holder",
			slog.String("key", short(key)),
			slog.String("peer", m.Name),
			slog.String("err", err.Error()))
	}

	c.stats.Misses.Add(1)
	if lastErr != nil {
		// Every holder was unreachable. That is not the same as "the object
		// does not exist", and conflating them would let a network partition
		// look like a cold cache -- which for a build cache means silently
		// rebuilding everything instead of reporting a problem.
		return nil, fmt.Errorf("all holders unreachable for %s: %w", short(key), lastErr)
	}
	return nil, storage.ErrNotFound
}

// readOrder returns the peers to try, holders first and then everyone else.
//
// Trying non-holders last is not pointless: after a membership change, or while
// repair is catching up, a copy can be on a node that is no longer a preferred
// holder. Two extra HEADs against a three-node cluster is a cheap price for not
// turning a recoverable read into a rebuild.
func (c *Coordinator) readOrder(key string) []Member {
	all := c.ring.Owners(key)
	out := make([]Member, 0, len(all))
	for _, m := range all {
		if m.Name != c.ring.Self() {
			out = append(out, m)
		}
	}
	return out
}

// Stat reports on an object without transferring it.
func (c *Coordinator) Stat(ctx context.Context, ns storage.Namespace, key string, hop protocol.Hop) (protocol.ObjectInfo, error) {
	st, err := c.local.Stat(ns, key)
	if err == nil {
		c.stats.LocalHits.Add(1)
		return protocol.ObjectInfo{Size: st.Size, Source: "local"}, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return protocol.ObjectInfo{}, err
	}
	if hop.Terminal() || c.peers == nil {
		c.stats.Misses.Add(1)
		return protocol.ObjectInfo{}, storage.ErrNotFound
	}

	var lastErr error
	for _, m := range c.readOrder(key) {
		info, err := c.peers.Stat(ctx, m, ns, key, protocol.HopRead)
		if err == nil {
			c.stats.PeerHits.Add(1)
			return info, nil
		}
		if errors.Is(err, ErrPeerNotFound) {
			continue
		}
		lastErr = err
	}
	c.stats.Misses.Add(1)
	if lastErr != nil {
		return protocol.ObjectInfo{}, fmt.Errorf("all holders unreachable for %s: %w", short(key), lastErr)
	}
	return protocol.ObjectInfo{}, storage.ErrNotFound
}

// localObject adapts a storage.Object to protocol.ObjectReader.
type localObject struct {
	*storage.Object
}

func (l *localObject) Size() int64 { return l.Object.Stat.Size }

func (l *localObject) Source() string { return "local" }

var _ protocol.ObjectReader = (*localObject)(nil)
var _ protocol.Backend = (*Coordinator)(nil)
