## ADDED Requirements

### Requirement: Enqueue PR
The system SHALL add a PR to the tail of its repository's merge queue when automerge is detected. Each PR SHALL appear at most once in a queue. Enqueueing an already-queued PR SHALL be a no-op.

#### Scenario: Enqueue a new PR
- **WHEN** automerge is detected for PR #42 in repo `org/app`
- **THEN** PR #42 is appended to the `org/app` queue
- **AND** the system posts a `gitea-mq` commit status of `pending` with description "Queued (position #N)"

#### Scenario: Enqueue an already-queued PR
- **WHEN** automerge is detected for PR #42 which is already in the queue
- **THEN** the queue is unchanged

### Requirement: Dequeue PR
The system SHALL remove a PR from its repository's merge queue when automerge is cancelled or when the PR fails. If the dequeued PR was head-of-queue (actively being tested), the system SHALL clean up the temporary merge branch and advance to the next PR.

#### Scenario: Dequeue a queued PR that is not head-of-queue
- **WHEN** automerge is cancelled for PR #42 which is at position 3 in the queue
- **THEN** PR #42 is removed from the queue

#### Scenario: Dequeue the head-of-queue PR
- **WHEN** automerge is cancelled for PR #42 which is head-of-queue and being tested
- **THEN** PR #42 is removed from the queue
- **AND** the temporary merge branch `gitea-mq/42` is deleted
- **AND** the system advances to the next PR in the queue

### Requirement: FIFO ordering
The system SHALL process PRs in strict first-in, first-out order within each repository's queue. The order is determined by the time the PR was enqueued (i.e., when automerge was first detected).

