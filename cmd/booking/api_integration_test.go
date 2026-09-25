//go:build integration

package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
	"github.com/huy205-dev/ticketrush/internal/platform/testenv"
)

var containers testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, testenv.Needs{Postgres: true, Redis: true}, &containers)
}

type client struct {
	t     *testing.T
	base  string
	token string
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (c *client) do(method, path, body string, headers map[string]string) response {
	c.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no transparent gzip handling
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: raw}
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %T: %v: %s", v, err, r.body)
	}
	return v
}

// startServer runs the booking router on fresh storage with the given
// inventory backend ("pg" or "redis").
func startServer(t *testing.T, backend string) (*client, int64) {
	t.Helper()
	pool := containers.DB.New(t, 20)
	rdb := containers.Redis.New(t)
	eventID, err := catalog.Seed(context.Background(), pool, catalog.DemoEvent())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	inv, err := inventory.New(backend, pool, rdb, logger)
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.NewService(pool, inv)
	orders := order.NewService(pool, cat, inv, redisx.NewLocker(rdb),
		order.Config{HoldTTL: 10 * time.Minute, HoldGrace: 30 * time.Second}, logger)
	a := newAPI(logger, pool, auth.NewTokens(strings.Repeat("k", 32), time.Hour), cat, orders, true)
	a.seatMaps.ttl = 0 // always fresh, so the test sees its own changes

	health := httpx.NewHealth(logger, time.Second, httpx.Check{Name: "postgres", Fn: pool.Ping})
	srv := httptest.NewServer(newRouter(logger, health, a))
	t.Cleanup(srv.Close)
	return &client{t: t, base: srv.URL}, eventID
}

func (c *client) login(userID int64) {
	c.t.Helper()
	r := c.do("POST", "/v1/auth/dev-login", `{"user_id":`+jsonInt(userID)+`}`, nil)
	if r.status != 200 {
		c.t.Fatalf("dev-login = %d %s", r.status, r.body)
	}
	c.token = decode[devLoginResponse](c.t, r).AccessToken
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }

func seatStatus(t *testing.T, c *client, eventID int64, seat string) string {
	t.Helper()
	r := c.do("GET", "/v1/events/"+jsonInt(eventID)+"/seats", "", nil)
	if r.status != 200 {
		t.Fatalf("seats = %d", r.status)
	}
	for _, s := range decode[seatMapResponse](t, r).Seats {
		if s.SeatID == seat {
			return s.Status
		}
	}
	t.Fatalf("seat %s not in seat map", seat)
	return ""
}

func TestBookingFlowOverHTTP(t *testing.T) {
	for _, backend := range []string{"pg", "redis"} {
		t.Run(backend, func(t *testing.T) { testBookingFlow(t, backend) })
	}
}

