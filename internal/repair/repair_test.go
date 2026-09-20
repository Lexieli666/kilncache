package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/cluster"
	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// fakePeers is a minimal in-memory peer set, enough to make a copy present or
// absent on demand. internal/cluster has a richer one; duplicating the small
// part needed here keeps this package's tests from importing another package's
// test helpers, which Go does not allow anyway.
type fakePeers struct {
	mu      sync.Mutex
	objects map[string]map[string][]byte
	down    map[string]bool
	puts    map[string]int
	stats   map[string]int
	hops    []protocol.Hop
	putErr  map[string]error
}

func newFakePeers(nodes ...string) *fakePeers {
	f := &fakePeers{
		objects: map[string]map[string][]byte{},
		down:    map[string]bool{},
		puts:    map[string]int{},
		stats:   map[string]int{},
		putErr:  map[string]error{},
	}
	for _, n := range nodes {
		f.objects[n] = map[string][]byte{}
	}
	return f
}

func ref(ns storage.Namespace, key string) string { return string(ns) + "/" + key }

func (f *fakePeers) setDown(node string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[node] = down
}

func (f *fakePeers) setPutErr(node string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putErr[node] = err
}

func (f *fakePeers) has(node string, ns storage.Namespace, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[node][ref(ns, key)]
	return ok
}

func (f *fakePeers) put(node string, ns storage.Namespace, key string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[node][ref(ns, key)] = b
}

func (f *fakePeers) drop(node string, ns storage.Namespace, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects[node], ref(ns, key))
}

func (f *fakePeers) putCount(node string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts[node]
}

func (f *fakePeers) recordedHops() []protocol.Hop {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]protocol.Hop, len(f.hops))
	copy(out, f.hops)
	return out
}

func (f *fakePeers) Put(ctx context.Context, peer cluster.Member, ns storage.Namespace, key string, body io.Reader, size int64, hop protocol.Hop) error {
	f.mu.Lock()
	f.puts[peer.Name]++
	f.hops = append(f.hops, hop)
	down := f.down[peer.Name]
	injected := f.putErr[peer.Name]
	f.mu.Unlock()

	if down {
		return &cluster.PeerError{Peer: peer.Name, Op: "put", Err: errConnRefused}
	}
	if injected != nil {
		return injected
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return err
	}
	if size >= 0 && int64(buf.Len()) != size {
		return fmt.Errorf("declared %d, got %d", size, buf.Len())
	}
	f.put(peer.Name, ns, key, buf.Bytes())
	_ = ctx
	return nil
}

func (f *fakePeers) Get(_ context.Context, peer cluster.Member, ns storage.Namespace, key string, _ protocol.Hop) (protocol.ObjectReader, error) {
	f.mu.Lock()
	down := f.down[peer.Name]
	b, ok := f.objects[peer.Name][ref(ns, key)]
	f.mu.Unlock()
	if down {
		return nil, &cluster.PeerError{Peer: peer.Name, Op: "get", Err: errConnRefused}
	}
	if !ok {
		return nil, cluster.ErrPeerNotFound
	}
	return &memReader{r: bytes.NewReader(b), size: int64(len(b)), src: peer.Name}, nil
}

func (f *fakePeers) Stat(_ context.Context, peer cluster.Member, ns storage.Namespace, key string, _ protocol.Hop) (protocol.ObjectInfo, error) {
	f.mu.Lock()
	f.stats[peer.Name]++
	down := f.down[peer.Name]
	b, ok := f.objects[peer.Name][ref(ns, key)]
	f.mu.Unlock()
	if down {
		return protocol.ObjectInfo{}, &cluster.PeerError{Peer: peer.Name, Op: "stat", Err: errConnRefused}
	}
	if !ok {
		return protocol.ObjectInfo{}, cluster.ErrPeerNotFound
	}
	return protocol.ObjectInfo{Size: int64(len(b)), Source: peer.Name}, nil
}

func (f *fakePeers) Close() {}

type connRefused struct{}

func (connRefused) Error() string { return "connection refused" }

var errConnRefused = connRefused{}

type memReader struct {
	r    *bytes.Reader
	size int64
	src  string
}

func (m *memReader) Read(p []byte) (int, error)         { return m.r.Read(p) }
func (m *memReader) WriteTo(w io.Writer) (int64, error) { return io.Copy(w, m.r) }
func (m *memReader) Close() error                       { return nil }
func (m *memReader) Size() int64                        { return m.size }
func (m *memReader) Source() string                     { return "peer:" + m.src }
func (m *memReader) Verify() error                      { return nil }

// --- fixture --------------------------------------------------------------

