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

const (
	MQContext           = "gitea-mq"
	MirrorContextPrefix = MQContext + "/"
)

// IsOwnContext recognises statuses gitea-mq itself produced so callers can
// drop them before they feed back into the monitor.
func IsOwnContext(ctx string) bool {
	return ctx == MQContext || strings.HasPrefix(ctx, MirrorContextPrefix)
}

// DashboardPRURL builds the gitea-mq dashboard link for a PR. It is the
// target_url of every MQStatus so users land on the queue page from the
// forge's check UI.
func DashboardPRURL(base string, kind Kind, owner, repo string, n int64) string {
	return fmt.Sprintf("%s/repo/%s/%s/%s/pr/%d", strings.TrimRight(base, "/"), kind, owner, repo, n)
}

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
type PR struct {
	Number           int64
	Title            string
	State            string // "open", "closed"
	Merged           bool
	AuthorLogin      string
	HeadBranch       string
	HeadSHA          string
	BaseBranch       string
	HTMLURL          string
	AutoMergeEnabled bool
}

// MQStatus is the lifecycle state reported by gitea-mq for a head SHA.
type MQStatus struct {
	State       CheckState
	Description string
	TargetURL   string
}

// CheckState aliases pg.CheckState so callers do not import the store package.
type CheckState = pg.CheckState

// ParseCheckState folds a forge status string to a CheckState. Gitea's
// "warning"/"skipped" are treated as success; GitHub's vocabulary is a subset.
func ParseCheckState(s string) CheckState {
	switch s {
	case "success", "warning", "skipped":
		return pg.CheckStateSuccess
	case "failure":
		return pg.CheckStateFailure
	case "error":
		return pg.CheckStateError
	default:
		return pg.CheckStatePending
	}
}

// Check is a single context's status as reported by GetCheckStates.
// Description and TargetURL are passed through so callers can implement
// status mirroring and stale-mirror cleanup without forge-specific code.
type Check struct {
	State       CheckState
	Description string
	TargetURL   string
}

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
	BranchHTMLURL(owner, name, branch string) string

	ListOpenPRs(ctx context.Context, owner, name string) ([]PR, error)
	GetPR(ctx context.Context, owner, name string, number int64) (*PR, error)

	SetMQStatus(ctx context.Context, owner, name, sha string, st MQStatus) error
	// MirrorCheck posts a status/check with an arbitrary context name on sha,
	// used to surface merge-branch CI results on the PR head. State values are
	// the standard set: pending, success, failure, error, skipped.
	MirrorCheck(ctx context.Context, owner, name, sha, checkContext, state, description, targetURL string) error
	GetRequiredChecks(ctx context.Context, owner, name, branch string) ([]string, error)
	GetCheckStates(ctx context.Context, owner, name, sha string) (map[string]Check, error)

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
