package github_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
)

func TestForge_ResolveStack(t *testing.T) {
	srv, f := newTestForge(t)
	srv.AddPR("org", "app", ghfake.PR{Number: 1, HeadRef: "b1", HeadSHA: "sha1", BaseRef: "main"})
	srv.AddPR("org", "app", ghfake.PR{Number: 2, HeadRef: "b2", HeadSHA: "sha2", BaseRef: "b1"})
	ctx := context.Background()

	sr := f.(forge.StackResolver)

	stack, err := sr.ResolveStack(ctx, "org", "app", 2)
	if err != nil {
		t.Fatalf("disabled stacks: %v", err)
	}
	if stack != nil {
		t.Fatalf("expected nil stack when feature disabled, got %+v", stack)
	}

	srv.Repo("org", "app").Stacks = []ghfake.Stack{{BaseRef: "main", PRs: []int64{1, 2}}}

	stack, err = sr.ResolveStack(ctx, "org", "app", 2)
	if err != nil {
		t.Fatalf("ResolveStack: %v", err)
	}
	if stack == nil || stack.BaseBranch != "main" || len(stack.PRs) != 2 {
		t.Fatalf("stack = %+v", stack)
	}
	if stack.PRs[0].HeadSHA != "sha1" || stack.PRs[1].Number != 2 {
		t.Errorf("members = %+v", stack.PRs)
	}

	members, ok := stack.MembersUpTo(1)
	if !ok || len(members) != 1 || members[0].Number != 1 {
		t.Errorf("MembersUpTo(1) = %+v, %v", members, ok)
	}
}

func TestForge_MergePR_Async(t *testing.T) {
	srv, f := newTestForge(t)
	srv.AddPR("org", "app", ghfake.PR{Number: 5, HeadSHA: "sha5", BaseRef: "main"})
	ctx := context.Background()
	rp := srv.Repo("org", "app")

	if err := f.MergePR(ctx, "org", "app", 5); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if len(rp.AsyncMerges) != 1 || rp.AsyncMerges[0] != 5 {
		t.Fatalf("AsyncMerges = %v", rp.AsyncMerges)
	}

	rp.AsyncMergeConflict = true
	if err := f.MergePR(ctx, "org", "app", 5); err != nil {
		t.Fatalf("409 must be treated as in-flight: %v", err)
	}

	rp.AsyncMergeConflict = false
	rp.AsyncMergeStatus = "failed"
	err := f.MergePR(ctx, "org", "app", 5)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected failure with server message, got %v", err)
	}
}

func TestForge_RemoveLabel(t *testing.T) {
	srv, f := newTestForge(t)
	srv.AddPR("org", "app", ghfake.PR{Number: 3, HeadSHA: "sha3", BaseRef: "main", Labels: []string{"merge-queue"}})
	ctx := context.Background()

	if err := f.RemoveLabel(ctx, "org", "app", 3, "merge-queue"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if got := srv.Repo("org", "app").PRs[3].Labels; len(got) != 0 {
		t.Fatalf("labels = %v", got)
	}
	if err := f.RemoveLabel(ctx, "org", "app", 3, "merge-queue"); err != nil {
		t.Fatalf("second removal must be a no-op: %v", err)
	}
}
