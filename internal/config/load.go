package config

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Load resolves the configuration from command-line arguments and the process
// environment. Precedence is: explicit flag > KILNCACHE_* environment variable >
// default. The flag set is created here rather than using the global one so
// that tests can call Load repeatedly in one process.
func Load(args []string, output io.Writer) (Config, error) {
	cfg := Defaults()

	fs := flag.NewFlagSet("kilncache", flag.ContinueOnError)
	fs.SetOutput(output)

	var (
		peersRaw     string
		maxBytesRaw  string
		maxObjectRaw string
	)

	fs.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "stable identity of this node; changing it moves every key it owns")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "address to listen on, e.g. :8080")
	fs.StringVar(&cfg.AdvertiseURL, "advertise-url", cfg.AdvertiseURL, "URL peers use to reach this node (defaults to this node's entry in --peers)")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "directory holding the sharded object tree and the metadata index")
	fs.StringVar(&maxBytesRaw, "max-bytes", "", "per-node disk quota, e.g. 20GiB (default 20GiB)")
	fs.StringVar(&maxObjectRaw, "max-object-bytes", "", "reject single objects larger than this, e.g. 4GiB (default 4GiB)")
	fs.Float64Var(&cfg.HighWater, "high-water", cfg.HighWater, "fraction of the quota at which eviction starts")
	fs.Float64Var(&cfg.LowWater, "low-water", cfg.LowWater, "fraction of the quota eviction drains down to")
	fs.StringVar(&peersRaw, "peers", "", "static membership as name=url,name=url")
	fs.IntVar(&cfg.ReplicaCount, "replica-count", cfg.ReplicaCount, "copies of each object that must exist before a PUT is acknowledged")
	fs.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", cfg.ReadHeaderTimeout, "server read header timeout")
	fs.DurationVar(&cfg.WriteTimeout, "write-timeout", cfg.WriteTimeout, "server write timeout; 0 means unlimited, which large streaming GETs need")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", cfg.IdleTimeout, "server keep-alive idle timeout")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "grace period for in-flight requests on SIGTERM")
	fs.DurationVar(&cfg.PeerTimeout, "peer-timeout", cfg.PeerTimeout, "per-hop timeout for a forwarded request")
	fs.IntVar(&cfg.RepairWorkers, "repair-workers", cfg.RepairWorkers, "size of the background repair worker pool; 0 disables repair")
	fs.IntVar(&cfg.RepairQueue, "repair-queue", cfg.RepairQueue, "bounded repair queue depth; a full queue applies backpressure to the auditor")
	fs.DurationVar(&cfg.RepairInterval, "repair-interval", cfg.RepairInterval, "how often the repair auditor sweeps local objects")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "debug, info, warn or error")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "json or text")
	fs.BoolVar(&cfg.DevMode, "dev", cfg.DevMode, "enable /debug/pprof; never enable on a shared host")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// Environment fallbacks, applied only where no flag was given.
	if !set["node-name"] {
		if v, ok := Env("NODE_NAME"); ok {
			cfg.NodeName = v
		}
	}
	if !set["listen"] {
		if v, ok := Env("LISTEN"); ok {
			cfg.ListenAddr = v
		}
	}
	if !set["advertise-url"] {
		if v, ok := Env("ADVERTISE_URL"); ok {
			cfg.AdvertiseURL = v
		}
	}
	if !set["data-dir"] {
		if v, ok := Env("DATA_DIR"); ok {
			cfg.DataDir = v
		}
	}
	if maxBytesRaw == "" {
		if v, ok := Env("MAX_BYTES"); ok {
			maxBytesRaw = v
		}
	}
	if maxObjectRaw == "" {
		if v, ok := Env("MAX_OBJECT_BYTES"); ok {
			maxObjectRaw = v
		}
	}
	if !set["high-water"] {
		if v, ok := Env("HIGH_WATER"); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return Config{}, fmt.Errorf("KILNCACHE_HIGH_WATER: %w", err)
			}
			cfg.HighWater = f
		}
	}
	if !set["low-water"] {
		if v, ok := Env("LOW_WATER"); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return Config{}, fmt.Errorf("KILNCACHE_LOW_WATER: %w", err)
			}
			cfg.LowWater = f
		}
	}
	if peersRaw == "" {
		if v, ok := Env("PEERS"); ok {
			peersRaw = v
		}
	}
	if !set["replica-count"] {
		if v, ok := Env("REPLICA_COUNT"); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return Config{}, fmt.Errorf("KILNCACHE_REPLICA_COUNT: %w", err)
			}
			cfg.ReplicaCount = n
		}
	}
	if !set["peer-timeout"] {
		if v, ok := Env("PEER_TIMEOUT"); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("KILNCACHE_PEER_TIMEOUT: %w", err)
			}
			cfg.PeerTimeout = d
		}
	}
	if !set["repair-workers"] {
		if v, ok := Env("REPAIR_WORKERS"); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return Config{}, fmt.Errorf("KILNCACHE_REPAIR_WORKERS: %w", err)
			}
			cfg.RepairWorkers = n
		}
	}
	if !set["repair-interval"] {
		if v, ok := Env("REPAIR_INTERVAL"); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return Config{}, fmt.Errorf("KILNCACHE_REPAIR_INTERVAL: %w", err)
			}
			cfg.RepairInterval = d
		}
	}
	if !set["log-level"] {
		if v, ok := Env("LOG_LEVEL"); ok {
			cfg.LogLevel = v
		}
	}
	if !set["log-format"] {
		if v, ok := Env("LOG_FORMAT"); ok {
			cfg.LogFormat = v
		}
	}
	if !set["dev"] {
		if v, ok := Env("DEV"); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return Config{}, fmt.Errorf("KILNCACHE_DEV: %w", err)
			}
			cfg.DevMode = b
		}
	}

	if maxBytesRaw != "" {
		n, err := ParseSize(maxBytesRaw)
		if err != nil {
			return Config{}, fmt.Errorf("max-bytes: %w", err)
		}
		cfg.MaxBytes = n
	}
	if maxObjectRaw != "" {
		n, err := ParseSize(maxObjectRaw)
		if err != nil {
			return Config{}, fmt.Errorf("max-object-bytes: %w", err)
		}
		cfg.MaxObjectBytes = n
	}
	if peersRaw != "" {
		peers, err := ParsePeers(peersRaw)
		if err != nil {
			return Config{}, fmt.Errorf("peers: %w", err)
		}
		cfg.Peers = peers
	}
	if len(cfg.Peers) == 0 {
		// A single-node cache is a legitimate configuration (Phase 1 and every
		// unit test use it), so default the membership to this node alone.
		cfg.Peers = []Peer{{Name: cfg.NodeName, URL: defaultSelfURL(cfg.ListenAddr)}}
		if cfg.ReplicaCount > 1 {
			cfg.ReplicaCount = 1
		}
	}

	cfg.LogLevel = strings.ToLower(cfg.LogLevel)
	cfg.LogFormat = strings.ToLower(cfg.LogFormat)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaultSelfURL(listen string) string {
	host, port, ok := strings.Cut(listen, ":")
	if !ok {
		return "http://" + listen
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + port
}
