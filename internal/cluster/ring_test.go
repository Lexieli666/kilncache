package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/Lexieli666/kilncache/internal/config"
)

func peers(names ...string) []config.Peer {
	out := make([]config.Peer, 0, len(names))
	for _, n := range names {
		out = append(out, config.Peer{Name: n, URL: "http://" + n + ":8080"})
	}
	return out
}

func mustRing(t *testing.T, self string, names ...string) *Ring {
	t.Helper()
	r, err := New(peers(names...), self)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// key returns a realistic object key: a lowercase hex SHA-256.
func key(i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("object-%d", i)))
	return hex.EncodeToString(sum[:])
}

func TestNewRejectsBadMembership(t *testing.T) {
	if _, err := New(nil, "a"); err == nil {
		t.Error("New with no peers = nil error")
	}
	if _, err := New(peers("a", "a"), "a"); err == nil {
		t.Error("New with a duplicate member = nil error")
	}
	if _, err := New(peers("a", "b"), "z"); err == nil {
		t.Error("New with self outside the membership = nil error")
	}
	if _, err := New([]config.Peer{{Name: "  ", URL: "http://x"}}, ""); err == nil {
		t.Error("New with an empty member name = nil error")
	}
}

// TestOrderIndependence is the falsifier for "two nodes given the same peers in
// a different order agree about placement". If it fails, a cluster whose
// operators wrote --peers in different orders silently disagrees about where
// every object lives, and half the reads become misses.
func TestOrderIndependence(t *testing.T) {
	a := mustRing(t, "node-a", "node-a", "node-b", "node-c")
	b := mustRing(t, "node-b", "node-c", "node-a", "node-b")

	for i := 0; i < 5000; i++ {
		k := key(i)
		oa, ob := a.Owners(k), b.Owners(k)
		for j := range oa {
			if oa[j].Name != ob[j].Name {
				t.Fatalf("key %s: ring A says %v, ring B says %v", k[:8], names(oa), names(ob))
			}
		}
	}
}

func names(ms []Member) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

func TestOwnersIsAPermutation(t *testing.T) {
	r := mustRing(t, "node-a", "node-a", "node-b", "node-c", "node-d", "node-e")
	for i := 0; i < 2000; i++ {
		owners := r.Owners(key(i))
		if len(owners) != r.Size() {
			t.Fatalf("Owners returned %d members, want %d", len(owners), r.Size())
		}
		seen := map[string]bool{}
		for _, m := range owners {
			if seen[m.Name] {
				t.Fatalf("key %s: %s appears twice in the preference list", key(i)[:8], m.Name)
			}
			seen[m.Name] = true
		}
	}
}

// TestPrimaryAndReplicaAreDistinct is a Phase 2 acceptance criterion. If the
// two copies of an object could land on one node, replication would be a no-op
// and every node-loss test would be passing for the wrong reason.
func TestPrimaryAndReplicaAreDistinct(t *testing.T) {
	r := mustRing(t, "node-a", "node-a", "node-b", "node-c")
	for i := 0; i < 50000; i++ {
		h := r.Holders(key(i), 2)
		if len(h) != 2 {
			t.Fatalf("Holders returned %d members, want 2", len(h))
		}
		if h[0].Name == h[1].Name {
			t.Fatalf("key %s: primary and replica are both %s", key(i)[:8], h[0].Name)
		}
	}
}

func TestDeterministicAcrossCalls(t *testing.T) {
	r := mustRing(t, "node-a", "node-a", "node-b", "node-c")
	for i := 0; i < 1000; i++ {
		k := key(i)
		first := names(r.Owners(k))
		for j := 0; j < 5; j++ {
			if got := names(r.Owners(k)); strings.Join(got, ",") != strings.Join(first, ",") {
				t.Fatalf("key %s: Owners is not deterministic: %v then %v", k[:8], first, got)
			}
		}
	}
}

// imbalance returns max(load)/mean(load) over the primary assignment.
func imbalance(counts map[string]int, members int) float64 {
	if len(counts) == 0 {
		return 0
	}
	total := 0
	maxLoad := 0
	for _, c := range counts {
		total += c
		if c > maxLoad {
			maxLoad = c
		}
	}
	// Nodes that received nothing still count towards the mean.
	mean := float64(total) / float64(members)
	if mean == 0 {
		return 0
	}
	return float64(maxLoad) / mean
}

