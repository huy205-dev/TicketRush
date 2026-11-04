//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/platform/testenv"
	"github.com/huy205-dev/ticketrush/internal/queue"
)

var containers testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, testenv.Needs{Postgres: true, Redis: true}, &containers)
}

type room struct {
	srv       *httptest.Server
	worker    *queue.Worker
	users     *auth.Tokens
	admission *auth.AdmissionTokens
	eventID   int64
}

func startRoom(t *testing.T) room {
	t.Helper()
	pool := containers.DB.New(t, 5)
	rdb := containers.Redis.New(t)
	eventID, err := catalog.Seed(context.Background(), pool, catalog.DemoEvent())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	cat := catalog.NewService(pool, nil)
	q := queue.New(rdb)
	rm := room{
		users:     auth.NewTokens(strings.Repeat("u", 32), time.Hour),
		admission: auth.NewAdmissionTokens(strings.Repeat("a", 32), 5*time.Minute),
		eventID:   eventID,
	}
	rm.worker = queue.NewWorker(q, rm.admission, cat, 1, logger)
	a := &api{logger: logger, tokens: rm.users, events: cat, queue: q, batch: 1, interval: time.Second, done: make(chan struct{})}
	health := httpx.NewHealth(logger, time.Second)
	rm.srv = httptest.NewServer(newRouter(logger, health, a))
	t.Cleanup(rm.srv.Close)
	return rm
}

func (rm room) join(t *testing.T, userID int64) (int, joinResponse) {
	t.Helper()
	tok, _ := rm.users.Issue(userID)
	req, _ := http.NewRequest("POST", rm.srv.URL+"/v1/queue/"+strconv.FormatInt(rm.eventID, 10)+"/join", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body joinResponse
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

type sse struct{ name, data string }

// readEvents reads SSE events from body until it ends or n events arrived.
func readEvents(t *testing.T, body io.Reader, n int) []sse {
	t.Helper()
	var out []sse
	var cur sse
	sc := bufio.NewScanner(body)
	for sc.Scan() && len(out) < n {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "" && cur.name != "":
			out = append(out, cur)
			cur = sse{}
		}
	}
	return out
}

func (rm room) stream(t *testing.T, queueID uuid.UUID) *http.Response {
	t.Helper()
	res, err := http.Get(rm.srv.URL + "/v1/queue/" + strconv.FormatInt(rm.eventID, 10) + "/stream?queue_id=" + queueID.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// SPEC.md 7.2 over HTTP: join, follow the position over SSE, get the token
// when the worker admits the buyer; a reconnect gets the token at once.
func TestWaitingRoomOverHTTP(t *testing.T) {
	rm := startRoom(t)

	if res, err := http.Post(rm.srv.URL+"/v1/queue/1/join", "", nil); err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Errorf("join without a token = %v, %v; want 401", res, err)
	}
	tok, _ := rm.users.Issue(1)
	req, _ := http.NewRequest("POST", rm.srv.URL+"/v1/queue/999/join", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusNotFound {
		t.Errorf("join an unknown event = %v, %v; want 404", res, err)
	}

	code, first := rm.join(t, 1)
	_, second := rm.join(t, 2)
	if code != 200 || first.Position != 1 || second.Position != 2 {
		t.Fatalf("joins = %d %+v %+v", code, first, second)
	}
	if _, again := rm.join(t, 2); again != second {
		t.Errorf("join again = %+v, want %+v", again, second)
	}

	res := rm.stream(t, second.QueueID)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	ev := readEvents(t, res.Body, 1)
	if len(ev) != 1 || ev[0].name != "position" || !strings.Contains(ev[0].data, `"position":2`) {
		t.Fatalf("first event = %+v, want position 2", ev)
	}

	// Batch 1: the first tick admits buyer 1, the second buyer 2.
	for range 2 {
		if _, err := rm.worker.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	ev = readEvents(t, res.Body, 2)
	last := ev[len(ev)-1]
	if last.name != "admitted" {
		t.Fatalf("events after admission = %+v, want admitted", ev)
	}
	var adm admittedEvent
	if err := json.Unmarshal([]byte(last.data), &adm); err != nil {
		t.Fatal(err)
	}
	got, err := rm.admission.Verify(adm.AdmissionToken)
	if err != nil || got.UserID != 2 || got.EventID != rm.eventID || got.QueueID != second.QueueID ||
		adm.ExpiresIn <= 0 || adm.ExpiresIn > 300 {
		t.Errorf("admitted = %+v (%+v, %v)", adm, got, err)
	}
	if extra := readEvents(t, res.Body, 1); len(extra) != 0 {
		t.Errorf("stream kept going after admitted: %+v", extra)
	}

	// Reconnecting after admission gets the token right away.
	again := readEvents(t, rm.stream(t, second.QueueID).Body, 1)
	if len(again) != 1 || again[0].name != "admitted" || !strings.Contains(again[0].data, adm.AdmissionToken) {
		t.Errorf("reconnect = %+v, want the same token", again)
	}

	if res := rm.stream(t, uuid.New()); res.StatusCode != http.StatusNotFound {
		t.Errorf("stream of an unknown queue id = %d, want 404", res.StatusCode)
	}
}
