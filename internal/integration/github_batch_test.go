package integration_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/batch"
	"github.com/Mic92/gitea-mq/internal/forge"
	githubpkg "github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

// TestGithub_BatchFlow_Green wires poller + webhook + monitor with batching
// enabled against ghfake. Three auto-merge PRs are batched onto one branch,
// a single check_run drives the engine to fast-forward main, and all three
// PRs end up dequeued — proving routing through ActiveBatchID, the engine's
// FastForward, and ensureMergedOrClose's close-fallback.
func TestGithub_BatchFlow_Green(t *testing.T) {
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.AddRepo("org", "app")
	srv.AddInstallation(100, "org/app")
	repo := srv.Repo("org", "app")
	repo.RequiredChecks["main"] = []string{"ci"}
	for _, n := range []int64{1, 2, 3} {
		sha := fmt.Sprintf("sha%d", n)
		srv.AddPR("org", "app", ghfake.PR{
			Number: n, BaseRef: "main", HeadSHA: sha, AutoMerge: true,
		})
		repo.CheckRuns[sha] = []*ghfake.CheckRun{
			{Name: "ci", Status: "completed", Conclusion: "success"},
		}
	}

	app, err := githubpkg.NewApp(1, testutil.GithubAppKey(), srv.URL+"/api/v3")
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	ctx := t.Context()
	if err := app.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	f := githubpkg.NewForge(app, "")

	pool := testutil.TestDB(t)
	svc := queue.NewService(pool)
	dbRepo, _ := svc.GetOrCreateRepo(ctx, "github", "org", "app")

	eng := &batch.Engine{
		Forge: f, Queue: svc, Owner: "org", Repo: "app", RepoID: dbRepo.ID,
		BatchMax: 0, MergedPollInterval: 1, MergedPollAttempts: 1,
	}
	pollerDeps := &poller.Deps{
		Forge: f, Queue: svc, RepoID: dbRepo.ID, Owner: "org", Repo: "app",
		SuccessTimeout: 5 * time.Minute, CheckTimeout: time.Hour, Batch: eng,
	}
	monDeps := &monitor.Deps{
		Forge: f, Queue: svc, Owner: "org", Repo: "app", RepoID: dbRepo.ID,
		CheckTimeout: time.Hour, Batch: eng,
	}
	const secret = "s"
	hooks := webhook.GithubHandler([]byte(secret),
		webhook.MapRepoLookup{"github:org/app": {Deps: monDeps}}, svc, nil)

	// --- Poll: enqueue 3, form one batch, build branch ---
	if _, err := poller.PollOnce(ctx, pollerDeps); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	live, _ := svc.GetLiveBatch(ctx, dbRepo.ID, "main")
	if live == nil || live.State != pg.BatchStateTesting || len(live.CurrentIds) != 3 {
		t.Fatalf("live batch = %+v", live)
	}
	branchSHA := live.BranchSha.String
	if repo.Refs[live.BranchName.String] != branchSHA {
		t.Fatalf("batch branch not on server: %v", repo.Refs)
	}

	postCheckRun := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", "check_run")
		req.Header.Set("X-Hub-Signature-256", "sha256="+webhook.ComputeSignature([]byte(body), secret))
		w := httptest.NewRecorder()
		hooks.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
		}
	}

	// Polling can mirror optional effects that are still running when all
	// required checks pass.
	if err := f.MirrorCheck(ctx, "org", "app", "sha1", forge.MirrorContextPrefix+"effects", forge.Check{
		State: pg.CheckStatePending, Description: "running effects", TargetURL: "https://ci/effects",
	}); err != nil {
		t.Fatalf("seed effects mirror: %v", err)
	}

	// --- Required CI reports success on the batch branch ---
	postCheckRun(fmt.Sprintf(`{"action":"completed","check_run":{"name":"ci","status":"completed","conclusion":"success","head_sha":%q},"repository":{"name":"app","owner":{"login":"org"}}}`, branchSHA))

	// --- Engine fast-forwarded main to the tested SHA, batch done ---
	if repo.Refs["main"] != branchSHA {
		t.Fatalf("main = %q, want %q", repo.Refs["main"], branchSHA)
	}
	for _, n := range []int64{1, 2, 3} {
		if e, _ := svc.GetEntry(ctx, dbRepo.ID, n); e != nil {
			t.Fatalf("PR #%d still queued", n)
		}
		// ghfake never auto-detects merged-by-ancestry → close fallback fires.
		if repo.PRs[n].State != "closed" {
			t.Fatalf("PR #%d state = %q, want closed", n, repo.PRs[n].State)
		}
		var ok bool
		for _, cr := range repo.CheckRuns[fmt.Sprintf("sha%d", n)] {
			if cr.Name == forge.MQContext && cr.Conclusion == "success" {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("PR #%d missing gitea-mq success", n)
		}
	}
	// Optional check finishes after landing. Its webhook no longer has an
	// active entry to route through, so terminal cleanup must already have
	// closed the mirror.
	postCheckRun(fmt.Sprintf(`{"action":"completed","check_run":{"name":"effects","status":"completed","conclusion":"success","head_sha":%q},"repository":{"name":"app","owner":{"login":"org"}}}`, branchSHA))

	var effects *ghfake.CheckRun
	for _, cr := range repo.CheckRuns["sha1"] {
		if cr.Name == forge.MirrorContextPrefix+"effects" {
			effects = cr
		}
	}
	if effects == nil || effects.Status != "completed" || effects.Conclusion != "skipped" {
		t.Fatalf("pending effects mirror not closed: %+v", effects)
	}

	if b, _ := svc.GetLiveBatch(ctx, dbRepo.ID, "main"); b != nil {
		t.Fatalf("batch still live: %+v", b)
	}
	if _, ok := repo.Refs[live.BranchName.String]; ok {
		t.Fatalf("batch branch %s not cleaned up", live.BranchName.String)
	}
}

