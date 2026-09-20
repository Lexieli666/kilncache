package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/Lexieli666/kilncache/internal/config"
)

// Server owns the listener and the http.Server lifecycle.
//
// The listener is created in New rather than in Run so that a bind failure is
// reported before the caller believes the node is up, and so that tests can ask
// for port 0 and then read back the real address.
type Server struct {
	cfg    config.Config
	log    *slog.Logger
	health *Health
	srv    *http.Server
	ln     net.Listener
}

// NewServer binds the listen address and wraps handler in an http.Server.
func NewServer(cfg config.Config, log *slog.Logger, health *Health, handler http.Handler) (*Server, error) {
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return &Server{cfg: cfg, log: log, health: health, srv: srv, ln: ln}, nil
}

// Addr returns the bound address, which differs from cfg.ListenAddr when the
// configured port was 0.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Run serves until ctx is cancelled, then drains in-flight requests.
//
// On cancellation the node is marked not-ready before Shutdown begins. A peer
// that polls /readyz therefore stops choosing this node for forwarding while
// the requests already in flight are still allowed to finish.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", slog.String("addr", s.Addr()))
		err := s.srv.Serve(s.ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.health.SetNotReady("shutting down")
	s.log.Info("draining in-flight requests", slog.Duration("grace", s.cfg.ShutdownTimeout))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	shutdownErr := s.srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		// Shutdown returning DeadlineExceeded means clients were still
		// streaming when the grace period ran out. Say so explicitly: this is
		// the difference between "we drained" and "we cut people off".
		s.log.Warn("graceful shutdown did not complete", slog.String("err", shutdownErr.Error()))
		_ = s.srv.Close()
	}

	serveErr := <-errCh
	if serveErr != nil {
		return serveErr
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return shutdownErr
	}
	return nil
}

// Close releases the listener without draining. Tests use it; production uses
// Run's context cancellation.
func (s *Server) Close() error {
	err := s.srv.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
