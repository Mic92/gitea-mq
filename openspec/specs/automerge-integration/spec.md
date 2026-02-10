## ADDED Requirements

### Requirement: Discover PRs with automerge scheduled
The system SHALL periodically poll the Gitea API to discover open PRs that have automerge scheduled. Discovery SHALL be done by querying each managed repo's open PRs and checking their timeline for `pull_scheduled_merge` and `pull_cancel_scheduled_merge` comment types. A PR is considered automerge-scheduled if its most recent automerge-related timeline comment is `pull_scheduled_merge`. Poller goroutines SHALL be started dynamically as repos are added to the managed set and stopped when repos are removed.

#### Scenario: PR has automerge scheduled
- **WHEN** the poller checks repo `org/app` and PR #42 has a `pull_scheduled_merge` timeline comment with no subsequent `pull_cancel_scheduled_merge`
- **AND** PR #42 is not already in the merge queue
- **THEN** the system enqueues PR #42

#### Scenario: PR had automerge scheduled then cancelled
- **WHEN** the poller checks repo `org/app` and PR #42 has a `pull_scheduled_merge` followed by a `pull_cancel_scheduled_merge`
- **THEN** the system does NOT enqueue PR #42

#### Scenario: PR already in queue
- **WHEN** the poller discovers PR #42 has automerge scheduled
- **AND** PR #42 is already in the merge queue
- **THEN** the system takes no action (no duplicate enqueue)

#### Scenario: Poller starts for dynamically discovered repo
- **WHEN** the discovery loop adds `org/new-repo` to the managed set
- **THEN** a poller goroutine is started for `org/new-repo` with the configured poll interval
- **AND** the first poll runs immediately

#### Scenario: Poller stops for removed repo
- **WHEN** the discovery loop removes `org/old-repo` from the managed set
- **THEN** the poller goroutine for `org/old-repo` is stopped (its context is cancelled)
- **AND** no further polls occur for `org/old-repo`

### Requirement: Detect automerge cancellation
The system SHALL detect when a user cancels automerge on a queued PR. On the next poll cycle, if a previously-queued PR no longer has automerge scheduled, the system SHALL remove it from the queue.

#### Scenario: User cancels automerge while PR is queued
- **WHEN** PR #42 is in the merge queue
- **AND** the poller detects a `pull_cancel_scheduled_merge` comment newer than the last `pull_scheduled_merge`
- **THEN** the system removes PR #42 from the queue
- **AND** if PR #42 was head-of-queue, cleans up the merge branch and advances to the next PR

### Requirement: Cancel automerge on failure
The system SHALL cancel a PR's automerge via the Gitea API (`DELETE /repos/{owner}/{repo}/pulls/{index}/merge`) when the PR is removed from the queue due to failure (check failure, timeout, or merge conflict). The system SHALL also post a comment on the PR explaining why.

#### Scenario: Check failure cancels automerge
- **WHEN** a required check fails for head-of-queue PR #42
- **THEN** the system calls `DELETE /repos/org/app/pulls/42/merge` to cancel automerge
- **AND** posts a comment on PR #42 explaining which check failed
- **AND** sets `gitea-mq` commit status to `failure`

#### Scenario: Timeout cancels automerge
- **WHEN** the check timeout is exceeded for head-of-queue PR #42
- **THEN** the system calls `DELETE /repos/org/app/pulls/42/merge` to cancel automerge
- **AND** posts a comment on PR #42 explaining the timeout
- **AND** sets `gitea-mq` commit status to `error`

#### Scenario: Merge conflict cancels automerge
- **WHEN** the merge branch cannot be created due to conflicts for PR #42
- **THEN** the system calls `DELETE /repos/org/app/pulls/42/merge` to cancel automerge
- **AND** posts a comment on PR #42 explaining the merge conflict
- **AND** sets `gitea-mq` commit status to `failure`

### Requirement: Set gitea-mq commit status to gate merge
The system SHALL post a `gitea-mq` commit status on the PR's head commit. When the PR is enqueued, the status SHALL be `pending`. When the merge branch CI passes, the status SHALL be `success` — which allows Gitea's automerge to proceed with the actual merge.

#### Scenario: PR enqueued — pending status
- **WHEN** PR #42 is enqueued
- **THEN** the system posts a `gitea-mq` commit status of `pending` with description "Queued (position #N)"

