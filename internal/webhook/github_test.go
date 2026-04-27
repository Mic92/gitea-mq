package webhook_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

func ghSign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func ghPost(t *testing.T, h http.Handler, event, body string, sign bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	if sign {
		req.Header.Set("X-Hub-Signature-256", ghSign([]byte(body)))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func ghHandler(repos webhook.MapRepoLookup, disc func()) http.Handler {
	return webhook.GithubHandler([]byte(testSecret), repos, nil, disc)
}

func TestGithubHandler_Signature(t *testing.T) {
	h := ghHandler(webhook.MapRepoLookup{}, nil)

	if w := ghPost(t, h, "ping", `{}`, false); w.Code != http.StatusUnauthorized {
		t.Errorf("missing sig: code=%d", w.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad sig: code=%d", w.Code)
	}
	if w := ghPost(t, h, "ping", `{}`, true); w.Code != http.StatusOK {
		t.Errorf("valid sig: code=%d", w.Code)
	}
}

func TestGithubHandler_PRTriggersPoll(t *testing.T) {
	var polled int
	rm := &webhook.RepoMonitor{TriggerPoll: func() { polled++ }}
	h := ghHandler(webhook.MapRepoLookup{"github:org/app": rm}, nil)

	for _, tc := range []struct {
		action string
		want   int
	}{
		{"auto_merge_enabled", 1},
		{"auto_merge_disabled", 1},
		{"closed", 1},
		{"synchronize", 1},
		{"labeled", 0}, // irrelevant action must not poke the poller
	} {
		polled = 0
		body := `{"action":"` + tc.action + `","pull_request":{"number":1},"repository":{"name":"app","owner":{"login":"org"}}}`
		if w := ghPost(t, h, "pull_request", body, true); w.Code != http.StatusOK {
			t.Errorf("%s: code=%d", tc.action, w.Code)
		}
		if polled != tc.want {
			t.Errorf("%s: polled=%d want %d", tc.action, polled, tc.want)
		}
	}
}

// check_run events for our own context (gitea-mq, gitea-mq/*) must be dropped
// before any monitor work to avoid feedback loops. The handler is constructed
// with a nil queue: if routing were attempted the test would panic.
func TestGithubHandler_CheckRunIgnoresOwn(t *testing.T) {
	rm := &webhook.RepoMonitor{Deps: &monitor.Deps{Owner: "org", Repo: "app"}}
	h := ghHandler(webhook.MapRepoLookup{"github:org/app": rm}, nil)

	for _, name := range []string{"gitea-mq", "gitea-mq/ci"} {
		body := `{"action":"completed","check_run":{"name":"` + name + `","status":"completed","conclusion":"success","head_sha":"abc"},"repository":{"name":"app","owner":{"login":"org"}}}`
		if w := ghPost(t, h, "check_run", body, true); w.Code != http.StatusOK {
			t.Errorf("%s: code=%d", name, w.Code)
		}
	}
}

func TestGithubHandler_InstallationTriggersDiscovery(t *testing.T) {
	var fired int
	h := ghHandler(webhook.MapRepoLookup{}, func() { fired++ })

	ghPost(t, h, "installation", `{"action":"created"}`, true)
	ghPost(t, h, "installation_repositories", `{"action":"added"}`, true)
	if fired != 2 {
		t.Errorf("discovery fired=%d want 2", fired)
	}
}
