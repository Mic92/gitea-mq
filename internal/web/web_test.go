package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jogman/gitea-mq/internal/config"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/web"
)

// staticRepoLister implements web.RepoLister for tests.
type staticRepoLister struct {
	repos []config.RepoRef
}

func (s *staticRepoLister) List() []config.RepoRef {
	return s.repos
}

func (s *staticRepoLister) Contains(fullName string) bool {
	for _, r := range s.repos {
		if r.String() == fullName {
			return true
		}
	}
	return false
}

func TestOverviewShowsRepoAndQueueData(t *testing.T) {
	pool := newTestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	repo, err := svc.GetOrCreateRepo(ctx, "org", "app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, repo.ID, 42, "abc123", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, repo.ID, 43, "def456", "main"); err != nil {
		t.Fatal(err)
	}

	deps := &web.Deps{
		Queue: svc,
		Repos: &staticRepoLister{repos: []config.RepoRef{
			{Owner: "org", Name: "app"},
			{Owner: "org", Name: "lib"},
		}},
		RefreshInterval: 5,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	// Both repos listed.
	if !strings.Contains(body, "org/app") {
		t.Error("expected org/app in overview")
	}
	if !strings.Contains(body, "org/lib") {
		t.Error("expected org/lib in overview")
	}

	// Queue size for org/app should be 2.
	if !strings.Contains(body, ">2<") {
		t.Errorf("expected queue size 2 in body:\n%s", body)
	}

	// Head-of-queue should be PR #42.
	if !strings.Contains(body, "PR #42") {
		t.Errorf("expected head-of-queue PR #42 in body:\n%s", body)
	}

	// org/lib should show empty.
	if !strings.Contains(body, "empty") {
		t.Error("expected empty badge for org/lib")
	}

	// Auto-refresh meta tag.
	if !strings.Contains(body, `content="5"`) {
		t.Error("expected meta refresh with interval 5")
	}
}

func TestRepoDetailShowsPRsAndChecks(t *testing.T) {
	pool := newTestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	repo, err := svc.GetOrCreateRepo(ctx, "org", "app")
	if err != nil {
		t.Fatal(err)
	}

	res1, err := svc.Enqueue(ctx, repo.ID, 42, "abc123", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, repo.ID, 43, "def456", "main"); err != nil {
		t.Fatal(err)
	}

	// Put head into testing state and add checks.
	if err := svc.UpdateState(ctx, repo.ID, 42, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveCheckStatus(ctx, res1.Entry.ID, "ci/build", pg.CheckStateSuccess); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveCheckStatus(ctx, res1.Entry.ID, "ci/lint", pg.CheckStatePending); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveCheckStatus(ctx, res1.Entry.ID, "ci/test", pg.CheckStateFailure); err != nil {
		t.Fatal(err)
	}

	deps := &web.Deps{
		Queue:           svc,
		Repos:           &staticRepoLister{repos: []config.RepoRef{{Owner: "org", Name: "app"}}},
		RefreshInterval: 10,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/repo/org/app", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	// Both PRs listed.
	if !strings.Contains(body, "PR #42") {
		t.Error("expected PR #42")
	}
	if !strings.Contains(body, "PR #43") {
		t.Error("expected PR #43")
	}

	// Check status icons.
	if !strings.Contains(body, "✅") {
		t.Error("expected success check icon ✅")
	}
	if !strings.Contains(body, "⏳") {
		t.Error("expected pending check icon ⏳")
	}
	if !strings.Contains(body, "❌") {
		t.Error("expected failure check icon ❌")
	}

	// Check contexts.
	if !strings.Contains(body, "ci/build") {
		t.Error("expected ci/build")
	}
	if !strings.Contains(body, "ci/lint") {
		t.Error("expected ci/lint")
	}
	if !strings.Contains(body, "ci/test") {
		t.Error("expected ci/test")
	}
}

func TestRepoDetailUnknownRepoReturns404(t *testing.T) {
	pool := newTestDB(t)
	svc := queue.NewService(pool)

	deps := &web.Deps{
		Queue:           svc,
		Repos:           &staticRepoLister{repos: []config.RepoRef{{Owner: "org", Name: "app"}}},
		RefreshInterval: 10,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/repo/org/unknown", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown repo, got %d", rec.Code)
	}
}
