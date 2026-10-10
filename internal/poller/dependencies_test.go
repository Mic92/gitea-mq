package poller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/poller"
)

// A PR the forge would refuse to merge is not queued: why shows on its head,
// once, and it is queued by itself as soon as the dependency closes.
func TestPollOnce_OpenDependency_WaitsOutsideTheQueue(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)
	mockAutomergePRs(mock, makePR(42, "sha42", "main"))
	mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
		return &gitea.MergeResult{SHA: "mock-merge-sha"}, nil
	}
	state := "open"
	mock.ListIssueDependenciesFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.Issue, error) {
		return []gitea.Issue{{Index: 7, State: state}}, nil
	}

	for range 2 {
		result, err := poller.PollOnce(ctx, deps)
		if err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		if len(result.Enqueued) != 0 {
			t.Fatalf("PR with an open dependency was enqueued: %v", result.Enqueued)
		}
	}
	if entry, _ := svc.GetEntry(ctx, repoID, 42); entry != nil {
		t.Fatalf("PR #42 queued: %+v", entry)
	}
	statuses := mock.CallsTo("CreateCommitStatus")
	if len(statuses) != 1 {
		t.Fatalf("want one blocked status over two polls, got %d", len(statuses))
	}
	if s := statuses[0].Args[3].(gitea.CommitStatus); s.State != "pending" || s.Description != "Blocked by open dependency #7" {
		t.Fatalf("unexpected blocked status: %+v", s)
	}
	if n := len(mock.CallsTo("CancelAutoMerge")); n != 0 {
		t.Fatalf("auto-merge cancelled %d time(s)", n)
	}

	state = "closed"
	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(result.Enqueued) != 1 || result.Enqueued[0] != 42 {
		t.Fatalf("want #42 enqueued once its dependency closed, got %v", result.Enqueued)
	}
}

// A queued PR that gains a dependency leaves the queue, unlike every other
// removal, with its auto-merge still scheduled.
func TestPollOnce_QueuedPRGainsDependency_HeldNotCancelled(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)
	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}
	mockAutomergePRs(mock, makePR(42, "sha42", "main"))
	mock.ListIssueDependenciesFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.Issue, error) {
		return []gitea.Issue{{Index: 7, State: "open"}}, nil
	}

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
	if len(comments) != 1 || !strings.Contains(comments[0].Args[3].(string), "depends on #7") {
		t.Fatalf("want one comment naming #7, got %v", comments)
	}
}
