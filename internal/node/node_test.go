package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/storage"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.NodeName = "node-a"
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	cfg.Peers = []config.Peer{{Name: "node-a", URL: "http://127.0.0.1:0"}}
	cfg.ReplicaCount = 1
	cfg.MaxBytes = 16 << 20
	cfg.ShutdownTimeout = 2 * time.Second
	cfg.RepairInterval = time.Hour
	return cfg
}

func newNode(t *testing.T, mutate func(*config.Config)) *Node {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n, err := New(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.NodeName = "not-a-member"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(context.Background(), cfg, log); err == nil {
		t.Fatal("New accepted a node that is not in its own peer list")
	}
}

func TestNewAssemblesEverySubsystem(t *testing.T) {
	n := newNode(t, nil)
	if n.Store() == nil || n.Ring() == nil || n.Coordinator() == nil || n.Evictor() == nil {
		t.Fatal("a subsystem is missing")
	}
	if n.Repair() == nil {
		t.Error("repair worker is nil with the default repair-workers setting")
	}
	if n.Store().Index() == nil {
		t.Error("the metadata index is not open")
	}
	if n.Name() != "node-a" {
		t.Errorf("Name = %q", n.Name())
	}
	if !strings.HasPrefix(n.BaseURL(), "http://127.0.0.1:") {
		t.Errorf("BaseURL = %q", n.BaseURL())
	}
}

func TestRepairCanBeDisabled(t *testing.T) {
	n := newNode(t, func(c *config.Config) { c.RepairWorkers = 0 })
	if n.Repair() != nil {
		t.Error("repair worker was built despite repair-workers=0")
	}
	// And the node must still serve: repair is optional, storage is not.
	rec := httptest.NewRecorder()
	n.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d with repair disabled", rec.Code)
	}
}

// TestStatsReportEverySubsystem is the falsifier for "the operator can see what
// the node is doing". A subsystem missing from /stats is one nobody can observe
// during an incident.
func TestStatsReportEverySubsystem(t *testing.T) {
	n := newNode(t, nil)

	content := []byte("stats payload")
	sum := sha256.Sum256(content)
	key := hex.EncodeToString(sum[:])
	if _, err := n.Store().Put(context.Background(), storage.NamespaceCAS, key,
		bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}

	stats := n.Stats()
	for _, want := range []string{"cluster", "storage", "index", "usage", "coordinator", "eviction", "repair"} {
		if _, ok := stats[want]; !ok {
			t.Errorf("/stats has no %q section", want)
		}
	}

	usage, ok := stats["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage section is %T", stats["usage"])
	}
	if usage["bytes"].(int64) != int64(len(content)) {
		t.Errorf("usage bytes = %v, want %d", usage["bytes"], len(content))
	}
	if usage["over_high_water"].(bool) {
		t.Error("a nearly empty node reports itself over the high-water mark")
	}

	// It must survive JSON encoding: /stats is read by the chaos runner, and a
	// type that cannot be marshalled would break it at the worst moment.
	if _, err := json.Marshal(stats); err != nil {
		t.Fatalf("stats do not marshal: %v", err)
	}
}

func TestStatsEndpointServesJSON(t *testing.T) {
	n := newNode(t, nil)
	rec := httptest.NewRecorder()
	n.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("/stats = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/stats is not JSON: %v", err)
	}
	if body["node"] != "node-a" {
		t.Errorf("node = %v", body["node"])
	}
}

// TestMetricsEndpointExposesEverySource is the falsifier for the dashboard. It
// catches the failure that crash-looped every node once already: two series of
// one metric registered with different help strings (docs/bugs.md, entry 10).
func TestMetricsEndpointExposesEverySource(t *testing.T) {
	n := newNode(t, nil)

	rec := httptest.NewRecorder()
	n.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"kilncache_stored_bytes",
		"kilncache_stored_objects",
		"kilncache_quota_bytes",
		"kilncache_high_water_bytes",
		"kilncache_evictions_total",
		"kilncache_hits_total",
		"kilncache_misses_total",
		"kilncache_verify_failures_total",
		"kilncache_insufficient_replicas_total",
		"kilncache_replication_failures_total",
		"kilncache_repair_queue_depth",
		"kilncache_repair_completed_total",
		"kilncache_index_touches_dropped_total",
		"go_goroutines",
		"process_open_fds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not expose %s", want)
		}
	}
}

// TestRunStartsAndStopsBackgroundWorkers covers the lifecycle: readiness is
// announced only once the store is open, and shutdown stops the workers.
func TestRunStartsAndStopsBackgroundWorkers(t *testing.T) {
	n := newNode(t, nil)

	if ready, _ := n.Health().Ready(); ready {
		t.Error("the node reported ready before Run")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready, _ := n.Health().Ready(); ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready, reason := n.Health().Ready(); !ready {
		t.Fatalf("node never became ready: %s", reason)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	if ready, _ := n.Health().Ready(); ready {
		t.Error("the node still reports ready after shutdown")
	}
	// Close after Run must be safe: both call stopBackground.
	if err := n.Close(); err != nil {
		t.Errorf("Close after Run: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	n := newNode(t, nil)
	if err := n.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestQuotaProbeReflectsUsage: the probe is what lets a node decline repair
// writes when it is full, so it has to track real usage rather than a constant.
func TestQuotaProbeReflectsUsage(t *testing.T) {
	n := newNode(t, func(c *config.Config) {
		c.MaxBytes = 64 << 10
		c.HighWater = 0.5
		c.LowWater = 0.25
	})

	stats := n.Stats()
	usage := stats["usage"].(map[string]any)
	if usage["over_high_water"].(bool) {
		t.Fatal("an empty node reports itself over the high-water mark")
	}

	big := bytes.Repeat([]byte("x"), 48<<10)
	sum := sha256.Sum256(big)
	key := hex.EncodeToString(sum[:])
	if _, err := n.Store().Put(context.Background(), storage.NamespaceCAS, key,
		bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatal(err)
	}

	usage = n.Stats()["usage"].(map[string]any)
	if !usage["over_high_water"].(bool) {
		t.Errorf("48 KiB against a 32 KiB high-water mark does not read as over quota: %v", usage)
	}
}
