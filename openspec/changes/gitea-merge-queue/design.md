## Context

Gitea has no built-in merge queue. It does have an "automerge" feature ("Merge when checks succeed") that merges a PR once all required status checks pass. We exploit this by adding `gitea-mq` as a required status check — our service controls when that check goes green, effectively serializing merges through a queue.

The service is a standalone Go binary that connects to Gitea via its REST API and webhooks. It manages one queue per repository. State lives in PostgreSQL. The web dashboard and webhook receiver share a single HTTP server.

Gitea does not expose automerge scheduling via webhook events or the PR API response. The only observable signal is timeline comments of type `pull_scheduled_merge` (34) and `pull_cancel_scheduled_merge` (35), which we discover by polling.

## Goals / Non-Goals

**Goals:**

- Serialize PR merges per-repo so main stays green
- Zero new UX — reuse Gitea's existing "Merge when checks succeed" button
- Survive restarts without losing queue state
- Simple deployment: single binary, env var config, NixOS module

**Non-Goals:**

- Batch/speculative merge testing (GitHub-style merge groups) — we test one PR at a time
- Bot commands — Gitea's UI is the interface
- Modifying Gitea's source code — we integrate purely via API and webhooks
- Supporting Gitea instances older than v1.22 (need `pull_scheduled_merge` comment types)
- High availability / multi-instance — single instance per deployment

## Decisions

### 1. Automerge-driven trigger instead of bot commands

**Decision:** Use Gitea's automerge as the trigger mechanism. Add `gitea-mq` as a required status check in branch protection. When a user clicks "Merge when checks succeed", our service detects it and enqueues the PR. No bot commands.

**Why:** Eliminates custom UX entirely. Users already know the Gitea merge UI. Fewer moving parts — no comment parsing, no bot user account needed for command handling.

**Alternative considered:** `@bot merge` commands in PR comments. Rejected because it adds a parallel interface to Gitea's existing one, requires a dedicated bot user for comment parsing, and the automerge approach is more native.

### 2. Polling for automerge state discovery

**Decision:** Poll the Gitea API on a configurable interval (default 30s) to discover PRs with automerge scheduled. Check each repo's open PRs and inspect their timeline for `pull_scheduled_merge` / `pull_cancel_scheduled_merge` comments.

**Why:** Gitea does not send webhooks when automerge is scheduled or cancelled. The automerge state is not included in the PR API response. Timeline comments are the only observable signal.

**Alternative considered:** Webhook on `issue_comment` events to detect automerge comments. Rejected because `CreateAutoMergeComment` in Gitea goes through the model layer (`models/issues/comment.go:CreateComment`), not through the notification service — so no webhook fires for these comment types.

**Optimization:** On each poll, fetch only open PRs and cache the last-seen timeline comment ID per PR to avoid re-scanning entire timelines.

### 3. Webhooks for commit status events (not polling)

**Decision:** Use Gitea webhooks (`status` event type) to receive commit status updates in real-time. This drives check monitoring — when CI posts a status on the merge branch, we get notified immediately.

**Why:** Status events are latency-sensitive. Polling would add up to 30s delay between a check completing and us noticing. Webhooks give us near-instant response.

**Design:** The webhook endpoint also serves as a liveness signal — if we stop receiving webhooks, something is wrong. The poller acts as a fallback reconciliation mechanism.

### 4. Merge branch naming: `mq/<pr-number>`

**Decision:** Name temporary merge branches `mq/<pr-number>` within each repository.

**Why:** The branch exists in the PR's own repo, so the repo name is redundant. Short, predictable names make it easy to identify merge queue branches and clean up stale ones.

### 5. Gitea performs the actual merge

**Decision:** Our service never calls the merge API. Instead, it sets `gitea-mq` commit status to `success` on the PR's head commit. Gitea's automerge detects all required checks pass and performs the merge itself.

**Why:** Gitea's automerge already handles merge commit creation, branch deletion, notifications, and all the edge cases. Duplicating that logic would be fragile. We just control the gate.

**Consequence:** After setting `gitea-mq` to success, the PR stays as head-of-queue in `success` state. We do NOT advance until the poller confirms the PR is actually merged (PR state = merged). This avoids a race where Gitea's automerge fails (e.g. review dismissed, branch protection changed) and we've already moved on. If the PR stays open for too long after success, we assume automerge failed, cancel it, and advance.

### 6. Failure handling: cancel automerge + comment

