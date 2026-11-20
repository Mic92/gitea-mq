package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/health"
)

func get(t *testing.T, h http.Handler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestReadiness(t *testing.T) {
	var dbErr error
	calls := 0
	now := time.Unix(0, 0)
	r := &health.Readiness{
		Checks: []health.Check{
			{Name: "database", Probe: func(context.Context) error { calls++; return dbErr }},
			{Name: "gitea", Probe: func(context.Context) error { return nil }},
		},
		Timeout: time.Second,
		TTL:     10 * time.Second,
		Now:     func() time.Time { return now },
	}

	code, body := get(t, r)
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthy: code=%d body=%v", code, body)
	}

	// Within the TTL the cached answer is served without probing.
	dbErr = errors.New("connection refused")
	if code, _ := get(t, r); code != http.StatusOK || calls != 1 {
		t.Fatalf("cached: code=%d calls=%d", code, calls)
	}

	now = now.Add(11 * time.Second)
	code, body = get(t, r)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("failing db: code=%d want 503", code)
	}
	checks := body["checks"].(map[string]any)
	if checks["database"] != "unavailable" || checks["gitea"] != "ok" {
		t.Fatalf("checks = %v", checks)
	}
}

func TestReadiness_Timeout(t *testing.T) {
	r := &health.Readiness{
		Checks: []health.Check{{Name: "gitea", Probe: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}},
		Timeout: 10 * time.Millisecond,
	}
	if code, _ := get(t, r); code != http.StatusServiceUnavailable {
		t.Fatalf("hung probe: code=%d want 503", code)
	}
}

func TestReadiness_ProbeOutcomes(t *testing.T) {
	names := []string{"database", "gitea", "github"}
	for mask := range 1 << len(names) {
		t.Run(fmt.Sprintf("%03b", mask), func(t *testing.T) {
			r := &health.Readiness{Timeout: time.Second}
			wantChecks := make(map[string]string)
			for i, name := range names {
				failed := mask&(1<<i) != 0
				wantChecks[name] = "ok"
				if failed {
					wantChecks[name] = "unavailable"
				}
				r.Checks = append(r.Checks, health.Check{
					Name: name,
					Probe: func(context.Context) error {
						if failed {
							return errors.New("private dependency error")
						}
						return nil
					},
				})
			}
			wantCode, wantStatus := http.StatusOK, "ok"
			if mask != 0 {
				wantCode, wantStatus = http.StatusServiceUnavailable, "unavailable"
			}
			code, body := get(t, r)
			if code != wantCode || body["status"] != wantStatus {
				t.Fatalf("response = %d %v, want %d %s", code, body, wantCode, wantStatus)
			}
			checks := body["checks"].(map[string]any)
			for name, want := range wantChecks {
				if got := checks[name]; got != want {
					t.Errorf("%s = %v, want %s", name, got, want)
				}
			}
		})
	}
}

func TestReadiness_RefreshIgnoresCallerCancellation(t *testing.T) {
	r := &health.Readiness{
		Checks: []health.Check{{Name: "database", Probe: func(ctx context.Context) error {
			return ctx.Err()
		}}},
		Timeout: time.Second,
		TTL:     time.Minute,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", nil))
	if code, _ := get(t, r); code != http.StatusOK {
		t.Fatalf("ready after canceled caller: code=%d want 200", code)
	}
}
