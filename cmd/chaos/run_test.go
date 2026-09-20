//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/node"
)

// TestControlRunAgainstRealNodes drives the whole chaos tool against a real
// three-node cluster running in this process, with faults disabled.
//
// It is the falsifier for the tool itself. An overnight run depends on this
// code to decide whether the system passed, and a bug in the driver invalidates
// the result rather than merely reporting it wrongly -- which has happened
// (docs/bugs.md, entry 9). Faults are off because stopping a container needs
// Docker; the fault scheduler has its own coverage in the Docker tier, and what
// is being checked here is traffic generation, independent verification,
// convergence measurement and reporting.
func TestControlRunAgainstRealNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the chaos control run in short mode")
	}

	const n = 3
	names := []string{"node-a", "node-b", "node-c"}
	ports := make([]int, n)
	for i := range ports {
		ports[i] = reservePort(t)
	}
	peers := make([]config.Peer, n)
	for i := range names {
		peers[i] = config.Peer{Name: names[i], URL: fmt.Sprintf("http://127.0.0.1:%d", ports[i])}
	}

	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var nodes []*node.Node
	for i := range names {
		cfg := config.Defaults()
		cfg.NodeName = names[i]
		cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", ports[i])
		cfg.DataDir = filepath.Join(root, names[i])
		cfg.Peers = peers
		cfg.ReplicaCount = 2
		cfg.MaxBytes = 256 << 20
		cfg.RepairInterval = 500 * time.Millisecond
		cfg.ShutdownTimeout = 2 * time.Second

		nd, err := node.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("start %s: %v", names[i], err)
		}
		go func() { _ = nd.Run(ctx) }()
		t.Cleanup(func() { _ = nd.Close() })
		nodes = append(nodes, nd)
	}

	nodeSpec := make([]string, 0, n)
	for i := range names {
		nodeSpec = append(nodeSpec, fmt.Sprintf("%s=http://127.0.0.1:%d", names[i], ports[i]))
	}

	out := t.TempDir()
	err := run([]string{
		"-nodes", strings.Join(nodeSpec, ","),
		"-duration", "6s",
		"-workers", "6",
		"-read-ratio", "0.6",
		"-object-min", "512",
		"-object-max", "8192",
		"-converge-wait", "30s",
		"-no-faults",
		"-seed", "12345",
		"-out", out,
	})
	if err != nil {
		t.Fatalf("control run reported a failure: %v", err)
	}

	b, readErr := os.ReadFile(filepath.Join(out, "chaos-latest.json"))
	if readErr != nil {
		t.Fatalf("no report written: %v", readErr)
	}
	var rep report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}

	if rep.Verdict != "PASS" {
		t.Errorf("verdict = %q, want PASS for a healthy control run", rep.Verdict)
	}
	if rep.Traffic.WritesOK == 0 {
		t.Fatal("the run wrote nothing; it proves nothing")
	}
	if rep.Traffic.ReadHits == 0 {
		t.Fatal("the run read nothing back")
	}
	if rep.Correctness.ReadsVerified == 0 {
		t.Fatal("no read was verified against the ledger")
	}
	if rep.Correctness.CorruptedReads != 0 {
		t.Errorf("%d corrupted reads on a healthy cluster", rep.Correctness.CorruptedReads)
	}
	if rep.FinalVerification.Corrupted != 0 || rep.FinalVerification.Missing != 0 {
		t.Errorf("final verification found %d corrupted and %d missing on a healthy cluster",
			rep.FinalVerification.Corrupted, rep.FinalVerification.Missing)
	}

	// With no faults and a quota far larger than the corpus, every acknowledged
	// object must be on two nodes. If this measurement reported convergence
	// while nothing was replicated, the chaos run's central claim would be
	// vacuous.
	if !rep.Convergence.Attempted {
		t.Fatal("convergence was not measured")
	}
	if !rep.Convergence.Converged || rep.Convergence.UnderReplicated != 0 {
		t.Errorf("a healthy cluster did not converge: %d of %d at 2+ replicas, %d under-replicated",
			rep.Convergence.FullyReplicated, rep.Convergence.ObjectsChecked,
			rep.Convergence.UnderReplicated)
	}
	if rep.Convergence.FullyReplicated == 0 {
		t.Error("convergence reported success with zero objects at 2 replicas")
	}

	// Node stats must have been collected, or a real run could not tell
	// eviction from data loss.
	if len(rep.NodeStats) != n {
		t.Errorf("collected stats from %d of %d nodes", len(rep.NodeStats), n)
	}
	if rep.Eviction.Observed {
		t.Errorf("eviction ran under a 256 MiB quota with a tiny corpus: %+v", rep.Eviction)
	}

	t.Logf("control run: %d writes, %d reads, %d verified, %d corrupted; %d/%d at 2+ replicas",
		rep.Traffic.WritesOK, rep.Traffic.ReadHits, rep.Correctness.ReadsVerified,
		rep.Correctness.CorruptedReads, rep.Convergence.FullyReplicated, rep.Convergence.ObjectsChecked)
	_ = nodes
}

func reservePort(t *testing.T) int {
	t.Helper()
	l, err := netListen()
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.port
	if err := l.close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}
