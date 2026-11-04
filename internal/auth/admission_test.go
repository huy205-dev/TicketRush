package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const admissionSecret = "fedcba9876543210fedcba9876543210"

func TestAdmissionIssueAndVerify(t *testing.T) {
	now := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	tokens := NewAdmissionTokens(admissionSecret, 5*time.Minute)
	tokens.now = fixedClock(now)
	qid := uuid.New()

	tok, issued, err := tokens.Issue(42, 7, qid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tokens.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got != issued || got.UserID != 42 || got.EventID != 7 || got.QueueID != qid ||
		got.JTI == uuid.Nil || !got.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Errorf("Verify = %+v, issued %+v", got, issued)
	}

	// SPEC.md 7.2 claim names.
	var raw jwt.MapClaims
	if _, _, err := jwt.NewParser().ParseUnverified(tok, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sub", "eid", "qid", "jti", "exp"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("claim %s missing from %v", k, raw)
		}
	}

	// Every token has its own jti, so each admits one order.
	_, again, _ := tokens.Issue(42, 7, qid)
	if again.JTI == issued.JTI {
		t.Error("two tokens share a jti")
	}
}

func TestAdmissionVerifyRejects(t *testing.T) {
	now := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	tokens := NewAdmissionTokens(admissionSecret, 5*time.Minute)
	tokens.now = fixedClock(now)
	good, _, err := tokens.Issue(42, 7, uuid.New())
	if err != nil {
		t.Fatal(err)
	}

	userTokens := NewTokens(testSecret, time.Hour)
	userTok, _ := userTokens.Issue(42)
	otherSecret := NewAdmissionTokens(strings.Repeat("x", 32), 5*time.Minute)
	forged, _, _ := otherSecret.Issue(42, 7, uuid.New())
	noneAlg, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "42", "eid": 7, "qid": uuid.NewString(), "jti": uuid.NewString(), "exp": now.Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	sign := func(c jwt.MapClaims) string {
		s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(admissionSecret))
		return s
	}
	exp := now.Add(time.Hour).Unix()

	cases := map[string]string{
		"empty":          "",
		"garbage":        "not-a-jwt",
		"user token":     userTok,
		"other secret":   forged,
		"alg none":       noneAlg,
		"no expiry":      sign(jwt.MapClaims{"sub": "42", "eid": 7, "qid": uuid.NewString(), "jti": uuid.NewString()}),
		"bad subject":    sign(jwt.MapClaims{"sub": "x", "eid": 7, "qid": uuid.NewString(), "jti": uuid.NewString(), "exp": exp}),
		"no event":       sign(jwt.MapClaims{"sub": "42", "qid": uuid.NewString(), "jti": uuid.NewString(), "exp": exp}),
		"bad queue id":   sign(jwt.MapClaims{"sub": "42", "eid": 7, "qid": "q", "jti": uuid.NewString(), "exp": exp}),
		"bad jti":        sign(jwt.MapClaims{"sub": "42", "eid": 7, "qid": uuid.NewString(), "jti": "j", "exp": exp}),
		"tampered event": good[:strings.LastIndex(good, ".")] + "x" + good[strings.LastIndex(good, ".")+1:],
	}
	for name, tok := range cases {
		if _, err := tokens.Verify(tok); !errors.Is(err, ErrInvalidAdmission) {
			t.Errorf("%s: err = %v, want ErrInvalidAdmission", name, err)
		}
	}

	tokens.now = fixedClock(now.Add(5*time.Minute + time.Second))
	if _, err := tokens.Verify(good); !errors.Is(err, ErrInvalidAdmission) {
		t.Errorf("expired: err = %v, want ErrInvalidAdmission", err)
	}
}