// TestPlacementBalance is the Phase 2 acceptance criterion "imbalance max/mean
// <= 1.10 over >= 50,000 keys". The measured value is logged so the number that
// reaches BENCHMARKS.md comes from a run, not from theory.
func TestPlacementBalance(t *testing.T) {
	const keys = 50000
	for _, size := range []int{3, 4, 5, 8} {
		memberNames := make([]string, size)
		for i := range memberNames {
			memberNames[i] = fmt.Sprintf("node-%c", 'a'+i)
		}
		r := mustRing(t, memberNames[0], memberNames...)

		primary := map[string]int{}
		anyHolder := map[string]int{}
		for i := 0; i < keys; i++ {
			h := r.Holders(key(i), 2)
			primary[h[0].Name]++
			for _, m := range h {
				anyHolder[m.Name]++
			}
		}

		pImb := imbalance(primary, size)
		hImb := imbalance(anyHolder, size)
		t.Logf("cluster of %d over %d keys: primary imbalance max/mean = %.4f, holder imbalance = %.4f",
			size, keys, pImb, hImb)

		if pImb > 1.10 {
			t.Errorf("cluster of %d: primary imbalance %.4f exceeds 1.10", size, pImb)
		}
		if hImb > 1.10 {
			t.Errorf("cluster of %d: holder imbalance %.4f exceeds 1.10", size, hImb)
		}
	}
}

// TestKeyMovementOnGrowth is the other Phase 2 acceptance criterion. Theory for
// rendezvous hashing says adding the fourth node of four moves 1/4 of primary
// assignments; the spec allows 20-35% and asks for the measured fraction.
//
// The comparison that matters is with modulo hashing, which would move about
// 3/4 of keys here. That is the entire reason this project does not use it.
func TestKeyMovementOnGrowth(t *testing.T) {
	const keys = 50000
	before := mustRing(t, "node-a", "node-a", "node-b", "node-c")
	after := mustRing(t, "node-a", "node-a", "node-b", "node-c", "node-d")

	movedPrimary := 0
	changedHolderSet := 0
	for i := 0; i < keys; i++ {
		k := key(i)
		b := before.Holders(k, 2)
		a := after.Holders(k, 2)
		if b[0].Name != a[0].Name {
			movedPrimary++
		}
		if !sameSet(b, a) {
			changedHolderSet++
		}
	}

	primaryFrac := float64(movedPrimary) / keys
	holderFrac := float64(changedHolderSet) / keys
	t.Logf("adding node 4 of 4 over %d keys: %.2f%% of primaries moved (%d keys), "+
		"%.2f%% of two-node holder sets changed (%d keys)",
		keys, primaryFrac*100, movedPrimary, holderFrac*100, changedHolderSet)

	if primaryFrac > 0.35 {
		t.Errorf("primary movement %.4f exceeds the 0.35 bound", primaryFrac)
	}
	// The theoretical value is 1/4. Far below it would mean the hash is not
	// actually redistributing, which is a bug that looks like good news.
	if primaryFrac < 0.15 {
		t.Errorf("primary movement %.4f is implausibly low; theory says ~0.25", primaryFrac)
	}
}

func sameSet(a, b []Member) bool {
	if len(a) != len(b) {
		return false
	}
	an, bn := names(a), names(b)
	sort.Strings(an)
	sort.Strings(bn)
	for i := range an {
		if an[i] != bn[i] {
			return false
		}
	}
	return true
}

// TestKeyMovementOnLoss covers the other direction: removing a node must move
// only the keys that node held, not reshuffle everything.
func TestKeyMovementOnLoss(t *testing.T) {
	const keys = 20000
	before := mustRing(t, "node-a", "node-a", "node-b", "node-c", "node-d")
	after := mustRing(t, "node-a", "node-a", "node-b", "node-c")

	moved, heldByD := 0, 0
	for i := 0; i < keys; i++ {
		k := key(i)
		b := before.Holders(k, 2)
		a := after.Holders(k, 2)
		if b[0].Name == "node-d" {
			heldByD++
		}
		if b[0].Name != a[0].Name {
			moved++
			if b[0].Name != "node-d" {
				t.Fatalf("key %s moved from %s to %s even though node-d was not its primary",
					k[:8], b[0].Name, a[0].Name)
			}
		}
	}
	t.Logf("removing node-d over %d keys: %d primaries moved, all of them node-d's (%d)", keys, moved, heldByD)
	if moved != heldByD {
		t.Errorf("moved %d primaries but node-d held %d", moved, heldByD)
	}
}

