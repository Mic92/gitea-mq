## 1. Project Scaffold

- [x] 1.1 Initialize Go module (`go mod init`), create directory structure per design (`cmd/gitea-mq/`, `internal/{queue,gitea,poller,webhook,monitor,store,web}/`)
- [x] 1.2 Set up Nix flake with Go build, devShell, and `flake-fmt`
- [x] 1.3 Add PostgreSQL embedded migration infrastructure (embed SQL files, run on startup)
- [x] 1.4 Write `001_initial.sql` migration: `repos`, `queue_entries`, `check_statuses` tables per design schema
- [x] 1.5 Implement env var config parsing (`cmd/gitea-mq/main.go`): all variables from design table, validation, defaults

## 2. Core Queue Logic (TDD)

NOTE: Queue logic and store merged into one `queue.Service` backed directly by PostgreSQL (no in-memory copy). Tests use a real PostgreSQL instance via `TestMain`.

- [x] 2.1 Write tests for `queue.Enqueue`: new PR appended to tail, duplicate is no-op, returns position
- [x] 2.2 Implement `queue.Enqueue`
- [x] 2.3 Write tests for `queue.Dequeue`: remove by PR number, head-of-queue returns cleanup flag, not-found is no-op
- [x] 2.4 Implement `queue.Dequeue`
- [x] 2.5 Write tests for FIFO ordering: multiple enqueues produce correct order, `Head()` returns first-enqueued
- [x] 2.6 Implement `queue.Head` and ordering guarantees
- [x] 2.7 Write tests for `queue.Advance`: removes head, returns new head (or nil if empty)
- [x] 2.8 Implement `queue.Advance`
- [x] 2.9 Write tests for per-repo per-branch queue isolation: operations on repo A / `main` don't affect repo A / `release` or repo B / `main`
- [x] 2.10 Implement `QueueManager` that holds per-repo-per-branch queues

## 3. PostgreSQL Store (TDD)

NOTE: Covered by section 2 — queue.Service operates directly on PostgreSQL via sqlc-generated queries with serializable transactions.

- [x] 3.1 Set up test harness with real PostgreSQL (testcontainers or test DB)
- [x] 3.2 Write tests for `store.EnqueuePR` / `store.DequeuePR` persistence round-trip
- [x] 3.3 Implement `store.EnqueuePR` and `store.DequeuePR`
- [x] 3.4 Write tests for `store.ListQueue`: returns entries ordered by `enqueued_at`
- [x] 3.5 Implement `store.ListQueue`
- [x] 3.6 Write tests for `store.UpdateEntryState`: state transitions (queued→testing→success/failed)
- [x] 3.7 Implement `store.UpdateEntryState`
- [x] 3.8 Write tests for `store.SaveCheckStatus` / `store.GetCheckStatuses`
- [x] 3.9 Implement check status persistence
- [x] 3.10 Write tests for state recovery on startup: load all non-terminal entries, reconstruct queue order
- [x] 3.11 Implement `store.LoadActiveQueues`

## 4. Gitea API Client

- [x] 4.1 Define `GiteaClient` interface per design (all methods)
- [x] 4.2 Implement mock `GiteaClient` for tests (records calls, returns configurable responses)
- [x] 4.3 Implement HTTP `GiteaClient`: `ListOpenPRs`, `GetPR`
- [x] 4.4 Implement HTTP `GiteaClient`: `GetPRTimeline` (parse comment types for automerge detection)
- [x] 4.5 Implement HTTP `GiteaClient`: `CreateCommitStatus`
- [x] 4.6 Implement HTTP `GiteaClient`: `CreateComment`
- [x] 4.7 Implement HTTP `GiteaClient`: `CancelAutoMerge` (`DELETE /repos/{owner}/{repo}/pulls/{index}/merge`)
- [x] 4.8 Implement HTTP `GiteaClient`: `GetBranchProtection` (extract `status_check_contexts`, filter out `gitea-mq`)
- [x] 4.9 Implement HTTP `GiteaClient`: `CreateBranch`, `DeleteBranch`
- [x] 4.10 Spike: determine how to create merge branch via Gitea API (merge two refs → push as `gitea-mq/<pr>`) and implement `MergeBranches`
- [x] 4.11 Implement HTTP `GiteaClient`: `ListBranchProtections`, `EditBranchProtection` (for auto-setup)
- [x] 4.12 Implement HTTP `GiteaClient`: `ListWebhooks`, `CreateWebhook` (for auto-setup)

