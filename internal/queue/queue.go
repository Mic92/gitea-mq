// Package queue implements merge queue operations on top of the PostgreSQL store.
// All state lives in the database — there is no in-memory copy.
// Multi-step operations use transactions to prevent races.
package queue

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

// EnqueueResult describes what happened when enqueueing a PR.
type EnqueueResult struct {
	Position int64 // 1-based position in queue
	IsNew    bool
	Entry    pg.QueueEntry
}

// DequeueResult describes what happened when dequeueing a PR.
type DequeueResult struct {
	WasHead bool // true if the removed entry was head-of-queue
	Found   bool // true if the PR was in the queue
	Entry   pg.QueueEntry
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

// Enqueue adds a PR to the tail of its repo+branch queue.
// If the PR is already queued, it is a no-op and returns the existing position.
// Runs in a transaction so the insert and position count are atomic.
func (s *Service) Enqueue(ctx context.Context, repoID, prNumber int64, prHeadSHA, targetBranch string) (*EnqueueResult, error) {
	var result EnqueueResult

	err := s.withTx(ctx, func(q *pg.Queries) error {
		entry, insertErr := q.EnqueuePR(ctx, pg.EnqueuePRParams{
			RepoID:       repoID,
			PrNumber:     prNumber,
			PrHeadSha:    prHeadSHA,
			TargetBranch: targetBranch,
		})
		if insertErr != nil {
			// ON CONFLICT DO NOTHING → pgx returns no rows.
			// The PR already exists; look it up.
			existing, getErr := q.GetQueueEntry(ctx, pg.GetQueueEntryParams{
				RepoID:   repoID,
				PrNumber: prNumber,
			})
			if getErr != nil {
				return fmt.Errorf("enqueue PR #%d: insert failed (%w), and lookup failed (%w)", prNumber, insertErr, getErr)
			}

			pos, posErr := q.CountQueuePosition(ctx, pg.CountQueuePositionParams{
				RepoID:       repoID,
				TargetBranch: existing.TargetBranch,
				PrNumber:     prNumber,
			})
			if posErr != nil {
				return fmt.Errorf("count position for PR #%d: %w", prNumber, posErr)
			}

			result = EnqueueResult{Position: pos, IsNew: false, Entry: existing}

			return nil
		}

		pos, posErr := q.CountQueuePosition(ctx, pg.CountQueuePositionParams{
			RepoID:       repoID,
			TargetBranch: targetBranch,
			PrNumber:     prNumber,
		})
		if posErr != nil {
			return fmt.Errorf("count position for PR #%d: %w", prNumber, posErr)
		}

		result = EnqueueResult{Position: pos, IsNew: true, Entry: entry}

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

// Dequeue removes a PR from the queue.
// Returns whether it was found and whether it was head-of-queue.
// Runs in a transaction so the head check and delete are atomic.
func (s *Service) Dequeue(ctx context.Context, repoID, prNumber int64) (*DequeueResult, error) {
	var result DequeueResult

	err := s.withTx(ctx, func(q *pg.Queries) error {
		entry, getErr := q.GetQueueEntry(ctx, pg.GetQueueEntryParams{
			RepoID:   repoID,
			PrNumber: prNumber,
		})
		if getErr != nil {
			// Not found.
			result = DequeueResult{Found: false}
			return nil
		}

		head, headErr := q.GetHeadOfQueue(ctx, pg.GetHeadOfQueueParams{
			RepoID:       repoID,
			TargetBranch: entry.TargetBranch,
		})
		wasHead := headErr == nil && head.PrNumber == prNumber

		if err := q.DequeuePR(ctx, pg.DequeuePRParams{
			RepoID:   repoID,
			PrNumber: prNumber,
		}); err != nil {
			return fmt.Errorf("dequeue PR #%d: %w", prNumber, err)
		}

		result = DequeueResult{WasHead: wasHead, Found: true, Entry: entry}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if result.Found {
		slog.Info("dequeued PR", "pr", prNumber, "was_head", result.WasHead)
	}

	return &result, nil
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

// Advance removes the head-of-queue and returns the new head (or nil).
// Runs in a transaction so the delete and new-head lookup are atomic.
func (s *Service) Advance(ctx context.Context, repoID int64, targetBranch string) (*pg.QueueEntry, error) {
	var newHead *pg.QueueEntry

	err := s.withTx(ctx, func(q *pg.Queries) error {
		head, headErr := q.GetHeadOfQueue(ctx, pg.GetHeadOfQueueParams{
			RepoID:       repoID,
			TargetBranch: targetBranch,
		})
		if headErr != nil {
			// Empty queue.
			return nil
		}

		if err := q.DequeuePR(ctx, pg.DequeuePRParams{
			RepoID:   repoID,
			PrNumber: head.PrNumber,
		}); err != nil {
			return fmt.Errorf("advance queue: %w", err)
		}

		next, nextErr := q.GetHeadOfQueue(ctx, pg.GetHeadOfQueueParams{
			RepoID:       repoID,
			TargetBranch: targetBranch,
		})
		if nextErr != nil {
			// Queue is now empty.
			return nil
		}

		newHead = &next

		return nil
	})
	if err != nil {
		return nil, err
	}

	return newHead, nil
}

// List returns all entries in a (repo, branch) queue in FIFO order.
func (s *Service) List(ctx context.Context, repoID int64, targetBranch string) ([]pg.QueueEntry, error) {
	entries, err := s.queries().ListQueue(ctx, pg.ListQueueParams{
		RepoID:       repoID,
		TargetBranch: targetBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("list queue: %w", err)
	}

	return entries, nil
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
func (s *Service) SaveCheckStatus(ctx context.Context, entryID int64, checkContext string, state pg.CheckState) error {
	return s.queries().SaveCheckStatus(ctx, pg.SaveCheckStatusParams{
		QueueEntryID: entryID,
		Context:      checkContext,
		State:        state,
	})
}

// GetCheckStatuses returns all check statuses for a queue entry.
func (s *Service) GetCheckStatuses(ctx context.Context, entryID int64) ([]pg.CheckStatus, error) {
	return s.queries().GetCheckStatuses(ctx, entryID)
}

// GetOrCreateRepo ensures a repo row exists and returns it.
func (s *Service) GetOrCreateRepo(ctx context.Context, owner, name string) (pg.Repo, error) {
	return s.queries().GetOrCreateRepo(ctx, pg.GetOrCreateRepoParams{
		Owner: owner,
		Name:  name,
	})
}

// LoadActiveQueues returns all non-terminal entries across all repos for startup recovery.
func (s *Service) LoadActiveQueues(ctx context.Context) ([]pg.LoadActiveQueuesRow, error) {
	return s.queries().LoadActiveQueues(ctx)
}
