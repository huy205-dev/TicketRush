package config

import (
	"math"
	"strconv"
	"time"
)

// Fakepay is the configuration of the fake payment gateway (SPEC.md 7.3).
// It is loaded separately because fakepay stands in for an external system:
// it needs none of the services' databases or secrets except the webhook
// secret it signs with.
type Fakepay struct {
	WebhookSecret string
	// PublicURL is where buyers reach fakepay; payment URLs start with it.
	PublicURL     string
	DelayMin      time.Duration
	DelayMax      time.Duration
	DuplicateRate float64
	LateRate      float64
	LateExtra     time.Duration
	// FailRate is the chance that fakepay treats a delivered webhook as
	// failed and sends it again (FAKEPAY_5XX_RATE).
	FailRate float64
	// CallbackPrefix is booking's PUBLIC_BASE_URL: fakepay only delivers
	// webhooks to callback URLs under it, so it cannot be used to post signed
	// requests anywhere else.
	CallbackPrefix string
}

// LoadFakepay reads FAKEPAY_*, WEBHOOK_SECRET and PUBLIC_BASE_URL.
func LoadFakepay(lookup func(string) (string, bool)) (*Fakepay, error) {
	p := &parser{lookup: lookup}
	c := &Fakepay{
		WebhookSecret:  p.secret("WEBHOOK_SECRET"),
		PublicURL:      p.httpURL("FAKEPAY_URL", "http://localhost:8090"),
		DelayMin:       p.millis("FAKEPAY_DELAY_MS_MIN", 100),
		DelayMax:       p.millis("FAKEPAY_DELAY_MS_MAX", 2000),
		DuplicateRate:  p.rate("FAKEPAY_DUPLICATE_RATE", 0.2),
		LateRate:       p.rate("FAKEPAY_LATE_RATE", 0.05),
		LateExtra:      p.millis("FAKEPAY_LATE_EXTRA_MS", 660_000),
		FailRate:       p.rate("FAKEPAY_5XX_RATE", 0.1),
		CallbackPrefix: p.httpURL("PUBLIC_BASE_URL", "http://localhost:8080"),
	}
	if c.DelayMin > c.DelayMax {
		p.fail("FAKEPAY_DELAY_MS_MIN must not exceed FAKEPAY_DELAY_MS_MAX")
	}
	if len(p.problems) > 0 {
		return nil, &ValidationError{Problems: p.problems}
	}
	return c, nil
}

// millis reads a non-negative number of milliseconds.
func (p *parser) millis(key string, def int64) time.Duration {
	v, ok := p.get(key)
	if !ok {
		return time.Duration(def) * time.Millisecond
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/int64(time.Millisecond) {
		p.fail("%s must be a non-negative number of milliseconds that fits a duration, got %q", key, v)
		return time.Duration(def) * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}

// rate reads a probability in [0, 1].
func (p *parser) rate(key string, def float64) float64 {
	v, ok := p.get(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	// ParseFloat accepts "NaN", which fails every comparison below.
	if err != nil || math.IsNaN(f) || f < 0 || f > 1 {
		p.fail("%s must be a number between 0 and 1, got %q", key, v)
		return def
	}
	return f
}
