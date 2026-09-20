package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/protocol"
	"github.com/Lexieli666/kilncache/internal/storage"
)

type fixture struct {
	coord *Coordinator
	ring  *Ring
	store *storage.Store
	peers *fakePeers
	self  string
}

func newFixture(t *testing.T, self string, rf int, members ...string) *fixture {
	t.Helper()
	if len(members) == 0 {
		members = []string{"node-a", "node-b", "node-c"}
	}
	ring, err := New(peers(members...), self)
	if err != nil {
		t.Fatalf("New ring: %v", err)
	}
	store, err := storage.Open(storage.Options{
		Root:        t.TempDir(),
		VerifyReads: true,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	fp := newFakePeers(members...)
	coord, err := NewCoordinator(CoordinatorOptions{
		Ring: ring, Local: store, Peers: fp, ReplicaCount: rf,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return &fixture{coord: coord, ring: ring, store: store, peers: fp, self: self}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// keyOwnedBy returns a key whose holder list starts with the given node.
func keyOwnedBy(t *testing.T, r *Ring, primary string, rf int) (string, []byte) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		content := []byte("payload-" + itoa(i))
		k := digest(content)
		if r.Holders(k, rf)[0].Name == primary {
			return k, content
		}
	}
	t.Fatalf("no key found with primary %s", primary)
	return "", nil
}

// keyNotHeldBy returns a key that the given node is not a holder of.
func keyNotHeldBy(t *testing.T, r *Ring, node string, rf int) (string, []byte) {
	t.Helper()
	for i := 0; i < 100000; i++ {
		content := []byte("payload-" + itoa(i))
		k := digest(content)
		if !r.HoldsKey(node, k, rf) {
			return k, content
		}
	}
	t.Fatalf("no key found that %s does not hold", node)
	return "", nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func put(t *testing.T, c *Coordinator, ns storage.Namespace, key string, body []byte, hop protocol.Hop) (protocol.PutOutcome, error) {
	t.Helper()
	return c.Put(context.Background(), ns, key, bytes.NewReader(body), int64(len(body)), hop)
}

func readAll(t *testing.T, c *Coordinator, ns storage.Namespace, key string, hop protocol.Hop) ([]byte, string, error) {
	t.Helper()
	obj, err := c.Open(context.Background(), ns, key, hop)
	if err != nil {
		return nil, "", err
	}
	defer obj.Close()
	var buf bytes.Buffer
	if _, err := obj.WriteTo(&buf); err != nil {
		return nil, obj.Source(), err
	}
	if err := obj.Verify(); err != nil {
		return nil, obj.Source(), err
	}
	return buf.Bytes(), obj.Source(), nil
}

// TestCoordinatePlacesBothCopies is the core Phase 2 claim: a PUT to an owner
// stores locally and on the replica before it returns.
func TestCoordinatePlacesBothCopies(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyOwnedBy(t, f.ring, "node-a", 2)
	replica := f.ring.Holders(key, 2)[1].Name

	out, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopClient)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if out.Copies != 2 || out.Wanted != 2 {
		t.Fatalf("copies = %d of %d, want 2 of 2", out.Copies, out.Wanted)
	}
	if _, err := f.store.Stat(storage.NamespaceCAS, key); err != nil {
		t.Errorf("local copy missing: %v", err)
	}
	if !f.peers.has(replica, storage.NamespaceCAS, key) {
		t.Errorf("replica %s does not have the object", replica)
	}
}

// TestCoordinatorOnlyIssuesReplicaWrites is the loop-prevention falsifier. A
// coordinator that forwarded as HopCoordinator would create an edge back into
// a non-terminal state, and two nodes that disagreed about placement could
// forward to each other indefinitely.
func TestCoordinatorOnlyIssuesReplicaWrites(t *testing.T) {
	f := newFixture(t, "node-a", 3)
	key, content := keyOwnedBy(t, f.ring, "node-a", 3)

	if _, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopClient); err != nil {
		t.Fatalf("Put: %v", err)
	}
	hops := f.peers.recordedHops()
	if len(hops) == 0 {
		t.Fatal("no peer calls were made")
	}
	for _, h := range hops {
		if h != protocol.HopReplica {
			t.Errorf("coordinator issued a %q hop; only %q is allowed", h, protocol.HopReplica)
		}
	}
}

// TestReplicaHopIsTerminal: a node receiving a replica write stores it and
// contacts nobody.
func TestReplicaHopIsTerminal(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyOwnedBy(t, f.ring, "node-b", 2)

	out, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopReplica)
	if err != nil {
		t.Fatalf("replica Put: %v", err)
	}
	if out.Copies != 1 {
		t.Errorf("copies = %d, want 1 for a replica write", out.Copies)
	}
	if puts, _, _ := f.peers.counts(); len(puts) != 0 {
		t.Errorf("a replica write contacted peers: %v", puts)
	}
	if _, err := f.store.Stat(storage.NamespaceCAS, key); err != nil {
		t.Errorf("replica write did not store locally: %v", err)
	}
}

// TestReadHopIsTerminal: a forwarded read is served locally or missed. It must
// never fan out again -- that is the read-side loop edge.
func TestReadHopIsTerminal(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyNotHeldBy(t, f.ring, "node-a", 2)
	f.peers.put(f.ring.Holders(key, 2)[0].Name, storage.NamespaceCAS, key, content)

	_, _, err := readAll(t, f.coord, storage.NamespaceCAS, key, protocol.HopRead)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("forwarded read = %v, want ErrNotFound (it must not fan out)", err)
	}
	if _, gets, _ := f.peers.counts(); len(gets) != 0 {
		t.Errorf("a forwarded read fanned out to peers: %v", gets)
	}
}

