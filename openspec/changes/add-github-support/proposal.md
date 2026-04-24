## Why

GitHub's native merge queue is gated behind paid organization plans, leaving personal and free-tier private repos without a way to serialize merges and keep `main` green. gitea-mq already solves this for Gitea; extending it to GitHub lets one self-hosted instance serve both forges with the same workflow.

## What Changes

- Introduce a `Forge` interface abstracting all repo/PR/status/branch/webhook operations currently performed against Gitea; the existing `internal/gitea` client becomes one implementation.
- Add a GitHub forge implementation backed by a **GitHub App**: JWT → installation access tokens, per-installation REST client, automatic token refresh.
- Repo registry becomes forge-aware: each managed repo is keyed by `(forge, owner, name)` and routed to the correct client.
- GitHub repo discovery via **App installations**: every repo the App is installed on is managed (union with any statically configured GitHub repos).
- GitHub enqueue/dequeue signal via the **"Enable auto-merge"** button: primary path is `pull_request` webhook actions `auto_merge_enabled` / `auto_merge_disabled`, with a periodic reconcile poll per installation (list open PRs with `auto_merge` set) as a fallback for missed/dropped webhooks.
- New webhook endpoint `/webhook/github` validating `X-Hub-Signature-256`; existing Gitea endpoint moves to `/webhook/gitea` with `/webhook` kept as an alias for backward compatibility.
- GitHub auto-setup: on each installation, enable repo auto-merge setting, add `gitea-mq` as a required status check via a repository **ruleset**, and rely on the App's installation webhook (no per-repo webhook creation needed).
- Map Gitea operations to GitHub equivalents: commit statuses → Checks API (`gitea-mq` check run), cancel automerge → GraphQL `disablePullRequestAutoMerge`, merge-branch creation via Git refs API, required checks read from branch protection.
- New configuration: `GITEA_MQ_GITHUB_APP_ID`, `GITEA_MQ_GITHUB_PRIVATE_KEY` (or `_FILE`), `GITEA_MQ_GITHUB_WEBHOOK_SECRET`, optional `GITEA_MQ_GITHUB_REPOS`. Gitea config becomes optional when only GitHub is configured; at least one forge must be configured.
- Dashboard and PR detail page show forge per repo and link to the correct host.
- Scope limited to github.com (no GHES base-URL support in this change).
- Project name and `GITEA_MQ_*` env prefix are kept; no renaming.

## Capabilities

### New Capabilities

- `forge-abstraction`: Forge-agnostic interface for PR, branch, commit-status, comment, branch-protection and webhook operations, plus a forge-aware repo registry that routes calls per repo.
- `github-integration`: GitHub App authentication, installation-based repo discovery, auto-merge webhook handling for enqueue/dequeue, check-run status reporting, auto-merge cancellation, and auto-setup of repo settings/branch protection.

### Modified Capabilities

- `automerge-integration`: Automerge discovery becomes forge-specific — Gitea keeps timeline polling; GitHub uses `pull_request` `auto_merge_*` webhook actions instead of polling. Cancel-automerge and failure comments are dispatched through the forge interface.
- `repo-discovery`: Managed repo set becomes a union across forges; adds GitHub App installation discovery alongside Gitea topic/static discovery. Gitea configuration becomes optional.
- `webhook-receiver`: Separate per-forge endpoints (`/webhook/gitea`, `/webhook/github`) with forge-specific signature validation (`X-Gitea-Signature` vs `X-Hub-Signature-256`) and event routing; `/webhook` retained as Gitea alias.
- `check-monitoring`: `gitea-mq` lifecycle status is reported via the forge interface (Gitea commit status vs GitHub check run); required-check resolution and status `target_url` use the repo's forge.
- `queue-management`: Queue identity becomes `(forge, owner, repo, target-branch)`; merge-branch create/delete and PR-merged detection go through the forge interface.
- `web-dashboard`: Repo rows include forge indicator and links point at the repo's forge host.
- `pr-detail-page`: PR/commit/check links resolve against the repo's forge host.

## Impact

- **Code**: new `internal/forge` (interface + registry), new `internal/github` (App auth, REST/GraphQL client, webhook handler, discovery, setup); refactor `internal/gitea` to implement `forge.Forge`; touch `internal/{config,registry,discovery,poller,webhook,monitor,merge,queue,setup,web,store}` and `cmd/` wiring.
- **Database**: `repos` (and dependent rows) gain a `forge` column; migration backfills existing rows with `gitea`.
- **Config**: new `GITEA_MQ_GITHUB_*` variables; `GITEA_MQ_GITEA_URL`/`GITEA_MQ_GITEA_TOKEN` become optional when GitHub-only.
- **Dependencies**: add `github.com/google/go-github` and `golang.org/x/oauth2` (JWT signing for App auth).
- **External**: operators must register a GitHub App, install it on target repos, and point its webhook at `{EXTERNAL_URL}/webhook/github`.
