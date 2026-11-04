package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPaymentLinkCache(t *testing.T) {
	now := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	c := newPaymentLinkCache(2)
	c.now = func() time.Time { return now }

	a, b, d := uuid.New(), uuid.New(), uuid.New()
	c.put(a, "http://pay/a", now.Add(time.Minute))
	if u, ok := c.get(a); !ok || u != "http://pay/a" {
		t.Errorf("get(a) = %q, %v", u, ok)
	}
	if _, ok := c.get(b); ok {
		t.Error("get of an unknown order hit")
	}

	// A link is not served once its hold has ended.
	now = now.Add(2 * time.Minute)
	if _, ok := c.get(a); ok {
		t.Error("expired link served")
	}

	// The cache stays bounded: expired links go first, then arbitrary ones.
	c.put(b, "http://pay/b", now.Add(time.Minute))
	c.put(d, "http://pay/d", now.Add(time.Minute))
	if len(c.links) != 2 {
		t.Errorf("cache holds %d links, want at most 2", len(c.links))
	}
	if u, ok := c.get(d); !ok || u != "http://pay/d" {
		t.Errorf("newest link lost: %q, %v", u, ok)
	}
	c.put(uuid.New(), "http://pay/e", now.Add(time.Minute))
	if len(c.links) != 2 {
		t.Errorf("cache holds %d links, want at most 2", len(c.links))
	}
}

func TestQRCache(t *testing.T) {
	c := newQRCache(2)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		c.put(id, []byte{byte(i)})
	}
	if len(c.pngs) != 2 {
		t.Errorf("cache holds %d images, want at most 2", len(c.pngs))
	}
	if png, ok := c.get(ids[2]); !ok || png[0] != 2 {
		t.Errorf("newest image lost: %v, %v", png, ok)
	}
}
