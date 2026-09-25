package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestSeatMapCacheLoadsOncePerTTL(t *testing.T) {
	var loads atomic.Int64
	release := make(chan struct{})
	c := newSeatMapCache(500*time.Millisecond, func(ctx context.Context, eventID int64) (renderedSeatMap, error) {
		loads.Add(1)
		<-release
		return renderedSeatMap{body: []byte(`{"version":0}`), etag: versionETag(0)}, nil
	})
	clock := &fakeClock{t: time.Unix(0, 0)}
	c.now = clock.now

	// 100 concurrent requests on a cold cache share one load.
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if _, err := c.get(context.Background(), 1); err != nil {
				t.Error(err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond) // let them pile up on the load
	close(release)
	wg.Wait()
	if n := loads.Load(); n != 1 {
		t.Fatalf("cold cache: %d loads, want 1", n)
	}

	clock.advance(400 * time.Millisecond)
	if _, err := c.get(context.Background(), 1); err != nil || loads.Load() != 1 {
		t.Fatalf("within TTL: loads=%d err=%v, want cached", loads.Load(), err)
	}

	clock.advance(200 * time.Millisecond)
	if _, err := c.get(context.Background(), 1); err != nil || loads.Load() != 2 {
		t.Fatalf("after TTL: loads=%d err=%v, want reload", loads.Load(), err)
	}

	if _, err := c.get(context.Background(), 2); err != nil || loads.Load() != 3 {
		t.Fatalf("other event: loads=%d, want its own entry", loads.Load())
	}
}

func TestSeatMapCacheGzipAndErrors(t *testing.T) {
	boom := errors.New("db down")
	fail := true
	c := newSeatMapCache(time.Second, func(context.Context, int64) (renderedSeatMap, error) {
		if fail {
			return renderedSeatMap{}, boom
		}
		body := []byte(`{"seats":[]}`)
		return renderedSeatMap{body: body, etag: contentETag(body)}, nil
	})

	if _, err := c.get(context.Background(), 1); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want load error", err)
	}
	fail = false // errors are not cached
	e, err := c.get(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(e.gzipped))
	if err != nil {
		t.Fatal(err)
	}
	unzipped, _ := io.ReadAll(zr)
	if !bytes.Equal(unzipped, e.plain) {
		t.Errorf("gzipped body decodes to %q, want %q", unzipped, e.plain)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                  false,
		"gzip":              true,
		"GZIP":              true,
		"deflate, gzip":     true,
		"gzip;q=0.5, br":    true,
		"gzip;q=0":          false,
		"gzip; q=0.000":     false,
		"br, deflate":       false,
		"x-gzip-not-really": false,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Accept-Encoding", header)
		}
		if got := acceptsGzip(r); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestETags(t *testing.T) {
	if got := versionETag(42); got != `W/"v42"` {
		t.Errorf("versionETag = %s", got)
	}
	a, b := contentETag([]byte("a")), contentETag([]byte("b"))
	if a == b || a != contentETag([]byte("a")) {
		t.Errorf("contentETag must be stable and content dependent: %s %s", a, b)
	}

	tag := `W/"v42"`
	for header, want := range map[string]bool{
		`W/"v42"`:         true,
		`"v42"`:           true, // weak comparison ignores W/
		`"v1", W/"v42"`:   true,
		`*`:               true,
		`W/"v41"`:         false,
		``:                false,
		`"v4"`:            false,
		`W/"v42" , "v43"`: true,
	} {
		if got := etagMatches(header, tag); got != want {
			t.Errorf("etagMatches(%q, %s) = %v, want %v", header, tag, got, want)
		}
	}
}
