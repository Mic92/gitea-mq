package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

const testSecret = "test-secret"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type testEnv struct {
	handler http.Handler
	mock    *gitea.MockClient
	svc     *queue.Service
	ctx     context.Context
	repoID  int64
}

func setup(t *testing.T) *testEnv {
	t.Helper()

	svc, ctx, repoID := testutil.TestQueueService(t)

	mock := &gitea.MockClient{}
	mock.GetBranchProtectionFn = func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
		return &gitea.BranchProtection{
			EnableStatusCheck:   true,
			StatusCheckContexts: []string{"gitea-mq", "ci/build"},
		}, nil
	}

	deps := &monitor.Deps{
		Forge:        gitea.NewForge(mock, "https://gitea.example.com"),
		Queue:        svc,
		Owner:        "org",
		Repo:         "app",
		RepoID:       repoID,
		CheckTimeout: 1 * time.Hour,
	}

	repos := webhook.MapRepoLookup{
		"gitea:org/app": {Deps: deps, RepoID: repoID},
	}

	return &testEnv{
		handler: webhook.Handler(testSecret, repos, svc),
		mock:    mock,
		svc:     svc,
		ctx:     ctx,
		repoID:  repoID,
	}
}

func makePayload(sha, checkContext, state, repo string) []byte {
	payload := map[string]any{
		"sha":     sha,
		"context": checkContext,
		"state":   state,
		"repository": map[string]string{
			"full_name": repo,
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func doRequest(handler http.Handler, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	if sig != "" {
		req.Header.Set("X-Gitea-Signature", sig)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// enqueueTesting enqueues a PR and transitions it to testing with a merge branch,
// which is the precondition for the webhook handler to find the entry.
func enqueueTesting(t *testing.T, svc *queue.Service, ctx context.Context, repoID, prNumber int64, prHeadSHA, mergeSHA string) {
	t.Helper()

	if _, err := svc.Enqueue(ctx, repoID, prNumber, prHeadSHA, "main"); err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdateState(ctx, repoID, prNumber, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetMergeBranch(ctx, repoID, prNumber, merge.BranchName(prNumber), mergeSHA); err != nil {
		t.Fatal(err)
	}
}

// HMAC is the security boundary — verify valid/missing/invalid signatures.
func TestHandler_SignatureValidation(t *testing.T) {
	env := setup(t)
	body := makePayload("abc", "ci/build", "success", "org/app")

	if rec := doRequest(env.handler, body, sign(body)); rec.Code != http.StatusOK {
		t.Fatalf("valid sig: expected 200, got %d", rec.Code)
	}
	if rec := doRequest(env.handler, body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: expected 401, got %d", rec.Code)
	}
	if rec := doRequest(env.handler, body, "deadbeef"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong sig: expected 401, got %d", rec.Code)
	}
}

// Prevents the feedback loop: gitea-mq posts status → webhook fires → must not re-process.
func TestHandler_IgnoresOwnStatus(t *testing.T) {
	env := setup(t)
	body := makePayload("abc", "gitea-mq", "success", "org/app")
	doRequest(env.handler, body, sign(body))

	if len(env.mock.CallsTo("CreateCommitStatus")) != 0 {
		t.Fatal("should not process own status — feedback loop risk")
	}
}

// Verifies that a CI status on the merge branch is mirrored to the PR head
// with a "gitea-mq/" prefix so users see queue progress on their PR.
func TestHandler_MirrorsStatusToPRHead(t *testing.T) {
	env := setup(t)

	const (
		prHeadSHA = "pr-head-abc"
		mergeSHA  = "merge-branch-def"
		prNumber  = int64(7)
	)

	enqueueTesting(t, env.svc, env.ctx, env.repoID, prNumber, prHeadSHA, mergeSHA)

	payload := map[string]any{
		"sha":         mergeSHA,
		"context":     "ci/build",
		"state":       "success",
		"description": "build passed",
		"target_url":  "https://ci.example.com/build/1",
		"repository":  map[string]string{"full_name": "org/app"},
	}
	body, _ := json.Marshal(payload)

	rec := doRequest(env.handler, body, sign(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Find the mirror call: CreateCommitStatus on the PR head with prefixed context.
	var mirror *gitea.MockCall
	for _, c := range env.mock.CallsTo("CreateCommitStatus") {
		sha := c.Args[2].(string)
		status := c.Args[3].(gitea.CommitStatus)
		if sha == prHeadSHA && status.Context == "gitea-mq/ci/build" {
			mirror = &c
			break
		}
	}

	if mirror == nil {
		t.Fatal("expected CreateCommitStatus mirror call on PR head, not found")
	}

	status := mirror.Args[3].(gitea.CommitStatus)
	if status.State != "success" {
		t.Errorf("mirror state = %q, want %q", status.State, "success")
	}
	if status.Description != "build passed" {
		t.Errorf("mirror description = %q, want %q", status.Description, "build passed")
	}
	if status.TargetURL != "https://ci.example.com/build/1" {
		t.Errorf("mirror target_url = %q, want %q", status.TargetURL, "https://ci.example.com/build/1")
	}
}
