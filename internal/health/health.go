// Package health serves a tiny readiness/liveness HTTP endpoint, plus the
// Prometheus metrics on /metrics.
package health

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/metrics"
)

type Server struct {
	addr   string
	srv    *http.Server
	logger *slog.Logger
}

func New(addr string, logger *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", metrics.Handler())
	return &Server{
		addr: addr,
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		},
		logger: logger,
	}
}

// Run blocks until ctx is cancelled or the server fails. ListenAndServe errors
// other than ErrServerClosed are returned.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("health server listening", "addr", s.addr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
