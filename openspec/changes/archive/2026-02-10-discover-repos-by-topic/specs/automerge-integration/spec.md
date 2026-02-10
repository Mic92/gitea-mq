## MODIFIED Requirements

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
