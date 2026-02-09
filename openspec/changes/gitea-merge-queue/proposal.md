## Why

Gitea lacks a built-in merge queue. Without one, maintainers must manually serialize PR merges to avoid broken main branches — PRs that passed CI individually can conflict or break when merged together. A merge queue rebases each PR onto the latest target branch, runs checks, and merges only if they pass, ensuring main stays green.

## What Changes

- New Go service that manages per-repository merge queues
- Integrates with Gitea's existing **"Merge when checks succeed"** (automerge) feature — no bot commands needed
- The service registers `gitea-mq` as a required status check on branch protection; this gates Gitea's automerge
- Discovers PRs entering automerge state by polling the Gitea API (PR timeline comments of type `pull_scheduled_merge` / `pull_cancel_scheduled_merge`)
- For the head-of-queue PR, pushes a temporary merge branch (e.g. `mq/<pr-number>`) merging the PR into the latest target branch, and CI runs on that branch
- When all required checks pass on the merge branch, sets the `gitea-mq` commit status to `success` on the PR's head commit — Gitea's automerge then performs the actual merge
- On failure (check failure, timeout, merge conflict): sets `gitea-mq` to `failure`, cancels the PR's automerge via `DELETE /repos/{owner}/{repo}/pulls/{index}/merge`, and posts a comment explaining why
- Enforces a configurable check timeout (default: 1 hour) — PRs exceeding it are removed from the queue
- Removes PRs that develop merge conflicts, with a notification comment
- Required checks for the merge branch sourced from Gitea branch protection settings (excluding `gitea-mq` itself), with env var fallback
- Minimal server-rendered HTML dashboard showing queue state and check progress per PR
- Multi-repo support: single instance manages queues for multiple repositories
- PostgreSQL for state persistence
- Packaged as a NixOS module with systemd service

## Capabilities

### New Capabilities

- `queue-management`: Core merge queue logic — enqueue via automerge detection, dequeue via automerge cancellation, FIFO ordering, concurrency (one PR at a time per repo), temporary merge branch for testing, signal Gitea to merge on success, cancel automerge on failure, conflict detection and removal, auto-advance on failure/timeout
- `automerge-integration`: Polling Gitea API to discover PRs with automerge scheduled (via timeline comments), detecting automerge cancellation, cancelling automerge on failure, setting `gitea-mq` commit status to gate Gitea's merge
- `check-monitoring`: Tracking commit status checks on the merge branch, resolving required checks from branch protection (excluding `gitea-mq`) or config fallback, enforcing check timeout, triggering next-in-queue on completion or timeout
- `webhook-receiver`: HTTP endpoint receiving Gitea webhooks (commit_status events), signature validation, event routing to check-monitoring
- `web-dashboard`: Minimal HTML dashboard showing per-repo queue contents, current PR check status and history, auto-refresh

### Modified Capabilities

_None — greenfield project._

## Development Approach

- **Test-Driven Development (TDD)**: Write failing tests first, then implement to make them pass
- Core queue logic, automerge detection, webhook signature validation, check timeout logic — all test-first
- Use Go's `testing` package and `httptest` for HTTP handlers
- Integration tests against a real PostgreSQL instance (use testcontainers or a test DB)
- Test Gitea API interactions via interface + mock implementations

## Impact

- **New Go module**: Entire codebase is new (`go.mod`, handlers, queue engine, DB layer, templates)
- **Gitea API dependency**: Requires a Gitea API token with repo read/write permissions (for commit status, branch protection, merge cancellation, comments)
- **Gitea branch protection**: The service expects `gitea-mq` to be added as a required status check — can be done manually or the service could ensure it via the branch protection API on startup
- **Gitea webhook configuration**: Each managed repo needs a webhook for `status` events pointed at this service
- **PostgreSQL**: Requires a PostgreSQL database for queue state
- **Network**: Service exposes an HTTP port for webhooks and the web dashboard
- **NixOS**: New NixOS module and systemd service definition
