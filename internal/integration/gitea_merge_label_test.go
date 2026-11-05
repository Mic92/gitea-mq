package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// Gitea lets anyone set labels when opening a PR.
func TestGiteaMergeLabelFromForkIsUnauthorized(t *testing.T) {
	giteaServer := testutil.GiteaInstance()
	if giteaServer == nil {
		t.Skip("gitea server not available")
	}
	ctx := t.Context()

	admin := testutil.NewGiteaAPI(giteaServer.URL)
	admin.CreateToken(t)
	const repoName = "merge-label-auth"
	admin.MustDo(t, "POST", "/user/repos", `{"name": "`+repoName+`", "auto_init": false, "default_branch": "main"}`)
	if err := giteaServer.PatchRepoHooks("testuser", repoName); err != nil {
		t.Fatal(err)
	}
	admin.MustDo(t, "POST", "/repos/testuser/"+repoName+"/contents/README.md", `{"content": "cmVhZG1l", "message": "init"}`)
	var label struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(admin.MustDo(t, "POST", "/repos/testuser/"+repoName+"/labels",
		`{"name": "merge-queue", "color": "#00ff00"}`), &label); err != nil {
		t.Fatal(err)
	}

	admin.MustDo(t, "POST", "/admin/users",
		`{"username": "mallory", "email": "mallory@example.com", "password": "mallorypass123", "must_change_password": false}`)
	mallory := testutil.NewGiteaAPI(giteaServer.URL)
	mallory.Token = userToken(t, mallory, "mallory", "mallorypass123")
	mallory.MustDo(t, "POST", "/repos/testuser/"+repoName+"/forks", `{}`)
	if err := giteaServer.PatchRepoHooks("mallory", repoName); err != nil {
		t.Fatal(err)
	}
	mallory.MustDo(t, "POST", "/repos/mallory/"+repoName+"/contents/evil.txt",
		`{"content": "ZXZpbA==", "message": "evil", "new_branch": "evil"}`)

	var pr struct {
		Number int64 `json:"number"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(mallory.MustDo(t, "POST", "/repos/testuser/"+repoName+"/pulls",
		fmt.Sprintf(`{"title": "evil", "head": "mallory:evil", "base": "main", "labels": [%d]}`, label.ID)), &pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Labels) == 0 {
		t.Skip("this Gitea version rejects labels from non-collaborators")
	}

	f := gitea.NewForge(gitea.NewHTTPClient(giteaServer.URL, admin.Token), giteaServer.URL)
	login, ok, err := f.MergeLabelAdder(ctx, "testuser", repoName, pr.Number, "merge-queue")
	if err != nil {
		t.Fatal(err)
	}
	if login != "mallory" || ok {
		t.Fatalf("got (%q, %v), want (mallory, false)", login, ok)
	}

	admin.MustDo(t, "DELETE", fmt.Sprintf("/repos/testuser/%s/issues/%d/labels/%d", repoName, pr.Number, label.ID), "")
	admin.MustDo(t, "POST", fmt.Sprintf("/repos/testuser/%s/issues/%d/labels", repoName, pr.Number),
		fmt.Sprintf(`{"labels": [%d]}`, label.ID))
	login, ok, err = f.MergeLabelAdder(ctx, "testuser", repoName, pr.Number, "merge-queue")
	if err != nil {
		t.Fatal(err)
	}
	if login != "testuser" || !ok {
		t.Fatalf("got (%q, %v), want (testuser, true)", login, ok)
	}
}

func userToken(t *testing.T, api *testutil.GiteaAPI, user, pass string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, api.BaseURL+"/api/v1/users/"+user+"/tokens",
		strings.NewReader(`{"name": "t", "scopes": ["all"]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)
	resp, err := api.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var tok struct {
		SHA1 string `json:"sha1"`
	}
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&tok) != nil {
		t.Fatalf("create token for %s: status %d", user, resp.StatusCode)
	}
	return tok.SHA1
}
