package merge_test

import (
	"context"
	"os"
	"testing"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/merge"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithPostgres(m))
}

func setup(t *testing.T) (*gitea.MockClient, *queue.Service, context.Context, int64) {
	t.Helper()

	pool := testutil.TestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	repo, err := svc.GetOrCreateRepo(ctx, "org", "app")
	if err != nil {
		t.Fatal(err)
	}

	return &gitea.MockClient{}, svc, ctx, repo.ID
}

// Successful merge → branch created, state transitions to testing, pending
// status updated to "Testing merge result".
func TestStartTesting_Success(t *testing.T) {
	mock, svc, ctx, repoID := setup(t)

	mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
		return &gitea.MergeResult{SHA: "mergesha123"}, nil
	}

	if _, err := svc.Enqueue(ctx, repoID, 42, "prsha", "main"); err != nil {
		t.Fatal(err)
	}
	entry, _ := svc.GetEntry(ctx, repoID, 42)

	result, err := merge.StartTesting(ctx, mock, svc, "org", "app", repoID, entry)
	if err != nil {
		t.Fatal(err)
	}

	if result.Conflict {
		t.Fatal("expected no conflict")
	}
	if result.MergeBranchName != "mq/42" {
		t.Fatalf("expected mq/42, got %s", result.MergeBranchName)
	}
	if result.MergeBranchSHA != "mergesha123" {
		t.Fatalf("expected mergesha123, got %s", result.MergeBranchSHA)
	}

	// Verify state is now testing with merge branch recorded.
	updated, _ := svc.GetEntry(ctx, repoID, 42)
	if updated.State != pg.EntryStateTesting {
		t.Fatalf("expected testing state, got %s", updated.State)
	}
	if updated.MergeBranchName.String != "mq/42" {
		t.Fatalf("expected merge branch mq/42 recorded, got %s", updated.MergeBranchName.String)
	}
}

// Merge conflict → PR removed from queue, automerge cancelled, failure status
// set, comment posted.
func TestStartTesting_Conflict(t *testing.T) {
	mock, svc, ctx, repoID := setup(t)

	mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
		return nil, &gitea.MergeConflictError{Base: "main", Head: "prsha", Message: "conflict"}
	}

	if _, err := svc.Enqueue(ctx, repoID, 42, "prsha", "main"); err != nil {
		t.Fatal(err)
	}
	entry, _ := svc.GetEntry(ctx, repoID, 42)

	result, err := merge.StartTesting(ctx, mock, svc, "org", "app", repoID, entry)
	if err != nil {
		t.Fatal(err)
	}

	if !result.Conflict {
		t.Fatal("expected conflict")
	}

	// PR should be removed from queue.
	remaining, _ := svc.GetEntry(ctx, repoID, 42)
	if remaining != nil {
		t.Fatal("conflicting PR should be dequeued")
	}

	// Automerge cancelled, failure status set, comment posted.
	if len(mock.CallsTo("CancelAutoMerge")) != 1 {
		t.Fatal("expected CancelAutoMerge")
	}
	if len(mock.CallsTo("CreateCommitStatus")) != 1 {
		t.Fatal("expected failure status")
	}
	if mock.CallsTo("CreateCommitStatus")[0].Args[3].(gitea.CommitStatus).State != "failure" {
		t.Fatal("expected failure state")
	}
	if len(mock.CallsTo("CreateComment")) != 1 {
		t.Fatal("expected conflict comment")
	}
}
