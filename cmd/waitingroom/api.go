package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
	"github.com/huy205-dev/ticketrush/internal/queue"
)

// streamInterval is how often the stream sends the position (SPEC.md 7.2).
const streamInterval = 2 * time.Second

// writeTimeout bounds one SSE write; the server's WriteTimeout would
// otherwise end every stream after a few seconds.
const writeTimeout = 10 * time.Second

// events is what the API needs from the catalog.
type events interface {
	Event(ctx context.Context, id int64) (*catalog.Event, error)
}

type api struct {
	logger *slog.Logger
	tokens *auth.Tokens
	events events
	queue  *queue.Queue
	// batch and interval estimate the wait: ADMIT_BATCH buyers leave the
	// queue every ADMIT_INTERVAL.
	batch    int
	interval time.Duration
	// done is closed when the server starts shutting down, so open streams
	// end instead of holding the shutdown up; browsers reconnect.
	done chan struct{}
}

func (a *api) routes(r chi.Router) {
	r.Route("/v1/queue/{eventID}", func(r chi.Router) {
		r.With(httpx.RequireUser(a.tokens, a.logger)).Post("/join", a.handleJoin)
		// EventSource cannot send an Authorization header: the stream is
		// authorized by the queue id, an unguessable value only its owner
		// got from join. The token it delivers names that owner anyway.
		r.Get("/stream", a.handleStream)
	})
}

func pathEventID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "eventID"), 10, 64)
	return id, err == nil && id > 0
}

type joinResponse struct {
	QueueID  uuid.UUID `json:"queue_id"`
	Position int64     `json:"position"`
}

// handleJoin puts the caller in the event's queue (SPEC.md 7.2). Joining
// again returns the same queue_id; position 0 means already admitted.
func (a *api) handleJoin(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathEventID(r)
	if !ok {
		httpx.WriteErr(w, r, a.logger, catalog.ErrEventNotFound)
		return
	}
	if _, err := a.events.Event(r.Context(), eventID); err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	userID, _ := auth.UserIDFrom(r.Context())
	e, err := a.queue.Join(r.Context(), eventID, userID)
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, joinResponse{QueueID: e.QueueID, Position: e.Position})
}

type positionEvent struct {
	Position             int64 `json:"position"`
	EstimatedWaitSeconds int64 `json:"estimated_wait_seconds"`
}

type admittedEvent struct {
	AdmissionToken string `json:"admission_token"`
	ExpiresIn      int64  `json:"expires_in"`
}

// handleStream sends the position every 2 s as Server-Sent Events, then the
// admission token, and ends (SPEC.md 7.2). A reconnect after admission gets
// the token at once. An entry that is no longer queued gets one "expired"
// event: the buyer joins again.
func (a *api) handleStream(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathEventID(r)
	queueID, err := uuid.Parse(r.URL.Query().Get("queue_id"))
	if !ok || err != nil {
		httpx.WriteErr(w, r, a.logger, queue.ErrNotQueued)
		return
	}
	ctx := logging.With(r.Context(), slog.String("queue_id", queueID.String()))
	r = r.WithContext(ctx)

	st, err := a.queue.Status(ctx, eventID, queueID)
	if err != nil {
		httpx.WriteErr(w, r, a.logger, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx must not buffer the stream
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	tick := time.NewTicker(streamInterval)
	defer tick.Stop()
	for {
		ended, err := a.send(rc, w, st)
		if err != nil {
			a.logger.DebugContext(ctx, "stream closed by the client", "err", err)
			return
		}
		if ended {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-a.done:
			return
		case <-tick.C:
		}
		st, err = a.queue.Status(ctx, eventID, queueID)
		switch {
		case errors.Is(err, queue.ErrNotQueued):
			_ = writeEvent(rc, w, "expired", struct{}{})
			return
		case err != nil:
			// Keep the stream; the next tick asks Redis again.
			a.logger.WarnContext(ctx, "queue status unavailable", "err", err)
			st = queue.Status{Position: -1}
		}
	}
}

// send writes the event for st and reports whether the stream is done.
func (a *api) send(rc *http.ResponseController, w http.ResponseWriter, st queue.Status) (bool, error) {
	switch {
	case st.Token != "":
		return true, writeEvent(rc, w, "admitted", admittedEvent{
			AdmissionToken: st.Token, ExpiresIn: int64(st.ExpiresIn.Seconds()),
		})
	case st.Position < 0: // status unknown this tick: keep the connection alive
		return false, writeComment(rc, w)
	default:
		return false, writeEvent(rc, w, "position", positionEvent{
			Position: st.Position, EstimatedWaitSeconds: a.estimatedWait(st.Position),
		})
	}
}

// estimatedWait assumes ADMIT_BATCH buyers leave every ADMIT_INTERVAL.
func (a *api) estimatedWait(position int64) int64 {
	batches := math.Ceil(float64(position) / float64(a.batch))
	return int64(batches * a.interval.Seconds())
}

func writeEvent(rc *http.ResponseController, w http.ResponseWriter, name string, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return err
	}
	return rc.Flush()
}

func writeComment(rc *http.ResponseController, w http.ResponseWriter) error {
	_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := fmt.Fprint(w, ": waiting\n\n"); err != nil {
		return err
	}
	return rc.Flush()
}