// TestPutFromNonOwnerIsProxied: a front door that does not own the key hands
// the body to an owner and stores nothing itself. Storing a copy here would be
// a third copy nobody asked for, on whichever node the client happened to pick.
func TestPutFromNonOwnerIsProxied(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyNotHeldBy(t, f.ring, "node-a", 2)

	out, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopClient)
	if err != nil {
		t.Fatalf("proxied Put: %v", err)
	}
	if out.Copies != 2 {
		t.Errorf("copies = %d, want 2", out.Copies)
	}
	if _, err := f.store.Stat(storage.NamespaceCAS, key); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the front door stored a copy it does not own: %v", err)
	}
	hops := f.peers.recordedHops()
	if len(hops) != 1 || hops[0] != protocol.HopCoordinator {
		t.Errorf("proxy hops = %v, want exactly one coordinator hop", hops)
	}
}

// TestReplicaWriteFailureIsA503Path is the Phase 2 acceptance criterion for the
// replica-write-failure path: when the second copy cannot be written, the PUT
// must fail loudly rather than report success with one copy.
func TestReplicaWriteFailureIsA503Path(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyOwnedBy(t, f.ring, "node-a", 2)
	replica := f.ring.Holders(key, 2)[1].Name
	f.peers.setDown(replica, true)

	out, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopClient)
	if err == nil {
		t.Fatal("Put succeeded with the replica down")
	}
	if !errors.Is(err, protocol.ErrInsufficientReplicas) {
		t.Fatalf("err = %v, want ErrInsufficientReplicas", err)
	}
	if out.Copies != 1 || out.Wanted != 2 {
		t.Errorf("outcome reports %d of %d copies, want 1 of 2", out.Copies, out.Wanted)
	}

	// The local copy exists; that is fine and honest. What matters is that the
	// client was told the write did not meet its durability contract.
	if _, err := f.store.Stat(storage.NamespaceCAS, key); err != nil {
		t.Errorf("local copy is missing even though the local write succeeded: %v", err)
	}
	var insufficient *InsufficientReplicasError
	if !errors.As(err, &insufficient) {
		t.Fatalf("error does not carry the detail: %v", err)
	}
	if len(insufficient.Causes) == 0 {
		t.Error("error does not say why the replica write failed")
	}
}

