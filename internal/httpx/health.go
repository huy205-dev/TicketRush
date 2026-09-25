package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Check is one readiness dependency, e.g. PostgreSQL or Redis.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Health serves /healthz and /readyz.
type Health struct {
	logger  *slog.Logger
	timeout time.Duration
	checks  []Check
}

// NewHealth returns health handlers that give each readiness check at most
// timeout to answer.
func NewHealth(logger *slog.Logger, timeout time.Duration, checks ...Check) *Health {
	return &Health{logger: logger, timeout: timeout, checks: checks}
}

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Live reports that the process is up. It never touches dependencies, so a
// database outage does not get healthy pods restarted.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// Ready runs every check concurrently and answers 200 only if all pass,
// otherwise 503. Failure reasons are logged, not returned, to avoid exposing
// internal addresses on a public port.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	errs := make([]error, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		wg.Go(func() { errs[i] = c.Fn(ctx) })
	}
	wg.Wait()

	resp := healthResponse{Status: "ok", Checks: make(map[string]string, len(h.checks))}
	status := http.StatusOK
	for i, c := range h.checks {
		if errs[i] != nil {
			h.logger.WarnContext(r.Context(), "readiness check failed", "check", c.Name, "err", errs[i])
			resp.Checks[c.Name] = "fail"
			resp.Status = "unavailable"
			status = http.StatusServiceUnavailable
			continue
		}
		resp.Checks[c.Name] = "ok"
	}
	WriteJSON(w, status, resp)
}
