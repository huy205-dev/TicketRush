// Package auth issues and verifies user access tokens (HS256 JWT) and carries
// the authenticated user id in the request context. Admission tokens for the
// waiting room arrive in M5.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/huy205-dev/ticketrush/internal/auth/authdb"
)

// AccessTokenTTL is the lifetime of a dev-login access token.
const AccessTokenTTL = 24 * time.Hour

// ErrUnauthorized means the access token is missing, malformed, badly signed
// or expired.
var ErrUnauthorized = errors.New("unauthorized")

// Tokens signs and verifies user access tokens.
type Tokens struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewTokens returns a signer/verifier using the given HMAC secret.
func NewTokens(secret string, ttl time.Duration) *Tokens {
	return &Tokens{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// TTL is the lifetime of issued tokens.
func (t *Tokens) TTL() time.Duration { return t.ttl }

// Issue returns a signed token whose subject is userID.
func (t *Tokens) Issue(userID int64) (string, error) {
	now := t.now()
	claims := jwt.RegisteredClaims{
		Subject:   strconv.FormatInt(userID, 10),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return signed, nil
}

// Verify checks the signature, algorithm and expiry of token and returns the
// user id. Every failure is reported as ErrUnauthorized.
func (t *Tokens) Verify(token string) (int64, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || userID <= 0 {
		return 0, fmt.Errorf("%w: invalid subject %q", ErrUnauthorized, claims.Subject)
	}
	return userID, nil
}

type userIDKey struct{}

// WithUserID returns a context carrying the authenticated user id.
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFrom returns the authenticated user id, if any.
func UserIDFrom(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(userIDKey{}).(int64)
	return id, ok
}

// EnsureUser creates the user row if it does not exist yet (dev-login).
func EnsureUser(ctx context.Context, db authdb.DBTX, userID int64) error {
	if err := authdb.New(db).EnsureUser(ctx, userID); err != nil {
		return fmt.Errorf("ensure user %d: %w", userID, err)
	}
	return nil
}