func testBookingFlow(t *testing.T, backend string) {
	c, eventID := startServer(t, backend)
	eid := jsonInt(eventID)

	t.Run("event", func(t *testing.T) {
		r := c.do("GET", "/v1/events/"+eid, "", nil)
		ev := decode[eventResponse](t, r)
		if r.status != 200 || ev.MaxSeatsPerOrder != 4 || len(ev.Zones) != 3 || ev.Zones[0].Zone != "VIP" || ev.Zones[0].Seats != 500 {
			t.Fatalf("event = %d %+v", r.status, ev)
		}
		if r := c.do("GET", "/v1/events/999", "", nil); r.status != 404 {
			t.Errorf("unknown event = %d, want 404", r.status)
		}
	})

	t.Run("seat map is gzipped when accepted", func(t *testing.T) {
		r := c.do("GET", "/v1/events/"+eid+"/seats", "", map[string]string{"Accept-Encoding": "gzip"})
		if r.status != 200 || r.header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("status %d, Content-Encoding %q", r.status, r.header.Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(strings.NewReader(string(r.body)))
		if err != nil {
			t.Fatal(err)
		}
		plain, _ := io.ReadAll(zr)
		m := decode[seatMapResponse](t, response{body: plain})
		if len(m.Seats) != 5000 || m.Seats[0].SeatID != "VIP-A-1" || m.Seats[0].Status != "AVAILABLE" {
			t.Errorf("seat map: %d seats, first %+v", len(m.Seats), m.Seats[0])
		}
	})

	t.Run("orders require a token", func(t *testing.T) {
		r := c.do("POST", "/v1/orders", `{}`, nil)
		if r.status != 401 || decode[struct{ Error httpx.ErrorBody }](t, r).Error.Code != "UNAUTHORIZED" {
			t.Errorf("no token = %d %s", r.status, r.body)
		}
	})

	c.login(42)
	key := uuid.NewString()
	body := `{"event_id":` + eid + `,"seat_ids":["VIP-A-1","VIP-A-2"]}`
	var created orderResponse

	t.Run("create", func(t *testing.T) {
		r := c.do("POST", "/v1/orders", body, map[string]string{"Idempotency-Key": key})
		if r.status != 201 {
			t.Fatalf("create = %d %s", r.status, r.body)
		}
		created = decode[orderResponse](t, r)
		if created.Status != order.StatusHeld || created.TotalVND != 7_000_000 || len(created.Seats) != 2 {
			t.Errorf("created = %+v", created)
		}
		if got := seatStatus(t, c, eventID, "VIP-A-1"); got != "HELD" {
			t.Errorf("VIP-A-1 is %s, want HELD", got)
		}
		m := decode[seatMapResponse](t, c.do("GET", "/v1/events/"+eid+"/seats", "", nil))
		if want := backend == "redis"; (m.Version > 0) != want {
			t.Errorf("seat map version = %d with backend %s", m.Version, backend)
		}
	})

	t.Run("replay returns 200 with the same order", func(t *testing.T) {
		reordered := `{"seat_ids":["VIP-A-2","VIP-A-1"],"event_id":` + eid + `}`
		r := c.do("POST", "/v1/orders", reordered, map[string]string{"Idempotency-Key": key})
		if r.status != 200 || decode[orderResponse](t, r).OrderID != created.OrderID {
			t.Errorf("replay = %d %s", r.status, r.body)
		}
	})

	t.Run("error responses", func(t *testing.T) {
		tests := []struct {
			name   string
			body   string
			key    string
			status int
			code   string
		}{
			{"key reused with another body", `{"event_id":` + eid + `,"seat_ids":["VIP-A-3"]}`, key, 422, "IDEMPOTENCY_KEY_REUSED"},
			{"missing key", body, "", 422, "VALIDATION_ERROR"},
			{"malformed key", body, "abc", 422, "VALIDATION_ERROR"},
			{"malformed json", `{"event_id":`, uuid.NewString(), 400, "BAD_REQUEST"},
			{"unknown field", `{"event_id":` + eid + `,"seat_ids":["VIP-A-3"],"x":1}`, uuid.NewString(), 400, "BAD_REQUEST"},
			{"too many seats", `{"event_id":` + eid + `,"seat_ids":["CAT1-A-1","CAT1-A-2","CAT1-A-3","CAT1-A-4","CAT1-A-5"]}`, uuid.NewString(), 422, "VALIDATION_ERROR"},
			{"active order exists", `{"event_id":` + eid + `,"seat_ids":["CAT1-A-1"]}`, uuid.NewString(), 409, "ACTIVE_ORDER_EXISTS"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				h := map[string]string{}
				if tc.key != "" {
					h["Idempotency-Key"] = tc.key
				}
				r := c.do("POST", "/v1/orders", tc.body, h)
				if got := decode[struct{ Error httpx.ErrorBody }](t, r).Error.Code; r.status != tc.status || got != tc.code {
					t.Errorf("got %d %s, want %d %s (%s)", r.status, got, tc.status, tc.code, r.body)
				}
			})
		}
	})

	t.Run("seats unavailable lists the seats", func(t *testing.T) {
		other := &client{t: t, base: c.base}
		other.login(43)
		r := other.do("POST", "/v1/orders", `{"event_id":`+eid+`,"seat_ids":["VIP-A-2","VIP-A-9"]}`,
			map[string]string{"Idempotency-Key": uuid.NewString()})
		e := decode[struct{ Error httpx.ErrorBody }](t, r).Error
		ids, _ := e.Details["seat_ids"].([]any)
		if r.status != 409 || e.Code != "SEATS_UNAVAILABLE" || len(ids) != 1 || ids[0] != "VIP-A-2" {
			t.Errorf("got %d %s", r.status, r.body)
		}

		if r := other.do("GET", "/v1/orders/"+created.OrderID.String(), "", nil); r.status != 404 {
			t.Errorf("other user's order = %d, want 404", r.status)
		}
	})

	t.Run("get and cancel", func(t *testing.T) {
		path := "/v1/orders/" + created.OrderID.String()
		if r := c.do("GET", path, "", nil); r.status != 200 || decode[orderResponse](t, r).Status != order.StatusHeld {
			t.Fatalf("get = %d %s", r.status, r.body)
		}
		for range 2 { // idempotent
			r := c.do("POST", path+"/cancel", "", nil)
			if r.status != 200 || decode[orderResponse](t, r).Status != order.StatusCancelled {
				t.Fatalf("cancel = %d %s", r.status, r.body)
			}
		}
		if got := seatStatus(t, c, eventID, "VIP-A-1"); got != "AVAILABLE" {
			t.Errorf("VIP-A-1 is %s after cancel, want AVAILABLE", got)
		}
		if r := c.do("GET", "/v1/orders/not-a-uuid", "", nil); r.status != 404 {
			t.Errorf("malformed order id = %d, want 404", r.status)
		}
	})
}

