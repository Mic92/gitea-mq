## ADDED Requirements

### Requirement: Discover repos by Gitea topic
The system SHALL periodically query the Gitea API to discover repositories that have a configured topic. Discovery SHALL call `GET /api/v1/user/repos` (paginated) to list all repos accessible to the authenticated user, then call `GET /api/v1/repos/{owner}/{name}/topics` for each repo to fetch its topics. Only repos whose topics include the configured topic string SHALL be included in the discovered set.

#### Scenario: Repo has matching topic
- **WHEN** the discovery loop runs with topic `merge-queue`
- **AND** repo `org/app` is accessible to the authenticated user and has topic `merge-queue`
- **THEN** `org/app` is included in the discovered repo set

#### Scenario: Repo does not have matching topic
- **WHEN** the discovery loop runs with topic `merge-queue`
- **AND** repo `org/lib` is accessible but has topics `["nix", "library"]`
- **THEN** `org/lib` is NOT included in the discovered repo set

#### Scenario: Repo has no topics
- **WHEN** the discovery loop runs with topic `merge-queue`
- **AND** repo `org/docs` has an empty topic list
- **THEN** `org/docs` is NOT included in the discovered repo set

### Requirement: Filter by admin access
The system SHALL only discover repos where the authenticated user has admin permission (`permissions.admin == true` in the Gitea API response). Repos without admin access SHALL be skipped with a debug log.

#### Scenario: User has admin access
- **WHEN** the discovery loop finds repo `org/app` with topic `merge-queue`
- **AND** the API response shows `permissions.admin: true`
- **THEN** `org/app` is included in the discovered set

#### Scenario: User lacks admin access
- **WHEN** the discovery loop finds repo `org/app` with topic `merge-queue`
- **AND** the API response shows `permissions.admin: false`
- **THEN** `org/app` is NOT included in the discovered set
- **AND** the system logs a debug message about skipping due to insufficient permissions

### Requirement: Merge discovered repos with explicit repos
The system SHALL compute the final managed repo set as the union of topic-discovered repos and explicitly-configured repos (`GITEA_MQ_REPOS`). Explicitly-configured repos SHALL always be included regardless of topic or access level.

#### Scenario: Topic-only mode
- **WHEN** `GITEA_MQ_TOPIC` is set to `merge-queue` and `GITEA_MQ_REPOS` is empty
- **AND** discovery finds repos `org/app` and `org/lib`
- **THEN** the managed set is `[org/app, org/lib]`

#### Scenario: Combined mode
- **WHEN** `GITEA_MQ_TOPIC` is set to `merge-queue` and `GITEA_MQ_REPOS` is `org/legacy`
- **AND** discovery finds repos `org/app` and `org/lib`
- **THEN** the managed set is `[org/legacy, org/app, org/lib]`

#### Scenario: Static-only mode (no topic)
- **WHEN** `GITEA_MQ_TOPIC` is not set and `GITEA_MQ_REPOS` is `org/app,org/lib`
- **THEN** the managed set is `[org/app, org/lib]` (no discovery runs)

### Requirement: Reconcile repo set on each discovery cycle
The system SHALL diff the newly-discovered repo set against the currently-managed set. For repos that are new (present in discovered but not currently managed), the system SHALL initialise them: register in the database, run auto-setup (branch protection, webhook), and start a poller goroutine. For repos that are removed (currently managed but not in discovered set and not explicitly configured), the system SHALL stop their poller goroutine, remove them from webhook routing, and log the removal.

#### Scenario: New repo discovered
- **WHEN** the discovery loop finds `org/new-repo` with topic `merge-queue`
- **AND** `org/new-repo` is not currently managed
- **THEN** the system registers `org/new-repo` in the database
- **AND** runs auto-setup (branch protection, webhook)
- **AND** starts a poller goroutine for `org/new-repo`
- **AND** makes `org/new-repo` available to the webhook router
- **AND** logs the addition at info level

#### Scenario: Repo loses topic
- **WHEN** the discovery loop runs and `org/old-repo` no longer has topic `merge-queue`
- **AND** `org/old-repo` is not in the explicit repo list
- **THEN** the system stops the poller goroutine for `org/old-repo`
- **AND** removes `org/old-repo` from the webhook router
- **AND** does NOT delete `org/old-repo`'s data from the database
- **AND** logs the removal at info level

#### Scenario: Explicit repo never removed
- **WHEN** the discovery loop runs and `org/legacy` is in `GITEA_MQ_REPOS`
- **AND** `org/legacy` does not have topic `merge-queue`
- **THEN** `org/legacy` remains in the managed set (explicit repos are never removed by discovery)

### Requirement: Configurable discovery interval
The system SHALL support a configurable discovery interval via `GITEA_MQ_DISCOVERY_INTERVAL`. The default interval SHALL be 5 minutes. The discovery loop SHALL run immediately on startup and then repeat at the configured interval.

#### Scenario: Default discovery interval
- **WHEN** `GITEA_MQ_DISCOVERY_INTERVAL` is not set
- **THEN** the system discovers repos every 5 minutes

#### Scenario: Custom discovery interval
- **WHEN** `GITEA_MQ_DISCOVERY_INTERVAL` is set to `2m`
- **THEN** the system discovers repos every 2 minutes

#### Scenario: Discovery runs at startup
- **WHEN** the service starts with `GITEA_MQ_TOPIC` configured
- **THEN** the system runs discovery immediately before accepting webhook events
- **AND** initialises all discovered repos before the HTTP server starts

### Requirement: Config validation for topic vs repos
The system SHALL require at least one of `GITEA_MQ_TOPIC` or `GITEA_MQ_REPOS` to be set. If neither is set, the system SHALL fail at startup with a clear error message.

#### Scenario: Neither topic nor repos configured
- **WHEN** neither `GITEA_MQ_TOPIC` nor `GITEA_MQ_REPOS` is set
- **THEN** the system exits with error: "at least one of GITEA_MQ_TOPIC or GITEA_MQ_REPOS must be set"

#### Scenario: Only topic configured
- **WHEN** `GITEA_MQ_TOPIC` is set to `merge-queue` and `GITEA_MQ_REPOS` is not set
- **THEN** the system starts successfully and discovers repos by topic

#### Scenario: Only repos configured
- **WHEN** `GITEA_MQ_REPOS` is set and `GITEA_MQ_TOPIC` is not set
- **THEN** the system starts successfully with the static repo list (existing behaviour)

### Requirement: Resilient discovery on API failure
The system SHALL handle Gitea API failures during discovery gracefully. If the API is unreachable during a discovery cycle, the system SHALL keep the current managed repo set unchanged and retry on the next cycle.

#### Scenario: API failure during discovery
- **WHEN** the discovery loop runs and the Gitea API returns an error
- **THEN** the system keeps the current managed repo set unchanged
- **AND** logs a warning about the failed discovery
- **AND** retries on the next discovery cycle

#### Scenario: Partial failure fetching topics
- **WHEN** the discovery loop successfully lists repos but fails to fetch topics for `org/app`
- **THEN** `org/app` is excluded from this cycle's discovered set
- **AND** if `org/app` was previously managed, it remains managed (no removal on partial failure)
- **AND** the system logs a warning for the failed topic fetch
