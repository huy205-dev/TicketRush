package ticket

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPayloadRoundTrip(t *testing.T) {
	secret := strings.Repeat("t", 32)
	id := uuid.New()
	p := Payload(secret, id)

	if got, ok := Verify(secret, p); !ok || got != id {
		t.Fatalf("Verify(own payload) = %v, %v", got, ok)
	}
	if p != Payload(secret, id) {
		t.Error("payload is not deterministic")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(p)
	if !strings.HasPrefix(string(raw), id.String()+".") {
		t.Errorf("payload decodes to %q, want <ticketId>.<mac>", raw)
	}
}

func TestVerifyRejectsForgeries(t *testing.T) {
	secret := strings.Repeat("t", 32)
	id := uuid.New()
	p := Payload(secret, id)
	raw, _ := base64.RawURLEncoding.DecodeString(p)
	_, sig, _ := strings.Cut(string(raw), ".")

	for name, payload := range map[string]string{
		"other secret": Payload(strings.Repeat("x", 32), id),
		"swapped id":   base64.RawURLEncoding.EncodeToString([]byte(uuid.NewString() + "." + sig)),
		"no separator": base64.RawURLEncoding.EncodeToString([]byte(id.String())),
		"not base64":   "!!!",
		"truncated":    p[:len(p)-3],
		"empty":        "",
		"not a uuid":   base64.RawURLEncoding.EncodeToString([]byte("abc." + mac(secret, "abc"))),
	} {
		if _, ok := Verify(secret, payload); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPNG(t *testing.T) {
	png, err := PNG(Payload(strings.Repeat("t", 32), uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
		t.Errorf("not a PNG: % x", png[:8])
	}
}