// ADR-005: ten parallel requests with one Idempotency-Key create one order.
// The others get 200 with that order or 409 IDEMPOTENCY_KEY_IN_PROGRESS with
// Retry-After; once the winner has answered, the same request returns 200.
func TestSameIdempotencyKeyInParallelOverHTTP(t *testing.T) {
	c, eventID := startServer(t, "redis")
	c.login(77)
	key := uuid.NewString()
	body := `{"event_id":` + jsonInt(eventID) + `,"seat_ids":["CAT2-B-1","CAT2-B-2"]}`

	type result struct {
		status     int
		orderID    uuid.UUID
		code       string
		retryAfter string
	}
	const workers = 10
	results := make([]result, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Go(func() {
			<-start
			req, _ := http.NewRequest("POST", c.base+"/v1/orders", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Idempotency-Key", key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			var payload struct {
				OrderID uuid.UUID       `json:"order_id"`
				Error   httpx.ErrorBody `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&payload)
			results[i] = result{status: resp.StatusCode, orderID: payload.OrderID, code: payload.Error.Code,
				retryAfter: resp.Header.Get("Retry-After")}
		})
	}
	close(start)
	wg.Wait()

	var winner uuid.UUID
	for _, r := range results {
		if r.status == http.StatusCreated {
			if winner != uuid.Nil {
				t.Fatalf("two requests got 201: %v and %v", winner, r.orderID)
			}
			winner = r.orderID
		}
	}
	if winner == uuid.Nil {
		t.Fatalf("no request got 201: %+v", results)
	}
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
		case http.StatusOK:
			if r.orderID != winner {
				t.Errorf("200 carried order %v, want %v", r.orderID, winner)
			}
		case http.StatusConflict:
			if r.code != "IDEMPOTENCY_KEY_IN_PROGRESS" || r.retryAfter != "1" {
				t.Errorf("409 with code %s and Retry-After %q, want IDEMPOTENCY_KEY_IN_PROGRESS and 1", r.code, r.retryAfter)
			}
		default:
			t.Errorf("unexpected status %d (%s)", r.status, r.code)
		}
	}

	for range 3 {
		r := c.do("POST", "/v1/orders", body, map[string]string{"Idempotency-Key": key})
		if got := decode[orderResponse](t, r); r.status != http.StatusOK || got.OrderID != winner {
			t.Fatalf("retry after the winner finished = %d %s, want 200 with order %v", r.status, r.body, winner)
		}
	}
}

// SPEC.md 7.1: the seat map carries an ETag; If-None-Match with the current
// one gets 304 without a body, and any seat change produces a new ETag.
func TestSeatMapETag(t *testing.T) {
	for _, backend := range []string{"pg", "redis"} {
		t.Run(backend, func(t *testing.T) {
			c, eventID := startServer(t, backend)
			path := "/v1/events/" + jsonInt(eventID) + "/seats"

			first := c.do("GET", path, "", nil)
			etag := first.header.Get("ETag")
			if first.status != 200 || etag == "" || first.header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("first GET = %d, ETag %q, Cache-Control %q", first.status, etag, first.header.Get("Cache-Control"))
			}

			same := c.do("GET", path, "", map[string]string{"If-None-Match": etag, "Accept-Encoding": "gzip"})
			if same.status != 304 || len(same.body) != 0 || same.header.Get("ETag") != etag {
				t.Fatalf("revalidation = %d with %d bytes and ETag %q, want 304 empty with %q",
					same.status, len(same.body), same.header.Get("ETag"), etag)
			}

			c.login(90)
			if r := c.do("POST", "/v1/orders", `{"event_id":`+jsonInt(eventID)+`,"seat_ids":["CAT1-A-1"]}`,
				map[string]string{"Idempotency-Key": uuid.NewString()}); r.status != 201 {
				t.Fatalf("create = %d %s", r.status, r.body)
			}

			changed := c.do("GET", path, "", map[string]string{"If-None-Match": etag})
			if changed.status != 200 || changed.header.Get("ETag") == etag || len(changed.body) == 0 {
				t.Fatalf("after a hold = %d with ETag %q (old %q), want 200 with a new ETag",
					changed.status, changed.header.Get("ETag"), etag)
			}
		})
	}
}
