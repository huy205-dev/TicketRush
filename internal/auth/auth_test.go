package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestIssueAndVerify(t *testing.T) {
	tokens := NewTokens(testSecret, time.Hour)
	tok, err := tokens.Issue(123)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tokens.Verify(tok)
	if err != nil || got != 123 {
		t.Fatalf("Verify = %d, %v; want 123, nil", got, err)
	}
}

func TestVerifyRejects(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tokens := NewTokens(testSecret, time.Hour)
	tokens.now = fixedClock(now)

	sign := func(method jwt.SigningMethod, key any, claims jwt.RegisteredClaims) string {
		s, err := jwt.NewWithClaims(method, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	valid := jwt.RegisteredClaims{Subject: "7", ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}
	good := sign(jwt.SigningMethodHS256, []byte(testSecret), valid)

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not.a.jwt"},
		{"wrong secret", sign(jwt.SigningMethodHS256, []byte(strings.Repeat("x", 32)), valid)},
		{"alg none", sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid)},
		{"other hmac alg", sign(jwt.SigningMethodHS512, []byte(testSecret), valid)},
		{"expired", sign(jwt.SigningMethodHS256, []byte(testSecret),
			jwt.RegisteredClaims{Subject: "7", ExpiresAt: jwt.NewNumericDate(now.Add(-time.Second))})},
		{"no expiry", sign(jwt.SigningMethodHS256, []byte(testSecret), jwt.RegisteredClaims{Subject: "7"})},
		{"non numeric subject", sign(jwt.SigningMethodHS256, []byte(testSecret),
			jwt.RegisteredClaims{Subject: "alice", ExpiresAt: valid.ExpiresAt})},
		{"zero subject", sign(jwt.SigningMethodHS256, []byte(testSecret),
			jwt.RegisteredClaims{Subject: "0", ExpiresAt: valid.ExpiresAt})},
		{"tampered", good[:len(good)-2] + "xx"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tokens.Verify(tc.token); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("Verify error = %v, want ErrUnauthorized", err)
			}
		})
	}
	if id, err := tokens.Verify(good); err != nil || id != 7 {
		t.Errorf("control token: Verify = %d, %v", id, err)
	}
}
