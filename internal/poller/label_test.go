package poller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/batch"
	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func setupLabelTest(t *testing.T) (*poller.Deps, *forge.MockForge, *queue.Service, context.Context, int64) {
	t.Helper()
	svc, ctx, repoID := testutil.TestQueueService(t)
	mock := &forge.MockForge{}
	deps := &poller.Deps{
		Forge:      mock,
		Queue:      svc,
		RepoID:     repoID,
		Owner:      "org",
		Repo:       "app",
		MergeLabel: "merge-queue",
	}
	return deps, mock, svc, ctx, repoID
}

func TestLabeledPREnqueuedAndTested(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}}}, nil
	}
	mock.CreateMergeBranchFn = func(context.Context, string, string, string, string, string) (string, bool, error) {
		return "m7", false, nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}

	entry, err := svc.GetEntry(ctx, repoID, 7)
	if err != nil || entry == nil {
		t.Fatalf("entry = %v, err = %v", entry, err)
	}
	if entry.TargetBranch != "main" || entry.State != pg.EntryStateTesting {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestLabeledStackPRUsesStackBase(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{
			{Number: 1, State: "open", HeadBranch: "b1", HeadSHA: "sha1", BaseBranch: "main"},
			{Number: 2, State: "open", HeadBranch: "b2", HeadSHA: "sha2", BaseBranch: "b1", Labels: []string{"merge-queue"}},
		}, nil
	}
	mock.ResolveStackFn = func(_ context.Context, _, _ string, number int64) (*forge.Stack, error) {
		return &forge.Stack{BaseBranch: "main", PRs: []forge.StackPR{
			{Number: 1, HeadSHA: "sha1"}, {Number: 2, HeadSHA: "sha2"},
		}}, nil
	}
	mock.CreateMergeBranchFn = func(context.Context, string, string, string, string, string) (string, bool, error) {
		return "m2", false, nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}

	entry, _ := svc.GetEntry(ctx, repoID, 2)
	if entry == nil || entry.TargetBranch != "main" {
		t.Fatalf("entry = %+v", entry)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 1); e != nil {
		t.Fatalf("unlabeled stack member must not be enqueued: %+v", e)
	}
}

func TestLabelRemovalDequeues(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	if _, err := svc.Enqueue(ctx, repoID, 7, "h7", "main"); err != nil {
		t.Fatal(err)
	}
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main"}}, nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}

	if e, _ := svc.GetEntry(ctx, repoID, 7); e != nil {
		t.Fatalf("entry should be dequeued: %+v", e)
	}
}

func TestFinalizeLabeledStackMerge(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	if _, err := svc.Enqueue(ctx, repoID, 2, "sha2", "main"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, repoID, 2, pg.EntryStateSuccess); err != nil {
		t.Fatal(err)
	}
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{
			{Number: 1, State: "open", HeadBranch: "b1", HeadSHA: "sha1", BaseBranch: "main"},
			{Number: 2, State: "open", HeadBranch: "b2", HeadSHA: "sha2", BaseBranch: "b1", Labels: []string{"merge-queue"}},
		}, nil
	}
	mock.ResolveStackFn = func(context.Context, string, string, int64) (*forge.Stack, error) {
		return &forge.Stack{BaseBranch: "main", PRs: []forge.StackPR{
			{Number: 1, HeadSHA: "sha1"}, {Number: 2, HeadSHA: "sha2"},
		}}, nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}

	var memberGreen bool
	for _, c := range mock.CallsTo("SetMQStatus") {
		if c.Args[2] == "sha1" && c.Args[3].(forge.MQStatus).State == pg.CheckStateSuccess {
			memberGreen = true
		}
	}
	if !memberGreen {
		t.Error("expected success status on stack member head sha1")
	}
	merges := mock.CallsTo("MergePR")
	if len(merges) != 1 || merges[0].Args[2].(int64) != 2 {
		t.Fatalf("MergePR calls = %+v", merges)
	}
}

// Regression: an up-to-date labeled stack head must be landed by gitea-mq
// (fast-forward), not handed to the forge's merge API which re-checks every
// stack member's own rules.
func TestLabeledStackUpToDateLandsViaFastForward(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	deps.SkipQueueIfUpToDate = true
	deps.Batch = &batch.Engine{
		Forge: mock, Queue: svc, Owner: "org", Repo: "app", RepoID: repoID,
		SkipIfUpToDate: true, MergedPollInterval: 1, MergedPollAttempts: 1,
	}
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{
			{Number: 1, State: "open", HeadBranch: "b1", HeadSHA: "sha1", BaseBranch: "main"},
			{Number: 2, State: "open", HeadBranch: "b2", HeadSHA: "sha2", BaseBranch: "b1", Labels: []string{"merge-queue"}},
		}, nil
	}
	mock.ResolveStackFn = func(context.Context, string, string, int64) (*forge.Stack, error) {
		return &forge.Stack{BaseBranch: "main", PRs: []forge.StackPR{
			{Number: 1, HeadSHA: "sha1"}, {Number: 2, HeadSHA: "sha2"},
		}}, nil
	}
	mock.IsUpToDateFn = func(_ context.Context, _, _, base, head string) (bool, error) {
		return base == "main" && head == "sha2", nil
	}
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) {
		return []string{"ci"}, nil
	}
	mock.GetCheckStatesFn = func(_ context.Context, _, _, sha string) (map[string]forge.Check, error) {
		return map[string]forge.Check{"ci": {State: pg.CheckStateSuccess}}, nil
	}
	var ffTo string
	mock.FastForwardFn = func(_ context.Context, _, _, branch, sha string) error {
		if branch != "main" {
			t.Errorf("fast-forward branch = %q", branch)
		}
		ffTo = sha
		return nil
	}

	if res, err := poller.PollOnce(ctx, deps); err != nil || len(res.Errors) > 0 {
		t.Fatalf("PollOnce: %v %v", err, res)
	}

	if ffTo != "sha2" {
		t.Fatalf("main fast-forwarded to %q, want sha2", ffTo)
	}
	if n := len(mock.CallsTo("MergePR")); n != 0 {
		t.Fatalf("MergePR called %d times; forge must not be asked to merge", n)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 2); e != nil {
		t.Fatalf("entry should be landed and removed: %+v", e)
	}
}

func TestStackHint(t *testing.T) {
	deps, mock, _, ctx, _ := setupLabelTest(t)
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{
			{Number: 1, State: "open", HeadBranch: "b1", HeadSHA: "sha1", BaseBranch: "main"},
			{Number: 2, State: "open", HeadBranch: "b2", HeadSHA: "sha2", BaseBranch: "b1"},
			{Number: 3, State: "open", HeadBranch: "b3", HeadSHA: "sha3", BaseBranch: "main"},
		}, nil
	}

	for range 2 {
		if _, err := poller.PollOnce(ctx, deps); err != nil {
			t.Fatal(err)
		}
	}

	hinted := map[string]int{}
	for _, c := range mock.CallsTo("SetMQStatus") {
		st := c.Args[3].(forge.MQStatus)
		if strings.Contains(st.Description, "Stack detected") {
			hinted[c.Args[2].(string)]++
		}
	}
	if hinted["sha1"] != 1 || hinted["sha2"] != 1 {
		t.Errorf("hints = %v, want exactly one per stack member", hinted)
	}
	if hinted["sha3"] != 0 {
		t.Errorf("non-stack PR must not be hinted: %v", hinted)
	}
}
