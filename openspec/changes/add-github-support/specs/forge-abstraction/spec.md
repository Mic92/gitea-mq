## ADDED Requirements

### Requirement: Forge-qualified repository identity
The system SHALL identify every managed repository by the triple `(forge, owner, name)`, where `forge` is one of `gitea` or `github`. Two repositories with identical `owner/name` on different forges SHALL be treated as distinct repositories with independent queues, persistence rows, and lifecycle. The string form SHALL be `<forge>:<owner>/<name>`.

#### Scenario: Same owner/name on two forges
- **WHEN** `gitea:org/app` and `github:org/app` are both managed
- **THEN** each has its own queue, poller, and database `repos` row
- **AND** enqueueing PR #1 on `gitea:org/app` does NOT affect `github:org/app`

#### Scenario: Registry lookup is forge-qualified
- **WHEN** a webhook for GitHub repo `org/app` arrives
- **AND** only `gitea:org/app` is in the managed set
- **THEN** the lookup for `github:org/app` returns not-found and the event is ignored

### Requirement: Forge interface routes all repo operations
The system SHALL perform every repository-scoped operation (list/get PRs, list auto-merge PRs, set `gitea-mq` status, read required checks, read check states, create/delete merge branch, list branches, cancel auto-merge, post comment, run repo auto-setup, build repo/PR HTML URLs) through a `Forge` implementation selected by the repository's `forge` field. Core packages (`queue`, `merge`, `monitor`, `poller`, `setup`, `registry`, `web`, `webhook`) SHALL NOT import `internal/gitea` or `internal/github` directly.

#### Scenario: Merge branch creation routed by forge
- **WHEN** PR #42 in `github:org/app` becomes head-of-queue
- **THEN** the merge branch is created via the GitHub `Forge` implementation
- **AND** no Gitea API call is made

#### Scenario: Status reporting routed by forge
- **WHEN** PR #7 in `gitea:org/lib` is enqueued
- **THEN** the `gitea-mq` status is posted via the Gitea `Forge` implementation as a commit status

### Requirement: Forge set resolution
The system SHALL hold at most one `Forge` instance per kind in a `forge.Set`. Resolving a `RepoRef` whose forge kind has no configured instance SHALL return an error and the repo SHALL NOT be added to the managed set.

#### Scenario: GitHub repo without GitHub configured
- **WHEN** GitHub is not configured (no `GITEA_MQ_GITHUB_APP_ID`)
- **AND** discovery or static config yields `github:org/app`
- **THEN** the registry refuses to add `github:org/app` and logs an error

#### Scenario: Both forges configured
- **WHEN** both Gitea and GitHub are configured
- **THEN** the `forge.Set` resolves `gitea:*` refs to the Gitea forge and `github:*` refs to the GitHub forge

### Requirement: Database forge column
The system SHALL store the forge kind in the `repos` table via a `forge TEXT NOT NULL` column with default `'gitea'`, and SHALL enforce uniqueness on `(forge, owner, name)`. All store queries that resolve a repo by owner/name SHALL also take `forge`.

#### Scenario: Migration backfills existing rows
- **WHEN** the `002_add_forge` migration runs against a database with existing `repos` rows
- **THEN** every existing row has `forge = 'gitea'`
- **AND** the old `UNIQUE(owner, name)` constraint is replaced by `UNIQUE(forge, owner, name)`

#### Scenario: Insert duplicate across forges
- **WHEN** `gitea:org/app` already exists in `repos`
- **AND** the system inserts `github:org/app`
- **THEN** the insert succeeds (different `forge` value)