## 5. Automerge Poller (TDD)

- [x] 5.1 Write tests for automerge detection: PR with `pull_scheduled_merge` as latest → detected, with subsequent `pull_cancel_scheduled_merge` → not detected
- [x] 5.2 Implement `poller.checkAutomergeState` (timeline comment parsing logic)
- [x] 5.3 Write tests for poll cycle: new automerge PR → enqueue + set `gitea-mq` pending; already queued → no-op
- [x] 5.4 Implement `poller.PollOnce` (single poll cycle for one repo)
- [x] 5.5 Write tests for cancellation detection: queued PR loses automerge → dequeue; head-of-queue → dequeue + cleanup
- [x] 5.6 Implement cancellation detection in poll cycle
- [x] 5.7 Write tests for merged PR detection: head-of-queue PR now merged → remove + advance
- [x] 5.8 Implement merged PR detection in poll cycle
- [x] 5.9 Write tests for new push detection: head SHA changed → remove from queue + cancel automerge + comment; head-of-queue → also cleanup merge branch + advance
- [x] 5.10 Implement new push detection in poll cycle (compare stored SHA vs API SHA)
- [x] 5.11 Write tests for success-but-not-merged detection: PR in `success` state but still open after multiple poll cycles → cancel automerge, set error, remove, advance
- [x] 5.12 Implement success-but-not-merged timeout in poll cycle
- [x] 5.13 Write tests for closed PR detection: closed PR in queue → silently remove; head-of-queue → cleanup + advance
- [x] 5.14 Implement closed PR detection in poll cycle
- [x] 5.15 Write tests for target branch change detection: PR retargeted → remove from old queue + cancel automerge + comment
- [x] 5.16 Implement target branch change detection in poll cycle
- [x] 5.17 Write tests for Gitea unavailability: API errors → pause processing, log error; API recovers → resume and reconcile
- [x] 5.18 Implement Gitea unavailability handling (pause/resume)
- [x] 5.19 Implement `poller.Run` (ticker loop, configurable interval, graceful shutdown)

## 6. Check Monitoring (TDD)

- [x] 6.1 Write tests for required check resolution: from branch protection (excluding `gitea-mq`), config fallback, no-config fallback (any success suffices)
- [x] 6.2 Implement `monitor.ResolveRequiredChecks`
- [x] 6.3 Write tests for check evaluation: all required pass → success, any required fail → failure, partial → still waiting
- [x] 6.4 Implement `monitor.EvaluateChecks`
- [x] 6.5 Write tests for check timeout: testing_started_at + timeout exceeded → remove PR, cancel automerge, post comment
- [x] 6.6 Implement `monitor.CheckTimeout`
- [x] 6.7 Write tests for full success flow: all checks pass → set `gitea-mq` success, delete merge branch, PR stays head-of-queue in `success` state (NOT advanced yet)
- [x] 6.8 Implement success handling in monitor (set status, delete merge branch, transition to `success` state)
- [x] 6.9 Write tests for latest-wins semantics: check goes failure → pending → success after retry → treated as success; success → failure → treated as failure
- [x] 6.10 Implement latest-wins status tracking
- [x] 6.11 Write tests for failure flow: check fails → set `gitea-mq` failure, cancel automerge, post comment, delete merge branch, advance
- [x] 6.12 Implement failure handling in monitor

