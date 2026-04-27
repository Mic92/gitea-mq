package github_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	githubpkg "github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func newTestApp(t *testing.T, srv *ghfake.Server) *githubpkg.App {
	t.Helper()
	app, err := githubpkg.NewApp(1, testutil.GithubAppKey(), srv.URL+"/api/v3")
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if err := app.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return app
}

func TestApp_RefreshRoutesByInstallation(t *testing.T) {
	srv := ghfake.New()
	defer srv.Close()

	srv.AddRepo("orgA", "app")
	srv.AddRepo("orgB", "lib")
	srv.AddInstallation(100, "orgA/app")
	srv.AddInstallation(200, "orgB/lib")

	app := newTestApp(t, srv)

	repos := app.Repos()
	slices.SortFunc(repos, func(a, b forge.RepoRef) int { return strings.Compare(a.String(), b.String()) })
	want := []forge.RepoRef{
		{Forge: forge.KindGithub, Owner: "orgA", Name: "app"},
		{Forge: forge.KindGithub, Owner: "orgB", Name: "lib"},
	}
	if !slices.Equal(repos, want) {
		t.Fatalf("Repos() = %v", repos)
	}

	// Distinct installations must yield distinct clients (different tokens).
	cA, err := app.ClientForRepo("orgA", "app")
	if err != nil {
		t.Fatalf("ClientForRepo orgA: %v", err)
	}
	cB, err := app.ClientForRepo("orgB", "lib")
	if err != nil {
		t.Fatalf("ClientForRepo orgB: %v", err)
	}
	if cA == cB {
		t.Error("expected distinct clients for distinct installations")
	}

	if _, err := app.ClientForRepo("orgC", "nope"); err == nil {
		t.Error("expected error for repo without installation")
	}
}