func TestSelfHoldsAndPrimary(t *testing.T) {
	r := mustRing(t, "node-b", "node-a", "node-b", "node-c")
	held, total := 0, 3000
	for i := 0; i < total; i++ {
		k := key(i)
		if r.SelfHolds(k, 2) {
			held++
		}
		if r.Primary(k).Name != r.Owners(k)[0].Name {
			t.Fatalf("Primary disagrees with Owners[0] for %s", k[:8])
		}
	}
	// With RF=2 over 3 nodes, a node should hold about two thirds of all keys.
	frac := float64(held) / float64(total)
	t.Logf("node-b holds %.1f%% of %d keys with RF=2 over 3 nodes (expected ~66.7%%)", frac*100, total)
	if frac < 0.55 || frac > 0.78 {
		t.Errorf("SelfHolds fraction %.3f is outside the plausible range for RF=2 of 3", frac)
	}
}

func TestHoldersClampsToClusterSize(t *testing.T) {
	r := mustRing(t, "node-a", "node-a", "node-b")
	if got := len(r.Holders(key(1), 5)); got != 2 {
		t.Errorf("Holders(key, 5) on a 2-node ring returned %d members, want 2", got)
	}
	if got := len(r.Holders(key(1), 0)); got != 1 {
		t.Errorf("Holders(key, 0) returned %d members, want 1", got)
	}
}

func TestSingleNodeRing(t *testing.T) {
	r := mustRing(t, "solo", "solo")
	for i := 0; i < 100; i++ {
		h := r.Holders(key(i), 2)
		if len(h) != 1 || h[0].Name != "solo" {
			t.Fatalf("single-node ring returned %v", names(h))
		}
		if !r.SelfHolds(key(i), 2) {
			t.Fatal("single-node ring does not hold its own key")
		}
	}
}

// TestNonHexKeyIsPlaceable guards the fallback path. The store rejects such
// keys, but placement must not panic on them: a crash in a lookup turns a 404
// into an outage.
func TestNonHexKeyIsPlaceable(t *testing.T) {
	r := mustRing(t, "node-a", "node-a", "node-b", "node-c")
	for _, k := range []string{"", "x", "NOT-HEX", strings.Repeat("z", 64), "../../etc/passwd"} {
		owners := r.Owners(k)
		if len(owners) != 3 {
			t.Errorf("Owners(%q) returned %d members", k, len(owners))
		}
	}
}

// --- property tests -------------------------------------------------------

func hexKeyGen() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		b := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "digest")
		return hex.EncodeToString(b)
	})
}

// TestPropOwnersDeterministic: for any key and any membership, two rings built
// from the same peers in any order produce the same preference list.
func TestPropOwnersDeterministic(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		size := rapid.IntRange(1, 8).Draw(rt, "size")
		memberNames := make([]string, size)
		for i := range memberNames {
			memberNames[i] = fmt.Sprintf("node-%d", i)
		}
		shuffled := append([]string(nil), memberNames...)
		perm := rapid.Permutation(shuffled).Draw(rt, "order")

		r1, err := New(peers(memberNames...), memberNames[0])
		if err != nil {
			rt.Fatalf("New: %v", err)
		}
		r2, err := New(peers(perm...), memberNames[0])
		if err != nil {
			rt.Fatalf("New: %v", err)
		}

		k := hexKeyGen().Draw(rt, "key")
		if a, b := names(r1.Owners(k)), names(r2.Owners(k)); strings.Join(a, ",") != strings.Join(b, ",") {
			rt.Fatalf("order changed placement for %s: %v vs %v", k[:8], a, b)
		}
	})
}

