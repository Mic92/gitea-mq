## 1. Forge primitives

- [x] 1.1 Add `MergeInto`, `FastForward`, `ClosePR`, `ErrNotFastForward`, `PushDeniedError` to `internal/forge/forge.go`; extend `MockForge`.
- [x] 1.2 GitHub adapter: `MergeInto` (`POST /merges`), `FastForward` (`PATCH git/refs/heads/{b}` `force:false`), `ClosePR` (`PATCH /pulls/{n}`). `ghfake`: ancestry check on `PATCH refs`, `ProtectedRefs`, `hEditPR`. Tests: happy, non-ff, permission-denied, close.
- [x] 1.3 `gitea.HTTPClient.FastForwardRef`: `git init --bare; fetch <sha>; push <sha>:refs/heads/<b>` over HTTPS with token-in-URL. Integration test against real Gitea (`TestFastForwardRef_RealGitea`) confirmed depth=1 is **insufficient** (client-side ff check needs ancestry) → full fetch. Stderr → `NotFastForwardError`/`ProtectedBranchError`.
- [x] 1.4 Gitea adapter: `MergeInto` = `MergeBranches(branch, head, branch)`; `FastForward` → `FastForwardRef`; `ClosePR` → `EditIssueState`. Unit tests for error mapping + delegation.
- [x] 1.5 `flake-fmt`; `go vet ./...`; `golangci-lint run` clean.

## 2. Storage

- [x] 2.1 Migration `004_batches.sql`: `batch_state` enum, `batches` table, `ux_batches_live`, `queue_entries.active_batch_id` FK.
- [x] 2.2 Audit `merge_branch_sha`: `webhook.findEntryForCommit` / `poller.pollMergeBranchChecks` pick the first matching entry; with all `current_ids` carrying the batch SHA the representative is deterministic (lowest `enqueued_at`). `Build` clears `check_statuses` for `current_ids` so a previous build's results cannot decide a rebuild.
- [x] 2.3 sqlc queries: `CreateBatch`, `GetBatch`, `GetLiveBatch`, `ListLiveBatchesByRepo`, `SaveBatch`, `TakeQueuedHead`, `GetEntriesByIDs`, `SetEntryActiveBatch`, `ClearEntryMergeBranch`, `ClearCheckStatuses`, `CancelBatchesByRepo`. Regen.
- [x] 2.4 `internal/queue/batch.go`: `FormBatch`, `GetLiveBatch`, `GetBatch`, `ListLiveBatches`, `SaveBatch` (nil→`{}` for NOT NULL arrays), `GetEntriesByIDs` (preserves order), `ClearMergeBranch`, `ClearCheckStatuses`, `CancelLiveBatches`.

## 3. Batch engine (`internal/batch`)

- [x] 3.1 `Build`: D3. Tests: `TestFormAndBuild_Green`, `TestBuild_ConflictEjectsAndContinues`.
- [x] 3.2 `HandlePass` + `ensureMergedOrClose`: D4. Tests: `TestFormAndBuild_Green` (merged → no close), `TestEnsureMergedOrClose_FallsBackToClose`, `TestHandlePass_NotFastForward_RetryThenCap`, `TestHandlePass_PushDenied`.
- [x] 3.3 `HandleFail` + `next`: D5. Tests: `TestBisect_CulpritAt2` (rebuild on top of landed half + 8-status discipline), `TestBisect_BothHalvesPass_Flaky`, `TestBisectMaxSteps`, `TestPendingDrop`.
- [x] 3.4 `OnMemberRemoved`: D6. Test: `TestOnMemberRemoved` (pending-only → no rebuild; current → rebuild).
- [x] 3.5 `ReconcileLive` + `LiveBranchNames` (consumed by `merge.CleanupStaleBranches` `spare`).
- [x] 3.6 `TimedOut(b, timeout)` on `b.TestingStartedAt`.
- [x] 3.7 `HandleResult` (`monitor.BatchHandler`): stale-SHA guard. Test: `TestHandleResult_DropsStaleSHA`.

## 4. Wire-up

- [x] 4.1 `internal/config`: `BatchMax` (default 1), `BisectMaxSteps` (default 0); `TestLoad_BatchMax`.
- [x] 4.2 `poller.startQueuedHeads`: `Batch.Enabled()` → `FormAndBuild`; `SkipQueueIfUpToDate` runs first so a singleton up-to-date head takes the legacy path.
- [x] 4.3 `monitor.ProcessCheckStatus`: dispatch to `Deps.Batch.HandleResult` when `entry.ActiveBatchID` set. `BatchHandler` interface defined in `monitor` to avoid an import cycle.
- [x] 4.4 `poller.reconcileEntries`: after a member is removed (push/close/retarget/cancel), call `Batch.OnMemberRemoved`. `handleTestingTimeout` skips batch members; `pollMergeBranchChecks` evaluates `batch.TimedOut`.
- [x] 4.5 `registry.Add`: build `*batch.Engine` when `BatchMax!=1`, pass to monitor/poller, spare live batch branches in `CleanupStaleBranches`, `ReconcileLive`. `Remove`: `CancelLiveBatches`. `cmd/gitea-mq/main.go` plumbs config.

## 5. Setup, cleanup, docs

- [ ] 5.1 GitHub `EnsureRepoSetup` already lists the App as a bypass actor when it creates the ruleset; verifying on every start is deferred.
- [x] 5.2 Orphan-branch reaper: `CleanupStaleBranches` already matches `gitea-mq/*`; live batch branches passed via `spare`.
- [x] 5.3 README: `BATCH_MAX`/`BISECT_MAX_STEPS` rows + "Batching" section (opt-in, merge-commit caveat, Gitea push-whitelist prerequisite, cross-PR caveat, closed-not-merged note).

## 6. Web

- [x] 6.1 Repo page: batch header (`id · branch link · build N · testing C/M`); member rows tagged with bucket. CSS pills.
- [x] 6.2 PR detail page: batch row (id, bucket, co-members, branch link).

## 7. Integration

- [x] 7.1 `TestGithub_BatchFlow_Green` (ghfake): 3 PRs → one batch → webhook → main fast-forwarded to batch SHA, all PRs dequeued + closed (fallback) + `gitea-mq=success`.
- [x] 7.2 `TestFastForwardRef_RealGitea`: real Gitea verifies fetch+push approach and non-ff detection.
- [x] 7.3 Existing `TestFullMergeQueueFlow` / `TestGithub_FullMergeQueueFlow` unchanged → `BATCH_MAX=1` regression-identical.
- [x] 7.4 `TestGitea_BatchFlow_Green`: 2-PR green batch through real Gitea (StackMerges → FastForwardRef → close/merged).
