## MODIFIED Requirements

### Requirement: PR detail page
The system SHALL serve an HTML page at `/repo/{forge}/{owner}/{name}/pr/{number}` showing the detail view for a single pull request in the merge queue. The system SHALL check whether the requested `(forge, owner, name)` is in the current managed set on each request. The PR number SHALL be parsed as a positive integer; invalid formats SHALL return HTTP 404. For backward compatibility, `/repo/{owner}/{name}/pr/{number}` (no forge segment) SHALL be served as `forge=gitea`. The page SHALL include a link to the PR on its forge host.

#### Scenario: Head-of-queue PR in testing state
- **WHEN** a user visits `/repo/github/org/app/pr/42` and `github:org/app` is in the managed set
- **AND** PR #42 is the head-of-queue entry in state `testing`
- **THEN** the page displays PR number, title, author, state (`testing`), queue position (1), and enqueued time
- **AND** the page displays a check statuses table showing each check's name and state with visual indicators (✅ ❌ ⏳)
- **AND** the page displays a link to `https://github.com/org/app/pull/42`

#### Scenario: Non-head PR in queued state
- **WHEN** a user visits `/repo/gitea/org/app/pr/43` and `gitea:org/app` is in the managed set
- **AND** PR #43 is in state `queued` at position 2
- **THEN** the page displays PR number, title, author, state (`queued`), queue position (2), and enqueued time
- **AND** the page does NOT display a check statuses section

#### Scenario: PR not in queue
- **WHEN** a user visits `/repo/gitea/org/app/pr/99` and `gitea:org/app` is in the managed set
- **AND** PR #99 is not in the queue (no active entry)
- **THEN** the page returns HTTP 200
- **AND** displays "PR #99 is not in the merge queue"
- **AND** includes a breadcrumb link back to the repo page

#### Scenario: Unknown repo
- **WHEN** a user visits `/repo/gitea/org/unknown/pr/42` and `gitea:org/unknown` is not in the managed set
- **THEN** the page returns HTTP 404

#### Scenario: Invalid PR number format
- **WHEN** a user visits `/repo/gitea/org/app/pr/abc`
- **THEN** the page returns HTTP 404

#### Scenario: Negative PR number
- **WHEN** a user visits `/repo/gitea/org/app/pr/-1`
- **THEN** the page returns HTTP 404

#### Scenario: Legacy path without forge segment
- **WHEN** a user visits `/repo/org/app/pr/42` and `gitea:org/app` is in the managed set
- **THEN** the page is served as if the path were `/repo/gitea/org/app/pr/42`

### Requirement: PR title and author from Gitea API
The system SHALL fetch the PR title and author via the repo's forge (`Forge.GetPR`) on each page load of the PR detail page. The title SHALL be displayed as-is. The author SHALL be the PR author's login name.

#### Scenario: Gitea API returns PR data
- **WHEN** a user visits `/repo/gitea/org/app/pr/42`
- **AND** the Gitea API returns PR #42 with title "Fix login bug" and author "alice"
- **THEN** the page displays title "Fix login bug" and author "alice"

#### Scenario: GitHub API returns PR data
- **WHEN** a user visits `/repo/github/alice/tool/pr/7`
- **AND** the GitHub forge returns PR #7 with title "Add cache" and author "bob"
- **THEN** the page displays title "Add cache" and author "bob"

#### Scenario: Gitea API unavailable
- **WHEN** a user visits `/repo/gitea/org/app/pr/42`
- **AND** the forge API call fails (timeout, 500, network error)
- **THEN** the page renders successfully with "—" as placeholder for title and author
- **AND** all other information (state, position, checks) is displayed normally

### Requirement: PR detail breadcrumb navigation
The system SHALL display a breadcrumb trail on the PR detail page in the format: `gitea-mq` › `<forge>:<owner>/<repo>` › `PR #N`. The `gitea-mq` segment SHALL link to `/`. The repo segment SHALL link to `/repo/{forge}/{owner}/{name}`. The `PR #N` segment SHALL be plain text (current page).

#### Scenario: Breadcrumb links
- **WHEN** a user visits `/repo/github/org/app/pr/42`
- **THEN** the breadcrumb shows: `gitea-mq` (linked to `/`) › `github:org/app` (linked to `/repo/github/org/app`) › `PR #42` (plain text)
