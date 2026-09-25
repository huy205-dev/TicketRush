// Package ticket creates and checks the QR payload printed on each ticket
// (SPEC.md 9.6) and renders it as a PNG.
package ticket

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/google/uuid"
	qrcode "github.com/skip2/go-qrcode"
)

// Payload returns base64url(ticketId + "." + hmac_sha256(secret, ticketId)),
// the string encoded in the ticket's QR code. The MAC is hex encoded before
// the whole string is base64url encoded.
func Payload(secret string, ticketID uuid.UUID) string {
	id := ticketID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(id + "." + mac(secret, id)))
}

// Verify checks a scanned payload and returns the ticket id it vouches for.
func Verify(secret, payload string) (uuid.UUID, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return uuid.Nil, false
	}
	id, sig, ok := strings.Cut(string(raw), ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(mac(secret, id))) {
		return uuid.Nil, false
	}
	ticketID, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, false
	}
	return ticketID, true
}

func mac(secret, id string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(id))
	return fmt.Sprintf("%x", m.Sum(nil))
}

// PNG renders payload as a 256x256 QR code image.
func PNG(payload string) ([]byte, error) {
	png, err := qrcode.Encode(payload, qrcode.Medium, 256)
	if err != nil {
		return nil, fmt.Errorf("render QR code: %w", err)
	}
	return png, nil
}
