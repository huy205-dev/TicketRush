package main

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// paymentLinks remembers the payment links booking has registered with the
// gateway, so that reading a HELD order again (clients poll it) does not
// register it again. A link is kept until its order's hold ends. Each booking
// instance keeps its own; registering twice is harmless, the gateway is
// idempotent.
type paymentLinkCache struct {
	mu    sync.Mutex
	max   int
	now   func() time.Time
	links map[uuid.UUID]cachedLink
}

type cachedLink struct {
	url   string
	until time.Time
}

func newPaymentLinkCache(max int) *paymentLinkCache {
	return &paymentLinkCache{max: max, now: time.Now, links: make(map[uuid.UUID]cachedLink)}
}

func (c *paymentLinkCache) get(orderID uuid.UUID) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.links[orderID]
	if !ok || !c.now().Before(l.until) {
		return "", false
	}
	return l.url, true
}

func (c *paymentLinkCache) put(orderID uuid.UUID, url string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.links) >= c.max {
		now := c.now()
		for id, l := range c.links {
			if !now.Before(l.until) {
				delete(c.links, id)
			}
		}
		// Still full of live links: drop arbitrary ones. They are only
		// registered again on their next read.
		for id := range c.links {
			if len(c.links) < c.max {
				break
			}
			delete(c.links, id)
		}
	}
	c.links[orderID] = cachedLink{url: url, until: until}
}

// qrCache keeps rendered QR images by ticket id. A ticket's QR payload never
// changes once issued, and encoding one costs about a millisecond and 1.6 MB
// of garbage, so GET /v1/orders/{id}/tickets serves repeated reads from here.
type qrCache struct {
	mu   sync.Mutex
	max  int
	pngs map[uuid.UUID][]byte
}

func newQRCache(max int) *qrCache {
	return &qrCache{max: max, pngs: make(map[uuid.UUID][]byte)}
}

func (c *qrCache) get(ticketID uuid.UUID) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	png, ok := c.pngs[ticketID]
	return png, ok
}

func (c *qrCache) put(ticketID uuid.UUID, png []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.pngs {
		if len(c.pngs) < c.max {
			break
		}
		delete(c.pngs, id)
	}
	c.pngs[ticketID] = png
}