type fixture struct {
	t      *testing.T
	coord  *cluster.Coordinator
	ring   *cluster.Ring
	store  *storage.Store
	peers  *fakePeers
	worker *Worker
}

func newFixture(t *testing.T, self string, rf int, opts Options) *fixture {
	t.Helper()
	members := []string{"node-a", "node-b", "node-c"}
	peerList := make([]config.Peer, 0, len(members))
	for _, m := range members {
		peerList = append(peerList, config.Peer{Name: m, URL: "http://" + m + ":8080"})
	}
	ring, err := cluster.New(peerList, self)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	store, err := storage.Open(storage.Options{Root: t.TempDir(), VerifyReads: true})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fp := newFakePeers(members...)
	coord, err := cluster.NewCoordinator(cluster.CoordinatorOptions{
		Ring: ring, Local: store, Peers: fp, ReplicaCount: rf,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	opts.Coordinator = coord
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	w, err := New(opts)
	if err != nil {
		t.Fatalf("New worker: %v", err)
	}
	return &fixture{t: t, coord: coord, ring: ring, store: store, peers: fp, worker: w}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keyHeldBy finds a key whose holder set includes the given node.
func keyHeldBy(t *testing.T, r *cluster.Ring, node string, rf int) (string, []byte) {
	t.Helper()
	for i := 0; i < 200000; i++ {
		content := []byte("repair-" + strconv.Itoa(i))
		k := digest(content)
		if r.HoldsKey(node, k, rf) {
			return k, content
		}
	}
	t.Fatalf("no key held by %s", node)
	return "", nil
}

func keyNotHeldBy(t *testing.T, r *cluster.Ring, node string, rf int) (string, []byte) {
	t.Helper()
	for i := 0; i < 200000; i++ {
		content := []byte("repair-" + strconv.Itoa(i))
		k := digest(content)
		if !r.HoldsKey(node, k, rf) {
			return k, content
		}
	}
	t.Fatalf("no key that %s does not hold", node)
	return "", nil
}

func (f *fixture) storeLocal(key string, content []byte) {
	f.t.Helper()
	if _, err := f.store.Put(context.Background(), storage.NamespaceCAS, key,
		bytes.NewReader(content), int64(len(content))); err != nil {
		f.t.Fatalf("seed local: %v", err)
	}
}

// drainQueue runs the worker pool briefly so queued tasks are executed.
func (f *fixture) drainQueue(d time.Duration) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.worker.Run(ctx, time.Hour); close(done) }()
	time.Sleep(d)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		f.t.Fatal("worker did not stop")
	}
}

// --- tests ----------------------------------------------------------------

// TestRepairRecreatesAMissingReplica is the core Phase 3 claim.
func TestRepairRecreatesAMissingReplica(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 2, QueueSize: 16})
	key, content := keyHeldBy(t, f.ring, "node-a", 2)
	f.storeLocal(key, content)

	// The other holder has nothing: the shape left by a peer that acknowledged
	// a write it did not persist, or that evicted its copy.
	var other string
	for _, m := range f.ring.Holders(key, 2) {
		if m.Name != "node-a" {
			other = m.Name
		}
	}
	if f.peers.has(other, storage.NamespaceCAS, key) {
		t.Fatal("fixture precondition: the peer should not have the object")
	}

	if err := f.worker.Audit(context.Background()); err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if f.worker.Snapshot().Enqueued == 0 {
		t.Fatal("the audit enqueued nothing")
	}
	f.drainQueue(500 * time.Millisecond)

	if !f.peers.has(other, storage.NamespaceCAS, key) {
		t.Fatalf("the missing replica on %s was not recreated", other)
	}
	snap := f.worker.Snapshot()
	if snap.Repaired != 1 {
		t.Errorf("Repaired = %d, want 1", snap.Repaired)
	}
	if snap.BytesRepaired != int64(len(content)) {
		t.Errorf("BytesRepaired = %d, want %d", snap.BytesRepaired, len(content))
	}
}

// TestRepairOnlyIssuesReplicaWrites: repair must not be able to start a
// forwarding chain.
func TestRepairOnlyIssuesReplicaWrites(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 1, QueueSize: 8})
	key, content := keyHeldBy(t, f.ring, "node-a", 2)
	f.storeLocal(key, content)

	if err := f.worker.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.drainQueue(500 * time.Millisecond)

	hops := f.peers.recordedHops()
	if len(hops) == 0 {
		t.Fatal("repair contacted nobody")
	}
	for _, h := range hops {
		// Repair may only issue terminal hops. A non-terminal one would give
		// the background worker the ability to start a forwarding chain, which
		// is the one thing the hop state machine exists to prevent.
		if !h.Terminal() {
			t.Errorf("repair issued a non-terminal %q hop", h)
		}
		if h != protocol.HopRepair && h != protocol.HopRead {
			t.Errorf("repair issued a %q hop; only repair writes and read probes are allowed", h)
		}
	}
}

