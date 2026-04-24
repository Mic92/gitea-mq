## MODIFIED Requirements

### Requirement: Merge discovered repos with explicit repos
The system SHALL compute the final managed repo set as the union of all per-forge discovered sets and all explicitly-configured repos. Gitea contributes topic-discovered repos plus `GITEA_MQ_REPOS` (forge `gitea`). GitHub contributes installation-discovered repos plus `GITEA_MQ_GITHUB_REPOS` (forge `github`). Explicitly-configured repos SHALL always be included for their forge regardless of discovery, except that a `GITEA_MQ_GITHUB_REPOS` entry with no covering App installation SHALL be skipped with a warning.

#### Scenario: Topic-only mode
- **WHEN** `GITEA_MQ_TOPIC` is set to `merge-queue` and `GITEA_MQ_REPOS` is empty
- **AND** discovery finds repos `org/app` and `org/lib`
- **THEN** the managed set is `[gitea:org/app, gitea:org/lib]`

#### Scenario: Combined mode
- **WHEN** `GITEA_MQ_TOPIC` is set to `merge-queue` and `GITEA_MQ_REPOS` is `org/legacy`
- **AND** discovery finds repos `org/app` and `org/lib`
- **THEN** the managed set is `[gitea:org/legacy, gitea:org/app, gitea:org/lib]`

#### Scenario: Static-only mode (no topic)
- **WHEN** `GITEA_MQ_TOPIC` is not set and `GITEA_MQ_REPOS` is `org/app,org/lib`
- **THEN** the managed set is `[gitea:org/app, gitea:org/lib]` (no Gitea discovery runs)

#### Scenario: Mixed forges
- **WHEN** Gitea discovery yields `gitea:org/app` and GitHub discovery yields `github:org/app` and `github:alice/tool`
- **THEN** the managed set is `[gitea:org/app, github:org/app, github:alice/tool]`

### Requirement: Config validation for topic vs repos
The system SHALL require at least one forge to be configured. Gitea is configured when `GITEA_MQ_GITEA_URL` is set; GitHub is configured when `GITEA_MQ_GITHUB_APP_ID` is set. When Gitea is configured, at least one of `GITEA_MQ_TOPIC` or `GITEA_MQ_REPOS` SHALL be set. When neither forge is configured the system SHALL fail at startup with a clear error message.

#### Scenario: Neither topic nor repos configured
- **WHEN** Gitea is configured but neither `GITEA_MQ_TOPIC` nor `GITEA_MQ_REPOS` is set
- **THEN** the system exits with error: "at least one of GITEA_MQ_TOPIC or GITEA_MQ_REPOS must be set"

#### Scenario: Only topic configured
- **WHEN** `GITEA_MQ_TOPIC` is set to `merge-queue` and `GITEA_MQ_REPOS` is not set
- **THEN** the system starts successfully and discovers repos by topic

#### Scenario: Only repos configured
- **WHEN** `GITEA_MQ_REPOS` is set and `GITEA_MQ_TOPIC` is not set
- **THEN** the system starts successfully with the static repo list (existing behaviour)

#### Scenario: No forge configured
- **WHEN** neither `GITEA_MQ_GITEA_URL` nor `GITEA_MQ_GITHUB_APP_ID` is set
- **THEN** the system exits with error: "at least one of Gitea or GitHub must be configured"

#### Scenario: GitHub only
- **WHEN** `GITEA_MQ_GITHUB_APP_ID` is set with private key and webhook secret, and no Gitea variables are set
- **THEN** the system starts successfully managing only GitHub repos

### Requirement: Reconcile repo set on each discovery cycle
The system SHALL diff the newly-discovered repo set (across all forges) against the currently-managed set. For repos that are new (present in discovered but not currently managed), the system SHALL initialise them: register in the database, run the forge's auto-setup, and start a poller goroutine. For repos that are removed (currently managed but not in the discovered set and not explicitly configured), the system SHALL stop their poller goroutine, remove them from webhook routing, and log the removal.

#### Scenario: New repo discovered
- **WHEN** the discovery loop finds `gitea:org/new-repo` with topic `merge-queue`
- **AND** `gitea:org/new-repo` is not currently managed
- **THEN** the system registers `gitea:org/new-repo` in the database
- **AND** runs Gitea auto-setup (branch protection, webhook)
- **AND** starts a poller goroutine for `gitea:org/new-repo`
- **AND** makes `gitea:org/new-repo` available to the webhook router
- **AND** logs the addition at info level

#### Scenario: Repo loses topic
- **WHEN** the discovery loop runs and `gitea:org/old-repo` no longer has topic `merge-queue`
- **AND** `org/old-repo` is not in the explicit repo list
- **THEN** the system stops the poller goroutine for `gitea:org/old-repo`
- **AND** removes `gitea:org/old-repo` from the webhook router
- **AND** does NOT delete `gitea:org/old-repo`'s data from the database
- **AND** logs the removal at info level

#### Scenario: Explicit repo never removed
- **WHEN** the discovery loop runs and `org/legacy` is in `GITEA_MQ_REPOS`
- **AND** `org/legacy` does not have topic `merge-queue`
- **THEN** `gitea:org/legacy` remains in the managed set (explicit repos are never removed by discovery)

#### Scenario: GitHub App uninstalled from repo
- **WHEN** the discovery loop runs and the App is no longer installed on `github:org/gone`
- **THEN** the system stops the poller goroutine for `github:org/gone`
- **AND** removes `github:org/gone` from the webhook router
- **AND** logs the removal at info level

## ADDED Requirements

### Requirement: Immediate discovery on installation webhook
The system SHALL trigger an immediate discovery run when a GitHub `installation` or `installation_repositories` webhook is received, in addition to the periodic interval, so that adding or removing the App on a repo takes effect without waiting for `GITEA_MQ_DISCOVERY_INTERVAL`.

#### Scenario: Repo added to installation
- **WHEN** an `installation_repositories` webhook with `action=added` arrives
- **THEN** the discovery loop runs immediately
- **AND** newly covered repos are added to the managed set before the next scheduled cycle
