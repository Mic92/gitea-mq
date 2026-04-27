package poller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func setupPollerTest(t *testing.T) (*poller.Deps, *gitea.MockClient, *queue.Service, context.Context, int64) {
	t.Helper()

	svc, ctx, repoID := testutil.TestQueueService(t)

	mock := &gitea.MockClient{}
	deps := &poller.Deps{
		Forge:          gitea.NewForge(mock, "https://gitea.example.com"),
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
	return []gitea.TimelineComment{{ID: 1, Type: "pull_scheduled_merge", CreatedAt: time.Now()}}
}

func cancelledTimeline() []gitea.TimelineComment {
	return []gitea.TimelineComment{
		{ID: 1, Type: "pull_scheduled_merge", CreatedAt: time.Now().Add(-time.Minute)},
		{ID: 2, Type: "pull_cancel_scheduled_merge", CreatedAt: time.Now()},
	}
}

// Happy path: a fresh auto-merge PR is discovered, queued, and immediately
// promoted to testing with the right commit statuses.
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

	entry, _ := svc.GetEntry(ctx, repoID, 42)
	if entry == nil || entry.PrHeadSha != "sha42" || entry.State != pg.EntryStateTesting {
		t.Fatalf("expected queued+testing entry for #42, got %+v", entry)
	}

	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 2 {
		t.Fatalf("expected 2 CreateCommitStatus calls, got %d", len(statusCalls))
	}
	if s := statusCalls[0].Args[3].(gitea.CommitStatus); s.State != "pending" || s.Context != "gitea-mq" {
		t.Fatalf("expected pending gitea-mq status on enqueue, got %+v", s)
	}
	if s := statusCalls[1].Args[3].(gitea.CommitStatus); s.State != "pending" || s.Description != "Testing merge result" {
		t.Fatalf("expected pending 'Testing merge result' status, got %+v", s)
	}
}

type reconcileCase struct {
	name        string
	openPRs     []gitea.PR
	getPR       *gitea.PR // returned for #42 when not in openPRs
	timeline    []gitea.TimelineComment
	wantAdvance bool
	wantCancel  bool
	wantComment bool
}

func (tc reconcileCase) run(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) { return tc.openPRs, nil }
	mock.GetPRFn = func(_ context.Context, _, _ string, _ int64) (*gitea.PR, error) { return tc.getPR, nil }
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
		return tc.timeline, nil
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 dequeued, got %v", result.Dequeued)
	}
	if entry, _ := svc.GetEntry(ctx, repoID, 42); entry != nil {
		t.Fatalf("PR #42 should be removed from queue, got %+v", entry)
	}
	if got := len(result.Advanced) == 1; got != tc.wantAdvance {
		t.Fatalf("advance: got %v want %v (%v)", got, tc.wantAdvance, result.Advanced)
	}
	if got := len(mock.CallsTo("CancelAutoMerge")); (got == 1) != tc.wantCancel {
		t.Fatalf("CancelAutoMerge calls: got %d want cancel=%v", got, tc.wantCancel)
	}
	if got := len(mock.CallsTo("CreateComment")); (got == 1) != tc.wantComment {
		t.Fatalf("CreateComment calls: got %d want comment=%v", got, tc.wantComment)
	}
}

