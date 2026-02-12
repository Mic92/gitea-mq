## MODIFIED Requirements

### Requirement: Commit status lifecycle reporting
The system SHALL post a `gitea-mq` commit status on the PR's original head commit reflecting the merge queue lifecycle: `pending` when queued, `pending` when testing, `success` when merge branch CI passes, `failure` when a check fails, `error` when timed out. Every `gitea-mq` commit status SHALL include a `target_url` pointing to the PR's dashboard page at `{GITEA_MQ_EXTERNAL_URL}/repo/{owner}/{name}/pr/{number}`. The URL SHALL be constructed using the `DashboardPRURL(baseURL, owner, repo, prNumber)` helper via `MQStatus(state, description, targetURL)`.

#### Scenario: Full lifecycle status updates
- **WHEN** PR #42 in `org/app` is enqueued
- **THEN** status is `pending` with description "Queued (position #3)" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`
- **WHEN** PR #42 becomes head-of-queue and merge branch is created
- **THEN** status is `pending` with description "Testing merge result" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`
- **WHEN** all checks pass on the merge branch
- **THEN** status is `success` with description "Merge queue passed" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`

#### Scenario: Failure lifecycle
- **WHEN** PR #42 in `org/app` is testing and check `ci/build` fails on the merge branch
- **THEN** status is `failure` with description "Check failed: ci/build" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`

#### Scenario: Timeout lifecycle
- **WHEN** PR #42 in `org/app` is testing and the timeout is exceeded
- **THEN** status is `error` with description "Check timeout exceeded" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`

#### Scenario: Merge conflict lifecycle
- **WHEN** PR #42 in `org/app` is head-of-queue and merge branch creation fails due to conflict
- **THEN** status is `failure` with description "Merge conflict with target branch" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`

#### Scenario: Merge branch creation error lifecycle
- **WHEN** PR #42 in `org/app` is head-of-queue and merge branch creation fails due to a non-conflict error
- **THEN** status is `error` with description "Failed to create merge branch" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`

#### Scenario: Automerge timeout lifecycle
- **WHEN** PR #42 in `org/app` has been in `success` state too long without being merged
- **THEN** status is `error` with description "Automerge did not complete in time" and `target_url` set to `https://mq.example.com/repo/org/app/pr/42`

## ADDED Requirements

### Requirement: ExternalURL required for commit status links
The system SHALL require the `GITEA_MQ_EXTERNAL_URL` environment variable to be set. The service SHALL fail to start if this variable is missing. The value SHALL be used as the base URL for constructing `target_url` values in commit statuses and for webhook auto-setup.

#### Scenario: ExternalURL set
- **WHEN** `GITEA_MQ_EXTERNAL_URL` is set to `https://mq.example.com`
- **THEN** the service starts successfully
- **AND** commit statuses use `https://mq.example.com` as the base for `target_url`

#### Scenario: ExternalURL not set
- **WHEN** `GITEA_MQ_EXTERNAL_URL` is not set
- **THEN** the service fails to start with an error message listing `GITEA_MQ_EXTERNAL_URL` as a missing required variable

#### Scenario: ExternalURL with trailing slash
- **WHEN** `GITEA_MQ_EXTERNAL_URL` is set to `https://mq.example.com/`
- **THEN** the `target_url` SHALL NOT contain a double slash (the trailing slash SHALL be stripped before constructing URLs)