#### Scenario: Multiple PRs enqueued
- **WHEN** PR #10 is enqueued, then PR #20, then PR #30
- **THEN** the queue order is [#10, #20, #30]
- **AND** PR #10 is processed first

### Requirement: One PR at a time per repo
The system SHALL test at most one PR per repository at any given time. The next PR SHALL only begin testing after the current head-of-queue PR completes (merged, failed, timed out, or removed).

#### Scenario: Head-of-queue PR is being tested
- **WHEN** PR #10 is head-of-queue and CI is running on the merge branch
- **AND** PR #20 is next in the queue
- **THEN** PR #20 remains in `queued` state and is not tested until PR #10 completes

### Requirement: Temporary merge branch for testing
The system SHALL create a temporary merge branch by merging the PR's head into the latest target branch. The branch SHALL be named `gitea-mq/<pr-number>`. CI runs on this branch. The system SHALL delete the merge branch after the PR is merged, removed, or fails.

#### Scenario: Create merge branch for head-of-queue PR
- **WHEN** PR #42 targeting `main` becomes head-of-queue in repo `org/app`
- **THEN** the system fetches the latest `main` ref
- **AND** creates branch `gitea-mq/42` containing the merge of PR #42's head into `main`
- **AND** the system updates the `gitea-mq` commit status to `pending` with description "Testing merge result"

#### Scenario: Merge branch deleted externally during testing
- **WHEN** the merge branch `gitea-mq/42` is deleted by someone while CI is running
- **THEN** the system treats this as a failure
- **AND** removes PR #42 from the queue
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining the merge branch was unexpectedly deleted
- **AND** sets `gitea-mq` commit status to `error` with description "Merge branch deleted"
- **AND** advances to the next PR

#### Scenario: Merge branch creation fails due to conflict
- **WHEN** the system attempts to create the merge branch for PR #42 but the merge has conflicts
- **THEN** PR #42 is removed from the queue
- **AND** the system cancels automerge on PR #42
- **AND** the system posts a comment on PR #42 explaining it was removed due to merge conflicts
- **AND** the system posts a `gitea-mq` commit status of `failure` with description "Merge conflict"
- **AND** the system advances to the next PR

### Requirement: Signal Gitea to merge on success
The system SHALL set the `gitea-mq` commit status to `success` on the PR's head commit when all required checks pass on the merge branch. The PR SHALL remain as head-of-queue until the poller confirms Gitea has actually merged it. The system SHALL NOT advance to the next PR until the merge is confirmed.

#### Scenario: All required checks pass on merge branch
- **WHEN** all required checks for the merge branch `gitea-mq/42` report `success`
- **THEN** the system posts a `gitea-mq` commit status of `success` with description "Merge queue passed"
- **AND** deletes the temporary merge branch `gitea-mq/42`
- **AND** the PR remains head-of-queue in `success` state, waiting for Gitea's automerge to complete

#### Scenario: Gitea's automerge succeeds
- **WHEN** the poller detects PR #42 has been merged by Gitea's automerge
- **THEN** the system removes PR #42 from the queue
- **AND** advances to the next PR

#### Scenario: Gitea's automerge fails to merge
- **WHEN** the system set `gitea-mq` to `success` for PR #42
- **AND** the poller detects PR #42 is still open after a reasonable period (e.g. multiple poll cycles)
- **THEN** the system sets `gitea-mq` to `error` with description "Automerge did not complete"
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining that the merge could not be completed
- **AND** removes PR #42 from the queue and advances

### Requirement: Auto-advance on failure
The system SHALL automatically advance to the next PR in the queue when the current head-of-queue PR is removed for any reason (timeout, conflict, automerge cancelled).

#### Scenario: Head-of-queue removed, next PR exists
- **WHEN** PR #10 is removed from head-of-queue
- **AND** PR #20 is next in the queue
- **THEN** PR #20 becomes the new head-of-queue and testing begins immediately

#### Scenario: Head-of-queue removed, queue is empty
- **WHEN** PR #10 is removed from head-of-queue
- **AND** the queue is empty
- **THEN** the queue enters idle state

### Requirement: Remove PR on new push
The system SHALL remove a PR from the merge queue when new commits are pushed to the PR's head branch. The system SHALL cancel automerge and post a comment explaining that the PR was removed because new commits were pushed.

#### Scenario: New commits pushed to head-of-queue PR
- **WHEN** PR #42 is head-of-queue and being tested
- **AND** new commits are pushed to PR #42's head branch (head SHA changes)
- **THEN** the system removes PR #42 from the queue
- **AND** deletes the temporary merge branch `gitea-mq/42`
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining the PR was removed due to new commits
- **AND** sets `gitea-mq` commit status to `error` with description "New commits pushed"
- **AND** advances to the next PR in the queue

#### Scenario: New commits pushed to a queued (non-head) PR
- **WHEN** PR #42 is at position 3 in the queue (not being tested)
- **AND** new commits are pushed to PR #42's head branch
- **THEN** the system removes PR #42 from the queue
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining the PR was removed due to new commits

### Requirement: Detect completed merge
The system SHALL detect when Gitea's automerge has successfully merged a head-of-queue PR (via polling PR state). Upon detecting the merge, the system SHALL remove the PR from the queue and advance to the next PR.

#### Scenario: Gitea merges the head-of-queue PR
- **WHEN** the system polls PR #42 and finds it has been merged
- **THEN** PR #42 is removed from the queue
- **AND** the system advances to the next PR

### Requirement: Remove PR on target branch change
The system SHALL remove a PR from the queue if its target branch changes. The system SHALL cancel automerge and post a comment explaining the removal.

#### Scenario: PR retargeted while queued
- **WHEN** PR #42 is in the `main` queue
- **AND** the poller detects PR #42 now targets `develop` instead of `main`
- **THEN** the system removes PR #42 from the `main` queue
- **AND** cancels automerge on PR #42
- **AND** posts a comment explaining the PR was removed because the target branch changed
- **AND** if PR #42 was head-of-queue, deletes the merge branch and advances

### Requirement: Remove PR when closed
The system SHALL remove a PR from the queue if it is closed (not merged). No comment is posted — the queue silently cleans up and advances.

#### Scenario: Queued PR is closed
- **WHEN** PR #42 is in the merge queue
- **AND** the poller detects PR #42 has been closed
- **THEN** the system removes PR #42 from the queue
- **AND** if PR #42 was head-of-queue, deletes the merge branch and advances to the next PR

### Requirement: Per-repository per-branch queues
The system SHALL maintain independent queues for each combination of repository and target branch. PRs targeting `main` and PRs targeting `release/1.0` in the same repo SHALL have separate queues. Operations on one queue SHALL NOT affect other queues.

#### Scenario: Independent repo queues
- **WHEN** PR #1 is enqueued in repo `org/app-a` targeting `main`
- **AND** PR #2 is enqueued in repo `org/app-b` targeting `main`
- **THEN** both PRs can be tested concurrently (one per queue)
- **AND** removing PR #1 from `org/app-a` does not affect `org/app-b`'s queue

#### Scenario: Independent branch queues within same repo
- **WHEN** PR #1 targets `main` in repo `org/app`
- **AND** PR #2 targets `release/1.0` in repo `org/app`
- **THEN** both PRs can be tested concurrently (one per target branch)
- **AND** each queue operates independently

### Requirement: State persistence
The system SHALL persist all queue state to PostgreSQL. On restart, the system SHALL recover queue state and resume processing from where it left off.

#### Scenario: Service restarts with active queue
- **WHEN** the service restarts while PR #42 is head-of-queue in `org/app`
- **THEN** after startup the system resumes monitoring PR #42's checks
- **AND** the queue order is preserved

#### Scenario: Service restarts with empty queues
- **WHEN** the service restarts with no PRs in any queue
- **THEN** the service starts normally and waits for new automerge-scheduled PRs
