## ADDED Requirements

### Requirement: Batch formation
When `GITEA_MQ_BATCH_MAX` ≠ 1 and no live batch exists for a `(repo, target-branch)` queue, the system SHALL form a batch from the first `min(BATCH_MAX, queued)` entries in FIFO order (`BATCH_MAX = 0` means all queued). The batch SHALL be a single `batches` row with `member_ids = current_ids =` those entry IDs; each member entry's `active_batch_id` SHALL reference it. At most one live (`forming|testing`) batch per queue SHALL exist.

#### Scenario: Form batch from queued PRs
- **WHEN** `BATCH_MAX=4`, the `main` queue holds [#10,#20,#30] and no live batch exists
- **THEN** a batch is created with `member_ids=current_ids=[#10,#20,#30]`
- **AND** all three entries are `testing` with `active_batch_id` set

#### Scenario: BATCH_MAX=1 disables batching
- **WHEN** `BATCH_MAX=1`
- **THEN** no `batches` row is created and the single-PR flow runs unchanged

#### Scenario: Singleton up-to-date in batch mode uses forge automerge
- **WHEN** `BATCH_MAX=4` but only PR #10 is queued, #10 already contains target tip, and `SKIP_QUEUE_IF_UP_TO_DATE=true`
- **THEN** the batch row is discarded and #10 is processed via the legacy forge-automerge path

### Requirement: Batch merge branch
The system SHALL build branch `gitea-mq/batch/<id>` by merging each `current_ids` member's head SHA, in order, onto the current target tip. The branch name is reused across rebuilds. A member that conflicts SHALL be ejected (automerge cancelled, `failure` status, comment) and construction continues with the rest.

#### Scenario: One member conflicts during construction
- **WHEN** building `current_ids=[#10,#20,#30]` and merging #20 conflicts
- **THEN** #20 is ejected with `failure` "Merge conflict in batch"
- **AND** the batch proceeds to testing with `current_ids=[#10,#30]`

### Requirement: Batch pass fast-forwards target
When all required checks pass on the batch branch, the system SHALL fast-forward the target branch to `branch_sha` (non-force), set `gitea-mq=success` on each `current_ids` member's head, dequeue them, move them to `landed_ids`, then poll each PR for `merged=true` briefly and close+comment any that the forge has not detected. Finally it SHALL pop the next slice from `pending` and rebuild, or finish if `pending` is empty.

#### Scenario: Batch passes
- **WHEN** checks on `gitea-mq/batch/7` (`current_ids=[#10,#20,#30]` → `main`) succeed
- **THEN** `main` is updated to `branch_sha`
- **AND** each member receives `gitea-mq` `success` "Merged via batch #7"
- **AND** each PR is reported merged by the forge (or closed with a "Merged as <sha>" comment)

#### Scenario: Target moved before fast-forward
- **WHEN** `FastForward` returns `ErrNotFastForward`
- **THEN** `ff_retries` is incremented and the same `current_ids` are rebuilt from the new target tip
- **AND** after 3 consecutive `ErrNotFastForward` the `current_ids` members are ejected with `error` "Target branch moved repeatedly"

#### Scenario: Push permission denied
- **WHEN** `FastForward` is rejected by branch protection
- **THEN** every `current_ids` member is ejected with `error` naming the branch and the user to whitelist

### Requirement: Batch fail bisection
When a batch with `len(current_ids) > 1` fails or times out, the system SHALL split `current_ids` in half, push the second half onto `pending`, set `current_ids` to the first half, and rebuild from the current target tip. When `len(current_ids) == 1` fails, that member SHALL be ejected with the failing check attributed and the next slice popped from `pending`. When `BISECT_MAX_STEPS > 0` and `builds ≥ BISECT_MAX_STEPS`, every `current_ids` member SHALL be ejected with `error` "Bisection limit reached" instead of splitting.

#### Scenario: Bisect lands good halves and ejects culprit
- **WHEN** `current=[#10,#20,#30,#40]` fails `ci/test`
- **THEN** `pending=[[#30,#40]]`, `current=[#10,#20]`, rebuild
- **WHEN** `[#10,#20]` passes
- **THEN** `main` fast-forwards, #10/#20 merged, `current=[#30,#40]` popped, rebuild on new `main`
- **WHEN** `[#30,#40]` fails, `[#30]` passes (land), `[#40]` fails
- **THEN** #40 is ejected with `failure` "Check failed: ci/test"; batch is `done`

#### Scenario: Both halves pass
- **WHEN** the root build fails but every subsequent slice passes and `ejected_ids` is empty at `done`
- **THEN** `flaky=true` is recorded and one informational comment is posted on each member

#### Scenario: Bisection build limit
- **WHEN** `BISECT_MAX_STEPS=2`, `builds=2`, and `current_ids` fails with size > 1
- **THEN** every `current_ids` member is ejected with `error` "Bisection limit reached"

### Requirement: Member change during live batch
If a member entry with `active_batch_id` set is removed (new push, automerge cancelled, closed, retargeted), the system SHALL eject it, drop its id from `current_ids` and every `pending` slice, and — only if it was in `current_ids` — rebuild the trimmed `current_ids`. The batch row remains live; no new root is formed.

#### Scenario: Push to a PR in current
- **WHEN** `current=[#10,#20]` is testing and #20 receives new commits
- **THEN** #20 is ejected, `current=[#10]`, the branch is rebuilt, and the previous `branch_sha`'s check events are ignored

#### Scenario: Push to a PR only in pending
- **WHEN** `current=[#10]` is testing, `pending=[[#20,#30]]`, and #30 receives new commits
- **THEN** #30 is ejected, `pending=[[#20]]`, and testing of `current` continues uninterrupted

### Requirement: Ignore stale batch check events
The system SHALL act on a check-status event only if its SHA matches a batch with `state='testing'` and equals that batch's current `branch_sha`.

#### Scenario: Late event for superseded build
- **WHEN** a rebuild changed `branch_sha` from `S1` to `S2`
- **AND** a `failure` status arrives for `S1`
- **THEN** the event is ignored

## MODIFIED Requirements

### Requirement: One PR at a time per repo
The system SHALL run at most one live **batch** per `(forge, repo, target-branch)` queue. When `BATCH_MAX=1` a batch is exactly one PR. A new root batch SHALL be formed only when no live batch exists for the queue.

#### Scenario: Live batch blocks new root
- **WHEN** a batch is `testing` on `main`
- **AND** PR #50 is enqueued for `main`
- **THEN** #50 stays `queued` and is not added to the live batch

### Requirement: Temporary merge branch for testing
Single-PR mode uses `gitea-mq/<pr>`. Batch mode uses `gitea-mq/batch/<id>`, reused across rebuilds. The system SHALL delete the batch branch when the batch reaches `done`/`cancelled`, and SHALL reap orphan `gitea-mq/batch/*` branches at startup.

#### Scenario: Batch branch naming
- **WHEN** batch id 12 is formed
- **THEN** its branch is `gitea-mq/batch/12`

### Requirement: Signal Gitea to merge on success
Single-PR mode is unchanged. In batch mode the system performs the merge itself via `FastForward`; it SHALL NOT wait for forge automerge before dequeuing batch members, and SHALL close (with comment) any PR the forge does not mark merged within a short poll window.

#### Scenario: Batch mode does not wait for forge automerge
- **WHEN** a batch fast-forward succeeds
- **THEN** members are dequeued immediately

### Requirement: State persistence
In addition to queue entries, the system SHALL persist the live batch row including `member_ids`, `current_ids`, `pending`, `landed_ids`, `ejected_ids`, `branch_sha`, `builds`, and `ff_retries`. On restart it SHALL resume monitoring a `testing` batch's `branch_sha` and SHALL re-run `Build` for a batch found in `forming`.

#### Scenario: Restart mid-bisection
- **WHEN** the service restarts while batch #7 has `current=[#30,#40]`, `pending=[]`, `state='testing'`
- **THEN** monitoring of `branch_sha` resumes and no duplicate rebuild occurs