// TestRepairDeclinedOverQuotaIsNotAFailure: a holder that is already above its
// high-water mark answers 507, and the sender must treat that as the system
// working rather than as a broken repair.
//
// Without this distinction, eviction and repair chase each other: a node evicts
// to get under quota, the other holder's auditor sees a missing replica and
// sends it straight back, and the quota is never enforced. A 10-minute chaos
// run found exactly that, with nodes sitting at 2.6x their configured quota
// (docs/bugs.md, entry 7).
func TestRepairDeclinedOverQuotaIsNotAFailure(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 1, QueueSize: 16})
	key, content := keyHeldBy(t, f.ring, "node-a", 2)
	f.storeLocal(key, content)

	var other string
	for _, m := range f.ring.Holders(key, 2) {
		if m.Name != "node-a" {
			other = m.Name
		}
	}
	f.peers.setPutErr(other, &cluster.PeerError{
		Peer: other, Op: "put", Status: http.StatusInsufficientStorage,
		Body: "node is over its storage high-water mark",
	})

	if err := f.worker.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.drainQueue(500 * time.Millisecond)

	snap := f.worker.Snapshot()
	if snap.DeclinedOverQuota == 0 {
		t.Error("a 507 from a holder was not recorded as declined")
	}
	if snap.Failed != 0 {
		t.Errorf("Failed = %d; an over-quota decline was counted as a repair failure", snap.Failed)
	}
	if snap.Repaired != 0 {
		t.Errorf("Repaired = %d despite the copy being declined", snap.Repaired)
	}
}

// TestRepairSkipsObjectsThisNodeDoesNotOwn: an object that is here because
// placement changed is served happily but is not this node's responsibility.
func TestRepairSkipsObjectsThisNodeDoesNotOwn(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 1, QueueSize: 8})
	key, content := keyNotHeldBy(t, f.ring, "node-a", 2)
	f.storeLocal(key, content)

	if err := f.worker.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := f.worker.Snapshot()
	if snap.Enqueued != 0 {
		t.Errorf("Enqueued = %d for an object this node does not own", snap.Enqueued)
	}
	if snap.NotOurs != 1 {
		t.Errorf("NotOurs = %d, want 1", snap.NotOurs)
	}
}

// TestRepairIsANoOpWhenTheReplicaIsPresent: repair must not re-send copies that
// already exist, or a healthy cluster would saturate itself with repair traffic.
func TestRepairIsANoOpWhenTheReplicaIsPresent(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 1, QueueSize: 8})
	key, content := keyHeldBy(t, f.ring, "node-a", 2)
	f.storeLocal(key, content)
	for _, m := range f.ring.Holders(key, 2) {
		if m.Name != "node-a" {
			f.peers.put(m.Name, storage.NamespaceCAS, key, content)
		}
	}

	if err := f.worker.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.drainQueue(400 * time.Millisecond)

	snap := f.worker.Snapshot()
	if snap.Repaired != 0 {
		t.Errorf("Repaired = %d; a present replica was re-sent", snap.Repaired)
	}
	if snap.AlreadyPresent == 0 {
		t.Error("AlreadyPresent = 0; the audit did not check the peer")
	}
	for _, m := range f.ring.Members() {
		if n := f.peers.putCount(m.Name); n != 0 {
			t.Errorf("repair sent %d copies to %s despite the replica being present", n, m.Name)
		}
	}
}

// TestRepairTreatsAnUnreachablePeerAsUnknown: a node that is down is not a node
// that is missing a copy. Counting it as a repair failure would make the metric
// say "repair is broken" when the truth is "a node is down".
func TestRepairTreatsAnUnreachablePeerAsUnknown(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 1, QueueSize: 8})
	key, content := keyHeldBy(t, f.ring, "node-a", 2)
	f.storeLocal(key, content)
	for _, m := range f.ring.Holders(key, 2) {
		if m.Name != "node-a" {
			f.peers.setDown(m.Name, true)
		}
	}

	if err := f.worker.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.drainQueue(400 * time.Millisecond)

	snap := f.worker.Snapshot()
	if snap.Failed != 0 {
		t.Errorf("Failed = %d; an unreachable peer was counted as a repair failure", snap.Failed)
	}
	if snap.Repaired != 0 {
		t.Errorf("Repaired = %d against a node that is down", snap.Repaired)
	}
}

