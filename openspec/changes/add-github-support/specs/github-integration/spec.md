## ADDED Requirements

### Requirement: GitHub App authentication
The system SHALL authenticate to github.com as a GitHub App. It SHALL sign a JWT with the configured private key and App ID, exchange it for installation access tokens, and use a per-installation token for all repository-scoped REST and GraphQL calls. Tokens SHALL be cached and transparently refreshed before expiry.

#### Scenario: First call for an installation
- **WHEN** the GitHub forge makes its first API call for a repo covered by installation `123`
- **THEN** it obtains an installation access token for `123` via `POST /app/installations/123/access_tokens`
- **AND** uses that token as the `Authorization: token …` header

#### Scenario: Token near expiry
- **WHEN** the cached installation token for `123` is within its refresh window
- **AND** a new API call is made
- **THEN** a fresh token is obtained before the call without surfacing an error to the caller

### Requirement: GitHub configuration variables
The system SHALL read GitHub configuration from `GITEA_MQ_GITHUB_APP_ID` (integer), `GITEA_MQ_GITHUB_PRIVATE_KEY` or `GITEA_MQ_GITHUB_PRIVATE_KEY_FILE` (PEM), `GITEA_MQ_GITHUB_WEBHOOK_SECRET`, optional `GITEA_MQ_GITHUB_REPOS` (comma-separated `owner/name`), and optional `GITEA_MQ_GITHUB_POLL_INTERVAL` (duration, defaults to `GITEA_MQ_POLL_INTERVAL`). GitHub SHALL be considered configured iff `GITEA_MQ_GITHUB_APP_ID` is set; when it is, the private key and webhook secret SHALL be required.

#### Scenario: GitHub fully configured
- **WHEN** `GITEA_MQ_GITHUB_APP_ID=42`, `GITEA_MQ_GITHUB_PRIVATE_KEY_FILE=/run/key.pem`, `GITEA_MQ_GITHUB_WEBHOOK_SECRET=s3cret` are set
- **THEN** the service starts with the GitHub forge enabled

#### Scenario: App ID without private key
- **WHEN** `GITEA_MQ_GITHUB_APP_ID=42` is set but neither private-key variable is set
- **THEN** the service fails to start with an error naming the missing variable

#### Scenario: GitHub poll interval inherits default
- **WHEN** `GITEA_MQ_POLL_INTERVAL=30s` and `GITEA_MQ_GITHUB_POLL_INTERVAL` is unset
- **THEN** GitHub repos are reconcile-polled every 30s

#### Scenario: GitHub poll interval overridden
- **WHEN** `GITEA_MQ_POLL_INTERVAL=30s` and `GITEA_MQ_GITHUB_POLL_INTERVAL=2m`
- **THEN** Gitea repos are polled every 30s and GitHub repos every 2m

### Requirement: Discover GitHub repos via App installations
The system SHALL list all installations of the configured GitHub App (`GET /app/installations`) and, for each installation, list its accessible repositories (`GET /installation/repositories`). Every such repository SHALL be included in the GitHub discovered set with forge `github`.

#### Scenario: App installed on two repos
- **WHEN** the App has one installation covering `org/app` and `org/lib`
- **THEN** the GitHub discovered set is `[github:org/app, github:org/lib]`

#### Scenario: Multiple installations
- **WHEN** the App has installation A covering `org/app` and installation B covering `alice/tool`
- **THEN** the GitHub discovered set is `[github:org/app, github:alice/tool]`
- **AND** API calls for `alice/tool` use installation B's token

### Requirement: Union GitHub static repos with installations
The system SHALL compute the managed GitHub repo set as the union of installation-discovered repos and `GITEA_MQ_GITHUB_REPOS`. A statically-listed repo with no covering installation SHALL be excluded with a warning log, since no installation token is available for it.

#### Scenario: Static repo also installed
- **WHEN** `GITEA_MQ_GITHUB_REPOS=org/app` and the App is installed on `org/app` and `org/lib`
- **THEN** the managed GitHub set is `[github:org/app, github:org/lib]`

#### Scenario: Static repo not installed
- **WHEN** `GITEA_MQ_GITHUB_REPOS=org/missing` and the App is not installed on `org/missing`
- **THEN** `github:org/missing` is NOT managed
- **AND** a warning is logged that `org/missing` has no App installation

