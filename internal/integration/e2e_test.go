// Package integration_test contains end-to-end tests that exercise the full
// flow against a real Gitea instance and real PostgreSQL:
// automerge detection → enqueue → merge branch → CI status on merge branch →
// webhook delivers to monitor → gitea-mq success → Gitea automerge → advance.
package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/monitor"
	"github.com/jogman/gitea-mq/internal/poller"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/testutil"
	"github.com/jogman/gitea-mq/internal/webhook"
)

// TestFullMergeQueueFlow exercises the complete lifecycle against a real
// Gitea instance:
//
//  1. Create a repo with branch protection requiring ci/build + gitea-mq
//  2. Create a PR and schedule automerge
//  3. Poll detects PR with automerge → enqueued, merge branch created
//  4. Set ci/build=success on merge branch SHA via Gitea API
//  5. Deliver the status event to the webhook handler
//  6. Monitor sets gitea-mq=success on PR head
//  7. Set ci/build=success on PR head for Gitea's automerge
//  8. Gitea automerge merges the PR
//  9. Poll detects merge → removed from queue
func TestFullMergeQueueFlow(t *testing.T) {
	giteaServer := testutil.GiteaInstance()
	if giteaServer == nil {
		t.Skip("gitea server not available")
	}

	pool := newTestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	// Set up Gitea: token, repo, branch protection.
	api := testutil.NewGiteaAPI(giteaServer.URL)
	api.CreateToken(t)

	giteaClient := gitea.NewHTTPClient(giteaServer.URL, api.Token)

	repoName := "e2e-mq-test"

	// Create repo without auto_init so we can patch Gitea's hook scripts
	// before any git operations. Gitea generates hooks with #!/usr/bin/env bash
	// which fails in nix build sandboxes where /usr/bin/env doesn't exist.
	api.MustDo(t, "POST", "/user/repos",
		`{"name": "`+repoName+`", "auto_init": false, "default_branch": "main"}`)

	if err := giteaServer.PatchRepoHooks("testuser", repoName); err != nil {
		t.Fatalf("patch hooks: %v", err)
	}

	// Initialize repo with a file on main.
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/contents/README.md",
		`{"content": "aW5pdA==", "message": "initial commit"}`)

	// Set up branch protection requiring ci/build and gitea-mq.
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/branch_protections",
		`{"branch_name": "main", "enable_status_check": true, "status_check_contexts": ["ci/build", "gitea-mq"]}`)

	// Create a feature branch with a file change.
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/contents/test.txt",
		`{"content": "dGVzdA==", "message": "add test file", "new_branch": "feature-1"}`)

	// Create a PR from feature-1 → main.
	prBody := api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/pulls",
		`{"title": "Test PR", "head": "feature-1", "base": "main"}`)

	var pr struct {
		Number int64 `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(prBody, &pr); err != nil {
		t.Fatalf("unmarshal PR: %v", err)
	}

	// Schedule automerge.
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/pulls/"+itoa(pr.Number)+"/merge",
		`{"Do": "merge", "merge_when_checks_succeed": true}`)

	// Register repo in DB.
	repo, err := svc.GetOrCreateRepo(ctx, "testuser", repoName)
	if err != nil {
		t.Fatalf("register repo: %v", err)
	}

	pollerDeps := &poller.Deps{
		Gitea:          giteaClient,
		Queue:          svc,
		RepoID:         repo.ID,
		Owner:          "testuser",
		Repo:           repoName,
		SuccessTimeout: 5 * time.Minute,
	}

	monDeps := &monitor.Deps{
		Gitea:        giteaClient,
		Queue:        svc,
		Owner:        "testuser",
		Repo:         repoName,
		RepoID:       repo.ID,
		CheckTimeout: 1 * time.Hour,
	}

	// Set up the webhook handler so we can deliver status events to it.
	repoKey := "testuser/" + repoName
	repoMonitors := map[string]*webhook.RepoMonitor{
		repoKey: {
			Deps:   monDeps,
			RepoID: repo.ID,
		},
	}
	webhookSecret := "test-secret"
	webhookHandler := webhook.Handler(webhookSecret, repoMonitors, svc)

	// --- Step 1: Poll detects automerge → enqueues PR, creates merge branch ---
	result, err := poller.PollOnce(ctx, pollerDeps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(result.Enqueued) != 1 || result.Enqueued[0] != pr.Number {
		t.Fatalf("expected PR #%d enqueued, got %v (errors: %v)", pr.Number, result.Enqueued, result.Errors)
	}

	// Verify state is testing with a merge branch.
	head, err := svc.Head(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head == nil || head.PrNumber != pr.Number {
		t.Fatalf("expected PR #%d as head, got %v", pr.Number, head)
	}
	if head.State != pg.EntryStateTesting {
		t.Fatalf("expected state=testing, got %s", head.State)
	}
	if !head.MergeBranchSha.Valid || head.MergeBranchSha.String == "" {
		t.Fatal("expected merge branch SHA to be set")
	}

	mergeBranchSHA := head.MergeBranchSha.String
	t.Logf("merge branch: %s sha: %s", head.MergeBranchName.String, mergeBranchSHA)

	// --- Step 2: Set ci/build=success on the merge branch SHA ---
	// This is what external CI would do in production.
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/statuses/"+mergeBranchSHA,
		`{"context": "ci/build", "state": "success", "description": "build passed"}`)

	// --- Step 3: Deliver the status event to the webhook handler ---
	// In production, Gitea sends this webhook. We simulate the delivery.
	statusPayload := fmt.Sprintf(`{
		"sha": %q,
		"context": "ci/build",
		"state": "success",
		"repository": {"full_name": %q}
	}`, mergeBranchSHA, repoKey)

	webhookReq, err := http.NewRequest(http.MethodPost, "/webhook", strings.NewReader(statusPayload))
	if err != nil {
		t.Fatalf("create webhook request: %v", err)
	}
	webhookReq.Header.Set("Content-Type", "application/json")
	webhookReq.Header.Set("X-Gitea-Signature", webhook.ComputeSignature([]byte(statusPayload), webhookSecret))

	recorder := &httpRecorder{}
	webhookHandler.ServeHTTP(recorder, webhookReq)
	if recorder.statusCode != http.StatusOK {
		t.Fatalf("webhook returned %d", recorder.statusCode)
	}

	// --- Step 4: Verify monitor set gitea-mq=success on PR head ---
	head, err = svc.Head(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head == nil || head.PrNumber != pr.Number {
		t.Fatal("expected PR still as head")
	}
	if head.State != pg.EntryStateSuccess {
		t.Fatalf("expected state=success after webhook, got %s", head.State)
	}

	// Verify gitea-mq=success was posted on the PR head SHA in Gitea.
	_, statusBody := api.Do(t, "GET", "/repos/testuser/"+repoName+"/statuses/"+pr.Head.SHA, "")

	var statuses []struct {
		Context string `json:"context"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(statusBody, &statuses); err != nil {
		t.Fatalf("unmarshal statuses: %v", err)
	}

	foundMQSuccess := false
	for _, s := range statuses {
		if s.Context == "gitea-mq" && s.Status == "success" {
			foundMQSuccess = true
		}
	}
	if !foundMQSuccess {
		t.Fatalf("expected gitea-mq=success on PR head, got: %s", statusBody)
	}

	// --- Step 5: Set ci/build=success on PR head for Gitea automerge ---
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/statuses/"+pr.Head.SHA,
		`{"context": "ci/build", "state": "success", "description": "build passed"}`)

	// --- Step 6: Wait for Gitea automerge ---
	var merged bool
	for range 60 {
		_, prRespBody := api.Do(t, "GET", "/repos/testuser/"+repoName+"/pulls/"+itoa(pr.Number), "")

		var prState struct {
			Merged bool `json:"merged"`
		}
		if err := json.Unmarshal(prRespBody, &prState); err != nil {
			t.Fatalf("unmarshal PR state: %v", err)
		}

		if prState.Merged {
			merged = true
			break
		}

		time.Sleep(500 * time.Millisecond)
	}
	if !merged {
		t.Fatal("PR was not merged by Gitea automerge within timeout")
	}

	// --- Step 7: Poll detects merge → removes from queue ---
	result, err = poller.PollOnce(ctx, pollerDeps)
	if err != nil {
		t.Fatalf("PollOnce after merge: %v", err)
	}

	found := false
	for _, d := range result.Dequeued {
		if d == pr.Number {
			found = true
		}
	}
	if !found {
		t.Errorf("expected PR #%d to be dequeued, got dequeued=%v", pr.Number, result.Dequeued)
	}

	// Queue should be empty.
	head, err = svc.Head(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head != nil {
		t.Fatalf("expected empty queue, got %v", head)
	}
}

// httpRecorder is a minimal ResponseWriter for testing handlers.
type httpRecorder struct {
	statusCode int
	body       []byte
}

func (r *httpRecorder) Header() http.Header  { return http.Header{} }
func (r *httpRecorder) WriteHeader(code int) { r.statusCode = code }
func (r *httpRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	return len(b), nil
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}
