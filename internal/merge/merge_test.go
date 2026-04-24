package merge_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithPostgres(m))
}

func setup(t *testing.T) (*gitea.MockClient, *queue.Service, context.Context, int64) {
	t.Helper()

	svc, ctx, repoID := testutil.TestQueueService(t)

	return &gitea.MockClient{}, svc, ctx, repoID
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

	result, err := merge.StartTesting(ctx, mock, svc, "org", "app", repoID, entry, "https://mq.example.com")
	if err != nil {
		t.Fatal(err)
	}

	wantBranch := merge.BranchName(42)

	if result.Removed {
		t.Fatal("expected PR to enter testing, not be removed")
	}
	if result.MergeBranchName != wantBranch {
		t.Fatalf("expected %s, got %s", wantBranch, result.MergeBranchName)
	}
	if result.MergeBranchSHA != "mergesha123" {
		t.Fatalf("expected mergesha123, got %s", result.MergeBranchSHA)
	}

	// Verify state is now testing with merge branch recorded.
	updated, _ := svc.GetEntry(ctx, repoID, 42)
	if updated.State != pg.EntryStateTesting {
		t.Fatalf("expected testing state, got %s", updated.State)
	}
	if updated.MergeBranchName.String != wantBranch {
		t.Fatalf("expected merge branch %s recorded, got %s", wantBranch, updated.MergeBranchName.String)
	}

	// Verify TargetURL is set on the "Testing merge result" status.
	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 CreateCommitStatus call, got %d", len(statusCalls))
	}
	status := statusCalls[0].Args[3].(gitea.CommitStatus)
	wantURL := "https://mq.example.com/repo/org/app/pr/42"
	if status.TargetURL != wantURL {
		t.Fatalf("expected TargetURL %s, got %s", wantURL, status.TargetURL)
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

	result, err := merge.StartTesting(ctx, mock, svc, "org", "app", repoID, entry, "https://mq.example.com")
	if err != nil {
		t.Fatal(err)
	}

	if !result.Removed {
		t.Fatal("expected PR to be removed from queue")
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
	status := mock.CallsTo("CreateCommitStatus")[0].Args[3].(gitea.CommitStatus)
	if status.State != "failure" {
		t.Fatal("expected failure state")
	}
	wantURL := "https://mq.example.com/repo/org/app/pr/42"
	if status.TargetURL != wantURL {
		t.Fatalf("expected TargetURL %s, got %s", wantURL, status.TargetURL)
	}
	if len(mock.CallsTo("CreateComment")) != 1 {
		t.Fatal("expected conflict comment")
	}
}

// StartTesting clears stale gitea-mq/* mirrored statuses from a previous merge
// queue attempt so they don't show outdated results while new CI runs.
func TestStartTesting_ClearsStaleStatuses(t *testing.T) {
	mock, svc, ctx, repoID := setup(t)

	mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
		return &gitea.MergeResult{SHA: "mergesha456"}, nil
	}

	// Simulate stale gitea-mq/* statuses from a previous merge queue attempt.
	mock.GetCombinedCommitStatusFn = func(_ context.Context, _, _, ref string) (*gitea.CombinedStatus, error) {
		return &gitea.CombinedStatus{
			SHA:   ref,
			State: "failure",
			Statuses: []gitea.CommitStatusResult{
				{Context: "gitea-mq/ci/build", Status: "success"},
				{Context: "gitea-mq/ci/lint", Status: "failure"},
				{Context: "gitea-mq/ci/test", Status: "pending", Description: "test running"}, // already pending with different desc
				{Context: "gitea-mq", Status: "failure"},                                      // overwritten by StartTesting's own status
				{Context: "ci/build", Status: "success"},                                      // non-MQ status, should be left alone
			},
		}, nil
	}

	if _, err := svc.Enqueue(ctx, repoID, 42, "prsha", "main"); err != nil {
		t.Fatal(err)
	}
	entry, _ := svc.GetEntry(ctx, repoID, 42)

	result, err := merge.StartTesting(ctx, mock, svc, "org", "app", repoID, entry, "https://mq.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed {
		t.Fatal("expected PR to enter testing, not be removed")
	}

	statusCalls := mock.CallsTo("CreateCommitStatus")

	clearedContexts := make(map[string]bool)
	var testingStatusPosted bool
	for _, call := range statusCalls {
		sha := call.Args[2].(string)
		status := call.Args[3].(gitea.CommitStatus)

		if sha != "prsha" {
			t.Fatalf("expected status on prsha, got %s", sha)
		}

		if status.Context == "gitea-mq" && status.Description == "Testing merge result" {
			testingStatusPosted = true
			continue
		}

		if status.State == "pending" && status.Description == "From a previous merge queue attempt" {
			clearedContexts[status.Context] = true
		}
	}

	if !testingStatusPosted {
		t.Fatal("expected 'Testing merge result' status to be posted")
	}
	if !clearedContexts["gitea-mq/ci/build"] {
		t.Fatal("expected stale gitea-mq/ci/build to be reset to pending")
	}
	if !clearedContexts["gitea-mq/ci/lint"] {
		t.Fatal("expected stale gitea-mq/ci/lint to be reset to pending")
	}
	if !clearedContexts["gitea-mq/ci/test"] {
		t.Fatal("expected stale gitea-mq/ci/test to be reset to pending even though it was already pending")
	}

	// Non-MQ statuses must not be touched.
	for _, call := range statusCalls {
		status := call.Args[3].(gitea.CommitStatus)
		if status.Context == "ci/build" {
			t.Fatal("should not touch non-MQ status ci/build")
		}
	}
}