### Requirement: Enqueue/dequeue via GitHub auto-merge webhooks
The system SHALL treat GitHub `pull_request` webhook events as the primary enqueue/dequeue signal: action `auto_merge_enabled` SHALL enqueue the PR; action `auto_merge_disabled` SHALL dequeue it as user-cancelled; action `closed` with `merged=true` SHALL mark it merged and advance the queue; action `closed` with `merged=false` SHALL dequeue it silently; action `synchronize` SHALL be treated as a new-push event for the PR.

#### Scenario: User enables auto-merge
- **WHEN** a `pull_request` webhook with `action=auto_merge_enabled` arrives for PR #42 in `github:org/app`
- **THEN** PR #42 is enqueued in `github:org/app`'s queue

#### Scenario: User disables auto-merge
- **WHEN** a `pull_request` webhook with `action=auto_merge_disabled` arrives for queued PR #42
- **THEN** PR #42 is removed from the queue
- **AND** if it was head-of-queue the merge branch is deleted and the queue advances

#### Scenario: PR merged by GitHub auto-merge
- **WHEN** a `pull_request` webhook with `action=closed` and `pull_request.merged=true` arrives for head-of-queue PR #42
- **THEN** PR #42 is removed from the queue and the queue advances

#### Scenario: New commits pushed
- **WHEN** a `pull_request` webhook with `action=synchronize` arrives for queued PR #42
- **THEN** the system treats it as a new-push event (dequeue, cancel auto-merge, comment)

### Requirement: GitHub reconcile poll fallback
The system SHALL, for each managed GitHub repo, periodically list open PRs and determine which currently have auto-merge enabled (`auto_merge != null`). The poll SHALL reconcile against the queue exactly like the Gitea poller: enqueue PRs that have auto-merge but are not queued, and dequeue PRs that are queued but no longer have auto-merge. The interval SHALL be `GITEA_MQ_GITHUB_POLL_INTERVAL` (or the inherited default).

#### Scenario: Missed enable webhook
- **WHEN** GitHub failed to deliver the `auto_merge_enabled` webhook for PR #42
- **AND** the reconcile poll runs and sees PR #42 with `auto_merge` set
- **THEN** PR #42 is enqueued

#### Scenario: Missed disable webhook
- **WHEN** PR #42 is queued and GitHub failed to deliver `auto_merge_disabled`
- **AND** the reconcile poll runs and sees PR #42 with `auto_merge == null`
- **THEN** PR #42 is removed from the queue

### Requirement: Report gitea-mq status as a GitHub check run
The GitHub forge SHALL report the `gitea-mq` lifecycle on the PR's head SHA as a **check run** named `gitea-mq` via the Checks API. State mapping: queued → `status=queued`; testing → `status=in_progress`; success → `status=completed, conclusion=success`; failure → `status=completed, conclusion=failure`; error/timeout → `status=completed, conclusion=cancelled`. Every check run SHALL set `details_url` to the PR's dashboard URL. Subsequent updates for the same SHA SHALL update the existing check run rather than create a new one.

#### Scenario: Enqueued check run
- **WHEN** PR #42 in `github:org/app` is enqueued
- **THEN** a check run `gitea-mq` is created on its head SHA with `status=queued`, summary "Queued (position #N)", and `details_url` pointing at the dashboard PR page

#### Scenario: Testing then success
- **WHEN** PR #42 becomes head-of-queue
- **THEN** the `gitea-mq` check run is updated to `status=in_progress`
- **WHEN** all required checks pass on the merge branch
- **THEN** the `gitea-mq` check run is updated to `status=completed, conclusion=success`

#### Scenario: Update after process restart
- **WHEN** the service restarts while PR #42 has an existing `gitea-mq` check run
- **AND** the next lifecycle update occurs
- **THEN** the forge finds the existing check run for the SHA by name and updates it (no duplicate check run)

### Requirement: Read required checks from GitHub rulesets
The GitHub forge SHALL determine the required check contexts for a target branch by reading active repository rules via `GET /repos/{owner}/{repo}/rules/branches/{branch}` and collecting `required_status_checks` contexts. If no ruleset yields contexts, it SHALL fall back to the classic branch protection `required_status_checks` (read-only). The `gitea-mq` context SHALL be excluded from the result.

