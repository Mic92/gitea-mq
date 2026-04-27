## MODIFIED Requirements

### Requirement: Per-repository per-branch queues
The system SHALL maintain independent queues for each combination of forge, repository, and target branch. PRs targeting `main` and PRs targeting `release/1.0` in the same repo SHALL have separate queues. Repositories with the same `owner/name` on different forges SHALL have separate queues. Operations on one queue SHALL NOT affect other queues.

#### Scenario: Independent repo queues
- **WHEN** PR #1 is enqueued in repo `gitea:org/app-a` targeting `main`
- **AND** PR #2 is enqueued in repo `gitea:org/app-b` targeting `main`
- **THEN** both PRs can be tested concurrently (one per queue)
- **AND** removing PR #1 from `gitea:org/app-a` does not affect `gitea:org/app-b`'s queue

#### Scenario: Independent branch queues within same repo
- **WHEN** PR #1 targets `main` in repo `gitea:org/app`
- **AND** PR #2 targets `release/1.0` in repo `gitea:org/app`
- **THEN** both PRs can be tested concurrently (one per target branch)
- **AND** each queue operates independently

#### Scenario: Independent forge queues for same owner/name
- **WHEN** PR #1 is enqueued in `gitea:org/app` targeting `main`
- **AND** PR #2 is enqueued in `github:org/app` targeting `main`
- **THEN** both PRs can be tested concurrently
- **AND** each queue operates independently

### Requirement: Temporary merge branch for testing
The system SHALL create a temporary merge branch by merging the PR's head into the latest target branch via the repo's forge (`Forge.CreateMergeBranch`). The branch SHALL be named `gitea-mq/<pr-number>`. CI runs on this branch. The system SHALL delete the merge branch via the repo's forge after the PR is merged, removed, or fails.

#### Scenario: Create merge branch for head-of-queue PR
- **WHEN** PR #42 targeting `main` becomes head-of-queue in repo `gitea:org/app`
- **THEN** the system fetches the latest `main` ref
- **AND** creates branch `gitea-mq/42` containing the merge of PR #42's head into `main`
- **AND** the system updates the `gitea-mq` status to `pending` with description "Testing merge result"

#### Scenario: Create merge branch on GitHub
- **WHEN** PR #7 targeting `main` becomes head-of-queue in repo `github:alice/tool`
- **THEN** the GitHub forge creates `refs/heads/gitea-mq/7` at `main`'s tip and merges PR #7's head SHA into it
- **AND** the system updates the `gitea-mq` check run to `in_progress`

#### Scenario: Merge branch deleted externally during testing
- **WHEN** the merge branch `gitea-mq/42` is deleted by someone while CI is running
- **THEN** the system treats this as a failure
- **AND** removes PR #42 from the queue
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining the merge branch was unexpectedly deleted
- **AND** sets `gitea-mq` status to `error` with description "Merge branch deleted"
- **AND** advances to the next PR

#### Scenario: Merge branch creation fails due to conflict
- **WHEN** the system attempts to create the merge branch for PR #42 but the merge has conflicts
- **THEN** PR #42 is removed from the queue
- **AND** the system cancels automerge on PR #42
- **AND** the system posts a comment on PR #42 explaining it was removed due to merge conflicts
- **AND** the system posts a `gitea-mq` status of `failure` with description "Merge conflict"
- **AND** the system advances to the next PR

### Requirement: Signal Gitea to merge on success
The system SHALL set the `gitea-mq` status to `success` on the PR's head commit, via the repo's forge, when all required checks pass on the merge branch. The PR SHALL remain as head-of-queue until the system confirms the forge's native automerge has actually merged it. The system SHALL NOT advance to the next PR until the merge is confirmed.

#### Scenario: All required checks pass on merge branch
- **WHEN** all required checks for the merge branch `gitea-mq/42` report `success`
- **THEN** the system posts a `gitea-mq` status of `success` with description "Merge queue passed"
- **AND** deletes the temporary merge branch `gitea-mq/42`
- **AND** the PR remains head-of-queue in `success` state, waiting for the forge's automerge to complete

#### Scenario: Gitea's automerge succeeds
- **WHEN** the poller detects PR #42 has been merged by Gitea's automerge
- **THEN** the system removes PR #42 from the queue
- **AND** advances to the next PR

#### Scenario: GitHub auto-merge succeeds via webhook
- **WHEN** a `pull_request` `closed` webhook with `merged=true` arrives for head-of-queue PR #7 in `github:alice/tool`
- **THEN** the system removes PR #7 from the queue
- **AND** advances to the next PR

#### Scenario: Gitea's automerge fails to merge
- **WHEN** the system set `gitea-mq` to `success` for PR #42
- **AND** the poller detects PR #42 is still open after a reasonable period (e.g. multiple poll cycles)
- **THEN** the system sets `gitea-mq` to `error` with description "Automerge did not complete"
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining that the merge could not be completed
- **AND** removes PR #42 from the queue and advances

### Requirement: Detect completed merge
The system SHALL detect when the forge's native automerge has successfully merged a head-of-queue PR. On Gitea this is via polling PR state; on GitHub this is via the `pull_request` `closed` (`merged=true`) webhook with the reconcile poll as fallback. Upon detecting the merge, the system SHALL remove the PR from the queue and advance to the next PR.

#### Scenario: Gitea merges the head-of-queue PR
- **WHEN** the system polls PR #42 and finds it has been merged
- **THEN** PR #42 is removed from the queue
- **AND** the system advances to the next PR

#### Scenario: GitHub merges the head-of-queue PR
- **WHEN** the reconcile poll for `github:org/app` finds head-of-queue PR #7 has `merged=true`
- **THEN** PR #7 is removed from the queue
- **AND** the system advances to the next PR
