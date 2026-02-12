package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jogman/gitea-mq/internal/config"
	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/testutil"
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
	return slices.ContainsFunc(s.repos, func(r config.RepoRef) bool {
		return r.String() == fullName
	})
}

func TestOverviewShowsRepoAndQueueData(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)
	if _, err := svc.Enqueue(ctx, repoID, 42, "abc123", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, repoID, 43, "def456", "main"); err != nil {
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

	// Both repos listed as links.
	if !strings.Contains(body, `href="/repo/org/app"`) {
		t.Error("expected link to org/app repo page")
	}
	if !strings.Contains(body, `href="/repo/org/lib"`) {
		t.Error("expected link to org/lib repo page")
	}

	// Queue count badge for org/app should be 2.
	if !strings.Contains(body, ">2<") {
		t.Errorf("expected queue count 2 in body:\n%s", body)
	}

	// org/lib should show 0 badge.
	if !strings.Contains(body, ">0<") {
		t.Errorf("expected queue count 0 for org/lib in body:\n%s", body)
	}

	// Auto-refresh meta tag.
	if !strings.Contains(body, `content="5"`) {
		t.Error("expected meta refresh with interval 5")
	}

	// Breadcrumb: gitea-mq as plain text (current page).
	if !strings.Contains(body, "<nav") {
		t.Error("expected breadcrumb nav element")
	}
}

func TestOverviewNoReposShowsHelpMessage(t *testing.T) {
	svc, _, _ := testutil.TestQueueService(t)

	deps := &web.Deps{
		Queue:           svc,
		Repos:           &staticRepoLister{repos: nil},
		RefreshInterval: 10,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "No repositories discovered yet") {
		t.Errorf("expected helpful setup message, got:\n%s", body)
	}
}

func TestRepoDetailShowsPRs(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "abc123", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, repoID, 43, "def456", "main"); err != nil {
		t.Fatal(err)
	}

	// Put head into testing state.
	if err := svc.UpdateState(ctx, repoID, 42, pg.EntryStateTesting); err != nil {
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

	// Both PRs listed as links to PR detail pages.
	if !strings.Contains(body, `href="/repo/org/app/pr/42"`) {
		t.Errorf("expected link to PR #42 detail page, body:\n%s", body)
	}
	if !strings.Contains(body, `href="/repo/org/app/pr/43"`) {
		t.Errorf("expected link to PR #43 detail page, body:\n%s", body)
	}

	// Check statuses should NOT be on this page.
	if strings.Contains(body, "ci/build") || strings.Contains(body, "ci/lint") || strings.Contains(body, "ci/test") {
		t.Error("repo page should not show check statuses")
	}

	// Breadcrumb: gitea-mq (link) › org/app (plain text).
	if !strings.Contains(body, `<nav`) {
		t.Error("expected breadcrumb nav element")
	}
	if !strings.Contains(body, `href="/"`) {
		t.Error("expected breadcrumb link to overview")
	}
}

