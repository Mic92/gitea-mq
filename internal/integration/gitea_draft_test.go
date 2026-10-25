package integration_test

import (
	"encoding/json"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// Gitea reports WIP-prefixed PRs as drafts; the forge adapter must surface it.
func TestGiteaDraftPR(t *testing.T) {
	giteaServer := testutil.GiteaInstance()
	if giteaServer == nil {
		t.Skip("gitea server not available")
	}
	ctx := t.Context()

	api := testutil.NewGiteaAPI(giteaServer.URL)
	api.CreateToken(t)
	const repoName = "draft-pr"
	api.MustDo(t, "POST", "/user/repos", `{"name": "`+repoName+`", "auto_init": true, "default_branch": "main"}`)
	api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/contents/a.txt",
		`{"content": "YQ==", "message": "a", "new_branch": "feature"}`)

	var pr struct {
		Number int64 `json:"number"`
	}
	body := api.MustDo(t, "POST", "/repos/testuser/"+repoName+"/pulls",
		`{"title": "WIP: not ready", "head": "feature", "base": "main"}`)
	if err := json.Unmarshal(body, &pr); err != nil {
		t.Fatal(err)
	}

	f := gitea.NewForge(gitea.NewHTTPClient(giteaServer.URL, api.Token), giteaServer.URL)
	got, err := f.GetPR(ctx, "testuser", repoName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Draft {
		t.Fatalf("WIP PR not reported as draft: %+v", got)
	}

	api.MustDo(t, "PATCH", "/repos/testuser/"+repoName+"/pulls/"+itoa(pr.Number), `{"title": "ready now"}`)
	got, err = f.GetPR(ctx, "testuser", repoName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.Draft {
		t.Fatalf("ready PR still reported as draft")
	}
}
