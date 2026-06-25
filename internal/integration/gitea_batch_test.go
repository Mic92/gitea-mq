package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/batch"
	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

// TestGitea_BatchFlow_Green drives a 2-PR batch through a real Gitea: poller
// forms the batch via giteaForge.StackMerges (one clone), a single status
// webhook lands it via FastForwardRef, and both PRs leave the queue. This is
// the only test that exercises the Gitea adapter end-to-end through the batch
// engine; the ghfake batch test covers the engine wiring, the unit tests cover
// the primitives.
func TestGitea_BatchFlow_Green(t *testing.T) {
	gs := testutil.GiteaInstance()
	if gs == nil {
		t.Skip("gitea server not available")
	}

	pool := testutil.TestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	api := testutil.NewGiteaAPI(gs.URL)
	api.CreateToken(t)
	c := gitea.NewHTTPClient(gs.URL, api.Token)
	f := gitea.NewForge(c, gs.URL)

	const repoName = "batch-e2e"
	api.MustDo(t, "POST", "/user/repos",
		`{"name": "`+repoName+`", "auto_init": false, "default_branch": "main"}`)
	if err := gs.PatchRepoHooks("testuser", repoName); err != nil {
		t.Fatalf("patch hooks: %v", err)
	}
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/contents/README.md",
		`{"content": "aW5pdA==", "message": "init"}`)

	// gitea-mq in required checks keeps automerge scheduled (we never set it
	// on the PR head); enable_push lets FastForwardRef write the protected
	// branch. GetRequiredChecks filters our own context, so the engine only
	// waits for ci/build on the batch SHA.
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/branch_protections",
		`{"branch_name":"main","enable_status_check":true,`+
			`"status_check_contexts":["ci/build","gitea-mq"],"enable_push":true}`)

	type pr struct {
		n    int64
		head string
	}
	mkPR := func(branch, file string) pr {
		api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/contents/"+file,
			`{"content": "dGVzdA==", "message": "add `+file+`", "new_branch": "`+branch+`"}`)
		body := api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/pulls",
			`{"title": "`+branch+`", "head": "`+branch+`", "base": "main"}`)
		var p struct {
			Number int64 `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		_ = json.Unmarshal(body, &p)
		// CI green on the PR head so the poller enqueues it.
		api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/statuses/"+p.Head.SHA,
			`{"context": "ci/build", "state": "success"}`)
		for i := range 10 {
			st, b := api.Do(t, "POST",
				"/repos/testuser/"+repoName+"/pulls/"+itoa(p.Number)+"/merge",
				`{"Do": "merge", "merge_when_checks_succeed": true}`)
			if st >= 200 && st < 300 {
				break
			}
			if i == 9 {
				t.Fatalf("schedule automerge #%d: %d %s", p.Number, st, b)
			}
			time.Sleep(300 * time.Millisecond)
		}
		return pr{p.Number, p.Head.SHA}
	}
	prs := []pr{mkPR("feature-1", "a.txt"), mkPR("feature-2", "b.txt")}

	repo, _ := svc.GetOrCreateRepo(ctx, "gitea", "testuser", repoName)
	eng := &batch.Engine{
		Forge: f, Queue: svc, Owner: "testuser", Repo: repoName, RepoID: repo.ID,
		BatchMax: 0, MergedPollInterval: 200 * time.Millisecond, MergedPollAttempts: 15,
	}
	pollerDeps := &poller.Deps{
		Forge: f, Queue: svc, RepoID: repo.ID, Owner: "testuser", Repo: repoName,
		SuccessTimeout: 5 * time.Minute, CheckTimeout: time.Hour, Batch: eng,
	}
	monDeps := &monitor.Deps{
		Forge: f, Queue: svc, Owner: "testuser", Repo: repoName, RepoID: repo.ID,
		CheckTimeout: time.Hour, Batch: eng,
	}
	const secret = "s"
	hooks := webhook.Handler(secret,
		webhook.MapRepoLookup{"gitea:testuser/" + repoName: {Deps: monDeps}}, svc)

	// --- Poll: enqueue 2, form batch via StackMerges, push gitea-mq/batch/<id> ---
	if r, err := poller.PollOnce(ctx, pollerDeps); err != nil {
		t.Fatalf("PollOnce: %v", err)
	} else if len(r.Errors) != 0 {
		t.Fatalf("poll errors: %v", r.Errors)
	}
	live, _ := svc.GetLiveBatch(ctx, repo.ID, "main")
	if live == nil || live.State != pg.BatchStateTesting || len(live.CurrentIds) != 2 {
		t.Fatalf("live batch = %+v", live)
	}
	batchSHA := live.BranchSha.String

	// Gitea's /branches API lags raw git pushes (post-receive updates the DB
	// async); retry until the StackMerges push is visible.
	if !waitFor(func() bool {
		br, _ := c.ListBranches(ctx, "testuser", repoName)
		return hasBranch(br, live.BranchName.String)
	}) {
		t.Fatalf("batch branch %s never appeared", live.BranchName.String)
	}

	// --- CI reports success on the batch SHA ---
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/statuses/"+batchSHA,
		`{"context": "ci/build", "state": "success"}`)
	payload := fmt.Sprintf(`{"sha":%q,"context":"ci/build","state":"success","repository":{"full_name":%q}}`,
		batchSHA, "testuser/"+repoName)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Signature", webhook.ComputeSignature([]byte(payload), secret))
	w := httptest.NewRecorder()
	hooks.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}

	// --- main fast-forwarded to the tested SHA, both PRs dequeued ---
	if !waitFor(func() bool {
		_, body := api.Do(t, "GET", "/repos/testuser/"+repoName+"/branches/main", "")
		var mb struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		_ = json.Unmarshal(body, &mb)
		return mb.Commit.ID == batchSHA
	}) {
		t.Fatalf("main never reached %q", batchSHA)
	}
	br, _ := c.ListBranches(ctx, "testuser", repoName)
	if hasBranch(br, live.BranchName.String) {
		t.Fatalf("batch branch %s not cleaned up", live.BranchName.String)
	}
	if b, _ := svc.GetLiveBatch(ctx, repo.ID, "main"); b != nil {
		t.Fatalf("batch still live: %+v", b)
	}
	for _, p := range prs {
		if e, _ := svc.GetEntry(ctx, repo.ID, p.n); e != nil {
			t.Fatalf("PR #%d still queued", p.n)
		}
		got, err := c.GetPR(ctx, "testuser", repoName, p.n)
		if err != nil {
			t.Fatalf("get PR #%d: %v", p.n, err)
		}
		// Gitea may or may not flag merged-by-ancestry within the poll budget;
		// either outcome is fine — what matters is the commits landed.
		if !got.HasMerged && got.State != "closed" {
			t.Fatalf("PR #%d still open: merged=%v state=%s", p.n, got.HasMerged, got.State)
		}
	}
}

func waitFor(cond func() bool) bool {
	for range 30 {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func hasBranch(br []gitea.Branch, name string) bool {
	for _, b := range br {
		if b.Name == name {
			return true
		}
	}
	return false
}
