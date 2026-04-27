## MODIFIED Requirements

### Requirement: Discover PRs with automerge scheduled
The system SHALL periodically poll each managed repo's forge to discover open PRs that currently have automerge enabled, using the repo's `Forge.ListAutoMergePRs` operation. The Gitea forge SHALL implement this by inspecting PR timeline comments for `pull_scheduled_merge` / `pull_cancel_scheduled_merge`; the GitHub forge SHALL implement this by listing open PRs and selecting those with `auto_merge != null`. Poller goroutines SHALL be started dynamically as repos are added to the managed set and stopped when repos are removed, using the poll interval configured for the repo's forge.

#### Scenario: PR has automerge scheduled
- **WHEN** the poller checks repo `gitea:org/app` and PR #42 has a `pull_scheduled_merge` timeline comment with no subsequent `pull_cancel_scheduled_merge`
- **AND** PR #42 is not already in the merge queue
- **THEN** the system enqueues PR #42

#### Scenario: PR had automerge scheduled then cancelled
- **WHEN** the poller checks repo `gitea:org/app` and PR #42 has a `pull_scheduled_merge` followed by a `pull_cancel_scheduled_merge`
- **THEN** the system does NOT enqueue PR #42

#### Scenario: GitHub PR has auto-merge enabled
- **WHEN** the poller checks repo `github:org/app` and PR #7 has `auto_merge` set
- **AND** PR #7 is not already in the merge queue
- **THEN** the system enqueues PR #7

#### Scenario: PR already in queue
- **WHEN** the poller discovers PR #42 has automerge scheduled
- **AND** PR #42 is already in the merge queue
- **THEN** the system takes no action (no duplicate enqueue)

#### Scenario: Poller starts for dynamically discovered repo
- **WHEN** the discovery loop adds `github:org/new-repo` to the managed set
- **THEN** a poller goroutine is started for `github:org/new-repo` with the GitHub poll interval
- **AND** the first poll runs immediately

#### Scenario: Poller stops for removed repo
- **WHEN** the discovery loop removes `gitea:org/old-repo` from the managed set
- **THEN** the poller goroutine for `gitea:org/old-repo` is stopped (its context is cancelled)
- **AND** no further polls occur for `gitea:org/old-repo`

### Requirement: Cancel automerge on failure
The system SHALL cancel a PR's automerge via the repo's forge when the PR is removed from the queue due to failure (check failure, timeout, or merge conflict). On Gitea this SHALL call `DELETE /repos/{owner}/{repo}/pulls/{index}/merge`; on GitHub this SHALL issue the GraphQL `disablePullRequestAutoMerge` mutation. The system SHALL also post a comment on the PR explaining why.

#### Scenario: Check failure cancels automerge
- **WHEN** a required check fails for head-of-queue PR #42
- **THEN** the system cancels automerge via the repo's forge
- **AND** posts a comment on PR #42 explaining which check failed
- **AND** sets the `gitea-mq` status to `failure` via the repo's forge

#### Scenario: Timeout cancels automerge
- **WHEN** the check timeout is exceeded for head-of-queue PR #42
- **THEN** the system cancels automerge via the repo's forge
- **AND** posts a comment on PR #42 explaining the timeout
- **AND** sets the `gitea-mq` status to `error` via the repo's forge

#### Scenario: Merge conflict cancels automerge
- **WHEN** the merge branch cannot be created due to conflicts for PR #42
- **THEN** the system cancels automerge via the repo's forge
- **AND** posts a comment on PR #42 explaining the merge conflict
- **AND** sets the `gitea-mq` status to `failure` via the repo's forge

### Requirement: Set gitea-mq commit status to gate merge
The system SHALL post a `gitea-mq` status on the PR's head commit via the repo's forge (`Forge.SetMQStatus`). On Gitea this is a commit status; on GitHub this is a check run. When the PR is enqueued, the status SHALL be `pending`/`queued`. When the merge branch CI passes, the status SHALL be `success` — which allows the forge's native automerge to proceed with the actual merge.

#### Scenario: PR enqueued — pending status
- **WHEN** PR #42 is enqueued
- **THEN** the system posts a `gitea-mq` status of `pending` with description "Queued (position #N)" via the repo's forge

