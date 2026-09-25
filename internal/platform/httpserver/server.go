// Package httpserver runs an http.Server with graceful shutdown.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ShutdownTimeout is how long in-flight requests get to finish once shutdown
// starts (SPEC.md M0).
const ShutdownTimeout = 10 * time.Second

// Serve serves srv on ln until ctx is cancelled, then shuts down gracefully:
// the listener is closed so no new connections are accepted, and in-flight
// requests get up to shutdownTimeout to finish before the remaining
// connections are closed forcibly.
//
// Request contexts are not derived from ctx, so cancelling ctx does not abort
// requests that are already running.
//
// Serve returns nil after a clean shutdown, or an error if the server failed
// or the timeout was exceeded.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, shutdownTimeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Timeout exceeded: drop whatever is still running.
		_ = srv.Close()
		<-serveErr
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
