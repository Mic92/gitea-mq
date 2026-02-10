// Package integration_test contains end-to-end tests that exercise the full
// flow: automerge detection → enqueue → merge branch → check pass →
// gitea-mq success → merged detection → advance.
package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/merge"
	"github.com/jogman/gitea-mq/internal/monitor"
	"github.com/jogman/gitea-mq/internal/poller"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

// TestFullMergeQueueFlow exercises the complete lifecycle:
//
//  1. Poll detects PR #1 with automerge → enqueued
//  2. Head-of-queue gets merge branch created → state=testing
//  3. Webhook delivers passing check → monitor sets gitea-mq=success
//  4. Next poll detects PR is merged → removed from queue
//  5. PR #2 becomes head, gets its own merge branch
func TestFullMergeQueueFlow(t *testing.T) {
	pool := newTestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	repo, err := svc.GetOrCreateRepo(ctx, "org", "app")
	if err != nil {
		t.Fatal(err)
	}

	// Track state that changes through the flow.
	var mu sync.Mutex
	pr1Merged := false
	pr1StatusState := "" // last gitea-mq status set on PR #1

	mock := &gitea.MockClient{
		ListOpenPRsFn: func(_ context.Context, _, _ string) ([]gitea.PR, error) {
			mu.Lock()
			defer mu.Unlock()

			prs := []gitea.PR{
				{Index: 2, State: "open",
					Head: &gitea.PRRef{Sha: "sha2"},
					Base: &gitea.PRRef{Ref: "main"}},
			}
			if !pr1Merged {
				prs = append([]gitea.PR{
					{Index: 1, State: "open",
						Head: &gitea.PRRef{Sha: "sha1"},
						Base: &gitea.PRRef{Ref: "main"}},
				}, prs...)
			}
			return prs, nil
		},
		GetPRFn: func(_ context.Context, _, _ string, index int64) (*gitea.PR, error) {
			mu.Lock()
			defer mu.Unlock()

			if index == 1 && pr1Merged {
				return &gitea.PR{Index: 1, State: "closed", HasMerged: true,
					Head: &gitea.PRRef{Sha: "sha1"},
					Base: &gitea.PRRef{Ref: "main"}}, nil
			}
			return &gitea.PR{Index: index, State: "open",
				Head: &gitea.PRRef{Sha: "sha" + string(rune('0'+index))},
				Base: &gitea.PRRef{Ref: "main"}}, nil
		},
		GetPRTimelineFn: func(_ context.Context, _, _ string, _ int64) ([]gitea.TimelineComment, error) {
			// Both PRs have automerge scheduled.
			return []gitea.TimelineComment{
				{ID: 1, Type: "pull_scheduled_merge", CreatedAt: time.Now()},
			}, nil
		},
		MergeBranchesFn: func(_ context.Context, _, _, _, _ string) (*gitea.MergeResult, error) {
			return &gitea.MergeResult{SHA: "merge-sha-abc"}, nil
		},
		CreateCommitStatusFn: func(_ context.Context, _, _, sha string, status gitea.CommitStatus) error {
			mu.Lock()
			defer mu.Unlock()

			if sha == "sha1" && status.Context == "gitea-mq" {
				pr1StatusState = status.State
			}
			return nil
		},
		GetBranchProtectionFn: func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
			return &gitea.BranchProtection{
				EnableStatusCheck:   true,
				StatusCheckContexts: []string{"ci/build", "gitea-mq"},
			}, nil
		},
	}

	pollerDeps := &poller.Deps{
		Gitea:          mock,
		Queue:          svc,
		RepoID:         repo.ID,
		Owner:          "org",
		Repo:           "app",
		SuccessTimeout: 5 * time.Minute,
	}

	monDeps := &monitor.Deps{
		Gitea:        mock,
		Queue:        svc,
		Owner:        "org",
		Repo:         "app",
		RepoID:       repo.ID,
		CheckTimeout: 1 * time.Hour,
	}

	// --- Step 1: First poll → PRs #1 and #2 enqueued ---
	result, err := poller.PollOnce(ctx, pollerDeps)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Enqueued) != 2 {
		t.Fatalf("expected 2 enqueued, got %d", len(result.Enqueued))
	}

	// Verify queue state.
	head, err := svc.Head(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head == nil || head.PrNumber != 1 {
		t.Fatalf("expected PR #1 as head, got %v", head)
	}

	// --- Step 2: Start testing for head-of-queue ---
	startResult, err := merge.StartTesting(ctx, mock, svc, "org", "app", repo.ID, head)
	if err != nil {
		t.Fatal(err)
	}
	if startResult.Conflict {
		t.Fatal("unexpected merge conflict")
	}

	// Verify state transitioned to testing.
	head, err = svc.Head(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head.State != pg.EntryStateTesting {
		t.Fatalf("expected state=testing, got %s", head.State)
	}

	// --- Step 3: Simulate CI check passing → monitor processes ---
	if err := monitor.ProcessCheckStatus(ctx, monDeps, head, "ci/build", pg.CheckStateSuccess); err != nil {
		t.Fatal(err)
	}

	// Verify gitea-mq was set to success.
	mu.Lock()
	status := pr1StatusState
	mu.Unlock()
	if status != "success" {
		t.Fatalf("expected gitea-mq=success on PR #1, got %q", status)
	}

	// Entry should be in success state (not yet removed — waiting for merge confirmation).
	head, err = svc.Head(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head == nil || head.PrNumber != 1 {
		t.Fatal("expected PR #1 still as head in success state")
	}
	if head.State != pg.EntryStateSuccess {
		t.Fatalf("expected state=success, got %s", head.State)
	}

	// --- Step 4: Mark PR #1 as merged, poll again ---
	mu.Lock()
	pr1Merged = true
	mu.Unlock()

	result, err = poller.PollOnce(ctx, pollerDeps)
	if err != nil {
		t.Fatal(err)
	}

	// PR #1 should be dequeued.
	found := false
	for _, d := range result.Dequeued {
		if d == 1 {
			found = true
		}
	}
	if !found {
		t.Error("expected PR #1 to be dequeued after merge")
	}

	// --- Step 5: PR #2 is now head-of-queue ---
	head, err = svc.Head(ctx, repo.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if head == nil || head.PrNumber != 2 {
		t.Fatalf("expected PR #2 as new head, got %v", head)
	}
}
