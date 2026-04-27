package discovery_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/discovery"
	"github.com/Mic92/gitea-mq/internal/forge"
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
	mock := &gitea.MockClient{}
	forges := forge.NewSet()
	forges.Register(gitea.NewForge(mock, "https://gitea.example.com"))
	forges.Register(&forge.MockForge{KindVal: forge.KindGithub})
	reg := registry.New(ctx, &registry.Deps{
		Forges:         forges,
		Queue:          queue.NewService(pool),
		PollInterval:   time.Hour,
		CheckTimeout:   time.Hour,
		SuccessTimeout: 5 * time.Minute,
	})
	return reg, mock, ctx
}

func giteaSrc(mock *gitea.MockClient) discovery.Source {
	return discovery.Source{Kind: forge.KindGitea, List: gitea.TopicSource(mock, "merge-queue")}
}

func TestDiscoverOnce_TopicMatching_AdminFilter(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)
	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
			{FullName: "org/ro", Owner: gitea.RepoOwner{Login: "org"}, Name: "ro", Permissions: gitea.RepoPermissions{Pull: true}},
		}, nil
	}

	discovery.DiscoverOnce(ctx, &discovery.Deps{Registry: reg, Sources: []discovery.Source{giteaSrc(mock)}})

	if !reg.Contains("gitea:org/app") {
		t.Error("admin repo not discovered")
	}
	if reg.Contains("gitea:org/ro") {
		t.Error("read-only repo should be skipped")
	}
}

func TestDiscoverOnce_RemovesAndKeepsExplicit(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)
	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}
	deps := &discovery.Deps{
		Registry:      reg,
		Sources:       []discovery.Source{giteaSrc(mock)},
		ExplicitRepos: []forge.RepoRef{{Forge: forge.KindGitea, Owner: "org", Name: "legacy"}},
	}

	discovery.DiscoverOnce(ctx, deps)
	if !reg.Contains("gitea:org/app") || !reg.Contains("gitea:org/legacy") {
		t.Fatalf("first cycle: app=%v legacy=%v", reg.Contains("gitea:org/app"), reg.Contains("gitea:org/legacy"))
	}

	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) { return nil, nil }
	discovery.DiscoverOnce(ctx, deps)

	if reg.Contains("gitea:org/app") {
		t.Error("topic-lost repo should be removed")
	}
	if !reg.Contains("gitea:org/legacy") {
		t.Error("explicit repo must never be removed")
	}
}

// One forge being down must not evict the other forge's repos.
func TestDiscoverOnce_SourceErrorIsolated(t *testing.T) {
	reg, mock, ctx := newTestSetup(t)
	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return []gitea.Repo{
			{FullName: "org/app", Owner: gitea.RepoOwner{Login: "org"}, Name: "app", Permissions: gitea.RepoPermissions{Admin: true}},
		}, nil
	}
	ghRefs := []forge.RepoRef{{Forge: forge.KindGithub, Owner: "gh", Name: "proj"}}
	ghSrc := discovery.Source{Kind: forge.KindGithub, List: func(context.Context) ([]forge.RepoRef, error) { return ghRefs, nil }}
	deps := &discovery.Deps{Registry: reg, Sources: []discovery.Source{giteaSrc(mock), ghSrc}}

	discovery.DiscoverOnce(ctx, deps)
	if !reg.Contains("gitea:org/app") || !reg.Contains("github:gh/proj") {
		t.Fatal("seed failed")
	}

	// Gitea goes down; GitHub source now returns empty.
	mock.SearchReposByTopicFn = func(_ context.Context, _ string) ([]gitea.Repo, error) {
		return nil, fmt.Errorf("connection refused")
	}
	ghRefs = nil
	discovery.DiscoverOnce(ctx, deps)

	if !reg.Contains("gitea:org/app") {
		t.Error("gitea repo evicted while gitea source is down")
	}
	if reg.Contains("github:gh/proj") {
		t.Error("github repo not removed despite healthy empty source")
	}
}
