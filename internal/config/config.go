// Package config holds the node configuration and its flag/env parsing.
//
// Configuration comes from flags first and environment variables second, so a
// container can be configured entirely through the environment (docker-compose)
// while a developer can override any single value on the command line. Every
// setting is validated up front: a cache node that boots with a nonsense quota
// and discovers it at eviction time is worse than one that refuses to start.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Peer is one member of the static cluster. Name is the stable identity used by
// the placement function; it must not change when a container is rescheduled,
// because changing it would move every key that node owns.
type Peer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Config is the fully resolved node configuration.
type Config struct {
	// Identity and networking.
	NodeName     string `json:"node_name"`
	ListenAddr   string `json:"listen_addr"`
	AdvertiseURL string `json:"advertise_url"`

	// Storage.
	DataDir   string  `json:"data_dir"`
	MaxBytes  int64   `json:"max_bytes"`
	HighWater float64 `json:"high_water"`
	LowWater  float64 `json:"low_water"`

	// Cluster.
	Peers        []Peer `json:"peers"`
	ReplicaCount int    `json:"replica_count"`

	// Timeouts.
	ReadHeaderTimeout time.Duration `json:"read_header_timeout"`
	WriteTimeout      time.Duration `json:"write_timeout"`
	IdleTimeout       time.Duration `json:"idle_timeout"`
	ShutdownTimeout   time.Duration `json:"shutdown_timeout"`
	PeerTimeout       time.Duration `json:"peer_timeout"`

	// Repair.
	RepairWorkers  int           `json:"repair_workers"`
	RepairQueue    int           `json:"repair_queue"`
	RepairInterval time.Duration `json:"repair_interval"`

	// Observability and behaviour switches.
	LogLevel  string `json:"log_level"`
	LogFormat string `json:"log_format"`
	DevMode   bool   `json:"dev_mode"`

	// MaxObjectBytes rejects absurd uploads before they consume the disk.
	MaxObjectBytes int64 `json:"max_object_bytes"`
}

// Defaults returns a Config with every field set to its documented default.
// Tests construct from Defaults() and override one field, so that adding a new
// setting does not silently leave a zero value in a test.
func Defaults() Config {
	return Config{
		NodeName:          "node-a",
		ListenAddr:        ":8080",
		DataDir:           "./data",
		MaxBytes:          20 << 30, // 20 GiB
		HighWater:         0.90,
		LowWater:          0.80,
		ReplicaCount:      2,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // streaming GETs of large objects must not be cut off
		IdleTimeout:       120 * time.Second,
		ShutdownTimeout:   15 * time.Second,
		PeerTimeout:       30 * time.Second,
		RepairWorkers:     4,
		RepairQueue:       1024,
		RepairInterval:    60 * time.Second,
		LogLevel:          "info",
		LogFormat:         "json",
		DevMode:           false,
		MaxObjectBytes:    4 << 30, // 4 GiB
	}
}

// ErrNoSuchPeer is returned when NodeName is not present in Peers.
var ErrNoSuchPeer = errors.New("node name is not a member of the peer list")

