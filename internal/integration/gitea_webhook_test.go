package integration_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// The auto-created hook must subscribe to PR events so label changes, pushes
// and reviews reconcile immediately, and Gitea must accept the event names.
func TestGiteaEnsureWebhookSubscribesPREvents(t *testing.T) {
	giteaServer := testutil.GiteaInstance()
	if giteaServer == nil {
		t.Skip("gitea server not available")
	}
	ctx := t.Context()

	api := testutil.NewGiteaAPI(giteaServer.URL)
	api.CreateToken(t)
	const repoName = "webhook-events"
	api.MustDo(t, "POST", "/user/repos", `{"name": "`+repoName+`", "auto_init": true, "default_branch": "main"}`)

	client := gitea.NewHTTPClient(giteaServer.URL, api.Token)
	const url = "https://mq.example.com/webhook/gitea"
	if err := gitea.EnsureWebhook(ctx, client, "testuser", repoName, url, "s3cret"); err != nil {
		t.Fatalf("EnsureWebhook: %v", err)
	}

	var hooks []struct {
		Events []string          `json:"events"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(api.MustDo(t, "GET", "/repos/testuser/"+repoName+"/hooks", ""), &hooks); err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Fatalf("got %d hooks, want 1", len(hooks))
	}
	for _, want := range []string{"status", "pull_request", "pull_request_label", "pull_request_sync"} {
		if !slices.Contains(hooks[0].Events, want) {
			t.Errorf("hook events %v missing %q", hooks[0].Events, want)
		}
	}
}
