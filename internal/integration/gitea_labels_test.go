package integration_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func TestApplyLabels_RealGitea(t *testing.T) {
	gs := testutil.GiteaInstance()
	if gs == nil {
		t.Skip("gitea server not available")
	}
	api := testutil.NewGiteaAPI(gs.URL)
	api.CreateToken(t)
	ctx := t.Context()
	repo := "label-test"
	api.MustDo(t, "POST", "/user/repos", `{"name": "`+repo+`", "auto_init": true, "default_branch": "main"}`)
	api.MustDo(t, "POST", "/repos/testuser/"+repo+"/issues", `{"title": "x"}`)

	l, ok := gitea.NewForge(gitea.NewHTTPClient(gs.URL, api.Token), gs.URL).(forge.Labeler)
	if !ok {
		t.Fatal("forge is not a Labeler")
	}
	labels := func() []string {
		var ls []struct{ Name string }
		if err := json.Unmarshal(api.MustDo(t, "GET", "/repos/testuser/"+repo+"/issues/1/labels", ""), &ls); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range ls {
			out = append(out, x.Name)
		}
		slices.Sort(out)
		return out
	}
	apply := func(add, remove []string) {
		t.Helper()
		if err := l.ApplyLabels(ctx, "testuser", repo, 1, add, remove); err != nil {
			t.Fatal(err)
		}
	}

	apply([]string{"mq/queued"}, nil)
	if got := labels(); !slices.Equal(got, []string{"mq/queued"}) {
		t.Fatalf("after queued: %v", got)
	}

	apply([]string{"mq/batch-3"}, []string{"mq/queued"})
	if got := labels(); !slices.Equal(got, []string{"mq/batch-3"}) {
		t.Fatalf("after exclusive swap: %v", got)
	}

	if err := l.DeleteLabel(ctx, "testuser", repo, "mq/batch-3"); err != nil {
		t.Fatal(err)
	}
	if got := labels(); len(got) != 0 {
		t.Fatalf("after delete: %v", got)
	}
	if err := l.DeleteLabel(ctx, "testuser", repo, "mq/batch-3"); err != nil {
		t.Fatalf("second delete: %v", err)
	}

	apply([]string{"mq/batch-3"}, nil)
	if got := labels(); !slices.Equal(got, []string{"mq/batch-3"}) {
		t.Fatalf("recreate after delete: %v", got)
	}
}
