package monitor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func setupMonitorTest(t *testing.T) (*monitor.Deps, *gitea.MockClient, *queue.Service, context.Context, int64) {
	t.Helper()

	svc, ctx, repoID := testutil.TestQueueService(t)

	mock := &gitea.MockClient{}
	deps := &monitor.Deps{
		Gitea:        mock,
		Queue:        svc,
		Owner:        "org",
		Repo:         "app",
		RepoID:       repoID,
		CheckTimeout: 1 * time.Hour,
	}

	return deps, mock, svc, ctx, repoID
}

// enqueueTesting is a helper that enqueues a PR and transitions it to
// the testing state with a merge branch, which is the precondition for
// ProcessCheckStatus.
func enqueueTesting(t *testing.T, svc *queue.Service, ctx context.Context, repoID, prNumber int64) {
	t.Helper()

	if _, err := svc.Enqueue(ctx, repoID, prNumber, "sha"+string(rune('0'+prNumber%10)), "main"); err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdateState(ctx, repoID, prNumber, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetMergeBranch(ctx, repoID, prNumber, merge.BranchName(prNumber), "mergesha"); err != nil {
		t.Fatal(err)
	}
}

func withBranchProtection(mock *gitea.MockClient, checks ...string) {
	mock.GetBranchProtectionFn = func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
		return &gitea.BranchProtection{
			EnableStatusCheck:   true,
			StatusCheckContexts: checks,
		}, nil
	}
}

// All required checks pass → gitea-mq set to success, merge branch deleted,
// entry stays in queue (poller confirms merge later).
func TestProcessCheckStatus_AllPass_TriggersSuccess(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupMonitorTest(t)
	withBranchProtection(mock, "gitea-mq", "ci/build")
	enqueueTesting(t, svc, ctx, repoID, 42)

	entry, _ := svc.GetEntry(ctx, repoID, 42)

	if err := monitor.ProcessCheckStatus(ctx, deps, entry, "ci/build", pg.CheckStateSuccess, ""); err != nil {
		t.Fatal(err)
	}

	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 CreateCommitStatus, got %d", len(statusCalls))
	}
	if statusCalls[0].Args[3].(gitea.CommitStatus).State != "success" {
		t.Fatal("expected success status")
	}

	// Entry must still be in queue — poller removes after merge.
	entry, _ = svc.GetEntry(ctx, repoID, 42)
	if entry == nil || entry.State != pg.EntryStateSuccess {
		t.Fatal("entry should be in success state, not removed")
	}
}

// A required check fails → gitea-mq set to failure, automerge cancelled,
// comment posted, merge branch deleted, queue advances.
func TestProcessCheckStatus_Failure_CancelsAndAdvances(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupMonitorTest(t)
	withBranchProtection(mock, "gitea-mq", "ci/build")
	enqueueTesting(t, svc, ctx, repoID, 42)

	// PR #43 is next in line.
	if _, err := svc.Enqueue(ctx, repoID, 43, "sha43", "main"); err != nil {
		t.Fatal(err)
	}

	entry, _ := svc.GetEntry(ctx, repoID, 42)

	if err := monitor.ProcessCheckStatus(ctx, deps, entry, "ci/build", pg.CheckStateFailure, "https://ci.example.com/build/42"); err != nil {
		t.Fatal(err)
	}

	if statusCalls := mock.CallsTo("CreateCommitStatus"); len(statusCalls) != 1 ||
		statusCalls[0].Args[3].(gitea.CommitStatus).State != "failure" {
		t.Fatal("expected failure status on PR head commit")
	}
	if len(mock.CallsTo("CancelAutoMerge")) != 1 {
		t.Fatal("expected CancelAutoMerge")
	}
	commentCalls := mock.CallsTo("CreateComment")
	if len(commentCalls) != 1 {
		t.Fatal("expected failure comment")
	}
	commentBody := commentCalls[0].Args[3].(string)
	if !strings.Contains(commentBody, "[ci/build](https://ci.example.com/build/42)") {
		t.Fatalf("expected markdown link in comment, got: %s", commentBody)
	}
	if len(mock.CallsTo("DeleteBranch")) != 1 {
		t.Fatal("expected merge branch cleanup")
	}

	head, _ := svc.Head(ctx, repoID, "main")
	if head == nil || head.PrNumber != 43 {
		t.Fatal("expected queue to advance to PR #43")
	}
}

