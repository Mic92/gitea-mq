package integration_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	githubpkg "github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

// TestGithub_FullMergeQueueFlow exercises the GitHub adapter end-to-end
// against the in-memory ghfake: poll enqueues an auto-merge PR, a check_run
// webhook drives the monitor to success, and a follow-up poll dequeues after
// the PR is merged. This proves the adapter, webhook, poller and monitor are
// wired coherently without requiring a live GitHub.
func TestGithub_FullMergeQueueFlow(t *testing.T) {
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.AddRepo("org", "app")
	srv.AddInstallation(100, "org/app")
	repo := srv.Repo("org", "app")
	repo.RequiredChecks["main"] = []string{"ci/build"}
	pr := srv.AddPR("org", "app", ghfake.PR{
		Number: 1, BaseRef: "main", HeadSHA: "sha-head", AutoMerge: true, User: "alice",
	})
	// PR's own CI must be green before the poller enqueues it.
	repo.CheckRuns["sha-head"] = []*ghfake.CheckRun{
		{Name: "ci/build", Status: "completed", Conclusion: "success"},
	}

	app, err := githubpkg.NewApp(1, ghTestKey(t), srv.URL+"/api/v3")
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
	dbRepo, err := svc.GetOrCreateRepo(ctx, "github", "org", "app")
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	pollerDeps := &poller.Deps{
		Forge: f, Queue: svc, RepoID: dbRepo.ID, Owner: "org", Repo: "app",
		SuccessTimeout: 5 * time.Minute,
	}
	monDeps := &monitor.Deps{
		Forge: f, Queue: svc, Owner: "org", Repo: "app", RepoID: dbRepo.ID,
		CheckTimeout: time.Hour,
	}
	const secret = "gh-hook-secret"
	hooks := webhook.GithubHandler([]byte(secret),
		webhook.MapRepoLookup{"github:org/app": {Deps: monDeps, RepoID: dbRepo.ID}}, svc, nil)

	// --- Poll enqueues and creates merge branch (no PR webhook needed:
	// proves the reconcile poll alone recovers after missed deliveries) ---
	res, err := poller.PollOnce(ctx, pollerDeps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(res.Enqueued) != 1 || res.Enqueued[0] != 1 {
		t.Fatalf("enqueued=%v errors=%v", res.Enqueued, res.Errors)
	}
	head, _ := svc.Head(ctx, dbRepo.ID, "main")
	if head == nil || head.State != pg.EntryStateTesting || !head.MergeBranchSha.Valid {
		t.Fatalf("head=%+v", head)
	}
	mergeSHA := head.MergeBranchSha.String
	if repo.Refs["gitea-mq/1"] != mergeSHA {
		t.Fatalf("merge branch not on server: refs=%v", repo.Refs)
	}

	// --- CI reports success on the merge branch via check_run webhook ---
	body := fmt.Sprintf(`{"action":"completed","check_run":{"name":"ci/build","status":"completed","conclusion":"success","head_sha":%q,"details_url":"https://ci/1"},"repository":{"name":"app","owner":{"login":"org"}}}`, mergeSHA)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "check_run")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	hooks.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}

	// Monitor must have flipped gitea-mq to success on the PR head.
	var mqRun *ghfake.CheckRun
	for _, cr := range repo.CheckRuns["sha-head"] {
		if cr.Name == githubpkg.MQCheckName {
			mqRun = cr
		}
	}
	if mqRun == nil || mqRun.Status != "completed" || mqRun.Conclusion != "success" {
		t.Fatalf("gitea-mq run = %+v (all=%+v)", mqRun, repo.CheckRuns["sha-head"])
	}
	// Mirror posted on the PR head so users see merge-branch CI there.
	var mirror *ghfake.CheckRun
	for _, cr := range repo.CheckRuns["sha-head"] {
		if cr.Name == "gitea-mq/ci/build" {
			mirror = cr
		}
	}
	if mirror == nil || mirror.Conclusion != "success" {
		t.Fatalf("mirror run = %+v", mirror)
	}

	// --- GitHub auto-merge fires (simulated); next poll dequeues ---
	pr.Merged = true
	pr.State = "closed"
	res, err = poller.PollOnce(ctx, pollerDeps)
	if err != nil {
		t.Fatalf("PollOnce after merge: %v", err)
	}
	if len(res.Dequeued) != 1 || res.Dequeued[0] != 1 {
		t.Fatalf("dequeued=%v", res.Dequeued)
	}
	if _, ok := repo.Refs["gitea-mq/1"]; ok {
		t.Error("merge branch not cleaned up")
	}
}

func ghTestKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