// TestReplicaRejectionIsNotRetriedElsewhere: a peer that returns 400 has
// rejected the object itself, and every other peer will reject it identically.
func TestReplicaRejectionIsNotRetriedElsewhere(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyOwnedBy(t, f.ring, "node-a", 2)
	replica := f.ring.Holders(key, 2)[1].Name
	f.peers.setRejecting(replica, http.StatusBadRequest)

	_, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopClient)
	if !errors.Is(err, protocol.ErrInsufficientReplicas) {
		t.Fatalf("err = %v, want ErrInsufficientReplicas", err)
	}
	puts, _, _ := f.peers.counts()
	if puts[replica] != 1 {
		t.Errorf("the rejecting replica was contacted %d times, want 1", puts[replica])
	}
	for name, n := range puts {
		if name != replica && n > 0 {
			t.Errorf("a rejected object was offered to %s as well", name)
		}
	}
}

// TestReadFallsBackToReplica is the read-side Phase 2 claim: losing the node
// that holds the local copy costs latency, not correctness.
func TestReadFallsBackToReplica(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	// A key this node does not hold, so both copies really are on peers and the
	// fallback path is the only way to read it.
	key, content := keyNotHeldBy(t, f.ring, "node-a", 2)
	holders := f.ring.Holders(key, 2)
	primary, replica := holders[0].Name, holders[1].Name

	f.peers.put(primary, storage.NamespaceCAS, key, content)
	f.peers.put(replica, storage.NamespaceCAS, key, content)
	f.peers.setDown(primary, true)

	got, source, err := readAll(t, f.coord, storage.NamespaceCAS, key, protocol.HopClient)
	if err != nil {
		t.Fatalf("read with the primary down: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("fallback read returned different bytes")
	}
	if source == primary {
		t.Errorf("source = %s, but the primary was down", source)
	}
	if f.coord.Snapshot().ReadFallbacks == 0 {
		t.Error("fallback was not counted")
	}
}

// TestReadPrefersLocalCopy: even when this node is not a holder, a local copy
// is served rather than fetched over the network. Placement is advisory --
// repair and membership changes both leave objects on non-holders.
func TestReadPrefersLocalCopy(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyNotHeldBy(t, f.ring, "node-a", 2)

	if _, err := f.store.Put(context.Background(), storage.NamespaceCAS, key,
		bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("seed local copy: %v", err)
	}
	for _, m := range f.ring.Holders(key, 2) {
		f.peers.put(m.Name, storage.NamespaceCAS, key, content)
	}

	got, source, err := readAll(t, f.coord, storage.NamespaceCAS, key, protocol.HopClient)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if source != "local" {
		t.Errorf("source = %q, want local", source)
	}
	if !bytes.Equal(got, content) {
		t.Error("local read returned different bytes")
	}
	if _, gets, _ := f.peers.counts(); len(gets) != 0 {
		t.Errorf("a local hit still contacted peers: %v", gets)
	}
}

// TestAllHoldersUnreachableIsNotAMiss: a partition must not look like a cold
// cache. If it did, Bazel would rebuild everything instead of reporting a
// problem -- the expensive failure this distinction exists to prevent.
func TestAllHoldersUnreachableIsNotAMiss(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyNotHeldBy(t, f.ring, "node-a", 2)
	for _, m := range f.ring.Members() {
		f.peers.put(m.Name, storage.NamespaceCAS, key, content)
		if m.Name != "node-a" {
			f.peers.setDown(m.Name, true)
		}
	}

	_, _, err := readAll(t, f.coord, storage.NamespaceCAS, key, protocol.HopClient)
	if err == nil {
		t.Fatal("read succeeded with every holder down")
	}
	if errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("an unreachable cluster reported a cache miss: %v", err)
	}
}

