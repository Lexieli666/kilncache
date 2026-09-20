//go:build integration

// Package integration runs KilnCache nodes as real processes-in-process: real
// listeners, real sockets, real files on a real filesystem.
//
// These tests are behind a build tag so that `go test ./...` stays fast enough
// for the edit loop. They are not optional — CI runs them on every push — but
// they take seconds rather than milliseconds, and a slow default test command
// is a test command people stop running.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/node"
)

// testNode is a running node plus the plumbing to stop and restart it.
type testNode struct {
	t       *testing.T
	Name    string
	DataDir string
	Node    *node.Node
	cancel  context.CancelFunc
	done    chan error
	cfg     config.Config
}

// nodeLogger writes node logs into the test's own output, so a failure shows
// what the server thought was happening rather than only what the client saw.
func nodeLogger(t *testing.T, name string) *slog.Logger {
	t.Helper()
	level := slog.LevelWarn
	if testing.Verbose() {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: level})).
		With(slog.String("node", name))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// dataRoot returns a directory on a local filesystem.
//
// t.TempDir() is used rather than a path inside the repository on purpose: this
// checkout may live on a 9p mount where rename and fsync do not behave as the
// store requires, and an integration test that ran there would be testing the
// wrong filesystem and passing for the wrong reason.
func dataRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if fs := filesystemOf(dir); fs == "9p" || fs == "drvfs" {
		t.Skipf("temp dir %s is on %s; integration tests need a local filesystem (set TMPDIR to one)", dir, fs)
	}
	return dir
}

func filesystemOf(path string) string {
	out, err := runCmd("df", "-PT", path)
	if err != nil {
		return "unknown"
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return "unknown"
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		return "unknown"
	}
	return fields[1]
}

// localCluster is a set of in-process nodes that know about each other.
//
// Named to leave "cluster" free for the internal/cluster package, which the
// Docker tier imports to compute the same placement the nodes compute.
type localCluster struct {
	t     *testing.T
	Nodes []*testNode
}

// startCluster brings up n nodes with a shared peer list.
//
// The peer list has to be complete before any node starts, and a node's URL is
// only known once it has bound a port. That is solved by binding listeners
// first and starting servers second: each node is constructed (which binds),
// then every node is told the full membership, then they all begin serving.
// Guessing ports in advance would make the suite flaky on a busy machine.
func startCluster(t *testing.T, n int, mutate func(*config.Config)) *localCluster {
	t.Helper()
	if n < 1 {
		t.Fatalf("cluster size %d", n)
	}
	root := dataRoot(t)

	// Reserve a port per node by binding and immediately releasing. There is a
	// race here in principle; in practice the window is microseconds and the
	// alternative -- a two-phase node constructor -- is a lot of production
	// complexity to serve a test.
	names := make([]string, n)
	ports := make([]int, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("node-%c", 'a'+i)
		ports[i] = reservePort(t)
	}
	peerList := make([]config.Peer, n)
	for i := range names {
		peerList[i] = config.Peer{Name: names[i], URL: fmt.Sprintf("http://127.0.0.1:%d", ports[i])}
	}

	c := &localCluster{t: t}
	for i := 0; i < n; i++ {
		i := i
		tn := newTestNode(t, names[i], filepath.Join(root, names[i]), func(cfg *config.Config) {
			cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", ports[i])
			cfg.Peers = peerList
			cfg.ReplicaCount = 2
			if mutate != nil {
				mutate(cfg)
			}
		})
		tn.start()
		t.Cleanup(tn.Stop)
		c.Nodes = append(c.Nodes, tn)
	}
	return c
}

// reservePort binds port 0, reads the port back, and releases it.
func reservePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// Node returns the node with the given name.
func (c *localCluster) Node(name string) *testNode {
	c.t.Helper()
	for _, n := range c.Nodes {
		if n.Name == name {
			return n
		}
	}
	c.t.Fatalf("no node named %s", name)
	return nil
}

// URLs returns every node's base URL.
func (c *localCluster) URLs() []string {
	out := make([]string, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		out = append(out, n.URL())
	}
	return out
}

// Any returns a node chosen by index, for spreading traffic across front doors.
func (c *localCluster) Any(i int) *testNode { return c.Nodes[i%len(c.Nodes)] }

// HoldersOf returns the names of the nodes that should hold key.
func (c *localCluster) HoldersOf(key string, rf int) []string {
	r := c.Nodes[0].Node.Ring()
	out := []string{}
	for _, m := range r.Holders(key, rf) {
		out = append(out, m.Name)
	}
	return out
}

