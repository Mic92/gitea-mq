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

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/monitor"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/testutil"
	"github.com/jogman/gitea-mq/internal/webhook"
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
		Gitea:        mock,
		Queue:        svc,
		Owner:        "org",
		Repo:         "app",
		RepoID:       repoID,
		CheckTimeout: 1 * time.Hour,
	}

	repos := webhook.MapRepoLookup{
		"org/app": {Deps: deps, RepoID: repoID},
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