// TestGenuineMissIsAMiss is the complement: when peers are up and simply do not
// have the object, that is a miss and must be reported as one.
func TestGenuineMissIsAMiss(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, _ := keyNotHeldBy(t, f.ring, "node-a", 2)

	_, _, err := readAll(t, f.coord, storage.NamespaceCAS, key, protocol.HopClient)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSlowPeerHitsTheHopTimeout covers the per-hop timeout: a peer that is
// reachable but hung must not hold a client request open indefinitely.
func TestSlowPeerHitsTheHopTimeout(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyOwnedBy(t, f.ring, "node-a", 2)
	replica := f.ring.Holders(key, 2)[1].Name
	f.peers.setSlow(replica, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := f.coord.Put(ctx, storage.NamespaceCAS, key, bytes.NewReader(content), int64(len(content)), protocol.HopClient)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Put succeeded against a hung replica")
	}
	if elapsed > time.Second {
		t.Errorf("Put took %v; the hop deadline did not apply", elapsed)
	}
}

// TestTruncatingPeerIsAcceptedThenDetectedByRepair documents a real limit: a
// peer that acknowledges a write it did not persist is indistinguishable from a
// successful one at write time. The write therefore succeeds, and the missing
// copy is the repair worker's problem (Phase 3). Writing this down as a test
// keeps the claim in docs/failure-model.md honest.
func TestTruncatingPeerIsAcceptedThenDetectedByRepair(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyOwnedBy(t, f.ring, "node-a", 2)
	replica := f.ring.Holders(key, 2)[1].Name
	f.peers.setTruncating(replica, true)

	out, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopClient)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if out.Copies != 2 {
		t.Fatalf("copies = %d; the coordinator cannot detect a lying peer at write time", out.Copies)
	}
	if f.peers.has(replica, storage.NamespaceCAS, key) {
		t.Fatal("the fixture did not actually drop the object; the test proves nothing")
	}
	// The second copy is absent despite a successful PUT. Phase 3's repair
	// auditor is what closes this gap.
}

func TestSingleNodeCoordinatorIsLocalOnly(t *testing.T) {
	f := newFixture(t, "solo", 1, "solo")
	content := []byte("single node")
	key := digest(content)

	out, err := put(t, f.coord, storage.NamespaceCAS, key, content, protocol.HopClient)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if out.Copies != 1 || out.Wanted != 1 {
		t.Errorf("copies = %d of %d, want 1 of 1", out.Copies, out.Wanted)
	}
	got, source, err := readAll(t, f.coord, storage.NamespaceCAS, key, protocol.HopClient)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if source != "local" || !bytes.Equal(got, content) {
		t.Errorf("source = %q, bytes equal = %v", source, bytes.Equal(got, content))
	}
}

func TestReplicaCountClampedToClusterSize(t *testing.T) {
	ring, err := New(peers("a", "b"), "a")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(storage.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	c, err := NewCoordinator(CoordinatorOptions{Ring: ring, Local: store, ReplicaCount: 9})
	if err != nil {
		t.Fatal(err)
	}
	if c.ReplicaCount() != 2 {
		t.Errorf("ReplicaCount = %d, want 2 (the cluster size)", c.ReplicaCount())
	}
}

func TestNewCoordinatorRejectsMissingParts(t *testing.T) {
	store, err := storage.Open(storage.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ring, _ := New(peers("a"), "a")

	if _, err := NewCoordinator(CoordinatorOptions{Local: store}); err == nil {
		t.Error("NewCoordinator without a ring = nil error")
	}
	if _, err := NewCoordinator(CoordinatorOptions{Ring: ring}); err == nil {
		t.Error("NewCoordinator without a store = nil error")
	}
}

// TestStatFallsBackLikeRead keeps HEAD and GET consistent. A HEAD that missed
// where a GET would have hit would make Bazel skip an object it could have had.
func TestStatFallsBackLikeRead(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, content := keyNotHeldBy(t, f.ring, "node-a", 2)
	holders := f.ring.Holders(key, 2)
	f.peers.put(holders[1].Name, storage.NamespaceCAS, key, content)
	f.peers.setDown(holders[0].Name, true)

	info, err := f.coord.Stat(context.Background(), storage.NamespaceCAS, key, protocol.HopClient)
	if err != nil {
		t.Fatalf("Stat with the primary down: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", info.Size, len(content))
	}
}

func TestACReplicatesLikeCAS(t *testing.T) {
	f := newFixture(t, "node-a", 2)
	key, _ := keyOwnedBy(t, f.ring, "node-a", 2)
	content := []byte("an ActionResult that does not hash to its key")
	replica := f.ring.Holders(key, 2)[1].Name

	out, err := put(t, f.coord, storage.NamespaceAC, key, content, protocol.HopClient)
	if err != nil {
		t.Fatalf("AC Put: %v", err)
	}
	if out.Copies != 2 {
		t.Errorf("AC copies = %d, want 2", out.Copies)
	}
	if !f.peers.has(replica, storage.NamespaceAC, key) {
		t.Error("AC entry was not replicated")
	}
}
