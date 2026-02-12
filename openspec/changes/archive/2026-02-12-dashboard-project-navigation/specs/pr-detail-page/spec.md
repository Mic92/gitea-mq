## ADDED Requirements

### Requirement: PR detail page
The system SHALL serve an HTML page at `/repo/{owner}/{name}/pr/{number}` showing the detail view for a single pull request in the merge queue. The system SHALL check whether the requested repo is in the current managed set on each request. The PR number SHALL be parsed as a positive integer; invalid formats SHALL return HTTP 404.

#### Scenario: Head-of-queue PR in testing state
- **WHEN** a user visits `/repo/org/app/pr/42` and `org/app` is in the managed set
- **AND** PR #42 is the head-of-queue entry in state `testing`
- **THEN** the page displays PR number, title, author, state (`testing`), queue position (1), and enqueued time
- **AND** the page displays a check statuses table showing each check's name and state with visual indicators (✅ ❌ ⏳)

#### Scenario: Non-head PR in queued state
- **WHEN** a user visits `/repo/org/app/pr/43` and `org/app` is in the managed set
- **AND** PR #43 is in state `queued` at position 2
- **THEN** the page displays PR number, title, author, state (`queued`), queue position (2), and enqueued time
- **AND** the page does NOT display a check statuses section

#### Scenario: PR not in queue
- **WHEN** a user visits `/repo/org/app/pr/99` and `org/app` is in the managed set
- **AND** PR #99 is not in the queue (no active entry)
- **THEN** the page returns HTTP 200
- **AND** displays "PR #99 is not in the merge queue"
- **AND** includes a breadcrumb link back to the repo page

#### Scenario: Unknown repo
- **WHEN** a user visits `/repo/org/unknown/pr/42` and `org/unknown` is not in the managed set
- **THEN** the page returns HTTP 404

#### Scenario: Invalid PR number format
- **WHEN** a user visits `/repo/org/app/pr/abc`
- **THEN** the page returns HTTP 404

#### Scenario: Negative PR number
- **WHEN** a user visits `/repo/org/app/pr/-1`
- **THEN** the page returns HTTP 404

### Requirement: PR title and author from Gitea API
The system SHALL fetch the PR title and author from the Gitea API (`GET /repos/{owner}/{repo}/pulls/{number}`) on each page load of the PR detail page. The title SHALL be displayed as-is. The author SHALL be the PR author's login name.

#### Scenario: Gitea API returns PR data
- **WHEN** a user visits `/repo/org/app/pr/42`
- **AND** the Gitea API returns PR #42 with title "Fix login bug" and author "alice"
- **THEN** the page displays title "Fix login bug" and author "alice"

#### Scenario: Gitea API unavailable
- **WHEN** a user visits `/repo/org/app/pr/42`
- **AND** the Gitea API call fails (timeout, 500, network error)
- **THEN** the page renders successfully with "—" as placeholder for title and author
- **AND** all other information (state, position, checks) is displayed normally

### Requirement: PR detail breadcrumb navigation
The system SHALL display a breadcrumb trail on the PR detail page in the format: `gitea-mq` › `owner/repo` › `PR #N`. The `gitea-mq` segment SHALL link to `/`. The `owner/repo` segment SHALL link to `/repo/{owner}/{name}`. The `PR #N` segment SHALL be plain text (current page).

#### Scenario: Breadcrumb links
- **WHEN** a user visits `/repo/org/app/pr/42`
- **THEN** the breadcrumb shows: `gitea-mq` (linked to `/`) › `org/app` (linked to `/repo/org/app`) › `PR #42` (plain text)

### Requirement: PR detail auto-refresh
The system SHALL include auto-refresh on the PR detail page using the same mechanism and interval as other dashboard pages (configurable, default 10 seconds).

#### Scenario: Check status updates during refresh
- **WHEN** a user has the PR detail page open for head-of-queue PR #42
- **AND** check `ci/build` transitions from `pending` to `success`
- **THEN** within the refresh interval the page updates to show the new check state