#### Scenario: PR becomes head-of-queue — testing status
- **WHEN** PR #42 becomes head-of-queue and the merge branch is created
- **THEN** the system updates the `gitea-mq` status to `pending` with description "Testing merge result" via the repo's forge

#### Scenario: Merge branch CI passes — success status
- **WHEN** all required checks pass on the merge branch for PR #42
- **THEN** the system posts a `gitea-mq` status of `success` with description "Merge queue passed" via the repo's forge
- **AND** the forge's native automerge will detect all required checks (including `gitea-mq`) have passed and perform the merge

### Requirement: Pause when Gitea is unavailable
The system SHALL pause queue processing for a repo when that repo's forge API is unreachable. No queue advances, merge branch creations, or state changes SHALL occur for repos on the affected forge while it is down. Repos on other forges SHALL continue processing normally. The system SHALL resume normal operation for the affected forge when its API becomes reachable again.

#### Scenario: Gitea API returns errors
- **WHEN** the poller attempts to poll a Gitea repo and the Gitea API returns connection errors or 5xx responses
- **THEN** the system logs the error
- **AND** pauses queue processing for Gitea repos
- **AND** retries on the next poll cycle

#### Scenario: GitHub down, Gitea up
- **WHEN** the GitHub API is unreachable but the Gitea API is healthy
- **THEN** GitHub repos pause processing
- **AND** Gitea repos continue processing normally

#### Scenario: Gitea API recovers
- **WHEN** the Gitea API becomes reachable again after being down
- **THEN** the system resumes normal polling and queue processing for Gitea repos
- **AND** reconciles queue state against current Gitea state

### Requirement: Auto-configure Gitea on startup
The system SHALL automatically configure each managed repository via its forge's `EnsureRepoSetup` operation. For Gitea repos: ensure `gitea-mq` is in every branch protection's required status checks, and ensure a webhook for `status` events points at this service. For GitHub repos: enable `allow_auto_merge` and ensure a `gitea-mq` repository ruleset (no per-repo webhook). Auto-configuration SHALL run both at startup for initially-managed repos AND when new repos are added to the managed set by the discovery loop.

#### Scenario: Branch protection missing gitea-mq check
- **WHEN** the service starts and Gitea repo `org/app` has branch protection on `main` but `gitea-mq` is not in `status_check_contexts`
- **THEN** the system adds `gitea-mq` to the branch protection's required status checks via `PATCH /repos/org/app/branch_protections/{name}`

#### Scenario: Branch protection already has gitea-mq
- **WHEN** the service starts and Gitea repo `org/app` already has `gitea-mq` in required status checks
- **THEN** the system takes no action

#### Scenario: No branch protection exists
- **WHEN** the service starts and Gitea repo `org/app` has no branch protection on its default branch
- **THEN** the system logs a warning that branch protection should be configured
- **AND** continues operating (automerge can still be used, but merges won't be gated by branch protection)

#### Scenario: Webhook not configured
- **WHEN** the service starts and Gitea repo `org/app` has no webhook pointing at this service
- **THEN** the system creates a webhook for `status` events with the configured shared secret

#### Scenario: Webhook already exists
- **WHEN** the service starts and Gitea repo `org/app` already has a webhook pointing at this service
- **THEN** the system takes no action

#### Scenario: Newly discovered repo auto-configured
- **WHEN** the discovery loop adds a repo (any forge) to the managed set
- **THEN** the system runs that forge's auto-configuration for the repo
- **AND** starts a poller goroutine for the repo

### Requirement: Configurable poll interval
The system SHALL support a configurable poll interval per forge. `GITEA_MQ_POLL_INTERVAL` (default 30 seconds) applies to Gitea repos and is the fallback for GitHub. `GITEA_MQ_GITHUB_POLL_INTERVAL`, if set, overrides the interval for GitHub repos only.

#### Scenario: Default poll interval
- **WHEN** no poll interval is configured
- **THEN** the system polls every 30 seconds for all forges

#### Scenario: Custom poll interval
- **WHEN** `GITEA_MQ_POLL_INTERVAL` is set to `15s`
- **THEN** the system polls every 15 seconds for all forges

#### Scenario: GitHub-specific override
- **WHEN** `GITEA_MQ_POLL_INTERVAL=15s` and `GITEA_MQ_GITHUB_POLL_INTERVAL=2m`
- **THEN** Gitea repos are polled every 15s and GitHub repos every 2m
