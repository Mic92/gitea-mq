package webhook_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

const testSecret = "test-secret"

func sign(body []byte) string { return webhook.ComputeSignature(body, testSecret) }

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
		"gitea:org/app": {Deps: deps},
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

// HMAC is the security boundary for both forge endpoints.
func TestHandler_SignatureValidation(t *testing.T) {
	giteaBody := string(makePayload("abc", "ci/build", "success", "x/y"))
	for _, tc := range []struct {
		name    string
		handler http.Handler
		header  string
		body    string
		good    string
	}{
		{
			"gitea", webhook.Handler(testSecret, webhook.MapRepoLookup{}, nil),
			"X-Gitea-Signature", giteaBody, sign([]byte(giteaBody)),
		},
		{
			"github", webhook.GithubHandler([]byte(testSecret), webhook.MapRepoLookup{}, nil, nil),
			"X-Hub-Signature-256", `{}`, "sha256=" + sign([]byte(`{}`)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, sig := range []struct {
				name string
				val  string
				code int
			}{
				{"valid", tc.good, http.StatusOK},
				{"missing", "", http.StatusUnauthorized},
				{"wrong", "deadbeef", http.StatusUnauthorized},
			} {
				req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-GitHub-Event", "ping")
				if sig.val != "" {
					req.Header.Set(tc.header, sig.val)
				}
				rec := httptest.NewRecorder()
				tc.handler.ServeHTTP(rec, req)
				if rec.Code != sig.code {
					t.Errorf("%s sig: code=%d want %d", sig.name, rec.Code, sig.code)
				}
			}
		})
	}
}

// A primed queue entry ensures a missed filter would reach MirrorCheck rather
// than fall out at SHA lookup; previously the Gitea handler only dropped the
// bare "gitea-mq" context, so gitea-mq/* mirrors fed back into the monitor.
func TestHandler_IgnoresOwnContexts(t *testing.T) {
	env := setup(t)

	if _, err := env.svc.Enqueue(env.ctx, env.repoID, 5, "pr-head", "main"); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.SetMergeBranch(env.ctx, env.repoID, 5, "gitea-mq/5", "merge-sha"); err != nil {
		t.Fatal(err)
	}

	for _, checkCtx := range []string{"gitea-mq", "gitea-mq/ci/build"} {
		body := makePayload("merge-sha", checkCtx, "success", "org/app")
		rec := doRequest(env.handler, body, sign(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", checkCtx, rec.Code)
		}
	}
	if n := len(env.mock.CallsTo("CreateCommitStatus")); n != 0 {
		t.Fatalf("own-context status was re-processed: %d CreateCommitStatus calls", n)
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

	testutil.EnqueueTesting(t, env.svc, env.repoID, prNumber, prHeadSHA, mergeSHA)

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
