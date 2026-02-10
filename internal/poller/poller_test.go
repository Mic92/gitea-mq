package poller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/poller"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/testutil"
)

// setupPollerTest creates a fresh DB, queue service, mock client, and deps.
func setupPollerTest(t *testing.T) (*poller.Deps, *gitea.MockClient, *queue.Service, context.Context, int64) {
	t.Helper()

	svc, ctx, repoID := testutil.TestQueueService(t)

	mock := &gitea.MockClient{}
	deps := &poller.Deps{
		Gitea:          mock,
		Queue:          svc,
		RepoID:         repoID,
		Owner:          "org",
		Repo:           "app",
		SuccessTimeout: 5 * time.Minute,
	}

	return deps, mock, svc, ctx, repoID
}

func makePR(index int64, headSHA, baseBranch string) gitea.PR {
	return gitea.PR{
		Index: index,
		State: "open",
		Head:  &gitea.PRRef{Sha: headSHA},
		Base:  &gitea.PRRef{Ref: baseBranch},
	}
}

func automergeTimeline() []gitea.TimelineComment {
	return []gitea.TimelineComment{
		{ID: 1, Type: "pull_scheduled_merge", CreatedAt: time.Now()},
	}
}

func cancelledTimeline() []gitea.TimelineComment {
	return []gitea.TimelineComment{
		{ID: 1, Type: "pull_scheduled_merge", CreatedAt: time.Now().Add(-time.Minute)},
		{ID: 2, Type: "pull_cancel_scheduled_merge", CreatedAt: time.Now()},
	}
}

// --- Task 5.3: Poll cycle: new automerge PR → enqueue + pending ---

func TestPollOnce_NewAutomergePR_Enqueues(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "sha42", "main")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return automergeTimeline(), nil
	}
	mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
		return &gitea.MergeResult{SHA: "mock-merge-sha"}, nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Enqueued) != 1 || result.Enqueued[0] != 42 {
		t.Fatalf("expected PR #42 enqueued, got %v", result.Enqueued)
	}

	// Verify the PR is in the queue and transitioned to testing.
	entry, _ := svc.GetEntry(ctx, repoID, 42)
	if entry == nil {
		t.Fatal("PR #42 should be in queue")
	}
	if entry.PrHeadSha != "sha42" {
		t.Fatalf("expected head SHA sha42, got %s", entry.PrHeadSha)
	}
	if entry.State != pg.EntryStateTesting {
		t.Fatalf("expected state=testing after poll, got %s", entry.State)
	}

	// Verify two status calls: pending on enqueue, then pending "Testing merge result".
	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 2 {
		t.Fatalf("expected 2 CreateCommitStatus calls, got %d", len(statusCalls))
	}
	enqueueStatus := statusCalls[0].Args[3].(gitea.CommitStatus)
	if enqueueStatus.State != "pending" || enqueueStatus.Context != "gitea-mq" {
		t.Fatalf("expected pending gitea-mq status on enqueue, got %+v", enqueueStatus)
	}
	testingStatus := statusCalls[1].Args[3].(gitea.CommitStatus)
	if testingStatus.State != "pending" || testingStatus.Description != "Testing merge result" {
		t.Fatalf("expected pending 'Testing merge result' status, got %+v", testingStatus)
	}
}

func TestPollOnce_AlreadyQueued_Noop(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	// Pre-enqueue the PR.
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "sha42", "main")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return automergeTimeline(), nil
	}
	mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
		return &gitea.MergeResult{SHA: "mock-merge-sha"}, nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Enqueued) != 0 {
		t.Fatalf("expected no enqueues, got %v", result.Enqueued)
	}

	// No enqueue status call, but StartTesting should set "Testing merge result".
	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 CreateCommitStatus call (from StartTesting), got %d", len(statusCalls))
	}
	status := statusCalls[0].Args[3].(gitea.CommitStatus)
	if status.Description != "Testing merge result" {
		t.Fatalf("expected 'Testing merge result' status, got %+v", status)
	}

	// Verify entry transitioned to testing.
	entry, _ := svc.GetEntry(ctx, repoID, 42)
	if entry == nil || entry.State != pg.EntryStateTesting {
		t.Fatalf("expected state=testing, got %v", entry)
	}
}

// --- Task 5.5: Cancellation detection ---

func TestPollOnce_AutomergeCancelled_Dequeues(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	// Pre-enqueue PR #42.
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "sha42", "main")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return cancelledTimeline(), nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}

	// Verify PR is no longer in queue.
	entry, _ := svc.GetEntry(ctx, repoID, 42)
	if entry != nil {
		t.Fatal("PR #42 should not be in queue after cancellation")
	}
}

func TestPollOnce_HeadOfQueueCancelled_CleansUpMergeBranch(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	// Pre-enqueue PR #42 as head, with a merge branch.
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}
	_ = svc.UpdateState(ctx, repoID, 42, pg.EntryStateTesting)
	_ = svc.SetMergeBranch(ctx, repoID, 42, "mq/42", "mergesha")

	// Enqueue another PR so we can verify advancement.
	if _, err := svc.Enqueue(ctx, repoID, 43, "sha43", "main"); err != nil {
		t.Fatal(err)
	}

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{
			makePR(42, "sha42", "main"),
			makePR(43, "sha43", "main"),
		}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, index int64) ([]gitea.TimelineComment, error) {
		if index == 42 {
			return cancelledTimeline(), nil
		}
		return automergeTimeline(), nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}

	// Verify merge branch was deleted.
	deleteCalls := mock.CallsTo("DeleteBranch")
	if len(deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteBranch call, got %d", len(deleteCalls))
	}
	if deleteCalls[0].Args[2] != "mq/42" {
		t.Fatalf("expected delete of mq/42, got %v", deleteCalls[0].Args[2])
	}
}

