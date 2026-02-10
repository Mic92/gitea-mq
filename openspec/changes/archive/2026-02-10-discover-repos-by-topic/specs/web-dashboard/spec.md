## MODIFIED Requirements

### Requirement: Queue overview page
The system SHALL serve an HTML page at the root path (`/`) listing all currently managed repositories and their current queue status. The repo list SHALL be read dynamically from the registry on each request, reflecting any changes from topic-based discovery.

#### Scenario: Multiple repos with queued PRs
- **WHEN** a user visits `/`
- **THEN** the page displays each currently managed repository
- **AND** for each repo shows: repo name, number of PRs in queue, and the head-of-queue PR (if any)

#### Scenario: All queues empty
- **WHEN** a user visits `/` and no PRs are queued in any repo
- **THEN** the page displays all currently managed repos with "Queue empty" status

#### Scenario: Repo added by discovery between requests
- **WHEN** a user visits `/` and `org/app` is the only managed repo
- **AND** the discovery loop adds `org/new-repo` before the next page load
- **THEN** the next page load shows both `org/app` and `org/new-repo`

#### Scenario: Repo removed by discovery between requests
- **WHEN** a user visits `/` and `org/app` and `org/old-repo` are managed
- **AND** the discovery loop removes `org/old-repo` before the next page load
- **THEN** the next page load shows only `org/app`

### Requirement: Repository queue detail page
The system SHALL serve an HTML page at `/repo/{owner}/{name}` showing the detailed queue for a specific repository. The system SHALL check whether the requested repo is in the current managed set on each request.

#### Scenario: Repo with queued PRs
- **WHEN** a user visits `/repo/org/app` and `org/app` is in the managed set
- **THEN** the page displays the queue for `org/app`
- **AND** each PR entry shows: PR number, title, author, queue position, and current state (queued, testing, merged, failed)
- **AND** the head-of-queue PR shows which checks have passed, which are pending, and which have failed

#### Scenario: Repo with no queued PRs
- **WHEN** a user visits `/repo/org/app` and the queue is empty
- **THEN** the page displays "No PRs in queue"

#### Scenario: Unknown repo
- **WHEN** a user visits `/repo/org/unknown` and `org/unknown` is not in the managed set
- **THEN** the page displays HTTP 404

#### Scenario: Repo removed between page loads
- **WHEN** a user visits `/repo/org/app` successfully
- **AND** `org/app` is removed from the managed set by the discovery loop
- **THEN** the next visit to `/repo/org/app` returns HTTP 404
