package poller_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/batch"
	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/queue"
)

func setupStateLabelTest(t *testing.T) (*poller.Deps, *forge.MockForge, *queue.Service, context.Context, int64) {
	t.Helper()
	deps, mock, svc, ctx, repoID := setupLabelTest(t)
	deps.LabelPrefix = "mq/"
	return deps, mock, svc, ctx, repoID
}

func openPRs(prs ...forge.PR) func(context.Context, string, string) ([]forge.PR, error) {
	return func(context.Context, string, string) ([]forge.PR, error) { return prs, nil }
}

// applied returns "number +add -remove" for every ApplyLabels call.
func applied(mock *forge.MockForge) []string {
	var out []string
	for _, c := range mock.CallsTo("ApplyLabels") {
		out = append(out, fmt.Sprintf("%d +%s -%s", c.Args[2], strings.Join(c.Args[3].([]string), ","), strings.Join(c.Args[4].([]string), ",")))
	}
	return out
}

func requireApplied(t *testing.T, mock *forge.MockForge, want ...string) {
	t.Helper()
	if got := applied(mock); !slices.Equal(got, want) {
		t.Fatalf("ApplyLabels calls = %q, want %q", got, want)
	}
}

func TestStateLabelWaitingWhenOwnCIPending(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) { return []string{"ci"}, nil }
	mock.ListOpenPRsFn = openPRs(forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}})

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock, "7 +mq/waiting -")
}

func TestStateLabelNoneWithoutMergeIntent(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.ListOpenPRsFn = openPRs(forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main"})

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock)
}

func TestStateLabelDisabledWithoutPrefix(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	deps.LabelPrefix = ""
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) { return []string{"ci"}, nil }
	mock.ListOpenPRsFn = openPRs(forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}})

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock)
}

func TestStateLabelTestingThenSteadyStateIsFree(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.CreateMergeBranchFn = func(context.Context, string, string, string, string, string) (string, bool, error) {
		return "m7", false, nil
	}
	pr := forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}}
	mock.ListOpenPRsFn = openPRs(pr)

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock, "7 +mq/testing -")

	// The forge now reports the label we set: nothing left to do.
	pr.Labels = []string{"merge-queue", "mq/testing"}
	mock.ListOpenPRsFn = openPRs(pr)
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock, "7 +mq/testing -")
}

func TestStateLabelSwapsStaleStateLabel(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) { return []string{"ci"}, nil }
	mock.ListOpenPRsFn = openPRs(forge.PR{
		Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main",
		Labels: []string{"merge-queue", "mq/queued", "bug"},
	})

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock, "7 +mq/waiting -mq/queued")
}

func TestStateLabelRemovedWhenMergeIntentWithdrawn(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.ListOpenPRsFn = openPRs(forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"mq/waiting"}})

	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock, "7 + -mq/waiting")
}

func TestStateLabelClearedWhenPRMerged(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) { return []string{"ci"}, nil }
	mock.ListOpenPRsFn = openPRs(forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}})
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}

	// Merged PRs vanish from the open list, so the poll never sees their labels.
	mock.ListOpenPRsFn = openPRs()
	mock.GetPRFn = func(context.Context, string, string, int64) (*forge.PR, error) {
		return &forge.PR{Number: 7, State: "closed", Merged: true, Labels: []string{"merge-queue", "mq/waiting"}}, nil
	}
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock, "7 +mq/waiting -", "7 + -mq/waiting")

	// Cleanup happens once.
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	requireApplied(t, mock, "7 +mq/waiting -", "7 + -mq/waiting")
}

func TestStateLabelStopsAfterForbidden(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) { return []string{"ci"}, nil }
	mock.ApplyLabelsFn = func(context.Context, string, string, int64, []string, []string) error {
		return forge.ErrLabelsForbidden
	}
	mock.ListOpenPRsFn = openPRs(forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}})

	for range 3 {
		res, err := poller.PollOnce(ctx, deps)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Errors) != 0 {
			t.Fatalf("forbidden labels must not surface as poll errors: %v", res.Errors)
		}
	}
	requireApplied(t, mock, "7 +mq/waiting -")
}

func TestStateLabelTransientErrorRetries(t *testing.T) {
	deps, mock, _, ctx, _ := setupStateLabelTest(t)
	mock.GetRequiredChecksFn = func(context.Context, string, string, string) ([]string, error) { return []string{"ci"}, nil }
	calls := 0
	mock.ApplyLabelsFn = func(context.Context, string, string, int64, []string, []string) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return nil
	}
	mock.ListOpenPRsFn = openPRs(forge.PR{Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main", Labels: []string{"merge-queue"}})

	for range 2 {
		if _, err := poller.PollOnce(ctx, deps); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("ApplyLabels calls = %d, want 2", calls)
	}
}

func TestStateLabelBatchMembersAndLabelCleanup(t *testing.T) {
	deps, mock, svc, ctx, repoID := setupStateLabelTest(t)
	deps.Batch = &batch.Engine{
		Forge: mock, Queue: svc, Owner: "org", Repo: "app", RepoID: repoID, BatchMax: 5,
		SkipIfUpToDate: true, MergedPollInterval: 1, MergedPollAttempts: 1,
	}
	mock.CreateMergeBranchFn = func(context.Context, string, string, string, string, string) (string, bool, error) {
		return "tip", false, nil
	}
	mock.MergeIntoFn = func(context.Context, string, string, string, string) (string, bool, error) {
		return "tip", false, nil
	}
	mock.ListOpenPRsFn = openPRs(
		forge.PR{Number: 1, State: "open", HeadSHA: "h1", BaseBranch: "main", Labels: []string{"merge-queue"}},
		forge.PR{Number: 2, State: "open", HeadSHA: "h2", BaseBranch: "main", Labels: []string{"merge-queue"}},
	)
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	live, err := svc.GetLiveBatch(ctx, repoID, "main")
	if err != nil || live == nil {
		t.Fatalf("live batch = %v, err = %v", live, err)
	}
	want := fmt.Sprintf("mq/batch-%d", live.ID)
	requireApplied(t, mock, "1 +"+want+" -", "2 +"+want+" -")

	// Batch gone (landed/cancelled) while PRs still carry its label.
	if err := svc.CancelLiveBatches(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	mock.ListOpenPRsFn = openPRs(
		forge.PR{Number: 1, State: "open", HeadSHA: "h1", BaseBranch: "main", Labels: []string{want}},
	)
	mock.GetPRFn = func(_ context.Context, _, _ string, n int64) (*forge.PR, error) {
		return &forge.PR{Number: n, State: "closed", Merged: true, HeadSHA: "h2", BaseBranch: "main"}, nil
	}
	if _, err := poller.PollOnce(ctx, deps); err != nil {
		t.Fatal(err)
	}
	del := mock.CallsTo("DeleteLabel")
	if len(del) != 1 || del[0].Args[2] != want {
		t.Fatalf("DeleteLabel calls = %v, want one for %s", del, want)
	}
}
