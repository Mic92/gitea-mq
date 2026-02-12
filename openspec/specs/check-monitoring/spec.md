## ADDED Requirements

### Requirement: Track commit statuses on merge branch
The system SHALL monitor commit status updates for the temporary merge branch of the head-of-queue PR. The system SHALL aggregate all reported statuses and compare them against the set of required checks.

#### Scenario: Commit status arrives for merge branch
- **WHEN** a commit status update arrives for the merge branch `gitea-mq/42` with context `ci/build` and state `success`
- **THEN** the system records this status and evaluates whether all required checks are now satisfied

#### Scenario: Commit status for unrelated branch
- **WHEN** a commit status update arrives for a branch that is not a merge queue branch
- **THEN** the system ignores the event

### Requirement: Resolve required checks from branch protection
The system SHALL query the Gitea API for the target branch's branch protection settings to determine the list of required status checks. The system SHALL exclude `gitea-mq` from the required checks list (to avoid circular dependency). If the Gitea API returns required checks (other than `gitea-mq`), those SHALL be used.

#### Scenario: Branch protection has required checks
- **WHEN** PR #42 targets `main` and the branch protection for `main` requires checks `gitea-mq`, `ci/build`, and `ci/lint`
- **THEN** the system requires `ci/build` and `ci/lint` to report `success` on the merge branch before setting `gitea-mq` to success (excludes `gitea-mq` itself)

#### Scenario: Branch protection has only gitea-mq as required check
- **WHEN** PR #42 targets `main` and the branch protection for `main` only requires `gitea-mq`
- **THEN** the system falls back to the config-defined required checks

#### Scenario: Branch protection has no required checks
- **WHEN** PR #42 targets `main` and the branch protection for `main` has no required status checks configured
- **THEN** the system falls back to the config-defined required checks

### Requirement: Config fallback for required checks
The system SHALL support a fallback list of required check context names via environment variable configuration. This fallback SHALL be used when branch protection does not specify required checks (or only specifies `gitea-mq`).

#### Scenario: No branch protection checks, config defines checks
- **WHEN** branch protection returns no usable required checks
- **AND** the environment variable `GITEA_MQ_REQUIRED_CHECKS` is set to `ci/build,ci/test`
- **THEN** the system requires `ci/build` and `ci/test` to pass on the merge branch

#### Scenario: No branch protection, no config
- **WHEN** branch protection returns no usable required checks
- **AND** no fallback checks are configured
- **THEN** the system SHALL treat any single `success` status on the merge branch as sufficient

### Requirement: Check timeout
The system SHALL enforce a configurable timeout (default: 1 hour) for required checks to complete on the merge branch. If the timeout is exceeded, the head-of-queue PR SHALL be removed from the queue.

#### Scenario: Checks complete within timeout
- **WHEN** all required checks report `success` within 45 minutes
- **AND** the timeout is configured at 1 hour
- **THEN** the system sets `gitea-mq` to `success`

#### Scenario: Timeout exceeded
- **WHEN** 1 hour passes since the merge branch was created
- **AND** not all required checks have reported `success`
- **THEN** the system removes the PR from the queue
- **AND** cancels automerge on the PR
- **AND** posts a comment on the PR explaining the timeout
- **AND** posts a `gitea-mq` commit status of `error` with description "Check timeout exceeded"
- **AND** deletes the temporary merge branch
- **AND** advances to the next PR in the queue

### Requirement: Latest status wins per context
The system SHALL always use the most recent status for each check context. If a check transitions from `failure` back to `pending` and then to `success` (e.g. CI retry), the final `success` state SHALL be used.

#### Scenario: Check retried after failure
- **WHEN** check `ci/build` reports `failure` on merge branch `gitea-mq/42`
- **AND** then `ci/build` reports `pending` (retry started)
- **AND** then `ci/build` reports `success`
- **THEN** the system treats `ci/build` as `success`

#### Scenario: Check goes from success to failure
- **WHEN** check `ci/build` reports `success` on merge branch `gitea-mq/42`
- **AND** then `ci/build` reports `failure` (re-run failed)
- **THEN** the system treats `ci/build` as `failure`

### Requirement: Check failure handling
The system SHALL remove the head-of-queue PR when any required check reports a terminal failure state (`failure` or `error`) on the merge branch.

#### Scenario: Required check reports failure
- **WHEN** check `ci/build` reports `failure` on merge branch `gitea-mq/42`
- **THEN** the system removes PR #42 from the queue
- **AND** cancels automerge on PR #42
- **AND** posts a comment on PR #42 explaining which check failed
- **AND** posts a `gitea-mq` commit status of `failure` with description "Check failed: ci/build"
- **AND** deletes the temporary merge branch
- **AND** advances to the next PR

### Requirement: Commit status lifecycle reporting
The system SHALL post a `gitea-mq` commit status on the PR's original head commit reflecting the merge queue lifecycle: `pending` when queued, `pending` when testing, `success` when merge branch CI passes, `failure` when a check fails, `error` when timed out.

#### Scenario: Full lifecycle status updates
- **WHEN** PR #42 is enqueued
- **THEN** status is `pending` with description "Queued (position #3)"
- **WHEN** PR #42 becomes head-of-queue and merge branch is created
- **THEN** status is `pending` with description "Testing merge result"
- **WHEN** all checks pass on the merge branch
- **THEN** status is `success` with description "Merge queue passed"

#### Scenario: Failure lifecycle
- **WHEN** PR #42 is testing and check `ci/build` fails on the merge branch
- **THEN** status is `failure` with description "Check failed: ci/build"

#### Scenario: Timeout lifecycle
- **WHEN** PR #42 is testing and the timeout is exceeded
- **THEN** status is `error` with description "Check timeout exceeded"
