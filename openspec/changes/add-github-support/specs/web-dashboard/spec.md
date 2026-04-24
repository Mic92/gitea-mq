## MODIFIED Requirements

### Requirement: Queue overview page
The system SHALL serve an HTML page at the root path (`/`) listing all currently managed repositories with a queue count badge. The repo list SHALL be read dynamically from the registry on each request, reflecting any changes from discovery. Each repository entry SHALL be a link to its repo detail page at `/repo/{forge}/{owner}/{name}`. Each entry SHALL display a forge indicator (icon or label `gitea`/`github`), the repository name, and a badge showing the number of active (queued + testing) PRs. The page SHALL NOT display head-of-queue PR information or per-PR state details.

#### Scenario: Multiple repos with queued PRs
- **WHEN** a user visits `/`
- **AND** `gitea:org/app` has 3 active PRs and `github:org/lib` has 1 active PR
- **THEN** the page displays each currently managed repository as a link
- **AND** `org/app` shows forge `gitea` and a badge with "3"
- **AND** `org/lib` shows forge `github` and a badge with "1"

#### Scenario: All queues empty
- **WHEN** a user visits `/` and no PRs are queued in any repo
- **THEN** the page displays all currently managed repos
- **AND** each repo shows a badge with "0" or an "empty" indicator

#### Scenario: No managed repositories
- **WHEN** a user visits `/` and no repositories are in the managed set
- **THEN** the page displays a helpful message: "No repositories discovered yet. Configure GITEA_MQ_REPOS / GITEA_MQ_GITHUB_APP_ID or set the topic on your repos."

#### Scenario: Repo added by discovery between requests
- **WHEN** a user visits `/` and `gitea:org/app` is the only managed repo
- **AND** the discovery loop adds `github:org/new-repo` before the next page load
- **THEN** the next page load shows both `gitea:org/app` and `github:org/new-repo`

#### Scenario: Repo removed by discovery between requests
- **WHEN** a user visits `/` and `gitea:org/app` and `gitea:org/old-repo` are managed
- **AND** the discovery loop removes `gitea:org/old-repo` before the next page load
- **THEN** the next page load shows only `gitea:org/app`

### Requirement: Repository queue detail page
The system SHALL serve an HTML page at `/repo/{forge}/{owner}/{name}` showing the list of active (queued + testing) pull requests for a specific repository. The system SHALL check whether the requested `(forge, owner, name)` is in the current managed set on each request. Each PR entry SHALL be a link to its PR detail page at `/repo/{forge}/{owner}/{name}/pr/{number}`. The page SHALL also display a link to the repository on its forge host. For backward compatibility, `/repo/{owner}/{name}` (no forge segment) SHALL be served as `forge=gitea`. The page SHALL NOT display check statuses — those are shown on the PR detail page.

#### Scenario: Repo with queued PRs
- **WHEN** a user visits `/repo/github/org/app` and `github:org/app` is in the managed set
- **AND** the queue has PR #42 (testing, position 1) and PR #43 (queued, position 2)
- **THEN** the page displays the queue for `github:org/app`
- **AND** each PR entry shows: PR number (as a link to `/repo/github/org/app/pr/{number}`), target branch, queue position, and current state
- **AND** the page does NOT display check statuses for any PR

#### Scenario: Repo with no queued PRs
- **WHEN** a user visits `/repo/gitea/org/app` and the queue is empty
- **THEN** the page displays "No PRs in queue"

#### Scenario: Unknown repo
- **WHEN** a user visits `/repo/gitea/org/unknown` and `gitea:org/unknown` is not in the managed set
- **THEN** the page returns HTTP 404

#### Scenario: Legacy two-segment path
- **WHEN** a user visits `/repo/org/app` and `gitea:org/app` is in the managed set
- **THEN** the page is served as if the path were `/repo/gitea/org/app`

#### Scenario: Repo removed between page loads
- **WHEN** a user visits `/repo/gitea/org/app` successfully
- **AND** `gitea:org/app` is removed from the managed set by the discovery loop
- **THEN** the next visit to `/repo/gitea/org/app` returns HTTP 404

### Requirement: Breadcrumb navigation
The system SHALL display a breadcrumb navigation trail on all dashboard pages. The breadcrumb SHALL use `›` as the separator. The current page segment SHALL be plain text (not linked). Parent segments SHALL be links. The repo segment SHALL show `<forge>:<owner>/<name>`. The breadcrumb SHALL replace any existing back-link navigation.

#### Scenario: Overview page breadcrumb
- **WHEN** a user visits `/`
- **THEN** the breadcrumb shows: `gitea-mq` (plain text, current page)

#### Scenario: Repo page breadcrumb
- **WHEN** a user visits `/repo/github/org/app`
- **THEN** the breadcrumb shows: `gitea-mq` (linked to `/`) › `github:org/app` (plain text, current page)

#### Scenario: PR page breadcrumb
- **WHEN** a user visits `/repo/gitea/org/app/pr/42`
- **THEN** the breadcrumb shows: `gitea-mq` (linked to `/`) › `gitea:org/app` (linked to `/repo/gitea/org/app`) › `PR #42` (plain text, current page)
