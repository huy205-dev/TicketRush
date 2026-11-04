//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/httpx"
)

func (c *client) createWith(eventID int64, admission, key string, seats ...string) response {
	c.t.Helper()
	ids, _ := json.Marshal(seats)
	h := map[string]string{"Idempotency-Key": key}
	if admission != "" {
		h[httpx.AdmissionHeader] = admission
	}
	return c.do("POST", "/v1/orders", `{"event_id":`+jsonInt(eventID)+`,"seat_ids":`+string(ids)+`}`, h)
}

func errorCode(t *testing.T, r response) string {
	t.Helper()
	return decode[struct{ Error httpx.ErrorBody }](t, r).Error.Code
}

// SPEC.md 7.1: with REQUIRE_ADMISSION=true, POST /v1/orders needs the
// waiting room's token for this user and event (403 ADMISSION_REQUIRED),
// and one token creates one order (403 ADMISSION_ALREADY_USED).
func TestAdmissionOverHTTP(t *testing.T) {
	for _, backend := range []string{"pg", "redis"} {
		t.Run(backend, func(t *testing.T) { testAdmissionOverHTTP(t, backend) })
	}
}

func testAdmissionOverHTTP(t *testing.T, backend string) {
	s := startFull(t, backend, serverOpts{requireAdmission: true})
	s.login(5)
	token := func(userID, eventID int64) string {
		tok, _, err := s.admissions.Issue(userID, eventID, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	for name, tok := range map[string]string{
		"no token":        "",
		"garbage":         "not-a-token",
		"another user's":  token(6, s.eventID),
		"another event's": token(5, s.eventID+1),
		"a user token":    s.token,
	} {
		r := s.createWith(s.eventID, tok, uuid.NewString(), "VIP-A-1")
		if r.status != http.StatusForbidden || errorCode(t, r) != "ADMISSION_REQUIRED" {
			t.Errorf("%s: %d %s, want 403 ADMISSION_REQUIRED", name, r.status, r.body)
		}
	}

	tok := token(5, s.eventID)
	key := uuid.NewString()
	first := s.createWith(s.eventID, tok, key, "VIP-A-1")
	if first.status != http.StatusCreated {
		t.Fatalf("admitted create = %d %s", first.status, first.body)
	}
	created := decode[orderResponse](t, first)

	// Replaying the same request returns the same order.
	if r := s.createWith(s.eventID, tok, key, "VIP-A-1"); r.status != http.StatusOK || decode[orderResponse](t, r).OrderID != created.OrderID {
		t.Errorf("replay = %d %s, want 200 with the same order", r.status, r.body)
	}
	// Even after cancelling, the token does not buy a second order.
	if r := s.do("POST", "/v1/orders/"+created.OrderID.String()+"/cancel", "", nil); r.status != http.StatusOK {
		t.Fatalf("cancel = %d %s", r.status, r.body)
	}
	if r := s.createWith(s.eventID, tok, uuid.NewString(), "VIP-A-2"); r.status != http.StatusForbidden || errorCode(t, r) != "ADMISSION_ALREADY_USED" {
		t.Errorf("second order with one token = %d %s, want 403 ADMISSION_ALREADY_USED", r.status, r.body)
	}

	// Seats taken: the token is given back and works for other seats.
	other := &client{t: t, base: s.base}
	other.login(7)
	if r := other.createWith(s.eventID, token(7, s.eventID), uuid.NewString(), "VIP-B-1"); r.status != http.StatusCreated {
		t.Fatalf("other buyer = %d %s", r.status, r.body)
	}
	retry := token(5, s.eventID)
	if r := s.createWith(s.eventID, retry, uuid.NewString(), "VIP-B-1"); r.status != http.StatusConflict || errorCode(t, r) != "SEATS_UNAVAILABLE" {
		t.Fatalf("taken seat = %d %s, want 409 SEATS_UNAVAILABLE", r.status, r.body)
	}
	if r := s.createWith(s.eventID, retry, uuid.NewString(), "VIP-B-2"); r.status != http.StatusCreated {
		t.Errorf("other seats with the same token = %d %s, want 201", r.status, r.body)
	}

	// Reading orders needs no admission.
	if r := s.do("GET", "/v1/orders/"+created.OrderID.String(), "", nil); r.status != http.StatusOK {
		t.Errorf("get order = %d", r.status)
	}
}
