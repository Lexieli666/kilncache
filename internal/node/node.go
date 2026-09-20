// Package node assembles a complete KilnCache node from its subsystems.
//
// It exists so that there is exactly one definition of what a node *is*.
// Without it, cmd/kilncache would wire the store, cluster and repair worker
// together one way and the integration tests would wire them together another,
// and the tests would be testing an arrangement that is not shipped.
package node

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Lexieli666/kilncache/internal/buildinfo"
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
	handler http.Handler
	server  *httpapi.Server
}

// New builds a node: opens storage, wires handlers, and binds the listener.
//
// Readiness is announced by Run, not here. Opening the store reconciles the
// temp directory against a possible crash, and a node that advertised itself
// before that finished would be offering a cache whose disk it has not checked.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
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

	log.Info("object store open",
		slog.String("root", store.Root()),
		slog.Bool("verify_reads", cfg.VerifyReads),
		slog.Bool("dir_fsync_supported", store.DirSyncSupported()),
		slog.Int64("max_object_bytes", cfg.MaxObjectBytes),
	)

	n := &Node{cfg: cfg, log: log, health: health, store: store}

	router := httpapi.NewRouter(httpapi.RouterOptions{
		Node:    cfg.NodeName,
		Version: buildinfo.String(),
		Health:  health,
		Log:     log,
		DevMode: cfg.DevMode,
		Cache:   httpapi.NewCacheHandler(store, log, cfg.MaxObjectBytes),
	})
	n.handler = router

	srv, err := httpapi.NewServer(cfg, log, health, router)
	if err != nil {
		_ = n.closeStore()
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

// Store exposes the object store for tests and for subsystems that need direct
// access. Production code goes through the HTTP handler.
func (n *Node) Store() *storage.Store { return n.store }

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

// Close releases resources in reverse dependency order: stop accepting, then
// close storage. Reversing that would let a request in flight touch a store
// that has already been closed.
func (n *Node) Close() error {
	var firstErr error
	if n.server != nil {
		if err := n.server.Close(); err != nil {
			firstErr = err
		}
	}
	if err := n.closeStore(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (n *Node) closeStore() error {
	if n.store == nil {
		return nil
	}
	return n.store.Close()
}
