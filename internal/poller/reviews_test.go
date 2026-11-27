package poller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/poller"
)

func requireApprovals(mock *gitea.MockClient, n int64, reviews *[]gitea.Review) {
	mock.GetBranchProtectionFn = func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
		return &gitea.BranchProtection{RequiredApprovals: n, BlockOnRejectedReviews: true}, nil
	}
	mock.ListPRReviewsFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.Review, error) {
		return *reviews, nil
	}
}

// An unapproved PR is not tested (Mic92/gitea-mq#152): it waits outside the
// queue with the reason on its head, keeps auto-merge, and is queued once
// approved.
func TestPollOnce_MissingApprovals_WaitsOutsideTheQueue(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)
	mockAutomergePRs(mock, makePR(42, "sha42", "main"))
	var reviews []gitea.Review
	requireApprovals(mock, 1, &reviews)

	for range 2 {
		result, err := poller.PollOnce(ctx, deps)
		if err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		if len(result.Enqueued) != 0 {
			t.Fatalf("unapproved PR was enqueued: %v", result.Enqueued)
		}
	}
	if entry, _ := svc.GetEntry(ctx, repoID, 42); entry != nil {
		t.Fatalf("PR #42 queued: %+v", entry)
	}
	statuses := mock.CallsTo("CreateCommitStatus")
	if len(statuses) != 1 {
		t.Fatalf("want one waiting status over two polls, got %d", len(statuses))
	}
	if s := statuses[0].Args[3].(gitea.CommitStatus); s.State != "pending" || s.Description != "Waiting for approvals (0/1)" {
		t.Fatalf("unexpected waiting status: %+v", s)
	}
	if n := len(mock.CallsTo("CancelAutoMerge")); n != 0 {
		t.Fatalf("auto-merge cancelled %d time(s)", n)
	}

	reviews = []gitea.Review{{State: "APPROVED", Official: true}}
	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(result.Enqueued) != 1 || result.Enqueued[0] != 42 {
		t.Fatalf("want #42 enqueued once approved, got %v", result.Enqueued)
	}
}

// A queued PR that gets changes requested leaves the queue with auto-merge
// still scheduled, instead of passing and blocking the head until timeout.
func TestPollOnce_QueuedPRChangesRequested_HeldNotCancelled(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main", false); err != nil {
		t.Fatal(err)
	}
	mockAutomergePRs(mock, makePR(42, "sha42", "main"))
	reviews := []gitea.Review{
		{State: "APPROVED", Official: true},
		{State: "REQUEST_CHANGES", Official: true},
	}
	requireApprovals(mock, 1, &reviews)

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("want #42 dequeued, got %v", result.Dequeued)
	}
	if entry, _ := svc.GetEntry(ctx, repoID, 42); entry != nil {
		t.Fatalf("PR #42 still queued: %+v", entry)
	}
	if n := len(mock.CallsTo("CancelAutoMerge")); n != 0 {
		t.Fatalf("auto-merge cancelled %d time(s); it must stay scheduled", n)
	}
	comments := mock.CallsTo("CreateComment")
	if len(comments) != 1 || !strings.Contains(comments[0].Args[3].(string), "Changes requested") {
		t.Fatalf("want one comment naming the requested changes, got %v", comments)
	}
}