// Only some required checks reported → no action, stay waiting.
func TestProcessCheckStatus_Partial_StaysWaiting(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupMonitorTest(t)
	withBranchProtection(mock, "gitea-mq", "ci/build", "ci/lint")
	enqueueTesting(t, svc, ctx, repoID, 42)

	entry, _ := svc.GetEntry(ctx, repoID, 42)

	// Only ci/build reported — ci/lint still pending.
	if err := monitor.ProcessCheckStatus(ctx, deps, entry, "ci/build", pg.CheckStateSuccess, ""); err != nil {
		t.Fatal(err)
	}

	// No success/failure status should be set.
	if len(mock.CallsTo("CreateCommitStatus")) != 0 {
		t.Fatal("should not set status while waiting for more checks")
	}

	// Entry should still be testing.
	entry, _ = svc.GetEntry(ctx, repoID, 42)
	if entry.State != pg.EntryStateTesting {
		t.Fatalf("expected testing state, got %s", entry.State)
	}
}

// On success, stale gitea-mq/* statuses (pending with the StaleMirrorDescription)
// are set to skipped, while other pending statuses and non-pending statuses are
// left alone.
func TestProcessCheckStatus_Success_SkipsStaleMirroredStatuses(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupMonitorTest(t)
	withBranchProtection(mock, "gitea-mq", "ci/build")
	enqueueTesting(t, svc, ctx, repoID, 42)

	// Simulate stale gitea-mq/* statuses on the PR head from a previous attempt.
	mock.GetCombinedCommitStatusFn = func(_ context.Context, _, _, _ string) (*gitea.CombinedStatus, error) {
		return &gitea.CombinedStatus{
			Statuses: []gitea.CommitStatusResult{
				{Context: "gitea-mq/ci/old-check", Status: "pending", Description: "From a previous merge queue attempt"},
				{Context: "gitea-mq/ci/build", Status: "success", Description: "build passed"},               // not pending — leave alone
				{Context: "gitea-mq/ci/other", Status: "pending", Description: "some other description"},     // wrong description — leave alone
				{Context: "ci/build", Status: "pending", Description: "From a previous merge queue attempt"}, // not gitea-mq/* — leave alone
			},
		}, nil
	}

	entry, _ := svc.GetEntry(ctx, repoID, 42)

	if err := monitor.ProcessCheckStatus(ctx, deps, entry, "ci/build", pg.CheckStateSuccess, ""); err != nil {
		t.Fatal(err)
	}

	statusCalls := mock.CallsTo("CreateCommitStatus")

	// Find the skipped call — should only be gitea-mq/ci/old-check.
	var skippedContexts []string
	for _, call := range statusCalls {
		status := call.Args[3].(gitea.CommitStatus)
		if status.State == "skipped" {
			skippedContexts = append(skippedContexts, status.Context)
		}
	}

	if len(skippedContexts) != 1 || skippedContexts[0] != "gitea-mq/ci/old-check" {
		t.Fatalf("expected only gitea-mq/ci/old-check to be skipped, got %v", skippedContexts)
	}

	// Verify the skipped status has the right description.
	for _, call := range statusCalls {
		status := call.Args[3].(gitea.CommitStatus)
		if status.State == "skipped" && status.Description != "From a previous merge queue attempt" {
			t.Fatalf("expected skipped status description 'From a previous merge queue attempt', got %q", status.Description)
		}
	}
}

// A check retries: failure → pending → success. The latest state (success)
// is what counts because SaveCheckStatus upserts.
func TestProcessCheckStatus_RetrySuccess(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupMonitorTest(t)
	withBranchProtection(mock, "gitea-mq", "ci/build")
	enqueueTesting(t, svc, ctx, repoID, 42)

	entry, _ := svc.GetEntry(ctx, repoID, 42)

	// First: failure.
	if err := monitor.ProcessCheckStatus(ctx, deps, entry, "ci/build", pg.CheckStateFailure, ""); err != nil {
		t.Fatal(err)
	}
	// This triggers failure handling — reset for the retry test.
	// Re-setup with a clean DB so the queue and mock state are fresh.
	deps, mock, svc, ctx, repoID = setupMonitorTest(t)
	withBranchProtection(mock, "gitea-mq", "ci/build")
	enqueueTesting(t, svc, ctx, repoID, 42)

	entry, _ = svc.GetEntry(ctx, repoID, 42)

	// Record failure, then overwrite with success (simulating retry).
	_ = svc.SaveCheckStatus(ctx, entry.ID, "ci/build", pg.CheckStateFailure, "")
	if err := monitor.ProcessCheckStatus(ctx, deps, entry, "ci/build", pg.CheckStateSuccess, ""); err != nil {
		t.Fatal(err)
	}

	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 1 || statusCalls[0].Args[3].(gitea.CommitStatus).State != "success" {
		t.Fatal("expected success after retry overwrites failure")
	}
}
