## MODIFIED Requirements

### Requirement: Queue overview page
The system SHALL serve an HTML page at the root path (`/`) listing all currently managed repositories with a queue count badge. The repo list SHALL be read dynamically from the registry on each request, reflecting any changes from topic-based discovery. Each repository entry SHALL be a link to its repo detail page at `/repo/{owner}/{name}`. Each entry SHALL display the repository name and a badge showing the number of active (queued + testing) PRs. The page SHALL NOT display head-of-queue PR information or per-PR state details.

#### Scenario: Multiple repos with queued PRs
- **WHEN** a user visits `/`
- **AND** `org/app` has 3 active PRs and `org/lib` has 1 active PR
- **THEN** the page displays each currently managed repository as a link
- **AND** `org/app` shows a badge with "3"
- **AND** `org/lib` shows a badge with "1"

#### Scenario: All queues empty
- **WHEN** a user visits `/` and no PRs are queued in any repo
- **THEN** the page displays all currently managed repos
- **AND** each repo shows a badge with "0" or an "empty" indicator

#### Scenario: No managed repositories
- **WHEN** a user visits `/` and no repositories are in the managed set
- **THEN** the page displays a helpful message: "No repositories discovered yet. Configure GITEA_MQ_REPOS or set the topic on your repos."

#### Scenario: Repo added by discovery between requests
- **WHEN** a user visits `/` and `org/app` is the only managed repo
- **AND** the discovery loop adds `org/new-repo` before the next page load
- **THEN** the next page load shows both `org/app` and `org/new-repo`

#### Scenario: Repo removed by discovery between requests
- **WHEN** a user visits `/` and `org/app` and `org/old-repo` are managed
- **AND** the discovery loop removes `org/old-repo` before the next page load
- **THEN** the next page load shows only `org/app`

### Requirement: Repository queue detail page
The system SHALL serve an HTML page at `/repo/{owner}/{name}` showing the list of active (queued + testing) pull requests for a specific repository. The system SHALL check whether the requested repo is in the current managed set on each request. Each PR entry SHALL be a link to its PR detail page at `/repo/{owner}/{name}/pr/{number}`. The page SHALL NOT display check statuses — those are shown on the PR detail page.

#### Scenario: Repo with queued PRs
- **WHEN** a user visits `/repo/org/app` and `org/app` is in the managed set
- **AND** the queue has PR #42 (testing, position 1) and PR #43 (queued, position 2)
- **THEN** the page displays the queue for `org/app`
- **AND** each PR entry shows: PR number (as a link to `/repo/org/app/pr/{number}`), target branch, queue position, and current state
- **AND** the page does NOT display check statuses for any PR

#### Scenario: Repo with no queued PRs
- **WHEN** a user visits `/repo/org/app` and the queue is empty
- **THEN** the page displays "No PRs in queue"

#### Scenario: Unknown repo
- **WHEN** a user visits `/repo/org/unknown` and `org/unknown` is not in the managed set
- **THEN** the page returns HTTP 404

#### Scenario: Repo removed between page loads
- **WHEN** a user visits `/repo/org/app` successfully
- **AND** `org/app` is removed from the managed set by the discovery loop
- **THEN** the next visit to `/repo/org/app` returns HTTP 404

### Requirement: Check status display
The system SHALL display the status of each required check for the head-of-queue PR on the PR detail page (at `/repo/{owner}/{name}/pr/{number}`). Each check SHALL show its name and current state (pending, success, failure, error). Check statuses SHALL NOT be displayed on the repository queue detail page.

#### Scenario: Head-of-queue PR with mixed check states
- **WHEN** PR #42 is head-of-queue in state `testing` and has checks: `ci/build` (success), `ci/lint` (pending), `ci/test` (failure)
- **AND** a user visits `/repo/org/app/pr/42`
- **THEN** the PR detail page shows all three checks with their respective states
- **AND** uses visual indicators (e.g., ✅ ❌ ⏳) for each state

#### Scenario: Check statuses not shown on repo page
- **WHEN** PR #42 is head-of-queue in state `testing` with check statuses
- **AND** a user visits `/repo/org/app`
- **THEN** the repo page does NOT display check statuses for PR #42

### Requirement: Breadcrumb navigation
The system SHALL display a breadcrumb navigation trail on all dashboard pages. The breadcrumb SHALL use `›` as the separator. The current page segment SHALL be plain text (not linked). Parent segments SHALL be links. The breadcrumb SHALL replace any existing back-link navigation.

#### Scenario: Overview page breadcrumb
- **WHEN** a user visits `/`
- **THEN** the breadcrumb shows: `gitea-mq` (plain text, current page)

#### Scenario: Repo page breadcrumb
- **WHEN** a user visits `/repo/org/app`
- **THEN** the breadcrumb shows: `gitea-mq` (linked to `/`) › `org/app` (plain text, current page)

#### Scenario: PR page breadcrumb
- **WHEN** a user visits `/repo/org/app/pr/42`
- **THEN** the breadcrumb shows: `gitea-mq` (linked to `/`) › `org/app` (linked to `/repo/org/app`) › `PR #42` (plain text, current page)

### Requirement: Auto-refresh
The system SHALL include auto-refresh functionality on all dashboard pages (overview, repo detail, PR detail) so users see updated state without manual reload. The refresh interval SHALL be configurable, defaulting to 10 seconds.

#### Scenario: Queue state changes
- **WHEN** a user has the dashboard open and a PR is merged
- **THEN** within the refresh interval the page updates to reflect the new queue state

### Requirement: No authentication required
The web dashboard SHALL be publicly accessible without authentication. All pages are read-only views of queue state.

#### Scenario: Unauthenticated access
- **WHEN** any user accesses the dashboard without credentials
- **THEN** all pages are accessible and display queue information

### Requirement: Server-rendered HTML
The dashboard SHALL be implemented as server-rendered HTML without JavaScript frameworks. Pages SHALL be functional without JavaScript enabled, with auto-refresh implemented via HTML meta refresh or minimal inline script.

#### Scenario: JavaScript disabled
- **WHEN** a user accesses the dashboard with JavaScript disabled
- **THEN** the page renders correctly and displays current queue state
- **AND** auto-refresh still works via `<meta http-equiv="refresh">`