// TestRepairConvergesAfterAPeerReturns is the Phase 3 convergence claim in
// miniature: with objects under-replicated and the peer back, successive audit
// passes bring every object to two copies.
func TestRepairConvergesAfterAPeerReturns(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 4, QueueSize: 256})
	ctx := context.Background()

	type obj struct {
		key   string
		other string
	}
	var objs []obj
	for i := 0; i < 60; i++ {
		content := []byte("converge-" + strconv.Itoa(i))
		key := digest(content)
		if !f.ring.HoldsKey("node-a", key, 2) {
			continue
		}
		f.storeLocal(key, content)
		var other string
		for _, m := range f.ring.Holders(key, 2) {
			if m.Name != "node-a" {
				other = m.Name
			}
		}
		// Seed the peer, then drop the copy: the shape left by an eviction on
		// the other holder.
		f.peers.put(other, storage.NamespaceCAS, key, content)
		f.peers.drop(other, storage.NamespaceCAS, key)
		objs = append(objs, obj{key: key, other: other})
	}
	if len(objs) < 10 {
		t.Fatalf("only %d objects landed on node-a; the fixture is too small", len(objs))
	}

	start := time.Now()
	ctxRun, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.worker.Run(ctxRun, 50*time.Millisecond); close(done) }()

	deadline := time.Now().Add(20 * time.Second)
	converged := false
	for time.Now().Before(deadline) {
		missing := 0
		for _, o := range objs {
			if !f.peers.has(o.other, storage.NamespaceCAS, o.key) {
				missing++
			}
		}
		if missing == 0 {
			converged = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	elapsed := time.Since(start)
	cancel()
	<-done

	if !converged {
		missing := 0
		for _, o := range objs {
			if !f.peers.has(o.other, storage.NamespaceCAS, o.key) {
				missing++
			}
		}
		t.Fatalf("%d of %d objects still under-replicated after %v; stats %+v",
			missing, len(objs), elapsed, f.worker.Snapshot())
	}
	t.Logf("converged to 2 replicas for all %d objects in %v (%+v)",
		len(objs), elapsed.Round(time.Millisecond), f.worker.Snapshot())
}

// TestFullQueueStopsThePassInsteadOfBlockingOrSkipping is the falsifier for the
// bounded queue's contract, which has two halves that pull against each other.
//
// It must not block: a blocked audit holds the index iterator open across a
// transaction while the workers are trying to write to the same database.
//
// And it must not skip: advancing the cursor past objects that did not fit is
// the subtler bug, because the pass still appears to make progress while the
// objects behind the full queue are not looked at again until the cursor wraps
// all the way round. That makes convergence time a function of total cache size
// rather than of how much is actually broken -- which is exactly what a chaos
// run caught before this was fixed (docs/bugs.md, entry 6).
func TestFullQueueStopsThePassInsteadOfBlockingOrSkipping(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 0, QueueSize: 4})

	seeded := 0
	for i := 0; i < 400 && seeded < 60; i++ {
		content := []byte("backpressure-" + strconv.Itoa(i))
		key := digest(content)
		if !f.ring.HoldsKey("node-a", key, 2) {
			continue
		}
		f.storeLocal(key, content)
		seeded++
	}

	// No workers, so nothing ever drains the queue.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := f.worker.Audit(context.Background()); err != nil {
			t.Errorf("Audit: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Audit blocked on a full queue instead of returning")
	}

	snap := f.worker.Snapshot()
	if snap.Enqueued > int64(snap.QueueCapacity) {
		t.Errorf("Enqueued %d exceeds the queue capacity %d", snap.Enqueued, snap.QueueCapacity)
	}
	if snap.DroppedFullQueue == 0 {
		t.Errorf("the pass never hit a full queue with %d objects and a queue of 4", seeded)
	}
	// The pass stopped rather than burning through the rest of the cache.
	if snap.ObjectsAudited > int64(snap.QueueCapacity)+1 {
		t.Errorf("the pass examined %d objects after the queue filled at %d; it should have stopped",
			snap.ObjectsAudited, snap.QueueCapacity)
	}
}

