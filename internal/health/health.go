// Package health implements the readiness endpoint. Unlike /healthz, which
// only proves the process serves HTTP, /readyz checks the dependencies the
// queue needs to make progress: the database and each configured forge.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Check probes one dependency. It must honour ctx cancellation.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// Readiness runs checks concurrently and caches the outcome for TTL so a
// tight probe interval does not turn into forge API traffic.
type Readiness struct {
	Checks  []Check
	Timeout time.Duration
	TTL     time.Duration
	// Now overrides the clock in tests; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	cachedAt time.Time
	cached   report
}

type report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (r *Readiness) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Readiness) run(ctx context.Context) report {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cachedAt.IsZero() && r.now().Sub(r.cachedAt) < r.TTL {
		return r.cached
	}

	// A canceled HTTP request must not affect the cached result for other callers.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.Timeout)
	defer cancel()

	passed := make([]bool, len(r.Checks))
	var wg sync.WaitGroup
	for i, c := range r.Checks {
		wg.Go(func() { passed[i] = c.Probe(ctx) == nil })
	}
	wg.Wait()

	rep := report{Status: "ok", Checks: make(map[string]string, len(r.Checks))}
	for i, c := range r.Checks {
		if passed[i] {
			rep.Checks[c.Name] = "ok"
		} else {
			rep.Checks[c.Name] = "unavailable"
			rep.Status = "unavailable"
		}
	}
	r.cached, r.cachedAt = rep, r.now()
	return rep
}

// ServeHTTP answers 200 when every check passes and 503 otherwise, with a
// JSON body naming each check's result.
func (r *Readiness) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rep := r.run(req.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if rep.Status != "ok" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(rep)
}