// StartTesting works normally when there are no stale gitea-mq/* statuses to clear.
func TestStartTesting_NoStaleStatuses(t *testing.T) {
	mock, svc, ctx, repoID := setup(t)

	mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
		return &gitea.MergeResult{SHA: "mergesha789"}, nil
	}

	// No gitea-mq/* statuses exist — fresh PR entering the queue for the first time.
	mock.GetCombinedCommitStatusFn = func(_ context.Context, _, _, ref string) (*gitea.CombinedStatus, error) {
		return &gitea.CombinedStatus{
			SHA:   ref,
			State: "success",
			Statuses: []gitea.CommitStatusResult{
				{Context: "ci/build", Status: "success"},
			},
		}, nil
	}

	if _, err := svc.Enqueue(ctx, repoID, 42, "prsha", "main"); err != nil {
		t.Fatal(err)
	}
	entry, _ := svc.GetEntry(ctx, repoID, 42)

	result, err := merge.StartTesting(ctx, mock, svc, "org", "app", repoID, entry, "https://mq.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed {
		t.Fatal("expected PR to enter testing, not be removed")
	}

	// Verify GetCombinedCommitStatus was called to check for stale statuses.
	if len(mock.CallsTo("GetCombinedCommitStatus")) != 1 {
		t.Fatalf("expected GetCombinedCommitStatus to be called, got %d calls", len(mock.CallsTo("GetCombinedCommitStatus")))
	}

	// Only the normal "Testing merge result" status should be posted.
	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 CreateCommitStatus call, got %d", len(statusCalls))
	}
	status := statusCalls[0].Args[3].(gitea.CommitStatus)
	if status.Context != "gitea-mq" || status.Description != "Testing merge result" {
		t.Fatalf("expected 'Testing merge result' status, got %+v", status)
	}
}

// CleanupStaleBranches deletes gitea-mq/* branches that have no active queue entry.
func TestCleanupStaleBranches_DeletesOrphans(t *testing.T) {
	mock, svc, ctx, repoID := setup(t)

	branch10 := merge.BranchName(10)
	branch99 := merge.BranchName(99)

	// Create one active entry with a merge branch.
	if _, err := svc.Enqueue(ctx, repoID, 10, "sha10", "main"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMergeBranch(ctx, repoID, 10, branch10, "mergesha10"); err != nil {
		t.Fatal(err)
	}

	// Simulate branches on the remote: gitea-mq/10 (active), gitea-mq/99 (stale), main (non-mq).
	mock.ListBranchesFn = func(_ context.Context, _, _ string) ([]gitea.Branch, error) {
		return []gitea.Branch{
			{Name: "main"},
			{Name: branch10},
			{Name: branch99},
		}, nil
	}

	if err := merge.CleanupStaleBranches(ctx, mock, svc, "org", "app", repoID); err != nil {
		t.Fatal(err)
	}

	// Only gitea-mq/99 should be deleted — gitea-mq/10 is active, main is not an mq branch.
	deletes := mock.CallsTo("DeleteBranch")
	if len(deletes) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(deletes))
	}
	if deletes[0].Args[2] != branch99 {
		t.Fatalf("expected %s deleted, got %s", branch99, deletes[0].Args[2])
	}
}

// CleanupStaleBranches continues if a single delete fails.
func TestCleanupStaleBranches_DeleteErrorContinues(t *testing.T) {
	mock, svc, ctx, repoID := setup(t)

	branch1 := merge.BranchName(1)
	branch2 := merge.BranchName(2)

	mock.ListBranchesFn = func(_ context.Context, _, _ string) ([]gitea.Branch, error) {
		return []gitea.Branch{
			{Name: branch1},
			{Name: branch2},
		}, nil
	}

	callCount := 0
	mock.DeleteBranchFn = func(_ context.Context, _, _, name string) error {
		callCount++
		if name == branch1 {
			return fmt.Errorf("permission denied")
		}
		return nil
	}

	if err := merge.CleanupStaleBranches(ctx, mock, svc, "org", "app", repoID); err != nil {
		t.Fatal(err)
	}

	// Both branches should be attempted even though gitea-mq/1 fails.
	if callCount != 2 {
		t.Fatalf("expected 2 delete attempts, got %d", callCount)
	}
}
