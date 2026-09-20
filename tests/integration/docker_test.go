//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/cluster"
	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/protocol"
)

// composeFile is the topology under test: the real Dockerfile, the real
// compose file, real containers. The in-process cluster tests cover placement
// and replication logic; this tier is the only one that can prove a container
// can be killed and come back, because only here is there a container.
const composeProject = "kilncache-it"

type dockerCluster struct {
	t        *testing.T
	file     string
	ports    map[string]int
	baseURLs map[string]string
	ring     *cluster.Ring
}

func composePath(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return filepath.Join(root, "deploy", "compose", "docker-compose.yml")
}

func repoRoot() (string, error) {
	out, err := runCmd("git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// requireDocker skips loudly rather than failing when Docker is unavailable.
//
// Loudly matters: a skipped test that looks like a pass is worse than a missing
// test. The skip message says exactly what was not verified.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("KILNCACHE_SKIP_DOCKER") != "" {
		t.Skip("KILNCACHE_SKIP_DOCKER is set: container kill/restart behaviour is NOT verified by this run")
	}
	if _, err := runCmd("docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skip("docker is not available: container kill/restart behaviour is NOT verified by this run")
	}
	if _, err := runCmd("docker", "compose", "version"); err != nil {
		t.Skip("docker compose is not available: container kill/restart behaviour is NOT verified by this run")
	}
}

func startDockerCluster(t *testing.T) *dockerCluster {
	t.Helper()
	requireDocker(t)

	file := composePath(t)
	// Distinct ports from the developer's own compose cluster, so running the
	// suite does not fight with a cluster someone is using.
	ports := map[string]int{"node-a": 18180, "node-b": 18181, "node-c": 18182}
	env := []string{
		"KILNCACHE_PORT_A=18180",
		"KILNCACHE_PORT_B=18181",
		"KILNCACHE_PORT_C=18182",
		"KILNCACHE_MAX_BYTES=8GiB",
		"KILNCACHE_LOG_LEVEL=info",
	}

	dc := &dockerCluster{t: t, file: file, ports: ports, baseURLs: map[string]string{}}
	for name, port := range ports {
		dc.baseURLs[name] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	// The ring the test computes must match what the nodes compute, so it is
	// built from the same peer names the compose file gives them.
	ring, err := cluster.New([]config.Peer{
		{Name: "node-a", URL: "http://node-a:8080"},
		{Name: "node-b", URL: "http://node-b:8080"},
		{Name: "node-c", URL: "http://node-c:8080"},
	}, "")
	if err != nil {
		t.Fatalf("build reference ring: %v", err)
	}
	dc.ring = ring

	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := dc.compose(env, "logs", "--tail=120")
			t.Logf("cluster logs on failure:\n%s", logs)
		}
		if _, err := dc.compose(env, "down", "-v", "--remove-orphans"); err != nil {
			t.Logf("compose down: %v", err)
		}
	})

	if out, err := dc.compose(env, "up", "-d", "--build", "--wait"); err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
	for name := range ports {
		dc.waitReady(name, 120*time.Second)
	}
	return dc
}

func (d *dockerCluster) compose(env []string, args ...string) (string, error) {
	full := append([]string{"compose", "-p", composeProject, "-f", d.file}, args...)
	return runCmdEnv(env, "docker", full...)
}