// Validate checks the invariants the rest of the system relies on.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.NodeName) == "" {
		return errors.New("node name must not be empty")
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("listen address must not be empty")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("data dir must not be empty")
	}
	if c.MaxBytes <= 0 {
		return fmt.Errorf("max bytes must be positive, got %d", c.MaxBytes)
	}
	if c.MaxObjectBytes <= 0 {
		return fmt.Errorf("max object bytes must be positive, got %d", c.MaxObjectBytes)
	}
	if c.HighWater <= 0 || c.HighWater > 1 {
		return fmt.Errorf("high water must be in (0,1], got %v", c.HighWater)
	}
	if c.LowWater <= 0 || c.LowWater >= c.HighWater {
		return fmt.Errorf("low water must be in (0,high), got low=%v high=%v", c.LowWater, c.HighWater)
	}
	if c.ReplicaCount < 1 {
		return fmt.Errorf("replica count must be >= 1, got %d", c.ReplicaCount)
	}
	if len(c.Peers) == 0 {
		return errors.New("peer list must not be empty")
	}
	if c.ReplicaCount > len(c.Peers) {
		return fmt.Errorf("replica count %d exceeds cluster size %d", c.ReplicaCount, len(c.Peers))
	}
	if c.RepairWorkers < 0 {
		return fmt.Errorf("repair workers must be >= 0, got %d", c.RepairWorkers)
	}
	if c.RepairQueue < 1 {
		return fmt.Errorf("repair queue must be >= 1, got %d", c.RepairQueue)
	}

	seen := make(map[string]struct{}, len(c.Peers))
	self := false
	for _, p := range c.Peers {
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("peer name must not be empty")
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("duplicate peer name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		u, err := url.Parse(p.URL)
		if err != nil {
			return fmt.Errorf("peer %q has invalid url %q: %w", p.Name, p.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("peer %q url %q must be http or https", p.Name, p.URL)
		}
		if u.Host == "" {
			return fmt.Errorf("peer %q url %q has no host", p.Name, p.URL)
		}
		if p.Name == c.NodeName {
			self = true
		}
	}
	if !self {
		return fmt.Errorf("%w: %q not in %v", ErrNoSuchPeer, c.NodeName, PeerNames(c.Peers))
	}
	return nil
}

// PeerNames returns the sorted peer names, for error messages and logs.
func PeerNames(peers []Peer) []string {
	names := make([]string, 0, len(peers))
	for _, p := range peers {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// SelfURL returns the advertised URL of this node, which peers use to reach it.
func (c *Config) SelfURL() string {
	if c.AdvertiseURL != "" {
		return c.AdvertiseURL
	}
	for _, p := range c.Peers {
		if p.Name == c.NodeName {
			return p.URL
		}
	}
	return ""
}

// ParsePeers parses the "name=url,name=url" form used by the --peers flag and
// the KILNCACHE_PEERS environment variable.
func ParsePeers(s string) ([]Peer, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	peers := make([]Peer, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, rawURL, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("peer %q is not in name=url form", part)
		}
		name = strings.TrimSpace(name)
		rawURL = strings.TrimSpace(rawURL)
		if name == "" || rawURL == "" {
			return nil, fmt.Errorf("peer %q has an empty name or url", part)
		}
		peers = append(peers, Peer{Name: name, URL: strings.TrimRight(rawURL, "/")})
	}
	return peers, nil
}

// ParseSize parses a byte quantity with an optional binary suffix: 512, 64KiB,
// 20GiB, 4MB. Decimal suffixes (KB, MB, GB) are treated as powers of 1000 and
// binary suffixes (KiB, MiB, GiB) as powers of 1024, because a disk quota that
// silently means 7% more than the operator wrote is a real incident.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	upper := strings.ToUpper(s)

	type suffix struct {
		text string
		mult int64
	}
	// Longest suffixes first so that "KIB" is not matched as "B".
	suffixes := []suffix{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	}
	for _, suf := range suffixes {
		if !strings.HasSuffix(upper, suf.text) {
			continue
		}
		num := strings.TrimSpace(upper[:len(upper)-len(suf.text)])
		if num == "" {
			return 0, fmt.Errorf("size %q has a suffix but no number", s)
		}
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("size %q: %w", s, err)
		}
		if v < 0 {
			return 0, fmt.Errorf("size %q must not be negative", s)
		}
		return int64(v * float64(suf.mult)), nil
	}
	v, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("size %q must not be negative", s)
	}
	return v, nil
}

// Env reads an environment variable with the KILNCACHE_ prefix.
func Env(key string) (string, bool) {
	v, ok := os.LookupEnv("KILNCACHE_" + key)
	if !ok {
		return "", false
	}
	return v, true
}

// AbsDataDir returns the absolute data directory, so that log lines and the
// runbook refer to a path that is meaningful regardless of the working
// directory the node was started from.
func (c *Config) AbsDataDir() string {
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return c.DataDir
	}
	return abs
}
