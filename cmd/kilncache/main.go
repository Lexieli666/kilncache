// Command kilncache runs one KilnCache node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Lexieli666/kilncache/internal/buildinfo"
	"github.com/Lexieli666/kilncache/internal/config"
	"github.com/Lexieli666/kilncache/internal/logging"
	"github.com/Lexieli666/kilncache/internal/node"
)

func main() {
	// The container healthcheck runs this same binary with -healthcheck; handle
	// it before flag parsing so it never touches the node configuration.
	if handled, code := selfProbe(os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "kilncache: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args, os.Stderr)
	if err != nil {
		return err
	}

	log := logging.New(os.Stdout, logging.Options{
		Level:  cfg.LogLevel,
		Format: cfg.LogFormat,
		NodeID: cfg.NodeName,
	})

	log.Info("starting kilncache",
		slog.String("version", buildinfo.String()),
		slog.String("go", buildinfo.GoVersion()),
		slog.String("data_dir", cfg.AbsDataDir()),
		slog.String("listen", cfg.ListenAddr),
		slog.Int("peers", len(cfg.Peers)),
		slog.Int("replica_count", cfg.ReplicaCount),
		slog.Int64("max_bytes", cfg.MaxBytes),
		slog.Bool("dev_mode", cfg.DevMode),
	)

	// Signals are installed before anything slow, so a node stuck opening a
	// damaged store still responds to Ctrl-C and to docker stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := node.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := n.Close(); err != nil {
			log.Error("closing node", slog.String("err", err.Error()))
		}
	}()

	if err := n.Run(ctx); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Info("stopped cleanly")
	return nil
}
