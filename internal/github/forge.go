package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v84/github"

	"github.com/Mic92/gitea-mq/internal/forge"
)

var _ forge.Forge = (*githubForge)(nil)

type githubForge struct {
	app       *App
	htmlURL   string // https://github.com or GHES web root
	checkRuns checkRunCache
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

func (f *githubForge) BranchHTMLURL(owner, name, branch string) string {
	return fmt.Sprintf("%s/%s/%s/tree/%s", f.htmlURL, owner, name, branch)
}

func toForgePR(p *gh.PullRequest) forge.PR {
	return forge.PR{
		Number:           int64(p.GetNumber()),
		Title:            p.GetTitle(),
		State:            p.GetState(),
		Merged:           p.GetMerged(),
		AuthorLogin:      p.GetUser().GetLogin(),
		HeadBranch:       p.GetHead().GetRef(),
		HeadSHA:          p.GetHead().GetSHA(),
		BaseBranch:       p.GetBase().GetRef(),
		HTMLURL:          p.GetHTMLURL(),
		AutoMergeEnabled: p.GetAutoMerge() != nil,
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

func (f *githubForge) SetMQStatus(ctx context.Context, owner, name, sha string, st forge.MQStatus) error {
	status, concl := checkRunFields(string(st.State))
	return f.upsertCheckRun(ctx, owner, name, sha, forge.MQContext, status, concl, st.Description, st.TargetURL)
}

func (f *githubForge) MirrorCheck(ctx context.Context, owner, name, sha, checkContext string, c forge.Check) error {
	status, concl := checkRunFields(string(c.State))
	return f.upsertCheckRun(ctx, owner, name, sha, checkContext, status, concl, c.Description, c.TargetURL)
}

func (f *githubForge) GetRequiredChecks(ctx context.Context, owner, name, branch string) ([]string, error) {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return nil, err
	}
	rules, _, err := c.Repositories.GetRulesForBranch(ctx, owner, name, branch, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rules.RequiredStatusChecks {
		for _, sc := range r.Parameters.RequiredStatusChecks {
			if forge.IsOwnContext(sc.Context) {
				continue
			}
			out = append(out, sc.Context)
		}
	}
	return out, nil
}

func (f *githubForge) GetCheckStates(ctx context.Context, owner, name, sha string) (map[string]forge.Check, error) {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return nil, err
	}
	out := map[string]forge.Check{}

	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for cr, err := range c.Checks.ListCheckRunsForRefIter(ctx, owner, name, sha, opts) {
		if err != nil {
			return nil, err
		}
		// gitea-mq/* mirrors are kept on purpose: stale-mirror cleanup needs them.
		if cr.GetName() == forge.MQContext {
			continue
		}
		out[cr.GetName()] = forge.Check{
			State:       CheckRunToState(cr.GetStatus(), cr.GetConclusion()),
			Description: cr.GetOutput().GetSummary(),
			TargetURL:   cr.GetDetailsURL(),
		}
	}

	// Legacy commit statuses (third-party CI not on Checks API). Check runs
	// win on context collision because they are the App-native surface.
	for st, err := range c.Repositories.ListStatusesIter(ctx, owner, name, sha, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, err
		}
		if st.GetContext() == forge.MQContext {
			continue
		}
		if _, ok := out[st.GetContext()]; ok {
			continue
		}
		out[st.GetContext()] = forge.Check{
			State:       forge.ParseCheckState(st.GetState()),
			Description: st.GetDescription(),
			TargetURL:   st.GetTargetURL(),
		}
	}
	return out, nil
}

func (f *githubForge) CreateMergeBranch(ctx context.Context, owner, name, base, headSHA, branch string) (string, bool, error) {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return "", false, err
	}

	baseRef, _, err := c.Git.GetRef(ctx, owner, name, "heads/"+base)
	if err != nil {
		return "", false, fmt.Errorf("resolve base %s: %w", base, err)
	}
	baseSHA := baseRef.GetObject().GetSHA()

	_, resp, err := c.Git.CreateRef(ctx, owner, name, gh.CreateRef{
		Ref: "refs/heads/" + branch,
		SHA: baseSHA,
	})
	if err != nil {
		if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
			return "", false, fmt.Errorf("create ref %s: %w", branch, err)
		}
		// 422: ref already exists from a crashed previous attempt. Force it
		// to the current base tip so the merge result reflects fresh base.
		if _, _, err := c.Git.UpdateRef(ctx, owner, name, "heads/"+branch,
			gh.UpdateRef{SHA: baseSHA, Force: gh.Ptr(true)}); err != nil {
			return "", false, fmt.Errorf("reset stale ref %s: %w", branch, err)
		}
	}

	commit, resp, err := c.Repositories.Merge(ctx, owner, name, &gh.RepositoryMergeRequest{
		Base: gh.Ptr(branch),
		Head: gh.Ptr(headSHA),
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusConflict {
			return "", true, nil
		}
		return "", false, fmt.Errorf("merge %s into %s: %w", headSHA, branch, err)
	}
	// 204: head already contained in base; the branch tip is the result.
	if commit.GetSHA() == "" {
		return baseSHA, false, nil
	}
	return commit.GetSHA(), false, nil
}

func (f *githubForge) DeleteBranch(ctx context.Context, owner, name, branch string) error {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return err
	}
	resp, err := c.Git.DeleteRef(ctx, owner, name, "heads/"+branch)
	// Idempotent: callers delete defensively after success/failure. GitHub
	// returns 422 for a missing ref on an existing repo, 404 otherwise.
	if resp != nil && (resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusNotFound) {
		return nil
	}
	return err
}

func (f *githubForge) ListBranches(ctx context.Context, owner, name string) ([]string, error) {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return nil, err
	}
	var out []string
	opts := &gh.BranchListOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for b, err := range c.Repositories.ListBranchesIter(ctx, owner, name, opts) {
		if err != nil {
			return nil, err
		}
		out = append(out, b.GetName())
	}
	return out, nil
}

const disableAutoMergeMutation = `mutation($id:ID!){disablePullRequestAutoMerge(input:{pullRequestId:$id}){clientMutationId}}`

func (f *githubForge) CancelAutoMerge(ctx context.Context, owner, name string, number int64) error {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return err
	}
	pr, _, err := c.PullRequests.Get(ctx, owner, name, int(number))
	if err != nil {
		return err
	}

	payload, _ := json.Marshal(map[string]any{
		"query":     disableAutoMergeMutation,
		"variables": map[string]any{"id": pr.GetNodeID()},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", f.app.graphqlURL(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode graphql response: %w", err)
	}
	if len(body.Errors) > 0 {
		msg := body.Errors[0].Message
		// Already disabled is success for the queue's purposes.
		if strings.Contains(strings.ToLower(msg), "not enabled") {
			return nil
		}
		return fmt.Errorf("disablePullRequestAutoMerge: %s", msg)
	}
	return nil
}

func (f *githubForge) Comment(ctx context.Context, owner, name string, number int64, body string) error {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return err
	}
	_, _, err = c.Issues.CreateComment(ctx, owner, name, int(number), &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}
