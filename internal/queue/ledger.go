package queue

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var (
	//go:embed lua/claim.lua
	claimLua string
	//go:embed lua/unclaim.lua
	unclaimLua string
)

// Ledger records which order each admission token created, as
// admission:used:<jti> = orderId for ADMISSION_TTL (SPEC.md 6.1), so that a
// token creates at most one order. It implements order.AdmissionLedger.
type Ledger struct {
	rdb     redis.UniversalClient
	ttl     time.Duration
	claim   *redis.Script
	unclaim *redis.Script
}

// NewLedger keeps claims for ttl (ADMISSION_TTL): a token cannot be used
// after it expires anyway.
func NewLedger(rdb redis.UniversalClient, ttl time.Duration) *Ledger {
	return &Ledger{rdb: rdb, ttl: ttl, claim: redis.NewScript(claimLua), unclaim: redis.NewScript(unclaimLua)}
}

// UsedKey is the Redis key of an admission token's claim.
func UsedKey(jti uuid.UUID) string { return "admission:used:" + jti.String() }

// Claim marks jti as used by orderID. It returns false when another order
// already used the token; claiming again for the same order succeeds.
func (l *Ledger) Claim(ctx context.Context, jti, orderID uuid.UUID) (bool, error) {
	n, err := l.claim.Run(ctx, l.rdb, []string{UsedKey(jti)}, orderID.String(), l.ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("claim admission %s: %w", jti, err)
	}
	return n == 1, nil
}

// Unclaim gives the token back if orderID still holds it, so the buyer can
// try again with the same admission (SPEC.md 9.1 step 6).
func (l *Ledger) Unclaim(ctx context.Context, jti, orderID uuid.UUID) error {
	if err := l.unclaim.Run(ctx, l.rdb, []string{UsedKey(jti)}, orderID.String()).Err(); err != nil {
		return fmt.Errorf("unclaim admission %s: %w", jti, err)
	}
	return nil
}
