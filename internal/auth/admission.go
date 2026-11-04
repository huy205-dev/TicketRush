package auth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidAdmission means an admission token is missing, malformed, badly
// signed or expired.
var ErrInvalidAdmission = errors.New("invalid admission token")

// Admission is what a waiting-room admission token grants: one order for
// one event by one user, until ExpiresAt (SPEC.md 7.2).
type Admission struct {
	UserID    int64
	EventID   int64
	QueueID   uuid.UUID
	JTI       uuid.UUID
	ExpiresAt time.Time
}

// admissionClaims is the JWT body: { sub, eid, qid, jti, exp }.
type admissionClaims struct {
	jwt.RegisteredClaims
	EventID int64  `json:"eid"`
	QueueID string `json:"qid"`
}

// AdmissionTokens signs and verifies admission tokens with ADMISSION_SECRET,
// a secret different from the user tokens' one.
type AdmissionTokens struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewAdmissionTokens returns a signer/verifier; issued tokens live for ttl
// (ADMISSION_TTL).
func NewAdmissionTokens(secret string, ttl time.Duration) *AdmissionTokens {
	return &AdmissionTokens{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// TTL is the lifetime of issued tokens.
func (t *AdmissionTokens) TTL() time.Duration { return t.ttl }

// Issue returns a signed token admitting userID to eventID, with a fresh
// jti, and the admission it encodes.
func (t *AdmissionTokens) Issue(userID, eventID int64, queueID uuid.UUID) (string, Admission, error) {
	jti, err := uuid.NewRandom()
	if err != nil {
		return "", Admission{}, fmt.Errorf("generate jti: %w", err)
	}
	// JWT times have second precision; keep the returned value in step.
	exp := t.now().Add(t.ttl).Truncate(time.Second).UTC()
	claims := admissionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(userID, 10),
			ID:        jti.String(),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		EventID: eventID,
		QueueID: queueID.String(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", Admission{}, fmt.Errorf("sign admission token: %w", err)
	}
	return signed, Admission{UserID: userID, EventID: eventID, QueueID: queueID, JTI: jti, ExpiresAt: exp}, nil
}

// Verify checks the signature, algorithm and expiry of token and returns the
// admission. Every failure is reported as ErrInvalidAdmission. Whether the
// token belongs to the caller and to the event is the caller's check.
func (t *AdmissionTokens) Verify(token string) (Admission, error) {
	var claims admissionClaims
	_, err := jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(t.now),
	)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: %w", ErrInvalidAdmission, err)
	}
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || userID <= 0 {
		return Admission{}, fmt.Errorf("%w: invalid subject %q", ErrInvalidAdmission, claims.Subject)
	}
	jti, err := uuid.Parse(claims.ID)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: invalid jti %q", ErrInvalidAdmission, claims.ID)
	}
	queueID, err := uuid.Parse(claims.QueueID)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: invalid qid %q", ErrInvalidAdmission, claims.QueueID)
	}
	if claims.EventID <= 0 {
		return Admission{}, fmt.Errorf("%w: invalid eid %d", ErrInvalidAdmission, claims.EventID)
	}
	return Admission{UserID: userID, EventID: claims.EventID, QueueID: queueID, JTI: jti, ExpiresAt: claims.ExpiresAt.UTC()}, nil
}

type admissionKey struct{}

// WithAdmission returns a context carrying a verified admission.
func WithAdmission(ctx context.Context, a Admission) context.Context {
	return context.WithValue(ctx, admissionKey{}, a)
}

// AdmissionFrom returns the verified admission, if any.
func AdmissionFrom(ctx context.Context) (Admission, bool) {
	a, ok := ctx.Value(admissionKey{}).(Admission)
	return a, ok
}
