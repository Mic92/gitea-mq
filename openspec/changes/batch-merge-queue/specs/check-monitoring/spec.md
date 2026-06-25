## MODIFIED Requirements

### Requirement: Commit status lifecycle reporting
In batch mode the `gitea-mq` status on a PR's head SHALL be written at exactly three points: enqueue (`pending`, "Queued (position #N)"), the batch's first build (`pending`, "Testing batch #<id> (N PRs)"), and terminal (`success` "Merged via batch #<id>" / `failure` "Check failed: <ctx>" / `error` <reason>). Bisection rebuilds SHALL NOT post additional `gitea-mq` statuses. `gitea-mq/<ctx>` mirrored checks SHALL be posted only on the first build.

#### Scenario: Batch lifecycle statuses
- **WHEN** PR #10 is in batch #7 with 3 members
- **THEN** on the first build its status is `pending` "Testing batch #7 (3 PRs)"
- **WHEN** #10 lands
- **THEN** its status is `success` "Merged via batch #7"

#### Scenario: No status churn during bisection
- **WHEN** batch #7 fails and rebuilds with `current=[#10,#20]`
- **THEN** PR #10's `gitea-mq` status is unchanged until #10 reaches a terminal outcome

## ADDED Requirements

### Requirement: Batch timeout clock
Check timeout in batch mode SHALL be measured from `batches.testing_started_at`, which SHALL be reset on every rebuild. Single-PR mode keeps the entry's `testing_started_at`.

#### Scenario: Timeout resets per rebuild
- **WHEN** the first build ran 50m of a 60m timeout before failing
- **AND** a bisection rebuild starts
- **THEN** the rebuild has a fresh 60m budget
