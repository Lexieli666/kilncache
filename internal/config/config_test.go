package config

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"512", 512, false},
		{"512B", 512, false},
		{"64KiB", 64 * 1024, false},
		{"64kib", 64 * 1024, false},
		{"8MiB", 8 * 1024 * 1024, false},
		{"20GiB", 20 * 1024 * 1024 * 1024, false},
		{"1TiB", 1 << 40, false},
		{"1K", 1024, false},
		{"1M", 1 << 20, false},
		{"1G", 1 << 30, false},
		// Decimal suffixes are powers of 1000, not 1024. An operator who writes
		// 1GB and gets 1073741824 bytes has been lied to.
		{"1KB", 1000, false},
		{"1MB", 1000 * 1000, false},
		{"1GB", 1000 * 1000 * 1000, false},
		{"1.5GiB", 1610612736, false},
		{" 4MiB ", 4 << 20, false},
		{"", 0, true},
		{"GiB", 0, true},
		{"-1", 0, true},
		{"-1GiB", 0, true},
		{"banana", 0, true},
		{"12x", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseSize(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParsePeers(t *testing.T) {
	peers, err := ParsePeers("a=http://a:8080,b=http://b:8080/ , c=http://c:8080")
	if err != nil {
		t.Fatalf("ParsePeers: %v", err)
	}
	want := []Peer{
		{Name: "a", URL: "http://a:8080"},
		{Name: "b", URL: "http://b:8080"},
		{Name: "c", URL: "http://c:8080"},
	}
	if len(peers) != len(want) {
		t.Fatalf("got %d peers, want %d: %+v", len(peers), len(want), peers)
	}
	for i := range want {
		if peers[i] != want[i] {
			t.Errorf("peer %d = %+v, want %+v", i, peers[i], want[i])
		}
	}
}

func TestParsePeersRejectsMalformed(t *testing.T) {
	for _, in := range []string{"a", "=http://a:8080", "a=", "a=http://a,b"} {
		if _, err := ParsePeers(in); err == nil {
			t.Errorf("ParsePeers(%q) = nil error, want error", in)
		}
	}
}

func TestParsePeersEmpty(t *testing.T) {
	peers, err := ParsePeers("")
	if err != nil {
		t.Fatalf("ParsePeers(\"\"): %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("got %d peers, want 0", len(peers))
	}
}

func validConfig() Config {
	c := Defaults()
	c.NodeName = "a"
	c.Peers = []Peer{
		{Name: "a", URL: "http://a:8080"},
		{Name: "b", URL: "http://b:8080"},
		{Name: "c", URL: "http://c:8080"},
	}
	return c
}

func TestValidateAcceptsGoodConfig(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValidateRejects is the falsifier for "a node refuses to start on a
// configuration that would misbehave later".
func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"empty node name":        func(c *Config) { c.NodeName = "" },
		"empty listen":           func(c *Config) { c.ListenAddr = "" },
		"empty data dir":         func(c *Config) { c.DataDir = "" },
		"zero quota":             func(c *Config) { c.MaxBytes = 0 },
		"negative quota":         func(c *Config) { c.MaxBytes = -1 },
		"zero max object":        func(c *Config) { c.MaxObjectBytes = 0 },
		"high water above 1":     func(c *Config) { c.HighWater = 1.5 },
		"high water zero":        func(c *Config) { c.HighWater = 0 },
		"low water above high":   func(c *Config) { c.LowWater = 0.95; c.HighWater = 0.9 },
		"low water equals high":  func(c *Config) { c.LowWater = 0.9; c.HighWater = 0.9 },
		"zero replicas":          func(c *Config) { c.ReplicaCount = 0 },
		"replicas beyond size":   func(c *Config) { c.ReplicaCount = 4 },
		"no peers":               func(c *Config) { c.Peers = nil },
		"self not in peers":      func(c *Config) { c.NodeName = "z" },
		"duplicate peer":         func(c *Config) { c.Peers = append(c.Peers, Peer{Name: "a", URL: "http://a2:8080"}) },
		"empty peer name":        func(c *Config) { c.Peers[1].Name = "" },
		"peer url without host":  func(c *Config) { c.Peers[1].URL = "http://" },
		"peer url bad scheme":    func(c *Config) { c.Peers[1].URL = "ftp://b:8080" },
		"negative repair worker": func(c *Config) { c.RepairWorkers = -1 },
		"zero repair queue":      func(c *Config) { c.RepairQueue = 0 },
	}
	for name, mutate := range cases {
		c := validConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

func TestValidateSelfNotInPeersIsTyped(t *testing.T) {
	c := validConfig()
	c.NodeName = "z"
	err := c.Validate()
	if !errors.Is(err, ErrNoSuchPeer) {
		t.Fatalf("err = %v, want ErrNoSuchPeer", err)
	}
}

func TestLoadFlagsBeatDefaults(t *testing.T) {
	cfg, err := Load([]string{
		"--node-name=b",
		"--listen=:9090",
		"--data-dir=/var/lib/kiln",
		"--max-bytes=1GiB",
		"--peers=a=http://a:8080,b=http://b:8080",
		"--replica-count=2",
		"--peer-timeout=5s",
		"--dev",
	}, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "b" {
		t.Errorf("NodeName = %q", cfg.NodeName)
	}
	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.MaxBytes != 1<<30 {
		t.Errorf("MaxBytes = %d", cfg.MaxBytes)
	}
	if cfg.PeerTimeout != 5*time.Second {
		t.Errorf("PeerTimeout = %v", cfg.PeerTimeout)
	}
	if !cfg.DevMode {
		t.Error("DevMode = false, want true")
	}
	if cfg.SelfURL() != "http://b:8080" {
		t.Errorf("SelfURL = %q", cfg.SelfURL())
	}
}

func TestLoadEnvFallback(t *testing.T) {
	t.Setenv("KILNCACHE_NODE_NAME", "c")
	t.Setenv("KILNCACHE_PEERS", "a=http://a:8080,c=http://c:8080")
	t.Setenv("KILNCACHE_MAX_BYTES", "256MiB")
	t.Setenv("KILNCACHE_LOG_LEVEL", "debug")

	cfg, err := Load(nil, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "c" {
		t.Errorf("NodeName = %q, want c", cfg.NodeName)
	}
	if cfg.MaxBytes != 256<<20 {
		t.Errorf("MaxBytes = %d", cfg.MaxBytes)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
}

// TestLoadFlagBeatsEnv pins the documented precedence. Reversing it would let a
// leftover container environment variable silently override an operator's
// explicit command line.
func TestLoadFlagBeatsEnv(t *testing.T) {
	t.Setenv("KILNCACHE_NODE_NAME", "from-env")
	t.Setenv("KILNCACHE_MAX_BYTES", "1GiB")

	cfg, err := Load([]string{"--node-name=from-flag", "--max-bytes=2GiB"}, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "from-flag" {
		t.Errorf("NodeName = %q, want from-flag", cfg.NodeName)
	}
	if cfg.MaxBytes != 2<<30 {
		t.Errorf("MaxBytes = %d, want 2GiB", cfg.MaxBytes)
	}
}

func TestLoadDefaultsToSingleNode(t *testing.T) {
	cfg, err := Load([]string{"--node-name=solo", "--listen=:8081"}, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Peers) != 1 || cfg.Peers[0].Name != "solo" {
		t.Fatalf("Peers = %+v, want a single self entry", cfg.Peers)
	}
	if cfg.ReplicaCount != 1 {
		t.Errorf("ReplicaCount = %d, want 1 for a single-node cluster", cfg.ReplicaCount)
	}
	if cfg.SelfURL() != "http://127.0.0.1:8081" {
		t.Errorf("SelfURL = %q", cfg.SelfURL())
	}
}

func TestLoadRejectsBadFlag(t *testing.T) {
	var sb strings.Builder
	if _, err := Load([]string{"--not-a-flag"}, &sb); err == nil {
		t.Fatal("Load = nil error, want error")
	}
}

func TestLoadRejectsInvalidCombination(t *testing.T) {
	if _, err := Load([]string{"--node-name=a", "--peers=b=http://b:8080"}, io.Discard); err == nil {
		t.Fatal("Load accepted a node that is not in its own peer list")
	}
}

func TestPeerNamesSorted(t *testing.T) {
	got := PeerNames([]Peer{{Name: "c"}, {Name: "a"}, {Name: "b"}})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("PeerNames = %v, want [a b c]", got)
	}
}

func TestAbsDataDir(t *testing.T) {
	c := validConfig()
	c.DataDir = "./data"
	if !strings.HasPrefix(c.AbsDataDir(), "/") {
		t.Errorf("AbsDataDir = %q, want an absolute path", c.AbsDataDir())
	}
}