## 7. Webhook Receiver (TDD)

- [x] 7.1 Write tests for HMAC signature validation: valid → accept, missing → 401, invalid → 401
- [x] 7.2 Implement `webhook.ValidateSignature`
- [x] 7.3 Write tests for HTTP handler: POST → 200, GET → 405, bad JSON → 400, missing fields → 400
- [x] 7.4 Implement `webhook.Handler` (HTTP handler)
- [x] 7.5 Write tests for ignoring own status: context `gitea-mq` → 200 but no action
- [x] 7.6 Implement `gitea-mq` context filter in webhook handler
- [x] 7.7 Write tests for idempotent handling: same event delivered twice → no additional effect
- [x] 7.8 Write tests for commit status event routing: merge branch commit → route to monitor, other commit → ignore, unmanaged repo → ignore + log
- [x] 7.9 Implement event parsing and routing
- [x] 7.7 Write integration test: webhook delivers status event → monitor evaluates checks → correct outcome

## 8. Merge Branch Management (TDD)

- [x] 8.1 Write tests for merge branch creation: success → branch created + status updated to testing, conflict → PR removed + automerge cancelled + comment
- [x] 8.2 Implement `queue.StartTesting` (create merge branch, update state)
- [x] 8.3 Write tests for merge branch cleanup: delete branch on success/failure/cancel
- [x] 8.4 Implement `queue.CleanupMergeBranch`
- [x] 8.5 Write tests for stale merge branch detection on startup: orphaned `gitea-mq/*` branches → cleaned up
- [x] 8.6 Implement startup cleanup scan

## 9. Web Dashboard

- [x] 9.1 Create `overview.html` template: list repos, queue sizes, head-of-queue PR per repo
- [x] 9.2 Implement `GET /` handler serving overview page
- [x] 9.3 Create `repo.html` template: queue list with PR number, title, author, position, state; check status table for head-of-queue (✅ ❌ ⏳)
- [x] 9.4 Implement `GET /repo/{owner}/{name}` handler; 404 for unknown repos
- [x] 9.5 Add `<meta http-equiv="refresh">` with configurable interval
- [x] 9.6 Write tests: overview shows correct repo/queue data, detail shows correct PR/check data, unknown repo → 404

## 10. Auto-Setup

- [x] 10.1 Implement `setup.EnsureBranchProtection`: check if `gitea-mq` is in required checks, add it if missing
- [x] 10.2 Implement `setup.EnsureWebhook`: check if webhook exists for this service, create if missing
- [x] 10.3 Write tests for auto-setup: missing check → added, already present → no-op, no branch protection → log warning, missing webhook → created, existing webhook → no-op
- [x] 10.4 Run auto-setup for all managed repos on startup

## 11. Main Entrypoint & Integration

- [x] 11.1 Wire everything in `main.go`: config → store → gitea client → auto-setup → queue manager → poller → monitor → webhook handler → web handlers → HTTP server
- [x] 11.2 Implement graceful shutdown (context cancellation, wait for in-flight operations)
- [x] 11.3 Add structured logging with `slog` throughout all components
- [x] 11.4 Write end-to-end integration test: mock Gitea server, full flow from automerge detection → enqueue → merge branch → check pass → gitea-mq success → merged detection → advance

## 12. NixOS Packaging & Deployment

- [x] 12.1 Add Nix flake package for the Go binary
- [x] 12.2 Create NixOS module: systemd service, env var options, PostgreSQL dependency
- [x] 12.3 Add health check endpoint (`GET /healthz`)
- [x] 12.4 Write NixOS VM integration test: spin up Gitea + PostgreSQL + gitea-mq in a NixOS test VM, verify auto-setup configures branch protection and webhooks, create a PR, schedule automerge, verify the merge queue processes it end-to-end
- [x] 12.5 Document setup: env vars, NixOS module usage (auto-setup handles Gitea config)
