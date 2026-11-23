package poller_test

import (
	"context"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// TestEnqueueRequiresConfiguredHeadChecks pins the gate that keeps a PR out of
// the queue until every configured head context is green, even when branch
// protection lists no checks and the PR carries no statuses at all.
func TestEnqueueRequiresConfiguredHeadChecks(t *testing.T) {
	cases := []struct {
		name     string
		required []string
		checks   map[string]forge.Check
		want     bool
	}{
		{
			name:     "no statuses at all",
			required: []string{"presubmit"},
			checks:   nil,
			want:     false,
		},
		{
			name:     "configured context red",
			required: []string{"presubmit"},
			checks:   map[string]forge.Check{"presubmit": {State: pg.CheckStateFailure}},
			want:     false,
		},
		{
			name:     "configured context pending",
			required: []string{"presubmit"},
			checks:   map[string]forge.Check{"presubmit": {State: pg.CheckStatePending}},
			want:     false,
		},
		{
			name:     "configured context green",
			required: []string{"presubmit"},
			checks:   map[string]forge.Check{"presubmit": {State: pg.CheckStateSuccess}},
			want:     true,
		},
		{
			name:     "gate disabled",
			required: nil,
			checks:   nil,
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, mock, svc, ctx, repoID := setupLabelTest(t)
			deps.RequiredHeadChecks = tc.required

			mock.ListOpenPRsFn = func(context.Context, string, string) ([]forge.PR, error) {
				return []forge.PR{{
					Number: 7, State: "open", HeadSHA: "h7", BaseBranch: "main",
					Labels: []string{"merge-queue"},
				}}, nil
			}
			mock.GetCheckStatesFn = func(context.Context, string, string, string) (map[string]forge.Check, error) {
				return tc.checks, nil
			}
			mock.CreateMergeBranchFn = func(context.Context, string, string, string, string, string) (string, bool, error) {
				return "m7", false, nil
			}

			if _, err := poller.PollOnce(ctx, deps); err != nil {
				t.Fatal(err)
			}

			entry, err := svc.GetEntry(ctx, repoID, 7)
			if err != nil {
				t.Fatal(err)
			}
			if got := entry != nil; got != tc.want {
				t.Fatalf("enqueued = %v, want %v (entry: %+v)", got, tc.want, entry)
			}
		})
	}
}
