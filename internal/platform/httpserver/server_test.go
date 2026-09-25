package httpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

type result struct {
	status int
	body   string
	err    error
}

// startBlockingServer serves a handler that signals on started and then
// waits for release before answering.
func startBlockingServer(t *testing.T, ctx context.Context, timeout time.Duration) (addr string, started, release chan struct{}, done chan error) {
	t.Helper()
	started = make(chan struct{})
	release = make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan error, 1)
	go func() { done <- Serve(ctx, srv, ln, timeout) }()
	return ln.Addr().String(), started, release, done
}

func get(url string) <-chan result {
	out := make(chan result, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			out <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		out <- result{status: resp.StatusCode, body: string(b), err: err}
	}()
	return out
}

func TestServeDrainsInFlightRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, started, release, done := startBlockingServer(t, ctx, 5*time.Second)

	inflight := get("http://" + addr)
	<-started
	cancel() // SIGTERM arrives while the request is still running.

	// New connections must be refused once shutdown has begun.
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("server still accepts connections after shutdown started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(release)
	res := <-inflight
	if res.err != nil || res.status != http.StatusOK || res.body != "finished" {
		t.Fatalf("in-flight request = %+v, want 200 finished", res)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve returned %v, want nil after clean shutdown", err)
	}
}

func TestServeGivesUpAfterTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, started, release, done := startBlockingServer(t, ctx, 50*time.Millisecond)
	defer close(release)

	inflight := get("http://" + addr)
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve returned %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the shutdown timeout")
	}
	if res := <-inflight; res.err == nil {
		t.Errorf("stuck request completed with %d, want connection closed", res.status)
	}
}

func TestServeReportsListenerFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()

	err = Serve(context.Background(), &http.Server{}, ln, time.Second)
	if err == nil {
		t.Fatal("Serve on a closed listener returned nil")
	}
}
