// Package forge defines a forge-agnostic interface for repo, PR, status,
// branch, comment and setup operations. Concrete implementations live in
// sibling packages (internal/gitea, internal/github).
package forge

import (
	"context"
	"fmt"
	"strings"

	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// Kind identifies the hosting forge of a repository.
type Kind string

const (
	KindGitea  Kind = "gitea"
	KindGithub Kind = "github"
)

// Valid reports whether k is a known forge kind.
func (k Kind) Valid() bool {
	switch k {
	case KindGitea, KindGithub:
		return true
	}
	return false
}

// RepoRef identifies a repository on a specific forge.
type RepoRef struct {
	Forge Kind
	Owner string
	Name  string
}

// String returns the canonical "<forge>:<owner>/<name>" form.
func (r RepoRef) String() string {
	return string(r.Forge) + ":" + r.Owner + "/" + r.Name
}

// ParseRepoRef parses a "<forge>:<owner>/<name>" string.
// Returns false on invalid format or unknown forge kind.
func ParseRepoRef(s string) (RepoRef, bool) {
	forge, rest, ok := strings.Cut(s, ":")
	if !ok {
		return RepoRef{}, false
	}
	k := Kind(forge)
	if !k.Valid() {
		return RepoRef{}, false
	}
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || name == "" {
		return RepoRef{}, false
	}
	return RepoRef{Forge: k, Owner: owner, Name: name}, true
}

// PR is a forge-agnostic pull request.
//
// AutoMergeEnabled normalises forge-specific signals (Gitea timeline comments,
// GitHub auto_merge field) so callers do not handle forge internals.
// NodeID carries the GitHub GraphQL node id when available; empty for Gitea.
type PR struct {
	Number           int64
	Title            string
	Body             string
	State            string // "open", "closed"
	Merged           bool
	AuthorLogin      string
	HeadBranch       string
	HeadSHA          string
	BaseBranch       string
	HTMLURL          string
	AutoMergeEnabled bool
	NodeID           string
}

// MQStatus is the lifecycle state reported by gitea-mq for a head SHA.
type MQStatus struct {
	State       CheckState
	Description string
	TargetURL   string
}

// CheckState aliases pg.CheckState so callers do not import the store package.
type CheckState = pg.CheckState

// SetupConfig holds inputs to EnsureRepoSetup.
type SetupConfig struct {
	// ExternalURL is the public base URL of the gitea-mq instance. Used by
	// Gitea for webhook URL construction and as the dashboard link target.
	ExternalURL string
	// WebhookSecret is the shared secret for Gitea webhook signatures.
	// Ignored by GitHub adapters (App webhook is configured out-of-band).
	WebhookSecret string
}

// Forge abstracts all operations gitea-mq performs against a hosting forge.
type Forge interface {
	Kind() Kind
	RepoHTMLURL(owner, name string) string
	PRHTMLURL(owner, name string, number int64) string

	ListOpenPRs(ctx context.Context, owner, name string) ([]PR, error)
	GetPR(ctx context.Context, owner, name string, number int64) (*PR, error)
	// ListAutoMergePRs returns open PRs currently scheduled for auto-merge.
	ListAutoMergePRs(ctx context.Context, owner, name string) ([]PR, error)

	SetMQStatus(ctx context.Context, owner, name, sha string, st MQStatus) error
	GetRequiredChecks(ctx context.Context, owner, name, branch string) ([]string, error)
	GetCheckStates(ctx context.Context, owner, name, sha string) (map[string]CheckState, error)

	// CreateMergeBranch creates branch at base's tip and merges headSHA into it.
	// conflict=true (err=nil) indicates the merge could not complete.
	CreateMergeBranch(ctx context.Context, owner, name, base, headSHA, branch string) (sha string, conflict bool, err error)
	DeleteBranch(ctx context.Context, owner, name, branch string) error
	ListBranches(ctx context.Context, owner, name string) ([]string, error)

	CancelAutoMerge(ctx context.Context, owner, name string, number int64) error
	Comment(ctx context.Context, owner, name string, number int64, body string) error

	EnsureRepoSetup(ctx context.Context, owner, name string, cfg SetupConfig) error
}

// UnknownForgeError is returned by Set.For when no adapter is registered for
// a ref's forge kind.
type UnknownForgeError struct {
	Kind Kind
}

func (e *UnknownForgeError) Error() string {
	return fmt.Sprintf("forge: no adapter registered for kind %q", e.Kind)
}
