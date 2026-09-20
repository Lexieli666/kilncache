// Package node assembles a complete KilnCache node from its subsystems.
//
// It exists so that there is exactly one definition of what a node *is*.
// Without it, cmd/kilncache would wire the store, ring and repair worker
// together one way and the integration tests would wire them together another,
// and the tests would be exercising an arrangement that is not shipped.
package node

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Lexieli666/kilncache/internal/buildinfo"
	"github.com/Lexieli666/kilncache/internal/cluster"
	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/httpapi"
	"github.com/Lexieli666/kilncache/internal/storage"
)

// Node owns every long-lived resource a running node holds, and the order they
// are torn down in.
type Node struct {
	cfg    config.Config
	log    *slog.Logger
	health *httpapi.Health

	store   *storage.Store
	ring    *cluster.Ring
	peers   cluster.PeerClient
	coord   *cluster.Coordinator
	handler http.Handler
	server  *httpapi.Server
}

// Options allows tests to substitute a peer client that can be made to fail,
// hang, or truncate on demand. Production passes nothing and gets the HTTP one.
type Options struct {
	PeerClient cluster.PeerClient
}

// New builds a node: opens storage, builds the ring, wires handlers, and binds
// the listener.
//
// Readiness is announced by Run, not here. Opening the store reconciles the
// temp directory against a possible crash, and a node that advertised itself
// before that finished would be offering a cache whose disk it has not checked.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, opts ...Options) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}

	health := httpapi.NewHealth()
	health.SetNotReady("opening object store")

	store, err := storage.Open(storage.Options{
		Root:           cfg.AbsDataDir(),
		MaxObjectBytes: cfg.MaxObjectBytes,
		VerifyReads:    cfg.VerifyReads,
		Logger:         log,
	})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	n := &Node{cfg: cfg, log: log, health: health, store: store}

	ring, err := cluster.New(cfg.Peers, cfg.NodeName)
	if err != nil {
		_ = n.closeAll()
		return nil, fmt.Errorf("build ring: %w", err)
	}
	n.ring = ring

	peers := opt.PeerClient
	if peers == nil {
		peers = cluster.NewHTTPPeerClient(cfg.NodeName, cfg.PeerTimeout)
	}
	n.peers = peers

	coord, err := cluster.NewCoordinator(cluster.CoordinatorOptions{
		Ring:         ring,
		Local:        store,
		Peers:        peers,
		ReplicaCount: cfg.ReplicaCount,
		Logger:       log,
	})
	if err != nil {
		_ = n.closeAll()
		return nil, fmt.Errorf("build coordinator: %w", err)
	}
	n.coord = coord

	log.Info("node assembled",
		slog.String("root", store.Root()),
		slog.Bool("verify_reads", cfg.VerifyReads),
		slog.Bool("dir_fsync_supported", store.DirSyncSupported()),
		slog.Int64("max_object_bytes", cfg.MaxObjectBytes),
		slog.Int("cluster_size", ring.Size()),
		slog.Int("replica_count", coord.ReplicaCount()),
		slog.Any("members", config.PeerNames(cfg.Peers)),
	)

	router := httpapi.NewRouter(httpapi.RouterOptions{
		Node:    cfg.NodeName,
		Version: buildinfo.String(),
		Health:  health,
		Log:     log,
		DevMode: cfg.DevMode,
		Cache:   httpapi.NewCacheHandler(coord, log, cfg.NodeName, cfg.MaxObjectBytes),
	})
	n.handler = router

	srv, err := httpapi.NewServer(cfg, log, health, router)
	if err != nil {
		_ = n.closeAll()
		return nil, err
	}
	n.server = srv

	_ = ctx
	return n, nil
}

// Addr returns the bound listen address.
func (n *Node) Addr() string { return n.server.Addr() }

// BaseURL returns the http:// URL a client should use to reach this node.
func (n *Node) BaseURL() string { return "http://" + n.Addr() }

// Name returns the node's cluster identity.
func (n *Node) Name() string { return n.cfg.NodeName }

// Store exposes the local object store, for tests and for the repair worker.
func (n *Node) Store() *storage.Store { return n.store }

// Coordinator exposes the placement and replication layer.
func (n *Node) Coordinator() *cluster.Coordinator { return n.coord }

// Ring exposes the placement function.
func (n *Node) Ring() *cluster.Ring { return n.ring }

// Health exposes the readiness signal.
func (n *Node) Health() *httpapi.Health { return n.health }

// Handler exposes the full middleware stack, for httptest-based tests.
func (n *Node) Handler() http.Handler { return n.handler }

// Run serves until ctx is cancelled and then drains.
func (n *Node) Run(ctx context.Context) error {
	n.health.SetReady()
	n.log.Info("ready", slog.String("addr", n.Addr()))
	return n.server.Run(ctx)
}

// Close releases resources in reverse dependency order: stop accepting, drop
// peer connections, then close storage. Reversing that would let a request in
// flight touch a store that has already been closed.
func (n *Node) Close() error {
	var firstErr error
	if n.server != nil {
		if err := n.server.Close(); err != nil {
			firstErr = err
		}
	}
	if err := n.closeAll(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (n *Node) closeAll() error {
	if n.peers != nil {
		n.peers.Close()
		n.peers = nil
	}
	if n.store == nil {
		return nil
	}
	err := n.store.Close()
	n.store = nil
	return err
}