**Decision:** On any failure (check failure, timeout, merge conflict), the service: (1) sets `gitea-mq` to `failure`/`error`, (2) cancels automerge via `DELETE /repos/{owner}/{repo}/pulls/{index}/merge`, (3) posts an explanatory comment on the PR.

**Why:** Cancelling automerge is critical — without it, Gitea would merge the PR as soon as someone re-runs CI and it passes (since the automerge schedule persists). The comment tells the user what happened so they can fix and re-queue.

### 7. PostgreSQL for state persistence

**Decision:** Use PostgreSQL with embedded SQL migrations (auto-run on startup).

**Why:** PostgreSQL is already commonly deployed alongside Gitea. Embedded migrations make deployment simpler — no separate migration step.

**Schema (conceptual):**

```
repos
  id, owner, name, created_at

queue_entries
  id, repo_id, pr_number, pr_head_sha, target_branch,
  state (queued|testing|success|failed|cancelled),
  enqueued_at, testing_started_at, completed_at,
  merge_branch_name, merge_branch_sha,
  error_message

check_statuses
  id, queue_entry_id, context, state, updated_at
```

`queue_entries` ordered by `enqueued_at` gives FIFO. `state` drives the queue processing loop. `check_statuses` tracks individual CI check results for the merge branch.

Queues are keyed by `(repo_id, target_branch)` — PRs targeting different branches get independent queues and can be tested concurrently.

### 8. Environment variable configuration

**Decision:** All configuration via environment variables, no config file.

**Variables:**

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITEA_MQ_GITEA_URL` | yes | — | Gitea instance URL |
| `GITEA_MQ_GITEA_TOKEN` | yes | — | API token with repo scope |
| `GITEA_MQ_REPOS` | yes | — | Comma-separated `owner/repo` list |
| `GITEA_MQ_DATABASE_URL` | yes | — | PostgreSQL connection string |
| `GITEA_MQ_WEBHOOK_SECRET` | yes | — | Shared secret for webhook HMAC |
| `GITEA_MQ_LISTEN_ADDR` | no | `:8080` | HTTP listen address |
| `GITEA_MQ_WEBHOOK_PATH` | no | `/webhook` | Webhook endpoint path |
| `GITEA_MQ_POLL_INTERVAL` | no | `30s` | Automerge discovery poll interval |
| `GITEA_MQ_CHECK_TIMEOUT` | no | `1h` | Timeout for required checks |
| `GITEA_MQ_REQUIRED_CHECKS` | no | — | Fallback required check contexts (comma-separated) |
| `GITEA_MQ_REFRESH_INTERVAL` | no | `10s` | Dashboard auto-refresh interval |

### 9. Go project structure

**Decision:** Flat-ish package structure, interfaces for external dependencies.

```
cmd/gitea-mq/main.go          # entrypoint, config parsing, wiring
internal/
  queue/                       # core queue logic (pure, no I/O)
    queue.go                   # Queue type, Enqueue/Dequeue/Advance
    queue_test.go
  gitea/                       # Gitea API client interface + implementation
    client.go                  # interface: GiteaClient
    http.go                    # real HTTP implementation
    mock.go                    # mock for tests
  poller/                      # automerge discovery poller
    poller.go
    poller_test.go
  webhook/                     # webhook HTTP handler
    handler.go
    signature.go               # HMAC validation
    signature_test.go
  monitor/                     # check monitoring logic
    monitor.go
    monitor_test.go
  store/                       # PostgreSQL persistence
    store.go                   # interface: Store
    postgres.go                # implementation
    migrations/                # embedded SQL migrations
      001_initial.sql
  web/                         # dashboard HTTP handlers + templates
    handler.go
    templates/
      overview.html
      repo.html