// newTestNode builds a testNode without starting it.
func newTestNode(t *testing.T, name, dir string, mutate func(*config.Config)) *testNode {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create data dir: %v", err)
	}
	cfg := config.Defaults()
	cfg.NodeName = name
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.DataDir = dir
	cfg.Peers = []config.Peer{{Name: name, URL: "http://127.0.0.1:0"}}
	cfg.ReplicaCount = 1
	cfg.ShutdownTimeout = 5 * time.Second
	cfg.PeerTimeout = 10 * time.Second
	cfg.LogLevel = "debug"
	if mutate != nil {
		mutate(&cfg)
	}
	return &testNode{t: t, Name: name, DataDir: dir, cfg: cfg}
}

// startNode brings up a single standalone node on an ephemeral port.
func startNode(t *testing.T, name string, mutate func(*config.Config)) *testNode {
	t.Helper()

	tn := newTestNode(t, name, filepath.Join(dataRoot(t), name), mutate)
	tn.start()
	t.Cleanup(tn.Stop)
	return tn
}

func (n *testNode) start() {
	n.t.Helper()
	log := nodeLogger(n.t, n.Name)

	nd, err := node.New(context.Background(), n.cfg, log)
	if err != nil {
		n.t.Fatalf("start node %s: %v", n.Name, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.Node = nd
	n.cancel = cancel
	n.done = make(chan error, 1)
	go func() { n.done <- nd.Run(ctx) }()

	waitReady(n.t, nd.BaseURL())
}

// Stop shuts the node down gracefully and waits for it to drain.
func (n *testNode) Stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	select {
	case err := <-n.done:
		if err != nil {
			n.t.Errorf("node %s exited with %v", n.Name, err)
		}
	case <-time.After(20 * time.Second):
		n.t.Errorf("node %s did not shut down within 20s", n.Name)
	}
	if err := n.Node.Close(); err != nil {
		n.t.Errorf("close node %s: %v", n.Name, err)
	}
	n.cancel = nil
}

// Restart stops the node and brings it back on the same data directory. The
// listen address changes, because port 0 is requested again; callers use
// URL() rather than caching the address.
func (n *testNode) Restart() {
	n.t.Helper()
	n.Stop()
	n.start()
}

// URL returns the node's base URL.
func (n *testNode) URL() string { return n.Node.BaseURL() }

func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/readyz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node at %s never reported ready", base)
}

// --- client helpers -------------------------------------------------------

func httpClient() *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 64,
			DisableCompression:  true,
		},
	}
}

func put(t *testing.T, base, path string, body []byte) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, base+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	resp, err := httpClient().Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp, string(b)
}

func get(t *testing.T, base, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := httpClient().Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET %s: %v", path, err)
	}
	return resp, b
}

func head(t *testing.T, base, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, base+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		t.Fatalf("HEAD %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp
}

// readBody reads and closes a response body.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blob returns n deterministic bytes. Deterministic so a failure is
// reproducible; not constant so a truncation cannot hide.
func blob(n int, seed int64) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(seed))
	_, _ = r.Read(b)
	return b
}

func runCmd(name string, args ...string) (string, error) {
	return runCmdIn("", name, args...)
}

func mustDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

func addrOf(base string) string {
	return strings.TrimPrefix(base, "http://")
}

func assertStatus(t *testing.T, got, want int, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: status %d, want %d", what, got, want)
	}
}

// tempFiles lists the in-progress uploads currently on disk.
func tempFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	tmp := filepath.Join(dataDir, "tmp")
	entries, err := os.ReadDir(tmp)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", tmp, err)
	}
	var leftover []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "incoming-") {
			leftover = append(leftover, e.Name())
		}
	}
	return leftover
}

// assertNoTempFiles is the on-disk half of "a rejected upload leaves nothing
// behind", for the case where the server answered *us*. The response cannot be
// written until the write path has unwound, so by the time the client has a
// status the cleanup has provably happened and no polling is warranted.
func assertNoTempFiles(t *testing.T, dataDir string) {
	t.Helper()
	if leftover := tempFiles(t, dataDir); len(leftover) > 0 {
		t.Errorf("partial uploads left in %s/tmp: %v", dataDir, leftover)
	}
}

// assertNoTempFilesEventually is the same assertion for the case where the
// client hung up and never waited for a response.
//
// There, cleanup is genuinely asynchronous with respect to the client: the
// server does not learn the connection is gone until its next read fails, which
// happens after the client has already moved on. Asserting synchronously would
// be testing that the server is faster than the test, which is not a property
// anything should depend on. What is worth asserting is that the cleanup
// happens promptly and without being prompted, so this waits with a deadline.
func assertNoTempFilesEventually(t *testing.T, dataDir string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last []string
	for time.Now().Before(deadline) {
		last = tempFiles(t, dataDir)
		if len(last) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("partial uploads still in %s/tmp after %v: %v", dataDir, within, last)
}

var _ = fmt.Sprintf
