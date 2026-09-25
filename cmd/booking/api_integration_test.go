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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/testdb"
)

var db *testdb.DB

func TestMain(m *testing.M) { testdb.Main(m, &db) }

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

func startServer(t *testing.T) (*client, int64) {
	t.Helper()
	pool := db.New(t, 20)
	eventID, err := catalog.Seed(context.Background(), pool, catalog.DemoEvent())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	inv := inventory.NewPG(pool)
	cat := catalog.NewService(pool, inv)
	orders := order.NewService(pool, cat, inv, 10*time.Minute, 30*time.Second, logger)
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
	c, eventID := startServer(t)
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
