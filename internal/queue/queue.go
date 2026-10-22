// Package queue implements merge queue operations on top of the PostgreSQL store.
// All state lives in the database — there is no in-memory copy.
// Multi-step operations use transactions to prevent races.
package queue

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnqueueResult describes what happened when enqueueing a PR.
type EnqueueResult struct {
	Position int64 // 1-based position in queue
	IsNew    bool
	Entry    pg.QueueEntry
}

// Service provides merge queue operations backed by the database.
type Service struct {
	pool *pgxpool.Pool
}

// NewService creates a new queue service.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// queries returns a non-transactional Queries handle for single-statement operations.
func (s *Service) queries() *pg.Queries {
	return pg.New(s.pool)
}

// withTx runs fn inside a serializable transaction.
// Serializable isolation prevents phantom reads and ensures multi-step
// operations see a consistent snapshot.
func (s *Service) withTx(ctx context.Context, fn func(q *pg.Queries) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.Serializable,
	})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(pg.New(tx)); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Enqueue adds a PR to its repo+branch queue, ahead of normal PRs if priority.
// If the PR is already queued, it is a no-op and returns the existing position.
// Runs in a transaction so the insert and position count are atomic.
func (s *Service) Enqueue(ctx context.Context, repoID, prNumber int64, prHeadSHA, targetBranch string, priority bool) (*EnqueueResult, error) {
	var result EnqueueResult

	err := s.withTx(ctx, func(q *pg.Queries) error {
		entry, insertErr := q.EnqueuePR(ctx, pg.EnqueuePRParams{
			RepoID:       repoID,
			PrNumber:     prNumber,
			PrHeadSha:    prHeadSHA,
			TargetBranch: targetBranch,
			Priority:     priority,
		})
		result.IsNew = insertErr == nil
		if !result.IsNew {
			// ON CONFLICT DO NOTHING → pgx returns no rows; the PR is already queued.
			var getErr error
			entry, getErr = q.GetQueueEntry(ctx, pg.GetQueueEntryParams{RepoID: repoID, PrNumber: prNumber})
			if getErr != nil {
				return fmt.Errorf("enqueue PR #%d: insert failed (%w), and lookup failed (%w)", prNumber, insertErr, getErr)
			}
		}
		result.Entry = entry

		pos, err := q.CountQueuePosition(ctx, pg.CountQueuePositionParams{
			RepoID:       repoID,
			TargetBranch: entry.TargetBranch,
			PrNumber:     prNumber,
		})
		if err != nil {
			return fmt.Errorf("count position for PR #%d: %w", prNumber, err)
		}
		result.Position = pos

		return nil
	})
	if err != nil {
		return nil, err
	}

	if result.IsNew {
		slog.Info("enqueued PR", "pr", prNumber, "position", result.Position)
	} else {
		slog.Debug("PR already in queue", "pr", prNumber, "position", result.Position)
	}

	return &result, nil
}

// Dequeue removes a PR from the queue; a missing PR is not an error.
func (s *Service) Dequeue(ctx context.Context, repoID, prNumber int64) error {
	if err := s.queries().DequeuePR(ctx, pg.DequeuePRParams{RepoID: repoID, PrNumber: prNumber}); err != nil {
		return fmt.Errorf("dequeue PR #%d: %w", prNumber, err)
	}
	slog.Info("dequeued PR", "pr", prNumber)
	return nil
}

// Head returns the head-of-queue entry for a (repo, branch), or nil if empty.
func (s *Service) Head(ctx context.Context, repoID int64, targetBranch string) (*pg.QueueEntry, error) {
	entry, err := s.queries().GetHeadOfQueue(ctx, pg.GetHeadOfQueueParams{
		RepoID:       repoID,
		TargetBranch: targetBranch,
	})
	if err != nil {
		return nil, nil //nolint:nilerr // empty queue is not an error
	}

	return &entry, nil
}

