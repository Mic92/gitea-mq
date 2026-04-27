package github_test

import (
	"context"
	"slices"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	githubpkg "github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

func newTestForge(t *testing.T) (*ghfake.Server, forge.Forge) {
	t.Helper()
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.AddRepo("org", "app")
	srv.AddInstallation(100, "org/app")
	// htmlURL irrelevant for API tests; use github.com so URL helpers are
	// asserted against the production shape.
	return srv, githubpkg.NewForge(newTestApp(t, srv), "")
}

func TestForge_URLHelpers(t *testing.T) {
	_, f := newTestForge(t)
	if got := f.RepoHTMLURL("o", "r"); got != "https://github.com/o/r" {
		t.Errorf("RepoHTMLURL = %q", got)
	}
	if got := f.BranchHTMLURL("o", "r", "gitea-mq/7"); got != "https://github.com/o/r/tree/gitea-mq/7" {
		t.Errorf("BranchHTMLURL = %q", got)
	}
}

func TestForge_ListAndGetPR(t *testing.T) {
	srv, f := newTestForge(t)
	srv.AddPR("org", "app", ghfake.PR{
		Number: 1, Title: "feat", User: "alice",
		HeadRef: "feature", HeadSHA: "sha1", BaseRef: "main",
		AutoMerge: true,
	})
	srv.AddPR("org", "app", ghfake.PR{
		Number: 2, Title: "wip", HeadSHA: "sha2", BaseRef: "main",
	})

	ctx := context.Background()

	prs, err := f.ListOpenPRs(ctx, "org", "app")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("len = %d, want 2", len(prs))
	}

	pr, err := f.GetPR(ctx, "org", "app", 1)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if !pr.AutoMergeEnabled || pr.HeadSHA != "sha1" || pr.AuthorLogin != "alice" {
		t.Errorf("GetPR mapping = %+v", pr)
	}
}

