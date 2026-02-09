// Package gitea provides a client interface and HTTP implementation for the
// Gitea REST API. The interface enables TDD — core queue and monitoring logic
// can be tested entirely with mocks.
package gitea

import (
	"context"
	"time"
)

// PR represents a pull request from the Gitea API.
// Field names and JSON tags match the Gitea API response.
type PR struct {
	ID        int64      `json:"id"`
	Index     int64      `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"` // "open", "closed"
	HasMerged bool       `json:"merged"`
	Merged    *time.Time `json:"merged_at"`
	User      *User      `json:"user"`
	Head      *PRRef     `json:"head"`
	Base      *PRRef     `json:"base"`
	HTMLURL   string     `json:"html_url"`
}

// PRRef holds a branch ref and its current SHA.
// Gitea uses "label" for the branch name in the JSON, "ref" for the short ref,
// and "sha" for the commit.
type PRRef struct {
	Label  string `json:"label"`
	Ref    string `json:"ref"`
	Sha    string `json:"sha"`
	RepoID int64  `json:"repo_id"`
}

// User represents a Gitea user (subset of fields).
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// TimelineComment represents a comment in a PR's timeline.
// The Type field is the string representation of Gitea's internal CommentType.
// Relevant values:
//   - "pull_scheduled_merge" (type 34) — automerge scheduled
//   - "pull_cancel_scheduled_merge" (type 35) — automerge cancelled
type TimelineComment struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// CommitStatus is a status to post on a commit via
// POST /repos/{owner}/{repo}/statuses/{sha}.
type CommitStatus struct {
	Context     string `json:"context"`
	State       string `json:"state"` // "pending", "success", "failure", "error"
	Description string `json:"description"`
	TargetURL   string `json:"target_url,omitempty"`
}

// BranchProtection holds the relevant fields from a branch protection rule.
// Matches Gitea's BranchProtection API response.
type BranchProtection struct {
	BranchName          string   `json:"branch_name"`
	RuleName            string   `json:"rule_name"`
	EnableStatusCheck   bool     `json:"enable_status_check"`
	StatusCheckContexts []string `json:"status_check_contexts"`
}

// MergeResult holds the outcome of merging two branches.
type MergeResult struct {
	SHA string // SHA of the merge commit on the new branch
}

// Webhook represents a Gitea webhook.
// Config is map[string]string per the Gitea API.
type Webhook struct {
	ID     int64             `json:"id"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
	Events []string          `json:"events"`
	Active bool              `json:"active"`
}

// EditBranchProtectionOpts holds options for editing branch protection.
// Only the fields we need — Gitea accepts partial updates via PATCH.
type EditBranchProtectionOpts struct {
	EnableStatusCheck   *bool    `json:"enable_status_check,omitempty"`
	StatusCheckContexts []string `json:"status_check_contexts"`
}

// CreateWebhookOpts holds options for creating a webhook via
// POST /repos/{owner}/{repo}/hooks.
type CreateWebhookOpts struct {
	Type   string            `json:"type"` // "gitea"
	Events []string          `json:"events"`
	Active bool              `json:"active"`
	Config map[string]string `json:"config"`
}

// Client defines the Gitea API surface used by gitea-mq.
// All methods accept a context for cancellation and return an error on failure.
type Client interface {
	// ListOpenPRs returns all open pull requests for a repository.
	ListOpenPRs(ctx context.Context, owner, repo string) ([]PR, error)

	// GetPR returns a single pull request by index.
	GetPR(ctx context.Context, owner, repo string, index int64) (*PR, error)

	// GetPRTimeline returns timeline comments for a pull request.
	// Used to detect automerge scheduling via "pull_scheduled_merge" /
	// "pull_cancel_scheduled_merge" comment types.
	GetPRTimeline(ctx context.Context, owner, repo string, index int64) ([]TimelineComment, error)

	// CreateCommitStatus posts a commit status on a specific SHA.
	// POST /repos/{owner}/{repo}/statuses/{sha}
	CreateCommitStatus(ctx context.Context, owner, repo, sha string, status CommitStatus) error

	// CreateComment posts a comment on a pull request.
	// POST /repos/{owner}/{repo}/issues/{index}/comments
	CreateComment(ctx context.Context, owner, repo string, index int64, body string) error

	// CancelAutoMerge cancels the scheduled automerge for a pull request.
	// DELETE /repos/{owner}/{repo}/pulls/{index}/merge
	CancelAutoMerge(ctx context.Context, owner, repo string, index int64) error

	// GetBranchProtection returns the branch protection rule for a branch.
	// GET /repos/{owner}/{repo}/branch_protections/{name}
	GetBranchProtection(ctx context.Context, owner, repo, branch string) (*BranchProtection, error)

	// CreateBranch creates a new branch from a target ref.
	// POST /repos/{owner}/{repo}/branches
	CreateBranch(ctx context.Context, owner, repo, name, target string) error

	// DeleteBranch deletes a branch.
	// DELETE /repos/{owner}/{repo}/branches/{branch}
	DeleteBranch(ctx context.Context, owner, repo, name string) error

	// MergeBranches creates a temporary merge of head into base, pushed as
	// a new branch named mq/<pr>. Returns the merge SHA, or an error if
	// there are conflicts.
	MergeBranches(ctx context.Context, owner, repo, base, head string) (*MergeResult, error)

	// ListBranchProtections lists all branch protection rules for a repository.
	// GET /repos/{owner}/{repo}/branch_protections
	ListBranchProtections(ctx context.Context, owner, repo string) ([]BranchProtection, error)

	// EditBranchProtection updates a branch protection rule.
	// PATCH /repos/{owner}/{repo}/branch_protections/{name}
	EditBranchProtection(ctx context.Context, owner, repo, name string, opts EditBranchProtectionOpts) error

	// ListWebhooks lists all webhooks for a repository.
	// GET /repos/{owner}/{repo}/hooks
	ListWebhooks(ctx context.Context, owner, repo string) ([]Webhook, error)

	// CreateWebhook creates a webhook on a repository.
	// POST /repos/{owner}/{repo}/hooks
	CreateWebhook(ctx context.Context, owner, repo string, opts CreateWebhookOpts) error
}
