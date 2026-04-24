// Package forge defines a forge-agnostic interface for repo, PR, status,
// branch, comment and setup operations. Concrete implementations live in
// sibling packages (internal/gitea, internal/github) and are resolved per
// RepoRef via a Set.
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
// Returns false if the format is invalid or the forge kind is unknown.
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
// AutoMergeEnabled is true when the PR is in the "scheduled auto-merge" state
// on its forge. Adapters are responsible for normalising forge-specific
// signals (Gitea timeline comments, GitHub `auto_merge` field) into this
// single boolean so callers never touch forge internals.
//
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

// CheckState mirrors pg.CheckState at the forge layer so callers do not have
// to import the store package. Values match pg.CheckState exactly.
type CheckState = pg.CheckState

// SetupConfig bundles the inputs an adapter needs during EnsureRepoSetup.
type SetupConfig struct {
	// ExternalURL is the public base URL of the gitea-mq instance. Used for
	// webhook URL construction (Gitea) and status target URLs.
	ExternalURL string
	// WebhookSecret is the shared secret for signature validation. For Gitea
	// it is the per-repo webhook secret; GitHub adapters ignore it since the
	// App-level webhook is configured out-of-band.
	WebhookSecret string
	// RequiredCheckContext is the status check context name gitea-mq posts
	// under (always "gitea-mq" in practice; parameterised for tests).
	RequiredCheckContext string
}

// Forge abstracts all operations gitea-mq performs against a hosting forge.
// Every method takes (ctx, owner, name) at minimum so a single adapter can
// service many repos on the same installation/instance.
type Forge interface {
	Kind() Kind
	RepoHTMLURL(owner, name string) string
	PRHTMLURL(owner, name string, number int64) string

	ListOpenPRs(ctx context.Context, owner, name string) ([]PR, error)
	GetPR(ctx context.Context, owner, name string, number int64) (*PR, error)
	// ListAutoMergePRs returns open PRs that currently have auto-merge
	// scheduled. Adapters must fold any forge-specific discovery (Gitea
	// timeline scan, GitHub `auto_merge` filter) into this call.
	ListAutoMergePRs(ctx context.Context, owner, name string) ([]PR, error)

	SetMQStatus(ctx context.Context, owner, name, sha string, st MQStatus) error
	GetRequiredChecks(ctx context.Context, owner, name, branch string) ([]string, error)
	GetCheckStates(ctx context.Context, owner, name, sha string) (map[string]CheckState, error)

	// CreateMergeBranch creates `branch` at `base`'s tip and merges `headSHA`
	// into it. Returns the merge commit SHA. conflict=true means the merge
	// could not be completed because of conflicts; err is nil in that case.
	CreateMergeBranch(ctx context.Context, owner, name, base, headSHA, branch string) (sha string, conflict bool, err error)
	DeleteBranch(ctx context.Context, owner, name, branch string) error
	ListBranches(ctx context.Context, owner, name string) ([]string, error)

	CancelAutoMerge(ctx context.Context, owner, name string, number int64) error
	Comment(ctx context.Context, owner, name string, number int64, body string) error

	EnsureRepoSetup(ctx context.Context, owner, name string, cfg SetupConfig) error
}

// ErrUnknownForge is returned by Set.For when no adapter is registered for a
// ref's forge kind.
type ErrUnknownForge struct {
	Kind Kind
}

func (e *ErrUnknownForge) Error() string {
	return fmt.Sprintf("forge: no adapter registered for kind %q", e.Kind)
}