// The merge label on the top of a 3-PR stack lands it through merge-async, not
// a fast-forward, and later polls must not merge the still-open top again.
func TestGithub_BatchFlow_LabeledStack(t *testing.T) {
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.AddRepo("org", "app")
	srv.AddInstallation(100, "org/app")
	repo := srv.Repo("org", "app")
	repo.RequiredChecks["main"] = []string{"ci"}
	repo.Stacks = []ghfake.Stack{{BaseRef: "main", PRs: []int64{1, 2, 3}}}
	repo.Permissions = map[string]string{"maint": "write"}
	repo.LabelEvents = map[int64][]ghfake.LabelEvent{3: {{Actor: "maint", Label: "merge-queue"}}}
	for n := int64(1); n <= 3; n++ {
		sha := fmt.Sprintf("sha%d", n)
		base := "main"
		if n > 1 {
			base = fmt.Sprintf("b%d", n-1)
		}
		pr := ghfake.PR{Number: n, BaseRef: base, HeadRef: fmt.Sprintf("b%d", n), HeadSHA: sha}
		if n == 3 {
			pr.Labels = []string{"merge-queue"}
		}
		srv.AddPR("org", "app", pr)
		repo.CheckRuns[sha] = []*ghfake.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}
		// The top head does not yet appear in main; the tested tip is current.
		repo.BehindBy[sha+"...main"] = 1
	}
	mainBefore := repo.Refs["main"]

	app, err := githubpkg.NewApp(1, testutil.GithubAppKey(), srv.URL+"/api/v3")
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	ctx := t.Context()
	if err := app.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	f := githubpkg.NewForge(app, "")

	pool := testutil.TestDB(t)
	svc := queue.NewService(pool)
	dbRepo, _ := svc.GetOrCreateRepo(ctx, "github", "org", "app")

	eng := &batch.Engine{
		Forge: f, Queue: svc, Owner: "org", Repo: "app", RepoID: dbRepo.ID,
		BatchMax: 5, SkipIfUpToDate: true, MergeLabel: "merge-queue",
		MergedPollInterval: 1, MergedPollAttempts: 1,
		StackMergedPollInterval: 1, StackMergedPollAttempts: 1,
	}
	pollerDeps := &poller.Deps{
		Forge: f, Queue: svc, RepoID: dbRepo.ID, Owner: "org", Repo: "app",
		SuccessTimeout: 5 * time.Minute, CheckTimeout: time.Hour, Batch: eng,
		MergeLabel: "merge-queue",
	}

	// GitHub keeps all three PRs open while merge-async is pending.
	for range 3 {
		if _, err := poller.PollOnce(ctx, pollerDeps); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
	}

	if len(repo.AsyncMerges) != 1 || repo.AsyncMerges[0] != 3 {
		t.Fatalf("AsyncMerges = %v, want exactly [3]", repo.AsyncMerges)
	}
	if repo.Refs["main"] != mainBefore {
		t.Fatalf("main moved to %q; stacks must land via the forge", repo.Refs["main"])
	}
	for n := int64(1); n <= 3; n++ {
		if repo.PRs[n].State != "open" {
			t.Fatalf("PR #%d state = %q; gitea-mq must not close stack members", n, repo.PRs[n].State)
		}
		var green bool
		for _, cr := range repo.CheckRuns[fmt.Sprintf("sha%d", n)] {
			if cr.Name == forge.MQContext && cr.Conclusion == "success" {
				green = true
			}
		}
		if !green {
			t.Fatalf("PR #%d missing gitea-mq success before the stack merge", n)
		}
	}
	if e, _ := svc.GetEntry(ctx, dbRepo.ID, 3); e != nil {
		t.Fatalf("landed top re-enqueued: %+v", e)
	}
}