// All reconcile remove-conditions share the same removePR plumbing; verify each
// predicate fires and carries the right side effects (cancel/comment/advance).
func TestPollOnce_Reconcile(t *testing.T) {
	cases := []reconcileCase{
		{
			name:        "merged",
			openPRs:     nil,
			getPR:       &gitea.PR{Index: 42, HasMerged: true, State: "closed"},
			wantAdvance: true,
		},
		{
			name:    "closed",
			openPRs: nil,
			getPR:   &gitea.PR{Index: 42, HasMerged: false, State: "closed"},
		},
		{
			name:        "retargeted",
			openPRs:     []gitea.PR{makePR(42, "sha42", "release")},
			timeline:    automergeTimeline(),
			wantCancel:  true,
			wantComment: true,
		},
		{
			name:        "pushed",
			openPRs:     []gitea.PR{makePR(42, "newsha", "main")},
			timeline:    automergeTimeline(),
			wantAdvance: true,
			wantCancel:  true,
			wantComment: true,
		},
		{
			name:     "automerge_cancelled",
			openPRs:  []gitea.PR{makePR(42, "sha42", "main")},
			timeline: cancelledTimeline(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// Removing the head-of-queue entry must also delete its merge branch so the
// next head starts from a clean slate.
func TestPollOnce_RemoveHead_CleansUpMergeBranch(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}
	_ = svc.UpdateState(ctx, repoID, 42, pg.EntryStateTesting)
	_ = svc.SetMergeBranch(ctx, repoID, 42, merge.BranchName(42), "mergesha")
	if _, err := svc.Enqueue(ctx, repoID, 43, "sha43", "main"); err != nil {
		t.Fatal(err)
	}

	mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
		return []gitea.PR{makePR(42, "sha42", "main"), makePR(43, "sha43", "main")}, nil
	}
	mock.GetPRTimelineFn = func(_ context.Context, _, _ string, index int64) ([]gitea.TimelineComment, error) {
		if index == 42 {
			return cancelledTimeline(), nil
		}
		return automergeTimeline(), nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	wantBranch := merge.BranchName(42)
	deleteCalls := mock.CallsTo("DeleteBranch")
	if len(deleteCalls) != 1 || deleteCalls[0].Args[2] != wantBranch {
		t.Fatalf("expected delete of %s, got %v", wantBranch, deleteCalls)
	}
}

func TestPollOnce_SuccessButNotMerged_TimesOut(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)
	deps.SuccessTimeout = 1 * time.Millisecond

	if _, err := svc.Enqueue(ctx, repoID, 42, "sha42", "main"); err != nil {
		t.Fatal(err)
	}
	_ = svc.UpdateState(ctx, repoID, 42, pg.EntryStateSuccess)
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
	if len(mock.CallsTo("CancelAutoMerge")) != 1 {
		t.Fatal("expected CancelAutoMerge call")
	}
	statusCalls := mock.CallsTo("CreateCommitStatus")
	if len(statusCalls) != 1 || statusCalls[0].Args[3].(gitea.CommitStatus).State != "error" {
		t.Fatalf("expected single error status, got %v", statusCalls)
	}
}

// prChecksGreen gating: only enqueue once required checks (or none) are green.
func TestPollOnce_CIGating(t *testing.T) {
	cases := []struct {
		name           string
		fallbackChecks []string
		status         *gitea.CombinedStatus
		wantEnqueued   bool
	}{
		{
			name:           "pending",
			fallbackChecks: []string{"ci/build"},
			status: &gitea.CombinedStatus{
				State: "pending", Statuses: []gitea.CommitStatusResult{{Context: "ci/build", Status: "pending"}},
			},
		},
		{
			name:           "failure",
			fallbackChecks: []string{"ci/build"},
			status: &gitea.CombinedStatus{
				State: "failure", Statuses: []gitea.CommitStatusResult{{Context: "ci/build", Status: "failure"}},
			},
		},
		{
			name:           "success",
			fallbackChecks: []string{"ci/build"},
			status: &gitea.CombinedStatus{
				State: "success", Statuses: []gitea.CommitStatusResult{{Context: "ci/build", Status: "success"}},
			},
			wantEnqueued: true,
		},
		{
			name:         "no_ci_configured",
			status:       &gitea.CombinedStatus{State: "pending", Statuses: nil},
			wantEnqueued: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, mock, svc, ctx, repoID := setupPollerTest(t)
			deps.FallbackChecks = tc.fallbackChecks

			mock.ListOpenPRsFn = func(_ context.Context, _, _ string) ([]gitea.PR, error) {
				return []gitea.PR{makePR(42, "sha42", "main")}, nil
			}
			mock.GetPRTimelineFn = func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
				return automergeTimeline(), nil
			}
			mock.GetCombinedCommitStatusFn = func(_ context.Context, _, _, _ string) (*gitea.CombinedStatus, error) {
				return tc.status, nil
			}
			mock.MergeBranchesFn = func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
				return &gitea.MergeResult{SHA: "mock-merge-sha"}, nil
			}

			result, err := poller.PollOnce(ctx, deps)
			if err != nil {
				t.Fatalf("PollOnce: %v", err)
			}

			if got := len(result.Enqueued) == 1; got != tc.wantEnqueued {
				t.Fatalf("enqueued: got %v want %v (%v)", got, tc.wantEnqueued, result.Enqueued)
			}
			entry, _ := svc.GetEntry(ctx, repoID, 42)
			if (entry != nil) != tc.wantEnqueued {
				t.Fatalf("queue entry presence: got %v want %v", entry != nil, tc.wantEnqueued)
			}
			if !tc.wantEnqueued && len(mock.CallsTo("CreateCommitStatus")) != 0 {
				t.Fatal("should not post status for unenqueued PR")
			}
		})
	}
}

func TestPollOnce_MergeBranchError_NotifiesUser(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupPollerTest(t)

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
		return nil, fmt.Errorf("merge: git merge: exit status 128\nfatal: refusing to merge unrelated histories")
	}

	result, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	if entry, _ := svc.GetEntry(ctx, repoID, 42); entry != nil {
		t.Fatalf("PR #42 should be removed after merge error, got state=%s", entry.State)
	}
	if len(result.Dequeued) != 1 || result.Dequeued[0] != 42 {
		t.Fatalf("expected PR #42 in result.Dequeued, got %v", result.Dequeued)
	}
	if len(result.Errors) == 0 {
		t.Fatal("expected error in result.Errors for logging")
	}

	var foundFailure bool
	for _, call := range mock.CallsTo("CreateCommitStatus") {
		if s := call.Args[3].(gitea.CommitStatus); s.State == "error" && s.Context == "gitea-mq" {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatal("expected error status on PR head after merge failure")
	}
	if len(mock.CallsTo("CreateComment")) == 0 {
		t.Fatal("expected a comment explaining the merge failure")
	}
	if len(mock.CallsTo("CancelAutoMerge")) == 0 {
		t.Fatal("expected CancelAutoMerge to be called")
	}
}

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