```

### 10. Gitea API client as interface

**Decision:** Define a `GiteaClient` interface covering the API calls we need. Implement it with a real HTTP client and a mock for tests.

```go
type GiteaClient interface {
    ListOpenPRs(ctx context.Context, owner, repo string) ([]PR, error)
    GetPRTimeline(ctx context.Context, owner, repo string, index int64) ([]TimelineComment, error)
    GetPR(ctx context.Context, owner, repo string, index int64) (*PR, error)
    CreateCommitStatus(ctx context.Context, owner, repo, sha string, status CommitStatus) error
    CreateComment(ctx context.Context, owner, repo string, index int64, body string) error
    CancelAutoMerge(ctx context.Context, owner, repo string, index int64) error
    GetBranchProtection(ctx context.Context, owner, repo, branch string) (*BranchProtection, error)
    CreateBranch(ctx context.Context, owner, repo, name, target string) error
    DeleteBranch(ctx context.Context, owner, repo, name string) error
    MergeBranches(ctx context.Context, owner, repo, base, head string) (*MergeResult, error)
    ListBranchProtections(ctx context.Context, owner, repo string) ([]BranchProtection, error)
    EditBranchProtection(ctx context.Context, owner, repo, name string, opts EditBranchProtectionOpts) error
    ListWebhooks(ctx context.Context, owner, repo string) ([]Webhook, error)
    CreateWebhook(ctx context.Context, owner, repo string, opts CreateWebhookOpts) error
}
```

**Why:** Enables TDD. Core queue logic and monitoring can be tested entirely with mocks. Integration tests use the real client against a test Gitea instance.

### 11. Merge branch creation via API

**Decision:** Create the merge branch using Gitea's API. Specifically: use the API to create a temporary merge of the PR head into the target branch and push it as `mq/<pr-number>`.

**Approach:** Gitea doesn't have a direct "merge two refs into a branch" API. We'll use the repository contents/git API:
1. Get the target branch HEAD SHA
2. Get the PR head SHA
3. Use the merge API or create a merge commit via Git operations
4. Push the result as branch `mq/<pr-number>`

**Fallback:** If the Gitea API doesn't support creating a merge commit directly, we may need to use a local git clone or Gitea's "test merge" mechanism. The `GiteaClient` interface abstracts this — the implementation detail can change without affecting queue logic.

### 12. Timeout enforcement

**Decision:** Use a background goroutine per repo that checks `testing_started_at` against the configured timeout on each poll cycle. This piggybacks on the existing poll loop rather than using per-PR timers.

**Why:** Simpler than managing individual timers. The poll interval (30s default) provides sufficient granularity — a timeout of 1 hour doesn't need second-level precision.

### 13. Dashboard: Go html/template with meta refresh

**Decision:** Server-rendered HTML using Go's `html/template` package. Auto-refresh via `<meta http-equiv="refresh" content="10">`.

**Why:** Zero JavaScript dependencies. Works with JS disabled. Trivial to implement. The dashboard is read-only and informational — no interactivity needed.

## Risks / Trade-offs

**[Polling latency for automerge discovery]** → Up to 30s delay between user clicking "Merge when checks succeed" and our service noticing. Mitigation: configurable poll interval. In practice, CI takes minutes anyway — 30s is negligible.

**[Race between setting gitea-mq=success and Gitea's automerge]** → After we set `gitea-mq` to success, Gitea's automerge may fail (review dismissed, branch protection changed, etc.). Mitigation: we don't advance until the poller confirms the PR is actually merged. If it stays open too long, we cancel automerge, set `gitea-mq` to error, and advance. Safe failure mode.

**[Webhook feedback loop]** → Posting `gitea-mq` commit status triggers a `status` webhook back to us. Mitigation: webhook handler ignores events where context = `gitea-mq`.

**[Merge branch CI runs different checks than the PR's own CI]** → CI systems trigger based on branch patterns. The `mq/*` branches need to be included in CI triggers. Mitigation: document this requirement. Most CI configs can be adjusted to include `mq/**` branches.

**[Timeline comment polling is fragile]** → If Gitea changes the comment type numbers or stops creating these comments, discovery breaks. Mitigation: version-pin Gitea compatibility. The comment types (34, 35) have been stable since v1.17.

**[Database connection loss]** → If PostgreSQL becomes unreachable, the service pauses all processing (same pattern as Gitea unavailability) and retries on each poll cycle. No queue advances or state changes without persistence. Systemd auto-restart as last resort.

**[Single instance — no HA]** → If the service goes down, the queue stalls. PRs stay in automerge state but won't progress. Mitigation: systemd auto-restart. PostgreSQL state means we resume cleanly after restart. The `gitea-mq` check stays `pending`, so nothing merges without our service running — safe failure mode.

**[Gitea API rate limits]** → Polling multiple repos with many open PRs could hit rate limits. Mitigation: only fetch open PRs (typically few per repo), cache timeline state, configurable poll interval.

## Open Questions

1. **Merge branch creation mechanism:** Does Gitea's API support creating a merge commit between two refs and pushing it as a branch? Or do we need a local git worktree / bare clone? Need to spike this.

2. **Stale merge branch cleanup:** On startup, should the service scan for orphaned `mq/*` branches (from a previous crash) and delete them?

3. **Branch protection auto-setup:** Should the service automatically add `gitea-mq` to branch protection's required checks on startup, or require manual configuration?
