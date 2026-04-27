package github_test

import (
	"context"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	githubpkg "github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
)

func newTestForge(t *testing.T) (*ghfake.Server, forge.Forge) {
	t.Helper()
	srv := ghfake.New()
	t.Cleanup(srv.Close)
	srv.AddRepo("org", "app")
	srv.AddInstallation(100, "org/app")
	// htmlURL irrelevant for API tests; use github.com so URL helpers are
	// asserted against the production shape.
	return srv, githubpkg.NewForge(newTestApp(t, srv), "")
}

func TestForge_URLHelpers(t *testing.T) {
	_, f := newTestForge(t)
	if got := f.PRHTMLURL("o", "r", 7); got != "https://github.com/o/r/pull/7" {
		t.Errorf("PRHTMLURL = %q", got)
	}
	if got := f.BranchHTMLURL("o", "r", "gitea-mq/7"); got != "https://github.com/o/r/tree/gitea-mq/7" {
		t.Errorf("BranchHTMLURL = %q", got)
	}
}

func TestForge_ListAndGetPR(t *testing.T) {
	srv, f := newTestForge(t)
	srv.AddPR("org", "app", ghfake.PR{
		Number: 1, Title: "feat", User: "alice",
		HeadRef: "feature", HeadSHA: "sha1", BaseRef: "main",
		AutoMerge: true,
	})
	srv.AddPR("org", "app", ghfake.PR{
		Number: 2, Title: "wip", HeadSHA: "sha2", BaseRef: "main",
	})

	ctx := context.Background()

	prs, err := f.ListOpenPRs(ctx, "org", "app")
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("len = %d, want 2", len(prs))
	}

	auto, err := f.ListAutoMergePRs(ctx, "org", "app")
	if err != nil {
		t.Fatalf("ListAutoMergePRs: %v", err)
	}
	if len(auto) != 1 || auto[0].Number != 1 {
		t.Fatalf("auto = %+v, want PR #1 only", auto)
	}

	pr, err := f.GetPR(ctx, "org", "app", 1)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if !pr.AutoMergeEnabled || pr.NodeID == "" || pr.HeadSHA != "sha1" || pr.AuthorLogin != "alice" {
		t.Errorf("GetPR mapping = %+v", pr)
	}
}
