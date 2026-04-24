package discovery_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/config"
	"github.com/Mic92/gitea-mq/internal/discovery"
	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/registry"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func newTestSetup(t *testing.T) (*registry.RepoRegistry, *gitea.MockClient, context.Context) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pool := testutil.TestDB(t)
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

	// SearchReposByTopic returns only repos that already have the topic,
	// so only org/app should be returned by the search.
	mock.SearchReposByTopicFn = func(_ context.Context, topic string) ([]gitea.Repo, error) {
		if topic != "merge-queue" {
			return nil, nil
		}
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
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
}

func TestDiscoverOnce_AdminFilter(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)

	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/admin-repo", Owner: gitea.RepoOwner{Login: "org"}, Name: "admin-repo", Permissions: gitea.RepoPermissions{Admin: true}},
			{FullName: "org/read-repo", Owner: gitea.RepoOwner{Login: "org"}, Name: "read-repo", Permissions: gitea.RepoPermissions{Admin: false, Pull: true}},
		}, nil
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

	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
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

	// Second cycle: org/app lost the topic (no longer returned by search).
	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return nil, nil
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

	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
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
	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return nil, nil
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

	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}

	deps := &discovery.Deps{Gitea: mock, Registry: reg, Topic: "merge-queue"}
	_ = discovery.DiscoverOnce(ctx, deps)
	if !reg.Contains("org/app") {
		t.Fatal("setup failed")
	}

	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
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