// Position returns the PR's 1-based position in its target branch queue, or 0
// when the PR is not queued.
func (s *Service) Position(ctx context.Context, repoID int64, targetBranch string, prNumber int64) (int64, error) {
	return s.queries().CountQueuePosition(ctx, pg.CountQueuePositionParams{
		RepoID:       repoID,
		TargetBranch: targetBranch,
		PrNumber:     prNumber,
	})
}

// SetPriority re-ranks a still-queued entry; in-flight entries are left alone.
func (s *Service) SetPriority(ctx context.Context, repoID, prNumber int64, priority bool) error {
	return s.queries().SetEntryPriority(ctx, pg.SetEntryPriorityParams{
		RepoID:   repoID,
		PrNumber: prNumber,
		Priority: priority,
	})
}

// UpdateState transitions a queue entry to a new state.
func (s *Service) UpdateState(ctx context.Context, repoID, prNumber int64, state pg.EntryState) error {
	return s.queries().UpdateEntryState(ctx, pg.UpdateEntryStateParams{
		RepoID:   repoID,
		PrNumber: prNumber,
		State:    state,
	})
}

// SetMergeBranch records the merge branch name and SHA for an entry.
func (s *Service) SetMergeBranch(ctx context.Context, repoID, prNumber int64, branchName, branchSHA string) error {
	return s.queries().UpdateEntryMergeBranch(ctx, pg.UpdateEntryMergeBranchParams{
		RepoID:          repoID,
		PrNumber:        prNumber,
		MergeBranchName: pgtype.Text{String: branchName, Valid: true},
		MergeBranchSha:  pgtype.Text{String: branchSHA, Valid: true},
	})
}

// SetError records an error message on an entry.
func (s *Service) SetError(ctx context.Context, repoID, prNumber int64, msg string) error {
	return s.queries().UpdateEntryError(ctx, pg.UpdateEntryErrorParams{
		RepoID:       repoID,
		PrNumber:     prNumber,
		ErrorMessage: pgtype.Text{String: msg, Valid: true},
	})
}

// GetEntry returns a queue entry by repo and PR number, or nil if not found.
func (s *Service) GetEntry(ctx context.Context, repoID, prNumber int64) (*pg.QueueEntry, error) {
	entry, err := s.queries().GetQueueEntry(ctx, pg.GetQueueEntryParams{
		RepoID:   repoID,
		PrNumber: prNumber,
	})
	if err != nil {
		return nil, nil //nolint:nilerr // not-found is not an error
	}

	return &entry, nil
}

// SaveCheckStatus records or updates a check status for an entry.
func (s *Service) SaveCheckStatus(ctx context.Context, entryID int64, checkContext string, state pg.CheckState, targetURL string) error {
	return s.queries().SaveCheckStatus(ctx, pg.SaveCheckStatusParams{
		QueueEntryID: entryID,
		Context:      checkContext,
		State:        state,
		TargetUrl:    targetURL,
	})
}

// GetCheckStatuses returns all check statuses for a queue entry.
func (s *Service) GetCheckStatuses(ctx context.Context, entryID int64) ([]pg.CheckStatus, error) {
	return s.queries().GetCheckStatuses(ctx, entryID)
}

// ListActiveEntries returns all entries for a repo in queue order.
func (s *Service) ListActiveEntries(ctx context.Context, repoID int64) ([]pg.QueueEntry, error) {
	return s.queries().ListActiveEntriesByRepo(ctx, repoID)
}

// GetOrCreateRepo ensures a repo row exists and returns it.
func (s *Service) GetOrCreateRepo(ctx context.Context, forge, owner, name string) (pg.Repo, error) {
	return s.queries().GetOrCreateRepo(ctx, pg.GetOrCreateRepoParams{
		Forge: forge,
		Owner: owner,
		Name:  name,
	})
}

// DequeueAll removes all queue entries for a repo. Used when a repo is
// removed from the registry to avoid leaving orphaned entries in the DB.
func (s *Service) DequeueAll(ctx context.Context, repoID int64) error {
	return s.queries().DequeueAllByRepo(ctx, repoID)
}
