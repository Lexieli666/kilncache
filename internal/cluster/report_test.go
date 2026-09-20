package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// placementReport is the raw result file behind every placement number that
// appears in README.md or BENCHMARKS.md.
//
// CONTRIBUTING rule 1 says a published figure must be reproducible by a
// committed command with committed raw output. The balance and key-movement
// tests already compute these values; without this they would only ever exist
// in a test log that nobody commits, and the number in the README would be a
// figure someone once saw. Set KILNCACHE_RESULTS_DIR (or run `make
// placement-report`) and the same computation writes its output to disk.
type placementReport struct {
	Kind        string    `json:"kind"`
	GeneratedAt time.Time `json:"generated_at"`
	Keys        int       `json:"keys"`
	KeyKind     string    `json:"key_kind"`

	Balance  []balanceRow  `json:"balance"`
	Movement []movementRow `json:"movement"`
}

type balanceRow struct {
	ClusterSize      int     `json:"cluster_size"`
	ReplicaCount     int     `json:"replica_count"`
	PrimaryImbalance float64 `json:"primary_imbalance_max_over_mean"`
	HolderImbalance  float64 `json:"holder_imbalance_max_over_mean"`
	MinPrimaryLoad   int     `json:"min_primary_load"`
	MaxPrimaryLoad   int     `json:"max_primary_load"`
}

type movementRow struct {
	Before                int     `json:"cluster_size_before"`
	After                 int     `json:"cluster_size_after"`
	ReplicaCount          int     `json:"replica_count"`
	PrimariesMoved        int     `json:"primaries_moved"`
	PrimaryMovedFraction  float64 `json:"primary_moved_fraction"`
	TheoreticalFraction   float64 `json:"theoretical_fraction"`
	HolderSetsChanged     int     `json:"holder_sets_changed"`
	HolderChangedFraction float64 `json:"holder_set_changed_fraction"`
}

// TestWritePlacementReport emits the raw result file. It is a no-op unless
// KILNCACHE_RESULTS_DIR is set, so the normal test run stays side-effect free.
func TestWritePlacementReport(t *testing.T) {
	dir := os.Getenv("KILNCACHE_RESULTS_DIR")
	if dir == "" {
		t.Skip("set KILNCACHE_RESULTS_DIR to write the placement report (make placement-report)")
	}
	const keys = 50000

	rep := placementReport{
		Kind:        "placement",
		GeneratedAt: time.Now().UTC(),
		Keys:        keys,
		KeyKind:     "lowercase hex SHA-256 of \"object-<i>\" for i in [0,50000)",
	}

	for _, size := range []int{3, 4, 5, 8} {
		memberNames := make([]string, size)
		for i := range memberNames {
			memberNames[i] = fmt.Sprintf("node-%c", 'a'+i)
		}
		r := mustRing(t, memberNames[0], memberNames...)

		primary := map[string]int{}
		holder := map[string]int{}
		for i := range memberNames {
			primary[memberNames[i]] = 0
			holder[memberNames[i]] = 0
		}
		for i := 0; i < keys; i++ {
			h := r.Holders(key(i), 2)
			primary[h[0].Name]++
			for _, m := range h {
				holder[m.Name]++
			}
		}
		minLoad, maxLoad := keys, 0
		for _, c := range primary {
			if c < minLoad {
				minLoad = c
			}
			if c > maxLoad {
				maxLoad = c
			}
		}
		rep.Balance = append(rep.Balance, balanceRow{
			ClusterSize:      size,
			ReplicaCount:     2,
			PrimaryImbalance: imbalance(primary, size),
			HolderImbalance:  imbalance(holder, size),
			MinPrimaryLoad:   minLoad,
			MaxPrimaryLoad:   maxLoad,
		})
	}

	for _, growth := range []struct{ from, to int }{{3, 4}, {4, 5}, {5, 6}} {
		beforeNames := make([]string, growth.from)
		for i := range beforeNames {
			beforeNames[i] = fmt.Sprintf("node-%c", 'a'+i)
		}
		afterNames := make([]string, growth.to)
		for i := range afterNames {
			afterNames[i] = fmt.Sprintf("node-%c", 'a'+i)
		}
		before := mustRing(t, beforeNames[0], beforeNames...)
		after := mustRing(t, afterNames[0], afterNames...)

		moved, changed := 0, 0
		for i := 0; i < keys; i++ {
			k := key(i)
			b := before.Holders(k, 2)
			a := after.Holders(k, 2)
			if b[0].Name != a[0].Name {
				moved++
			}
			if !sameSet(b, a) {
				changed++
			}
		}
		rep.Movement = append(rep.Movement, movementRow{
			Before:                growth.from,
			After:                 growth.to,
			ReplicaCount:          2,
			PrimariesMoved:        moved,
			PrimaryMovedFraction:  float64(moved) / keys,
			TheoreticalFraction:   1 / float64(growth.to),
			HolderSetsChanged:     changed,
			HolderChangedFraction: float64(changed) / keys,
		})
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create results dir: %v", err)
	}
	path := filepath.Join(dir, "placement.json")
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %s", path)
	for _, row := range rep.Balance {
		t.Logf("  cluster of %d: primary imbalance %.4f, holder imbalance %.4f",
			row.ClusterSize, row.PrimaryImbalance, row.HolderImbalance)
	}
	for _, row := range rep.Movement {
		t.Logf("  %d -> %d nodes: %.2f%% of primaries moved (theory %.2f%%), %.2f%% of holder sets changed",
			row.Before, row.After, row.PrimaryMovedFraction*100,
			row.TheoreticalFraction*100, row.HolderChangedFraction*100)
	}
}
