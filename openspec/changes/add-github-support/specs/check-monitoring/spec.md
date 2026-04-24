## MODIFIED Requirements

### Requirement: Commit status lifecycle reporting
The system SHALL post a `gitea-mq` status on the PR's original head commit, via the repo's forge (`Forge.SetMQStatus`), reflecting the merge queue lifecycle: `pending` when queued, `pending` when testing, `success` when merge branch CI passes, `failure` when a check fails, `error` when timed out. On Gitea this SHALL be a commit status; on GitHub this SHALL be a check run named `gitea-mq`. Every `gitea-mq` status SHALL include a target/details URL pointing to the PR's dashboard page at `{GITEA_MQ_EXTERNAL_URL}/repo/{forge}/{owner}/{name}/pr/{number}`. The URL SHALL be constructed using the `DashboardPRURL(baseURL, forge, owner, repo, prNumber)` helper via `MQStatus(state, description, targetURL)`.

#### Scenario: Full lifecycle status updates
- **WHEN** PR #42 in `gitea:org/app` is enqueued
- **THEN** status is `pending` with description "Queued (position #3)" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`
- **WHEN** PR #42 becomes head-of-queue and merge branch is created
- **THEN** status is `pending` with description "Testing merge result" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`
- **WHEN** all checks pass on the merge branch
- **THEN** status is `success` with description "Merge queue passed" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`

#### Scenario: GitHub lifecycle via check run
- **WHEN** PR #7 in `github:alice/tool` is enqueued
- **THEN** a `gitea-mq` check run is created with `status=queued`, summary "Queued (position #1)" and `details_url` set to `https://mq.example.com/repo/github/alice/tool/pr/7`

#### Scenario: Failure lifecycle
- **WHEN** PR #42 in `gitea:org/app` is testing and check `ci/build` fails on the merge branch
- **THEN** status is `failure` with description "Check failed: ci/build" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`

#### Scenario: Timeout lifecycle
- **WHEN** PR #42 in `gitea:org/app` is testing and the timeout is exceeded
- **THEN** status is `error` with description "Check timeout exceeded" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`

#### Scenario: Merge conflict lifecycle
- **WHEN** PR #42 in `gitea:org/app` is head-of-queue and merge branch creation fails due to conflict
- **THEN** status is `failure` with description "Merge conflict with target branch" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`

#### Scenario: Merge branch creation error lifecycle
- **WHEN** PR #42 in `gitea:org/app` is head-of-queue and merge branch creation fails due to a non-conflict error
- **THEN** status is `error` with description "Failed to create merge branch" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`

#### Scenario: Automerge timeout lifecycle
- **WHEN** PR #42 in `gitea:org/app` has been in `success` state too long without being merged
- **THEN** status is `error` with description "Automerge did not complete in time" and `target_url` set to `https://mq.example.com/repo/gitea/org/app/pr/42`

## ADDED Requirements

### Requirement: Required-check resolution via forge
The system SHALL determine the set of required check contexts for a head-of-queue PR by calling `Forge.GetRequiredChecks(owner, name, targetBranch)` on the repo's forge. The Gitea forge reads branch-protection `status_check_contexts`; the GitHub forge reads active ruleset `required_status_checks` with classic branch protection as fallback. If the forge returns an empty set, `GITEA_MQ_REQUIRED_CHECKS` SHALL be used as the fallback. The `gitea-mq` context SHALL always be excluded.

#### Scenario: GitHub ruleset checks
- **WHEN** PR #7 in `github:org/app` targets `main` and the active ruleset requires `[ci/build, gitea-mq]`
- **THEN** the monitor waits for `[ci/build]` on the merge branch

#### Scenario: Fallback when forge returns empty
- **WHEN** the forge returns no required checks for `gitea:org/app` `main`
- **AND** `GITEA_MQ_REQUIRED_CHECKS=ci/build,ci/test`
- **THEN** the monitor waits for `[ci/build, ci/test]`

### Requirement: Check-state ingestion via forge
The system SHALL obtain check states for the merge-branch SHA via the repo's forge: webhook events routed from the forge-specific endpoint, plus on-demand `Forge.GetCheckStates(owner, name, sha)` for reconciliation. The GitHub forge SHALL combine check-run conclusions and commit statuses into a unified `context → state` map.

#### Scenario: GitHub check run reported via webhook
- **WHEN** a `check_run` `completed` webhook with name `ci/build` and conclusion `success` arrives for the merge-branch SHA of `github:org/app` PR #7
- **THEN** the monitor records `ci/build = success` for that entry

#### Scenario: Reconcile via API after restart
- **WHEN** the service restarts while `github:org/app` PR #7 is testing
- **THEN** the monitor calls `Forge.GetCheckStates` on the merge-branch SHA to recover current check states