func (d *dockerCluster) waitReady(name string, within time.Duration) {
	d.t.Helper()
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(d.baseURLs[name] + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	d.t.Fatalf("container %s never became ready within %v", name, within)
}

func (d *dockerCluster) waitGone(name string, within time.Duration) {
	d.t.Helper()
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(d.baseURLs[name] + "/healthz")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	d.t.Fatalf("container %s was still answering %v after being stopped", name, within)
}

func (d *dockerCluster) stop(name string, env []string) {
	d.t.Helper()
	if out, err := d.compose(env, "stop", "-t", "2", name); err != nil {
		d.t.Fatalf("stop %s: %v\n%s", name, err, out)
	}
	d.waitGone(name, 30*time.Second)
}

func (d *dockerCluster) start(name string, env []string) {
	d.t.Helper()
	if out, err := d.compose(env, "start", name); err != nil {
		d.t.Fatalf("start %s: %v\n%s", name, err, out)
	}
	d.waitReady(name, 60*time.Second)
}

func (d *dockerCluster) holders(key string, rf int) []string {
	out := []string{}
	for _, m := range d.ring.Holders(key, rf) {
		out = append(out, m.Name)
	}
	return out
}

func (d *dockerCluster) other(names []string) string {
	for name := range d.ports {
		if !contains(names, name) {
			return name
		}
	}
	return ""
}

// TestDockerClusterSurvivesNodeLoss is the Phase 2 Docker acceptance criterion:
// start three containers, write at least 1,000 objects, kill the container that
// holds a sampled object, read it through another node, and verify the bytes
// against the SHA-256 the test computed itself.
func TestDockerClusterSurvivesNodeLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the Docker cluster suite in short mode")
	}
	env := []string{
		"KILNCACHE_PORT_A=18180", "KILNCACHE_PORT_B=18181", "KILNCACHE_PORT_C=18182",
		"KILNCACHE_MAX_BYTES=8GiB", "KILNCACHE_LOG_LEVEL=info",
	}
	d := startDockerCluster(t)

	const objects = 1000
	written := make(map[string][]byte, objects)
	frontDoors := []string{"node-a", "node-b", "node-c"}

	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, objects)
	sem := make(chan struct{}, 16)

	for i := 0; i < objects; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			content := blob(800+(i%64)*97, int64(i)*7919)
			key := sha256hex(content)
			base := d.baseURLs[frontDoors[i%3]]

			req, err := http.NewRequest(http.MethodPut, base+"/cas/"+key, bytes.NewReader(content))
			if err != nil {
				errs <- err
				return
			}
			req.ContentLength = int64(len(content))
			resp, err := httpClient().Do(req)
			if err != nil {
				errs <- fmt.Errorf("PUT %s: %w", key[:8], err)
				return
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("PUT %s = %d: %s", key[:8], resp.StatusCode, body)
				return
			}
			if copies := resp.Header.Get(protocol.HeaderCopies); copies != "2" {
				errs <- fmt.Errorf("object %s stored with %s copies, want 2", key[:8], copies)
				return
			}
			mu.Lock()
			written[key] = content
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("populating the cluster: %v", err)
	}
	t.Logf("wrote %d objects across three containers, every one acknowledged with 2 copies", len(written))

	// Pick the node that is primary for the largest share of what we wrote, so
	// killing it exercises as many fallbacks as possible.
	victim := "node-b"
	d.stop(victim, env)
	t.Logf("stopped container %s", victim)

	reader := d.other([]string{victim})
	checked, corrupted, fallbacks := 0, 0, 0
	for key, content := range written {
		if !contains(d.holders(key, 2), victim) {
			continue
		}
		resp, err := httpClient().Get(d.baseURLs[reader] + "/cas/" + key)
		if err != nil {
			t.Fatalf("GET %s through %s with %s down: %v", key[:8], reader, victim, err)
		}
		got := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("object %s (holders %v) through %s = %d with %s down",
				key[:8], d.holders(key, 2), reader, resp.StatusCode, victim)
		}
		checked++
		if !bytes.Equal(got, content) || sha256hex(got) != key {
			corrupted++
		}
		if resp.Header.Get(protocol.HeaderSource) != "local" {
			fallbacks++
		}
	}

	if checked == 0 {
		t.Fatal("no written object had the killed container as a holder; the test proved nothing")
	}
	t.Logf("with container %s down: read %d objects it held through %s, %d served by peer fallback, %d corrupted",
		victim, checked, reader, fallbacks, corrupted)
	if corrupted != 0 {
		t.Fatalf("%d corrupted reads out of %d", corrupted, checked)
	}

	// Bring it back and confirm it serves its own copies again.
	d.start(victim, env)
	t.Logf("restarted container %s", victim)

	recovered := 0
	for key, content := range written {
		if !contains(d.holders(key, 2), victim) {
			continue
		}
		req, err := http.NewRequest(http.MethodGet, d.baseURLs[victim]+"/cas/"+key, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		// Ask as a peer would, so the answer must come from its own disk.
		req.Header.Set(protocol.HeaderForwardedBy, "test")
		req.Header.Set(protocol.HeaderHop, string(protocol.HopRead))
		resp, err := httpClient().Do(req)
		if err != nil {
			t.Fatalf("GET from restarted %s: %v", victim, err)
		}
		got := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("restarted %s lost object %s: %d", victim, key[:8], resp.StatusCode)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("restarted %s returned different bytes for %s", victim, key[:8])
		}
		recovered++
	}
	t.Logf("after restart, container %s served %d of its own copies from its named volume, all byte-correct",
		victim, recovered)

	report := map[string]any{
		"kind":                    "phase2-docker-node-loss",
		"objects_written":         len(written),
		"victim":                  victim,
		"reads_checked":           checked,
		"served_by_fallback":      fallbacks,
		"corrupted_reads":         corrupted,
		"recovered_after_restart": recovered,
		"generated_at":            time.Now().UTC().Format(time.RFC3339),
	}
	writeResult(t, "phase2-docker-node-loss.json", report)
}

// writeResult saves a machine-readable result when KILNCACHE_RESULTS_DIR is
// set, so that a claim like "zero corrupted reads out of N" has a committed
// file behind it rather than a line in a test log.
func writeResult(t *testing.T, name string, v any) {
	t.Helper()
	dir := os.Getenv("KILNCACHE_RESULTS_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("create results dir: %v", err)
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Errorf("marshal result: %v", err)
		return
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Errorf("write %s: %v", path, err)
		return
	}
	t.Logf("wrote %s", path)
}