#### Scenario: Ruleset defines required checks
- **WHEN** branch `main` of `github:org/app` has an active ruleset requiring `ci/build` and `gitea-mq`
- **THEN** `GetRequiredChecks` returns `[ci/build]`

#### Scenario: Only classic protection defines checks
- **WHEN** no ruleset applies to `main` but classic branch protection requires `ci/test`
- **THEN** `GetRequiredChecks` returns `[ci/test]`

### Requirement: GitHub repo auto-setup
On adding a GitHub repo to the managed set, the system SHALL (a) enable the repo's `allow_auto_merge` setting via `PATCH /repos/{owner}/{repo}`, and (b) ensure a repository ruleset named `gitea-mq` exists targeting all branches (`include: ["~ALL"]`) with a `required_status_checks` rule containing `{context: "gitea-mq", integration_id: <app-id>}`. Both steps SHALL be idempotent. A `403`/`422` from either step SHALL be logged as a warning and SHALL NOT prevent the repo from being managed. The system SHALL NOT create per-repo webhooks on GitHub.

#### Scenario: Fresh repo
- **WHEN** `github:org/app` is added and has no `gitea-mq` ruleset and `allow_auto_merge=false`
- **THEN** the system sets `allow_auto_merge=true`
- **AND** creates ruleset `gitea-mq` with target `~ALL` requiring check `gitea-mq` from this App

#### Scenario: Ruleset already present
- **WHEN** `github:org/app` already has ruleset `gitea-mq` requiring `gitea-mq`
- **THEN** the system makes no change to the ruleset

#### Scenario: Insufficient permissions
- **WHEN** the App lacks `Administration: write` on `github:org/app` and ruleset creation returns 403
- **THEN** the system logs a warning
- **AND** `github:org/app` is still added to the managed set

### Requirement: Create GitHub merge branch via refs+merges
The GitHub forge SHALL implement merge-branch creation by (1) creating `refs/heads/gitea-mq/<pr>` at the current target-branch tip via `POST /repos/{o}/{r}/git/refs`, then (2) merging the PR head SHA into it via `POST /repos/{o}/{r}/merges`. A `409 Conflict` from step 2 SHALL be reported as a merge conflict (not an error).

#### Scenario: Clean merge
- **WHEN** the forge creates the merge branch for PR #42 targeting `main`
- **AND** `POST /merges` returns 201 with merge commit `abc123`
- **THEN** `CreateMergeBranch` returns SHA `abc123` and `conflict=false`

#### Scenario: Conflict
- **WHEN** `POST /merges` returns 409
- **THEN** `CreateMergeBranch` returns `conflict=true`
- **AND** the caller handles it as a merge-conflict failure

### Requirement: Cancel GitHub auto-merge via GraphQL
The GitHub forge SHALL cancel a PR's auto-merge by issuing the GraphQL mutation `disablePullRequestAutoMerge` with the PR's node ID. If the mutation indicates auto-merge is already disabled, the call SHALL be treated as success.

#### Scenario: Cancel after check failure
- **WHEN** a required check fails for head-of-queue PR #42 in `github:org/app`
- **THEN** the forge issues `disablePullRequestAutoMerge` for PR #42's node ID
- **AND** posts a comment explaining which check failed

#### Scenario: Already disabled
- **WHEN** the mutation responds that auto-merge is not enabled for PR #42
- **THEN** the forge returns success without error

### Requirement: React to installation lifecycle webhooks
The system SHALL handle GitHub `installation` and `installation_repositories` webhook events by triggering an immediate GitHub discovery run, so that newly installed/removed repos are added to or removed from the managed set without waiting for the discovery interval.

#### Scenario: App installed on new repo
- **WHEN** an `installation_repositories` webhook with `action=added` for `org/new` arrives
- **THEN** discovery runs immediately
- **AND** `github:org/new` is added to the managed set and auto-setup runs

#### Scenario: App uninstalled
- **WHEN** an `installation` webhook with `action=deleted` arrives for installation `123`
- **THEN** discovery runs immediately
- **AND** all repos previously covered by installation `123` are removed from the managed set