// --- Task 5.7: Merged PR detection ---

func TestPollOnce_MergedPR_RemovesAndAdvances(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	// Pre-enqueue PR #42 as head in success state.
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}
	_ = svc.UpdateState(ctx, repoID, 42, pg.EntryStateSuccess)

	// Enqueue PR #43 behind it.
	if _, err := svc.Enqueue(ctx, repoID, 43, "sha43", "main"); err != nil {
		t.Fatal(err)
	}

	// PR #42 is merged (not in open PRs).
	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(43, "sha43", "main")}, nil
	}
	mock.GetPRFn = func(_ context.Context, _, _ string, index int64) (*gitea.PR, error) {
		if index == 42 {
			return &gitea.PR{Index: 42, HasMerged: true, State: "closed"}, nil
		}
		return nil, fmt.Errorf("not found")
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return automergeTimeline(), nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}
	if len(result.Advanced) != 1 || result.Advanced[0] != 42 {
		t.Fatalf("expected advancement from PR #42, got %v", result.Advanced)
	}
}

// --- Task 5.9: New push detection ---

func TestPollOnce_NewPush_RemovesAndCancels(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	// Pre-enqueue PR #42 with SHA "sha42".
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}

	// PR #42 has a new SHA.
	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "newsha", "main")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return automergeTimeline(), nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}

	// Verify automerge was cancelled.
	cancelCalls := mock.CallsTo("CancelAutoMerge")
	if len(cancelCalls) != 1 {
		t.Fatalf("expected 1 CancelAutoMerge call, got %d", len(cancelCalls))
	}

	// Verify comment was posted.
	commentCalls := mock.CallsTo("CreateComment")
	if len(commentCalls) != 1 {
		t.Fatalf("expected 1 CreateComment call, got %d", len(commentCalls))
	}
}

func TestPollOnce_NewPush_HeadOfQueue_CleansUpMergeBranch(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	// PR #42 is head with a merge branch.
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}
	_ = svc.UpdateState(ctx, repoID, 42, pg.EntryStateTesting)
	_ = svc.SetMergeBranch(ctx, repoID, 42, "mq/42", "mergesha")

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "newsha", "main")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return automergeTimeline(), nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 {
		t.Fatalf("expected 1 dequeue, got %v", result.Dequeued)
	}

	// Verify merge branch was deleted.
	deleteCalls := mock.CallsTo("DeleteBranch")
	if len(deleteCalls) != 1 || deleteCalls[0].Args[2] != "mq/42" {
		t.Fatalf("expected delete of mq/42, got %v", deleteCalls)
	}
}

// --- Task 5.11: Success-but-not-merged timeout ---

func TestPollOnce_SuccessButNotMerged_TimesOut(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)
	deps.SuccessTimeout = 1 * time.Millisecond // tiny timeout for testing

	// Pre-enqueue and set to success state.
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}
	_ = svc.UpdateState(ctx, repoID, 42, pg.EntryStateSuccess)

	// Wait for the timeout to elapse.
	time.Sleep(5 * time.Millisecond)

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "sha42", "main")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return automergeTimeline(), nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}

	// Verify cancel automerge was called.
	if len(mock.CallsTo("CancelAutoMerge")) != 1 {
		t.Fatal("expected CancelAutoMerge call")
	}

	// Verify error status was set.
	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 CreateCommitStatus call, got %d", len(statusCalls))
	}
	status := statusCalls[0].Args[3].(gitea.CommitStatus)
	if status.State != "error" {
		t.Fatalf("expected error status, got %s", status.State)
	}
}

// --- Task 5.13: Closed PR detection ---

func TestPollOnce_ClosedPR_SilentlyRemoves(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}

	// PR #42 is not in open PRs (closed).
	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return nil, nil
	}
	mock.GetPRFn = func(_ context.Context, _, _ string, index int64) (*gitea.PR, error) {
		return &gitea.PR{Index: 42, HasMerged: false, State: "closed"}, nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}

	// No comment or cancel for silently closed PRs.
	if len(mock.CallsTo("CancelAutoMerge")) != 0 {
		t.Fatal("should not cancel automerge for closed PR")
	}
	if len(mock.CallsTo("CreateComment")) != 0 {
		t.Fatal("should not comment for closed PR")
	}
}

// --- Task 5.15: Target branch change detection ---

func TestPollOnce_TargetBranchChanged_Removes(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	// Enqueue targeting "main".
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}

	// PR #42 now targets "release" instead.
	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "sha42", "release")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return automergeTimeline(), nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}

	// Verify automerge was cancelled and comment was posted.
	if len(mock.CallsTo("CancelAutoMerge")) != 1 {
		t.Fatal("expected CancelAutoMerge call for retargeted PR")
	}
	if len(mock.CallsTo("CreateComment")) != 1 {
		t.Fatal("expected comment for retargeted PR")
	}
}

// --- Task 5.17: Gitea unavailability ---

func TestPollOnce_GiteaUnavailable_Pauses(t *testing.T) {
	deps, mock, _, ctx, _ := setupPollerTest(t)

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return nil, fmt.Errorf("connection refused")
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce should not return error, got: %v", err)
	}

	if !result.Paused {
		t.Fatal("expected Paused=true when Gitea is unreachable")
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
}
