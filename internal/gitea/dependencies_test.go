package gitea_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
)

func TestForge_OpenDependencies_KeepsOnlyOpenOnes(t *testing.T) {
	mock := &gitea.MockClient{
		ListIssueDependenciesFn: func(_ context.Context, _, _ string, _ int64) ([]gitea.Issue, error) {
			return []gitea.Issue{
				{Index: 7, State: "open", Repository: &gitea.IssueRepo{FullName: "org/app"}},
				{Index: 8, State: "closed", Repository: &gitea.IssueRepo{FullName: "org/app"}},
				{Index: 3, State: "open", Repository: &gitea.IssueRepo{FullName: "org/lib"}},
			}, nil
		},
	}
	deps, err := forge.OpenDependencies(context.Background(), newForge(mock), "org", "app", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got := forge.DependencyRefs(deps, "org", "app"); got != "#7, org/lib#3" {
		t.Fatalf("open dependencies: got %q", got)
	}
}

// A repository with dependencies disabled answers 404: that means none, not an
// error that would stall the queue.
func TestForge_OpenDependencies_DisabledMeansNone(t *testing.T) {
	mock := &gitea.MockClient{
		ListIssueDependenciesFn: func(_ context.Context, _, _ string, _ int64) ([]gitea.Issue, error) {
			return nil, &gitea.APIError{StatusCode: 404, Body: "disabled repo dependencies"}
		},
	}
	deps, err := forge.OpenDependencies(context.Background(), newForge(mock), "org", "app", 42)
	if err != nil || len(deps) != 0 {
		t.Fatalf("want no dependencies and no error, got %v, %v", deps, err)
	}
}

// Any other failure must surface: treating it as "no dependencies" would let a
// blocked PR land.
func TestForge_OpenDependencies_OtherErrorsSurface(t *testing.T) {
	mock := &gitea.MockClient{
		ListIssueDependenciesFn: func(_ context.Context, _, _ string, _ int64) ([]gitea.Issue, error) {
			return nil, &gitea.APIError{StatusCode: 500, Body: "boom"}
		},
	}
	_, err := forge.OpenDependencies(context.Background(), newForge(mock), "org", "app", 42)
	var apiErr *gitea.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 {
		t.Fatalf("want the 500 back, got %v", err)
	}
}

// A forge without native dependencies (GitHub) reports none.
func TestOpenDependencies_UnsupportedForge(t *testing.T) {
	deps, err := forge.OpenDependencies(context.Background(), struct{ forge.Forge }{}, "org", "app", 42)
	if err != nil || deps != nil {
		t.Fatalf("want nil, nil; got %v, %v", deps, err)
	}
}
