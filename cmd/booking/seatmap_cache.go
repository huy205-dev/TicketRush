package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// seatMapLoadTimeout bounds a cache refresh. It runs detached from the
// request that triggered it, because other requests wait on the same load.
const seatMapLoadTimeout = 5 * time.Second

// seatMapCache keeps the rendered seat map of each event for a short TTL, in
// both plain and gzipped form, with its ETag. Thousands of buyers polling the
// map during a sale then cost one inventory query per TTL instead of one per
// request, and a poll that finds nothing changed costs a 304 (SPEC.md 7.1).
type seatMapCache struct {
	ttl  time.Duration
	load func(ctx context.Context, eventID int64) (renderedSeatMap, error)
	now  func() time.Time

	mu      sync.Mutex
	entries map[int64]seatMapEntry
	loads   singleflight.Group
}

// renderedSeatMap is what the loader produces: the JSON body and its ETag.
type renderedSeatMap struct {
	body []byte
	etag string
}

type seatMapEntry struct {
	plain   []byte
	gzipped []byte
	etag    string
	expires time.Time
}

func newSeatMapCache(ttl time.Duration, load func(context.Context, int64) (renderedSeatMap, error)) *seatMapCache {
	return &seatMapCache{ttl: ttl, load: load, now: time.Now, entries: make(map[int64]seatMapEntry)}
}

// get returns a fresh entry, loading it at most once per TTL no matter how
// many requests arrive at the same time.
func (c *seatMapCache) get(ctx context.Context, eventID int64) (seatMapEntry, error) {
	c.mu.Lock()
	e, ok := c.entries[eventID]
	c.mu.Unlock()
	if ok && c.now().Before(e.expires) {
		return e, nil
	}

	v, err, _ := c.loads.Do(strconv.FormatInt(eventID, 10), func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seatMapLoadTimeout)
		defer cancel()
		r, err := c.load(ctx, eventID)
		if err != nil {
			return seatMapEntry{}, err
		}
		gz, err := gzipBytes(r.body)
		if err != nil {
			return seatMapEntry{}, err
		}
		e := seatMapEntry{plain: r.body, gzipped: gz, etag: r.etag, expires: c.now().Add(c.ttl)}
		c.mu.Lock()
		c.entries[eventID] = e
		c.mu.Unlock()
		return e, nil
	})
	if err != nil {
		return seatMapEntry{}, err
	}
	return v.(seatMapEntry), nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, fmt.Errorf("gzip seat map: %w", err)
	}
	if _, err := zw.Write(b); err != nil {
		return nil, fmt.Errorf("gzip seat map: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip seat map: %w", err)
	}
	return buf.Bytes(), nil
}

// versionETag tags a seat map by the backend's seat map version. It is weak
// (W/): the version is read before the seats, so two renders at one version
// may differ by a seat that changed in between. Such a render is newer than
// its version, and the next change moves the version on, so a client is never
// left behind for longer than the cache TTL.
func versionETag(version int64) string {
	return `W/"v` + strconv.FormatInt(version, 10) + `"`
}

// contentETag tags a seat map by a hash of its body, for backends without a
// seat map version (pg). Weak for the same reason as versionETag: the gzipped
// and plain representations share it.
func contentETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"h` + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches reports whether an If-None-Match header matches etag, using the
// weak comparison RFC 9110 prescribes for If-None-Match.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == want {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether the client listed gzip in Accept-Encoding
// without disabling it via q=0.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}