#### Scenario: PR becomes head-of-queue — testing status
- **WHEN** PR #42 becomes head-of-queue and the merge branch is created
- **THEN** the system updates the `gitea-mq` commit status to `pending` with description "Testing merge result"

#### Scenario: Merge branch CI passes — success status
- **WHEN** all required checks pass on the merge branch for PR #42
- **THEN** the system posts a `gitea-mq` commit status of `success` with description "Merge queue passed"
- **AND** Gitea's automerge will detect all required checks (including `gitea-mq`) have passed and perform the merge

### Requirement: Detect new push to queued PR
The system SHALL detect when new commits are pushed to a queued PR by comparing the PR's current head SHA (from the API) against the stored head SHA. If they differ, the PR SHALL be removed from the queue with automerge cancelled.

#### Scenario: Head SHA changes for queued PR
- **WHEN** PR #42 was enqueued with head SHA `abc123`
- **AND** the poller fetches PR #42 and sees head SHA `def456`
- **THEN** the system removes PR #42 from the queue
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining the PR was removed due to new commits

### Requirement: Pause when Gitea is unavailable
The system SHALL pause all queue processing when the Gitea API is unreachable. No queue advances, merge branch creations, or state changes SHALL occur while Gitea is down. The system SHALL resume normal operation when the API becomes reachable again.

#### Scenario: Gitea API returns errors
- **WHEN** the poller attempts to poll and the Gitea API returns connection errors or 5xx responses
- **THEN** the system logs the error
- **AND** pauses all queue processing
- **AND** retries on the next poll cycle

#### Scenario: Gitea API recovers
- **WHEN** the Gitea API becomes reachable again after being down
- **THEN** the system resumes normal polling and queue processing
- **AND** reconciles queue state against current Gitea state

### Requirement: Handle deleted or transferred repository
The system SHALL detect when a managed repository no longer exists (API returns 404). The system SHALL remove the repository from its managed set, clean up any queue entries, and log a warning.

#### Scenario: Managed repo deleted
- **WHEN** the poller attempts to poll repo `org/app` and the Gitea API returns 404
- **THEN** the system removes `org/app` from its managed repos
- **AND** cleans up any active queue entries and merge branches for `org/app`
- **AND** logs a warning that the repository no longer exists

### Requirement: Auto-configure Gitea on startup
The system SHALL automatically configure each managed repository: ensure `gitea-mq` is in the branch protection's required status checks, and ensure a webhook for `status` events is pointed at this service's webhook endpoint. Auto-configuration SHALL run both at startup for initially-managed repos AND when new repos are added to the managed set by the discovery loop.

#### Scenario: Branch protection missing gitea-mq check
- **WHEN** the service starts and repo `org/app` has branch protection on `main` but `gitea-mq` is not in `status_check_contexts`
- **THEN** the system adds `gitea-mq` to the branch protection's required status checks via `PATCH /repos/org/app/branch_protections/{name}`

#### Scenario: Branch protection already has gitea-mq
- **WHEN** the service starts and repo `org/app` already has `gitea-mq` in required status checks
- **THEN** the system takes no action

#### Scenario: No branch protection exists
- **WHEN** the service starts and repo `org/app` has no branch protection on its default branch
- **THEN** the system logs a warning that branch protection should be configured
- **AND** continues operating (automerge can still be used, but merges won't be gated by branch protection)

#### Scenario: Webhook not configured
- **WHEN** the service starts and repo `org/app` has no webhook pointing at this service
- **THEN** the system creates a webhook for `status` events with the configured shared secret

#### Scenario: Webhook already exists
- **WHEN** the service starts and repo `org/app` already has a webhook pointing at this service
- **THEN** the system takes no action

#### Scenario: Newly discovered repo auto-configured
- **WHEN** the discovery loop finds `org/new-repo` with the matching topic
- **AND** `org/new-repo` is not yet managed
- **THEN** the system runs auto-configuration for `org/new-repo` (branch protection + webhook setup)
- **AND** starts a poller goroutine for `org/new-repo`

### Requirement: Configurable poll interval
The system SHALL support a configurable poll interval (via environment variable) for checking automerge state. The default poll interval SHALL be 30 seconds.

#### Scenario: Default poll interval
- **WHEN** no poll interval is configured
- **THEN** the system polls every 30 seconds

#### Scenario: Custom poll interval
- **WHEN** `GITEA_MQ_POLL_INTERVAL` is set to `15s`
- **THEN** the system polls every 15 seconds