// TestPropHoldersAreDistinct: for any key and any cluster of at least two
// nodes, the requested number of holders are all different nodes.
func TestPropHoldersAreDistinct(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		size := rapid.IntRange(2, 8).Draw(rt, "size")
		n := rapid.IntRange(1, size).Draw(rt, "replicas")
		memberNames := make([]string, size)
		for i := range memberNames {
			memberNames[i] = fmt.Sprintf("node-%d", i)
		}
		r, err := New(peers(memberNames...), memberNames[0])
		if err != nil {
			rt.Fatalf("New: %v", err)
		}
		k := hexKeyGen().Draw(rt, "key")
		h := r.Holders(k, n)
		if len(h) != n {
			rt.Fatalf("Holders(%s, %d) returned %d", k[:8], n, len(h))
		}
		seen := map[string]bool{}
		for _, m := range h {
			if seen[m.Name] {
				rt.Fatalf("duplicate holder %s for key %s", m.Name, k[:8])
			}
			seen[m.Name] = true
		}
	})
}

// TestPropAddingANodeOnlyMovesToTheNewNode is the structural property behind
// the movement bound: when a node joins, a key either stays where it was or
// moves to the newcomer. Nothing shuffles between existing nodes. A hash that
// violated this would still pass a crude "less than 35% moved" check while
// churning the cluster.
func TestPropAddingANodeOnlyMovesToTheNewNode(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		size := rapid.IntRange(1, 6).Draw(rt, "size")
		memberNames := make([]string, size)
		for i := range memberNames {
			memberNames[i] = fmt.Sprintf("node-%d", i)
		}
		newcomer := fmt.Sprintf("node-%d", size)

		before, err := New(peers(memberNames...), memberNames[0])
		if err != nil {
			rt.Fatalf("New: %v", err)
		}
		after, err := New(peers(append(append([]string(nil), memberNames...), newcomer)...), memberNames[0])
		if err != nil {
			rt.Fatalf("New: %v", err)
		}

		k := hexKeyGen().Draw(rt, "key")
		b := before.Primary(k).Name
		a := after.Primary(k).Name
		if a != b && a != newcomer {
			rt.Fatalf("key %s moved from %s to %s; only moves to the newcomer %s are allowed",
				k[:8], b, a, newcomer)
		}
	})
}

// TestPropWeightsAreWellSpread is a sanity property on the hash itself: over a
// batch of random keys, no single member of a small cluster takes an absurd
// share. It is weaker than TestPlacementBalance but runs against rapid's
// adversarially chosen inputs rather than a fixed sequence.
func TestPropWeightsAreWellSpread(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		const batch = 2000
		size := rapid.IntRange(2, 5).Draw(rt, "size")
		prefix := rapid.StringMatching(`[a-z]{1,6}`).Draw(rt, "prefix")
		memberNames := make([]string, size)
		for i := range memberNames {
			memberNames[i] = fmt.Sprintf("%s-%d", prefix, i)
		}
		r, err := New(peers(memberNames...), memberNames[0])
		if err != nil {
			rt.Fatalf("New: %v", err)
		}
		counts := map[string]int{}
		base := rapid.SliceOfN(rapid.Byte(), 8, 8).Draw(rt, "base")
		for i := 0; i < batch; i++ {
			sum := sha256.Sum256(append(base, byte(i), byte(i>>8)))
			counts[r.Primary(hex.EncodeToString(sum[:])).Name]++
		}
		// A generous bound: this is a smoke test against a broken hash, not a
		// statistical claim. TestPlacementBalance makes the tight one.
		if imb := imbalance(counts, size); imb > 1.35 {
			rt.Fatalf("cluster %v: imbalance %.3f over %d keys is too high; counts %v",
				memberNames, imb, batch, counts)
		}
	})
}

func TestMixAvalanche(t *testing.T) {
	// A one-bit change in the input must change about half the output bits. If
	// mix ever degrades, every balance property above degrades with it, and
	// this says so directly instead of via a statistical test.
	var total, samples int
	for i := uint64(0); i < 1000; i++ {
		base := mix(i * 0x9e3779b97f4a7c15)
		for bit := 0; bit < 64; bit++ {
			flipped := mix((i * 0x9e3779b97f4a7c15) ^ (1 << bit))
			total += popcount(base ^ flipped)
			samples++
		}
	}
	avg := float64(total) / float64(samples)
	t.Logf("mix avalanche: %.2f of 64 bits change per input bit flip", avg)
	if math.Abs(avg-32) > 2 {
		t.Errorf("avalanche = %.2f bits, want ~32", avg)
	}
}

func popcount(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}