func TestPRDetailHeadOfQueueTesting(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	res1, err := svc.Enqueue(ctx, repoID, 42, "abc123", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, repoID, 42, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}
	// Set merge branch so the link can be constructed.
	if err := svc.SetMergeBranch(ctx, repoID, 42, "gitea-mq/abc123", "mergesha"); err != nil {
		t.Fatal(err)
	}
	// Only ci/build has reported — ci/lint and ci/test have not.
	if err := svc.SaveCheckStatus(ctx, res1.Entry.ID, "ci/build", pg.CheckStateSuccess, "https://ci.example.com/build/1"); err != nil {
		t.Fatal(err)
	}

	mock := &gitea.MockClient{}
	mock.GetPRFn = func(_ context.Context, _, _ string, _ int64) (*gitea.PR, error) {
		return &gitea.PR{
			Index:   42,
			Title:   "Fix login bug",
			User:    &gitea.User{Login: "alice"},
			HTMLURL: "https://gitea.example.com/org/app/pulls/42",
		}, nil
	}
	// Branch protection requires ci/build, ci/lint, ci/test.
	mock.GetBranchProtectionFn = func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
		return &gitea.BranchProtection{
			EnableStatusCheck:   true,
			StatusCheckContexts: []string{"gitea-mq", "ci/build", "ci/lint", "ci/test"},
		}, nil
	}

	deps := &web.Deps{
		Queue:           svc,
		Repos:           &staticRepoLister{repos: []config.RepoRef{{Owner: "org", Name: "app"}}},
		Gitea:           mock,
		RefreshInterval: 10,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/repo/org/app/pr/42", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Fix login bug") {
		t.Error("expected PR title")
	}
	// PR number in h1 should link to Gitea.
	if !strings.Contains(body, `<a href="https://gitea.example.com/org/app/pulls/42">PR #42</a>`) {
		t.Errorf("expected PR link in heading, got:\n%s", body)
	}
	if !strings.Contains(body, "alice") {
		t.Error("expected PR author")
	}
	if !strings.Contains(body, "testing") {
		t.Error("expected testing state")
	}
	// ci/build reported success.
	if !strings.Contains(body, "✅") {
		t.Error("expected success check icon for ci/build")
	}
	if !strings.Contains(body, "ci/build") {
		t.Error("expected ci/build check name")
	}
	// ci/lint and ci/test haven't reported yet — should appear as pending.
	if !strings.Contains(body, "ci/lint") {
		t.Error("expected ci/lint (unreported required check should show as pending)")
	}
	if !strings.Contains(body, "ci/test") {
		t.Error("expected ci/test (unreported required check should show as pending)")
	}
	if !strings.Contains(body, "⏳") {
		t.Error("expected pending check icon for unreported checks")
	}
	// Merge branch link derived from PR.HTMLURL.
	if !strings.Contains(body, `href="https://gitea.example.com/org/app/src/branch/gitea-mq/abc123"`) {
		t.Errorf("expected merge branch link, got:\n%s", body)
	}
	if !strings.Contains(body, "view on Gitea") {
		t.Error("expected 'view on Gitea' link text")
	}
	// ci/build has a target URL — should be a clickable link.
	if !strings.Contains(body, `href="https://ci.example.com/build/1"`) {
		t.Errorf("expected clickable check link for ci/build, got:\n%s", body)
	}
	// ci/lint has no target URL — should NOT be a link.
	if strings.Contains(body, `>ci/lint ↗</a>`) {
		t.Error("ci/lint should not be a link (no target_url)")
	}
}

func TestPRDetailNonHeadQueued(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "abc123", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Enqueue(ctx, repoID, 43, "def456", "main"); err != nil {
		t.Fatal(err)
	}

	mock := &gitea.MockClient{}
	mock.GetPRFn = func(_ context.Context, _, _ string, index int64) (*gitea.PR, error) {
		return &gitea.PR{Index: index, Title: "Some PR", User: &gitea.User{Login: "bob"}}, nil
	}

	deps := &web.Deps{
		Queue:           svc,
		Repos:           &staticRepoLister{repos: []config.RepoRef{{Owner: "org", Name: "app"}}},
		Gitea:           mock,
		RefreshInterval: 10,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/repo/org/app/pr/43", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "queued") {
		t.Error("expected queued state")
	}
	if !strings.Contains(body, "#2") {
		t.Error("expected position #2")
	}
	// Non-head PR should NOT show check statuses.
	if strings.Contains(body, "ci/build") {
		t.Error("non-head PR should not show checks")
	}
}

func TestPRDetailNotInQueue(t *testing.T) {
	svc, _, _ := testutil.TestQueueService(t)

	deps := &web.Deps{
		Queue:           svc,
		Repos:           &staticRepoLister{repos: []config.RepoRef{{Owner: "org", Name: "app"}}},
		RefreshInterval: 10,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/repo/org/app/pr/99", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for PR not in queue, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "not in the merge queue") {
		t.Errorf("expected 'not in the merge queue' message, got:\n%s", body)
	}
}

func TestPRDetailGiteaAPIFailure(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "abc123", "main"); err != nil {
		t.Fatal(err)
	}

	mock := &gitea.MockClient{}
	mock.GetPRFn = func(_ context.Context, _, _ string, _ int64) (*gitea.PR, error) {
		return nil, fmt.Errorf("connection refused")
	}

	deps := &web.Deps{
		Queue:           svc,
		Repos:           &staticRepoLister{repos: []config.RepoRef{{Owner: "org", Name: "app"}}},
		Gitea:           mock,
		RefreshInterval: 10,
	}

	mux := web.NewMux(deps)
	req := httptest.NewRequest(http.MethodGet, "/repo/org/app/pr/42", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 even on API failure, got %d", rec.Code)
	}

	body := rec.Body.String()
	// Should show placeholders.
	if !strings.Contains(body, "—") {
		t.Error("expected '—' placeholder for title/author on API failure")
	}
	// Should still show queue state.
	if !strings.Contains(body, "queued") {
		t.Error("expected queue state even on API failure")
	}
}

func TestRepoDetailUnknownRepoReturns404(t *testing.T) {
	svc, _, _ := testutil.TestQueueService(t)

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