// TestFullQueueDoesNotSkipObjects: with the queue draining between passes,
// successive passes must reach every object rather than leaving the ones that
// did not fit until the cursor wraps.
func TestFullQueueDoesNotSkipObjects(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 2, QueueSize: 4})

	var keys []string
	for i := 0; i < 400 && len(keys) < 30; i++ {
		content := []byte("nodrop-" + strconv.Itoa(i))
		key := digest(content)
		if !f.ring.HoldsKey("node-a", key, 2) {
			continue
		}
		f.storeLocal(key, content)
		keys = append(keys, key)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.worker.Run(ctx, 20*time.Millisecond); close(done) }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		missing := 0
		for _, k := range keys {
			replicated := false
			for _, m := range f.ring.Holders(k, 2) {
				if m.Name != "node-a" && f.peers.has(m.Name, storage.NamespaceCAS, k) {
					replicated = true
				}
			}
			if !replicated {
				missing++
			}
		}
		if missing == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	missing := 0
	for _, k := range keys {
		replicated := false
		for _, m := range f.ring.Holders(k, 2) {
			if m.Name != "node-a" && f.peers.has(m.Name, storage.NamespaceCAS, k) {
				replicated = true
			}
		}
		if !replicated {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("%d of %d objects never got a second copy with a queue of 4; work behind a full queue was skipped (%+v)",
			missing, len(keys), f.worker.Snapshot())
	}
	t.Logf("all %d objects repaired through a queue of depth 4 (%+v)", len(keys), f.worker.Snapshot())
}

// TestAuditBudgetAndCursor: a bounded pass must cover the whole cache over
// successive passes rather than re-checking the same prefix forever.
func TestAuditBudgetAndCursor(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 0, QueueSize: 4096, AuditBudget: 5})

	seeded := 0
	for i := 0; i < 500 && seeded < 25; i++ {
		content := []byte("cursor-" + strconv.Itoa(i))
		key := digest(content)
		if !f.ring.HoldsKey("node-a", key, 2) {
			continue
		}
		f.storeLocal(key, content)
		seeded++
	}

	ctx := context.Background()
	for pass := 0; pass < 10; pass++ {
		if err := f.worker.Audit(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	snap := f.worker.Snapshot()
	if snap.AuditRuns != 10 {
		t.Errorf("AuditRuns = %d, want 10", snap.AuditRuns)
	}
	// Ten passes with a budget of 5 must have examined at least the whole set;
	// a cursor that never advanced would cap this at 5.
	if snap.ObjectsAudited < int64(seeded) {
		t.Errorf("examined %d objects across 10 passes of budget 5 with %d objects present; the cursor is not advancing",
			snap.ObjectsAudited, seeded)
	}
}

func TestAuditIsANoOpOnASingleNodeCluster(t *testing.T) {
	peerList := []config.Peer{{Name: "solo", URL: "http://solo:8080"}}
	ring, err := cluster.New(peerList, "solo")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(storage.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	coord, err := cluster.NewCoordinator(cluster.CoordinatorOptions{
		Ring: ring, Local: store, ReplicaCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(Options{Coordinator: coord})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("solo")
	if _, err := store.Put(context.Background(), storage.NamespaceCAS, digest(content),
		bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := w.Audit(context.Background()); err != nil {
		t.Fatalf("Audit on a single-node cluster: %v", err)
	}
	if snap := w.Snapshot(); snap.Enqueued != 0 || snap.ObjectsAudited != 0 {
		t.Errorf("a single-node audit did work: %+v", snap)
	}
}

func TestNewRejectsMissingCoordinator(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New without a coordinator = nil error")
	}
}

func TestRunStopsCleanly(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 3, QueueSize: 32})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.worker.Run(ctx, 20*time.Millisecond); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	f.worker.Wait()
	// Close is idempotent and safe after Run.
	f.worker.Close()
	f.worker.Close()
}

func TestCloseWithoutRun(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 1, QueueSize: 4})
	f.worker.Close()
	f.worker.Wait()
}

func TestRepairFailureIsCounted(t *testing.T) {
	f := newFixture(t, "node-a", 2, Options{Workers: 1, QueueSize: 16})
	key, content := keyHeldBy(t, f.ring, "node-a", 2)
	f.storeLocal(key, content)

	var other string
	for _, m := range f.ring.Holders(key, 2) {
		if m.Name != "node-a" {
			other = m.Name
		}
	}
	// The peer answers HEAD with "not found" but refuses the write for a reason
	// that is a genuine fault -- an I/O error, not a deliberate over-quota
	// decline, which has its own test and its own counter.
	f.peers.setPutErr(other, &cluster.PeerError{
		Peer: other, Op: "put", Status: http.StatusInternalServerError, Body: "write failed",
	})

	if err := f.worker.Audit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.drainQueue(500 * time.Millisecond)

	snap := f.worker.Snapshot()
	if snap.Failed == 0 {
		t.Error("a refused repair write was not counted as a failure")
	}
	if snap.Repaired != 0 {
		t.Errorf("Repaired = %d despite the write being refused", snap.Repaired)
	}
}