// Two SetMQStatus calls on the same SHA must result in exactly one check run
// server-side; the second is a PATCH, not a duplicate create.
func TestForge_SetMQStatus_UpsertsSingleRun(t *testing.T) {
	srv, f := newTestForge(t)
	ctx := context.Background()

	if err := f.SetMQStatus(ctx, "org", "app", "abc", forge.MQStatus{State: pg.CheckStatePending, Description: "queued"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := f.SetMQStatus(ctx, "org", "app", "abc", forge.MQStatus{State: pg.CheckStateSuccess, Description: "merged"}); err != nil {
		t.Fatalf("second: %v", err)
	}

	runs := srv.Repo("org", "app").CheckRuns["abc"]
	if len(runs) != 1 {
		t.Fatalf("want 1 check run, got %d: %+v", len(runs), runs)
	}
	if runs[0].Name != forge.MQContext || runs[0].Status != "completed" || runs[0].Conclusion != "success" {
		t.Errorf("run = %+v", runs[0])
	}
}

// After a process restart the cache is cold; the adapter must look up the
// existing run by name and PATCH it rather than create a second.
func TestForge_SetMQStatus_ColdCacheFindsExisting(t *testing.T) {
	srv, f := newTestForge(t)
	ctx := context.Background()

	if err := f.SetMQStatus(ctx, "org", "app", "abc", forge.MQStatus{State: pg.CheckStatePending}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Fresh forge instance, same server state.
	f2 := githubpkg.NewForge(newTestApp(t, srv), "")
	if err := f2.SetMQStatus(ctx, "org", "app", "abc", forge.MQStatus{State: pg.CheckStateFailure}); err != nil {
		t.Fatalf("restart: %v", err)
	}

	runs := srv.Repo("org", "app").CheckRuns["abc"]
	if len(runs) != 1 || runs[0].Conclusion != "failure" {
		t.Fatalf("want 1 run with failure, got %+v", runs)
	}
}

func TestForge_GetRequiredChecks_ExcludesSelf(t *testing.T) {
	srv, f := newTestForge(t)
	srv.Repo("org", "app").RequiredChecks["main"] = []string{"ci/build", forge.MQContext, "ci/test"}

	got, err := f.GetRequiredChecks(context.Background(), "org", "app", "main")
	if err != nil {
		t.Fatalf("GetRequiredChecks: %v", err)
	}
	if !slices.Equal(got, []string{"ci/build", "ci/test"}) {
		t.Errorf("got %v", got)
	}
}

func TestForge_GetCheckStates_MapsRunsAndExcludesSelf(t *testing.T) {
	srv, f := newTestForge(t)
	repo := srv.Repo("org", "app")
	repo.CheckRuns["abc"] = []*ghfake.CheckRun{
		{ID: 1, Name: "ci/build", Status: "completed", Conclusion: "success", DetailsURL: "https://ci/1"},
		{ID: 2, Name: "ci/test", Status: "in_progress"},
		{ID: 3, Name: forge.MQContext, Status: "completed", Conclusion: "success"},
	}

	got, err := f.GetCheckStates(context.Background(), "org", "app", "abc")
	if err != nil {
		t.Fatalf("GetCheckStates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 contexts (self excluded), got %v", got)
	}
	if got["ci/build"].State != pg.CheckStateSuccess || got["ci/build"].TargetURL != "https://ci/1" {
		t.Errorf("ci/build = %+v", got["ci/build"])
	}
	if got["ci/test"].State != pg.CheckStatePending {
		t.Errorf("ci/test = %+v", got["ci/test"])
	}
}

func TestForge_CreateMergeBranch(t *testing.T) {
	srv, f := newTestForge(t)
	ctx := context.Background()

	sha, conflict, err := f.CreateMergeBranch(ctx, "org", "app", "main", "sha-feature", "gitea-mq/1")
	if err != nil || conflict {
		t.Fatalf("err=%v conflict=%v", err, conflict)
	}
	if sha != "merge(sha-main,sha-feature)" || srv.Repo("org", "app").Refs["gitea-mq/1"] != sha {
		t.Errorf("sha=%q refs=%v", sha, srv.Repo("org", "app").Refs)
	}

	srv.Repo("org", "app").ConflictOn["sha-conflict"] = true
	_, conflict, err = f.CreateMergeBranch(ctx, "org", "app", "main", "sha-conflict", "gitea-mq/2")
	if err != nil {
		t.Fatalf("conflict path err: %v", err)
	}
	if !conflict {
		t.Error("expected conflict=true on 409")
	}
}

// A leftover merge branch from a crashed attempt must be reset to the current
// base tip before merging, otherwise the result misses newer base commits.
func TestForge_CreateMergeBranch_ResetsStaleRef(t *testing.T) {
	srv, f := newTestForge(t)
	repo := srv.Repo("org", "app")
	repo.Refs["gitea-mq/5"] = "stale-base"
	repo.Refs["main"] = "fresh-base"

	sha, conflict, err := f.CreateMergeBranch(context.Background(), "org", "app", "main", "sha-head", "gitea-mq/5")
	if err != nil || conflict {
		t.Fatalf("err=%v conflict=%v", err, conflict)
	}
	if sha != "merge(fresh-base,sha-head)" {
		t.Errorf("sha=%q, want merge from fresh-base (stale ref not reset)", sha)
	}
}

func TestForge_DeleteAndListBranches(t *testing.T) {
	srv, f := newTestForge(t)
	srv.Repo("org", "app").Refs["gitea-mq/9"] = "x"
	ctx := context.Background()

	if err := f.DeleteBranch(ctx, "org", "app", "gitea-mq/9"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := srv.Repo("org", "app").Refs["gitea-mq/9"]; ok {
		t.Error("branch still present")
	}
	// Idempotent.
	if err := f.DeleteBranch(ctx, "org", "app", "gitea-mq/9"); err != nil {
		t.Fatalf("second delete: %v", err)
	}

	bs, err := f.ListBranches(ctx, "org", "app")
	if err != nil || !slices.Contains(bs, "main") {
		t.Fatalf("ListBranches = %v, %v", bs, err)
	}
}

func TestForge_CancelAutoMerge(t *testing.T) {
	srv, f := newTestForge(t)
	p := srv.AddPR("org", "app", ghfake.PR{Number: 1, AutoMerge: true, BaseRef: "main"})
	ctx := context.Background()

	if err := f.CancelAutoMerge(ctx, "org", "app", 1); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if p.AutoMerge {
		t.Error("AutoMerge still set")
	}

	// Second call hits "not enabled" — must be a no-op success.
	if err := f.CancelAutoMerge(ctx, "org", "app", 1); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
}
