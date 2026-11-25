package poller_test

import (
	"context"
	"errors"
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
	if _, err := svc.Enqueue(ctx, repoID, 7, "h7", "main", false, ""); err != nil {
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
	if _, err := svc.Enqueue(ctx, repoID, 2, "sha2", "main", false, ""); err != nil {
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

// The top stays open and labeled while the forge merges the stack; polling
// in the meantime must not enqueue it again.
func TestLabeledStackLandsViaStackMergeOnce(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	deps.SkipQueueIfUpToDate = true
	deps.Batch = &batch.Engine{
		Forge: mock, Queue: svc, Owner: "org", Repo: "app", RepoID: repoID,
		SkipIfUpToDate: true, MergedPollInterval: 1, MergedPollAttempts: 1,
		StackMergedPollInterval: 1, StackMergedPollAttempts: 1,
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

	// The forge keeps both PRs open (async merge still running).
	for range 3 {
		if res, err := poller.PollOnce(ctx, deps); err != nil || len(res.Errors) > 0 {
			t.Fatalf("PollOnce: %v %v", err, res)
		}
	}

	if n := len(mock.CallsTo("FastForward")); n != 0 {
		t.Fatalf("FastForward called %d times; a stack must land via the forge", n)
	}
	merges := mock.CallsTo("MergePR")
	if len(merges) != 1 || merges[0].Args[2].(int64) != 2 {
		t.Fatalf("MergePR calls = %+v, want exactly one for #2", merges)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 2); e != nil {
		t.Fatalf("landed entry must not be re-enqueued: %+v", e)
	}
	if n := len(mock.CallsTo("ClosePR")); n != 0 {
		t.Fatalf("ClosePR called %d times for a stack", n)
	}

	var parentGreen bool
	for _, c := range mock.CallsTo("SetMQStatus") {
		if c.Args[2] == "sha1" && c.Args[3].(forge.MQStatus).State == pg.CheckStateSuccess {
			parentGreen = true
		}
	}
	if !parentGreen {
		t.Error("expected success status on stack parent head sha1")
	}
}

// After a restart the landed record is gone; a head already in the target is
// still not queued.
func TestLabeledPRContainedInTargetIsNotEnqueued(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	deps.Batch = &batch.Engine{Forge: mock, Queue: svc, Owner: "org", Repo: "app", RepoID: repoID}
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}}}, nil
	}
	mock.IsUpToDateFn = func(_ context.Context, _, _, base, head string) (bool, error) {
		return base == "h7" && head == "main", nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 7); e != nil {
		t.Fatalf("PR already in target was enqueued: %+v", e)
	}
}

// Mic92/nixbot#179: the forge's stack merge re-checks every member's own CI,
// so a stack is only queued once all of them are green.
func TestLabeledStackWaitsForMemberCI(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{
			{Number: 1, State: "open", HeadBranch: "b1", HeadSHA: "sha1", BaseBranch: "main"},
			{Number: 2, State: "open", HeadBranch: "b2", HeadSHA: "sha2", BaseBranch: "b1"},
			{Number: 3, State: "open", HeadBranch: "b3", HeadSHA: "sha3", BaseBranch: "b2", Labels: []string{"merge-queue"}},
		}, nil
	}
	mock.ResolveStackFn = func(context.Context, string, string, int64) (*forge.Stack, error) {
		return &forge.Stack{BaseBranch: "main", PRs: []forge.StackPR{
			{Number: 1, HeadSHA: "sha1"}, {Number: 2, HeadSHA: "sha2"}, {Number: 3, HeadSHA: "sha3"},
		}}, nil
	}
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) {
		return []string{"ci"}, nil
	}
	pending := true
	mock.GetCheckStatesFn = func(_ context.Context, _, _, sha string) (map[string]forge.Check, error) {
		if sha == "sha2" && pending {
			return map[string]forge.Check{"ci": {State: pg.CheckStatePending}}, nil
		}
		return map[string]forge.Check{"ci": {State: pg.CheckStateSuccess}}, nil
	}
	mock.CreateMergeBranchFn = func(context.Context, string, string, string, string, string) (string, bool, error) {
		return "m3", false, nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 3); e != nil {
		t.Fatalf("stack with a pending member was enqueued: %+v", e)
	}
	var waiting bool
	for _, c := range mock.CallsTo("SetMQStatus") {
		st := c.Args[3].(forge.MQStatus)
		if c.Args[2] == "sha3" && st.State == pg.CheckStatePending && strings.Contains(st.Description, "#2") {
			waiting = true
		}
	}
	if !waiting {
		t.Fatalf("no waiting status naming member #2: %+v", mock.CallsTo("SetMQStatus"))
	}
	if n := len(mock.CallsTo("RemoveLabel")); n != 0 {
		t.Fatal("merge intent must be kept while waiting")
	}

	pending = false
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 3); e == nil {
		t.Fatal("stack not enqueued once members are green")
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

// The priority label decides which queued PR starts first and is re-read on
// every poll, so adding it just after the merge label still counts.
func TestPriorityLabel(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	deps.PriorityLabel = "urgent"
	labels := map[int64][]string{7: {"merge-queue"}, 8: {"merge-queue", "urgent"}}
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{
			{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: labels[7]},
			{Number: 8, State: "open", HeadSHA: "h8", BaseBranch: "main", Labels: labels[8]},
		}, nil
	}
	mock.CreateMergeBranchFn = func(context.Context, string, string, string, string, string) (string, bool, error) {
		return "m", false, nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 8); e == nil || e.State != pg.EntryStateTesting || !e.Priority {
		t.Fatalf("priority PR #8 should be testing: %+v", e)
	}

	labels[7] = append(labels[7], "urgent")
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 7); e == nil || !e.Priority {
		t.Fatalf("label added while queued should promote PR #7: %+v", e)
	}
}

func TestMergeLabelWithoutMergeRightsIsRemoved(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}}}, nil
	}
	mock.MergeLabelAdderFn = func(context.Context, string, string, int64, string) (string, bool, error) {
		return "mallory", false, nil
	}

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}

	if e, _ := svc.GetEntry(ctx, repoID, 7); e != nil {
		t.Fatalf("unauthorized label must not enqueue: %+v", e)
	}
	if rm := mock.CallsTo("RemoveLabel"); len(rm) != 1 || rm[0].Args[3] != "merge-queue" {
		t.Fatalf("RemoveLabel calls = %+v", rm)
	}
	if c := mock.CallsTo("Comment"); len(c) != 1 || !strings.Contains(c[0].Args[3].(string), "@mallory") {
		t.Fatalf("Comment calls = %+v", c)
	}
}

// A failed permission lookup must not strip a maintainer's label.
func TestMergeLabelLookupErrorKeepsLabel(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
		return []forge.PR{{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}}}, nil
	}
	mock.MergeLabelAdderFn = func(context.Context, string, string, int64, string) (string, bool, error) {
		return "", false, errors.New("boom")
	}

	res, err := poller.PollOnce(ctx, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("expected lookup error in result")
	}
	if e, _ := svc.GetEntry(ctx, repoID, 7); e != nil {
		t.Fatalf("must not enqueue on lookup error: %+v", e)
	}
	if n := len(mock.CallsTo("RemoveLabel")); n != 0 {
		t.Fatalf("RemoveLabel called %d times", n)
	}
}
