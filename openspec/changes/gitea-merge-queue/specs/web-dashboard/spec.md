## ADDED Requirements

### Requirement: Queue overview page
The system SHALL serve an HTML page at the root path (`/`) listing all managed repositories and their current queue status.

#### Scenario: Multiple repos with queued PRs
- **WHEN** a user visits `/`
- **THEN** the page displays each managed repository
- **AND** for each repo shows: repo name, number of PRs in queue, and the head-of-queue PR (if any)

#### Scenario: All queues empty
- **WHEN** a user visits `/` and no PRs are queued in any repo
- **THEN** the page displays all repos with "Queue empty" status

### Requirement: Repository queue detail page
The system SHALL serve an HTML page at `/repo/{owner}/{name}` showing the detailed queue for a specific repository. The page SHALL list all queued PRs in order with their current state.

#### Scenario: Repo with queued PRs
- **WHEN** a user visits `/repo/org/app`
- **THEN** the page displays the queue for `org/app`
- **AND** each PR entry shows: PR number, title, author, queue position, and current state (queued, testing, merged, failed)
- **AND** the head-of-queue PR shows which checks have passed, which are pending, and which have failed

#### Scenario: Repo with no queued PRs
- **WHEN** a user visits `/repo/org/app` and the queue is empty
- **THEN** the page displays "No PRs in queue"

#### Scenario: Unknown repo
- **WHEN** a user visits `/repo/org/unknown`
- **THEN** the page displays HTTP 404

### Requirement: Check status display
The system SHALL display the status of each required check for the head-of-queue PR on the repository detail page. Each check SHALL show its name and current state (pending, success, failure, error).

#### Scenario: Head-of-queue PR with mixed check states
- **WHEN** PR #42 is head-of-queue and has checks: `ci/build` (success), `ci/lint` (pending), `ci/test` (failure)
- **THEN** the detail page shows all three checks with their respective states
- **AND** uses visual indicators (e.g., ✅ ❌ ⏳) for each state

### Requirement: Auto-refresh
The system SHALL include auto-refresh functionality on dashboard pages so users see updated state without manual reload. The refresh interval SHALL be configurable, defaulting to 10 seconds.

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
