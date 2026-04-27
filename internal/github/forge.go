package github

import (
	"context"
	"errors"
	"fmt"

	gh "github.com/google/go-github/v84/github"

	"github.com/Mic92/gitea-mq/internal/forge"
)

var _ forge.Forge = (*githubForge)(nil)

var errNotImplemented = errors.New("github: not implemented")

type githubForge struct {
	app     *App
	htmlURL string // https://github.com or GHES web root
}

// NewForge wraps a GitHub App as a forge. htmlURL is the user-facing web root
// (distinct from the API base URL); empty defaults to github.com.
func NewForge(app *App, htmlURL string) forge.Forge {
	if htmlURL == "" {
		htmlURL = "https://github.com"
	}
	return &githubForge{app: app, htmlURL: htmlURL}
}

func (f *githubForge) Kind() forge.Kind { return forge.KindGithub }

func (f *githubForge) RepoHTMLURL(owner, name string) string {
	return fmt.Sprintf("%s/%s/%s", f.htmlURL, owner, name)
}

func (f *githubForge) PRHTMLURL(owner, name string, number int64) string {
	return fmt.Sprintf("%s/%s/%s/pull/%d", f.htmlURL, owner, name, number)
}

func (f *githubForge) BranchHTMLURL(owner, name, branch string) string {
	return fmt.Sprintf("%s/%s/%s/tree/%s", f.htmlURL, owner, name, branch)
}

func toForgePR(p *gh.PullRequest) forge.PR {
	return forge.PR{
		Number:           int64(p.GetNumber()),
		Title:            p.GetTitle(),
		Body:             p.GetBody(),
		State:            p.GetState(),
		Merged:           p.GetMerged(),
		AuthorLogin:      p.GetUser().GetLogin(),
		HeadBranch:       p.GetHead().GetRef(),
		HeadSHA:          p.GetHead().GetSHA(),
		BaseBranch:       p.GetBase().GetRef(),
		HTMLURL:          p.GetHTMLURL(),
		AutoMergeEnabled: p.GetAutoMerge() != nil,
		NodeID:           p.GetNodeID(),
	}
}

func (f *githubForge) ListOpenPRs(ctx context.Context, owner, name string) ([]forge.PR, error) {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return nil, err
	}
	var out []forge.PR
	opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
	for p, err := range c.PullRequests.ListIter(ctx, owner, name, opts) {
		if err != nil {
			return nil, err
		}
		out = append(out, toForgePR(p))
	}
	return out, nil
}

func (f *githubForge) GetPR(ctx context.Context, owner, name string, number int64) (*forge.PR, error) {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return nil, err
	}
	p, _, err := c.PullRequests.Get(ctx, owner, name, int(number))
	if err != nil {
		return nil, err
	}
	fp := toForgePR(p)
	return &fp, nil
}

func (f *githubForge) ListAutoMergePRs(ctx context.Context, owner, name string) ([]forge.PR, error) {
	prs, err := f.ListOpenPRs(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	var out []forge.PR
	for _, pr := range prs {
		if pr.AutoMergeEnabled {
			out = append(out, pr)
		}
	}
	return out, nil
}

// --- stubs filled in by sections 7/8 ---

func (f *githubForge) SetMQStatus(context.Context, string, string, string, forge.MQStatus) error {
	return errNotImplemented
}

func (f *githubForge) MirrorCheck(context.Context, string, string, string, string, string, string, string) error {
	return errNotImplemented
}

func (f *githubForge) GetRequiredChecks(context.Context, string, string, string) ([]string, error) {
	return nil, errNotImplemented
}

func (f *githubForge) GetCheckStates(context.Context, string, string, string) (map[string]forge.Check, error) {
	return nil, errNotImplemented
}

func (f *githubForge) CreateMergeBranch(context.Context, string, string, string, string, string) (string, bool, error) {
	return "", false, errNotImplemented
}

func (f *githubForge) DeleteBranch(context.Context, string, string, string) error {
	return errNotImplemented
}

func (f *githubForge) ListBranches(context.Context, string, string) ([]string, error) {
	return nil, errNotImplemented
}

func (f *githubForge) CancelAutoMerge(context.Context, string, string, int64) error {
	return errNotImplemented
}

func (f *githubForge) Comment(context.Context, string, string, int64, string) error {
	return errNotImplemented
}

func (f *githubForge) EnsureRepoSetup(context.Context, string, string, forge.SetupConfig) error {
	return errNotImplemented
}
