package discovery_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jogman/gitea-mq/internal/config"
	"github.com/jogman/gitea-mq/internal/discovery"
	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/registry"
	"github.com/jogman/gitea-mq/internal/testutil"
)

func newTestSetup(t *testing.T) (*registry.RepoRegistry, *gitea.MockClient, context.Context) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pool := testutil.NewTestDB(t, testutil.Server())
	queueSvc := queue.NewService(pool)
	mock := &gitea.MockClient{}

	regDeps := &registry.Deps{
		Gitea:          mock,
		Queue:          queueSvc,
		PollInterval:   1 * time.Hour,
		CheckTimeout:   1 * time.Hour,
		SuccessTimeout: 5 * time.Minute,
	}

	reg := registry.New(ctx, regDeps)
	return reg, mock, ctx
}

func TestDiscoverOnce_TopicMatching(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)

	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
			{FullName: "org/lib", Owner: gitea.RepoOwner{Login: "org"}, Name: "lib", Permissions: gitea.RepoPermissions{Admin: true}},
			{FullName: "org/docs", Owner: gitea.RepoOwner{Login: "org"}, Name: "docs", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}
	mock.GetRepoTopicsFn = func(_ context.Context, owner, repo string) ([]string, error) {
		switch owner + "/" + repo {
		case "org/app":
			return []string{"merge-queue", "go"}, nil
		case "org/lib":
			return []string{"nix", "library"}, nil
		case "org/docs":
			return []string{}, nil
		}
		return nil, nil
	}

	deps := &discovery.Deps{
		Gitea:    mock,
		Registry: reg,
		Topic:    "merge-queue",
	}

	if err := discovery.DiscoverOnce(ctx, deps); err != nil {
		t.Fatalf("DiscoverOnce: %v", err)
	}

	if !reg.Contains("org/app") {
		t.Error("expected org/app to be discovered (has merge-queue topic)")
	}
	if reg.Contains("org/lib") {
		t.Error("expected org/lib to NOT be discovered (no merge-queue topic)")
	}
	if reg.Contains("org/docs") {
		t.Error("expected org/docs to NOT be discovered (no topics)")
	}
}

func TestDiscoverOnce_AdminFilter(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)

	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/admin-repo", Owner: gitea.RepoOwner{Login: "org"}, Name: "admin-repo", Permissions: gitea.RepoPermissions{Admin: true}},
			{FullName: "org/read-repo", Owner: gitea.RepoOwner{Login: "org"}, Name: "read-repo", Permissions: gitea.RepoPermissions{Admin: false, Pull: true}},
		}, nil
	}
	mock.GetRepoTopicsFn = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"merge-queue"}, nil
	}

	deps := &discovery.Deps{
		Gitea:    mock,
		Registry: reg,
		Topic:    "merge-queue",
	}

	if err := discovery.DiscoverOnce(ctx, deps); err != nil {
		t.Fatalf("DiscoverOnce: %v", err)
	}

	if !reg.Contains("org/admin-repo") {
		t.Error("expected admin-repo to be discovered")
	}
	if reg.Contains("org/read-repo") {
		t.Error("expected read-repo to be skipped (no admin)")
	}
}

func TestDiscoverOnce_RemovesRepoThatLostTopic(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)

	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}
	mock.GetRepoTopicsFn = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"merge-queue"}, nil
	}

	deps := &discovery.Deps{
		Gitea:    mock,
		Registry: reg,
		Topic:    "merge-queue",
	}

	if err := discovery.DiscoverOnce(ctx, deps); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if !reg.Contains("org/app") {
		t.Fatal("expected org/app after first cycle")
	}

	// Second cycle: org/app lost the topic.
	mock.GetRepoTopicsFn = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"go"}, nil
	}

	if err := discovery.DiscoverOnce(ctx, deps); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if reg.Contains("org/app") {
		t.Error("expected org/app to be removed after losing topic")
	}
}

func TestDiscoverOnce_ExplicitRepoNeverRemoved(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)

	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}
	mock.GetRepoTopicsFn = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"merge-queue"}, nil
	}

	deps := &discovery.Deps{
		Gitea:         mock,
		Registry:      reg,
		Topic:         "merge-queue",
		ExplicitRepos: []config.RepoRef{{Owner: "org", Name: "legacy"}},
	}

	if err := discovery.DiscoverOnce(ctx, deps); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	// Both topic-discovered and explicit should be present.
	if !reg.Contains("org/app") {
		t.Error("expected org/app (topic-discovered)")
	}
	if !reg.Contains("org/legacy") {
		t.Error("expected org/legacy (explicit)")
	}

	// Second cycle: no repos discovered at all, but explicit stays.
	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return []gitea.Repo{}, nil
	}

	if err := discovery.DiscoverOnce(ctx, deps); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if reg.Contains("org/app") {
		t.Error("expected org/app to be removed (lost topic)")
	}
	if !reg.Contains("org/legacy") {
		t.Error("explicit repo should never be removed by discovery")
	}
}

func TestDiscoverOnce_APIFailureKeepsCurrentSet(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)

	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}
	mock.GetRepoTopicsFn = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"merge-queue"}, nil
	}

	deps := &discovery.Deps{Gitea: mock, Registry: reg, Topic: "merge-queue"}
	_ = discovery.DiscoverOnce(ctx, deps)
	if !reg.Contains("org/app") {
		t.Fatal("setup failed")
	}

	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return nil, fmt.Errorf("connection refused")
	}

	err := discovery.DiscoverOnce(ctx, deps)
	if err == nil {
		t.Fatal("expected error on API failure")
	}
	if !reg.Contains("org/app") {
		t.Error("expected org/app to remain managed after API failure")
	}
}

func TestDiscoverOnce_PartialTopicFetchKeepsManagedRepo(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)

	mock.ListUserReposFn = func(_ context.Context) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
			{FullName: "org/lib", Owner: gitea.RepoOwner{Login: "org"}, Name: "lib", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}
	mock.GetRepoTopicsFn = func(_ context.Context, _, _ string) ([]string, error) {
		return []string{"merge-queue"}, nil
	}

	deps := &discovery.Deps{Gitea: mock, Registry: reg, Topic: "merge-queue"}
	_ = discovery.DiscoverOnce(ctx, deps)
	if !reg.Contains("org/app") || !reg.Contains("org/lib") {
		t.Fatal("setup failed")
	}

	// Second cycle: topic fetch fails for org/app only.
	mock.GetRepoTopicsFn = func(_ context.Context, _, repo string) ([]string, error) {
		if repo == "app" {
			return nil, fmt.Errorf("timeout")
		}
		return []string{"merge-queue"}, nil
	}

	_ = discovery.DiscoverOnce(ctx, deps)

	// Spec: "if org/app was previously managed, it remains managed (no removal on partial failure)"
	if !reg.Contains("org/app") {
		t.Error("org/app should remain managed when its topic fetch failed (conservative reconciliation)")
	}
	if !reg.Contains("org/lib") {
		t.Error("org/lib should remain managed (topic fetch succeeded)")
	}
}
