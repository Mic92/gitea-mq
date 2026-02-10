package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// HTTPClient implements Client using Gitea's REST API over HTTP.
type HTTPClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewHTTPClient creates a new HTTP-based Gitea API client.
func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{},
	}
}

// do executes an HTTP request with authentication and returns the response.
// The caller is responsible for closing the response body.
func (c *HTTPClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	url := c.baseURL + "/api/v1" + path

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}

		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request %s %s: %w", method, path, err)
	}

	return resp, nil
}

// decodeJSON reads the response body and decodes JSON into v.
// It also checks for non-2xx status codes.
func (c *HTTPClient) decodeJSON(resp *http.Response, v any) error {
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)

		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}

// expectStatus checks the response has the expected status code.
func (c *HTTPClient) expectStatus(resp *http.Response, expected int) error {
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode != expected {
		bodyBytes, _ := io.ReadAll(resp.Body)

		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	return nil
}

// APIError represents a non-2xx response from the Gitea API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitea API error (status %d): %s", e.StatusCode, e.Body)
}

// IsNotFound returns true if the error is a 404 response.
func IsNotFound(err error) bool {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.StatusCode == http.StatusNotFound
	}

	return false
}

// ListUserRepos returns all repositories accessible to the authenticated user.
// Handles pagination.
func (c *HTTPClient) ListUserRepos(ctx context.Context) ([]Repo, error) {
	var allRepos []Repo

	page := 1

	for {
		path := fmt.Sprintf("/user/repos?page=%d&limit=50", page)

		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}

		var repos []Repo
		if err := c.decodeJSON(resp, &repos); err != nil {
			return nil, fmt.Errorf("list user repos: %w", err)
		}

		allRepos = append(allRepos, repos...)

		if len(repos) < 50 {
			break
		}

		page++
	}

	return allRepos, nil
}

// GetRepoTopics returns the topics for a repository.
// Gitea doesn't include topics in the repo listing, so this needs a separate call.
func (c *HTTPClient) GetRepoTopics(ctx context.Context, owner, repo string) ([]string, error) {
	path := fmt.Sprintf("/repos/%s/%s/topics", owner, repo)

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Topics []string `json:"topics"`
	}
	if err := c.decodeJSON(resp, &result); err != nil {
		return nil, fmt.Errorf("get topics for %s/%s: %w", owner, repo, err)
	}

	return result.Topics, nil
}

// ListOpenPRs returns all open pull requests for a repository.
// Handles pagination to get all results.
func (c *HTTPClient) ListOpenPRs(ctx context.Context, owner, repo string) ([]PR, error) {
	var allPRs []PR

	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&page=%d&limit=50", owner, repo, page)

		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}

		var prs []PR
		if err := c.decodeJSON(resp, &prs); err != nil {
			return nil, fmt.Errorf("list open PRs for %s/%s: %w", owner, repo, err)
		}

		allPRs = append(allPRs, prs...)

		if len(prs) < 50 {
			break
		}

		page++
	}

	return allPRs, nil
}

// GetPR returns a single pull request by index.
func (c *HTTPClient) GetPR(ctx context.Context, owner, repo string, index int64) (*PR, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, index)

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var pr PR
	if err := c.decodeJSON(resp, &pr); err != nil {
		return nil, fmt.Errorf("get PR #%d in %s/%s: %w", index, owner, repo, err)
	}

	return &pr, nil
}

// GetPRTimeline returns timeline comments for a pull request.
// Handles pagination. The endpoint is GET /repos/{owner}/{repo}/issues/{index}/timeline.
func (c *HTTPClient) GetPRTimeline(ctx context.Context, owner, repo string, index int64) ([]TimelineComment, error) {
	var allComments []TimelineComment

	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?page=%d&limit=50", owner, repo, index, page)

		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}

		var comments []TimelineComment
		if err := c.decodeJSON(resp, &comments); err != nil {
			return nil, fmt.Errorf("get PR #%d timeline in %s/%s: %w", index, owner, repo, err)
		}

		allComments = append(allComments, comments...)

		if len(comments) < 50 {
			break
		}

		page++
	}

	return allComments, nil
}

// CreateCommitStatus posts a commit status on a specific SHA.
// POST /repos/{owner}/{repo}/statuses/{sha}
func (c *HTTPClient) CreateCommitStatus(ctx context.Context, owner, repo, sha string, status CommitStatus) error {
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s", owner, repo, sha)

	resp, err := c.do(ctx, http.MethodPost, path, status)
	if err != nil {
		return err
	}

	// Gitea returns 201 Created for commit status.
	if err := c.expectStatus(resp, http.StatusCreated); err != nil {
		return fmt.Errorf("create commit status on %s in %s/%s: %w", sha[:8], owner, repo, err)
	}

	slog.Debug("created commit status", "owner", owner, "repo", repo, "sha", sha[:8], "context", status.Context, "state", status.State)

	return nil
}

// CreateComment posts a comment on a pull request.
// POST /repos/{owner}/{repo}/issues/{index}/comments
func (c *HTTPClient) CreateComment(ctx context.Context, owner, repo string, index int64, body string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, index)

	payload := map[string]string{"body": body}

	resp, err := c.do(ctx, http.MethodPost, path, payload)
	if err != nil {
		return err
	}

	if err := c.expectStatus(resp, http.StatusCreated); err != nil {
		return fmt.Errorf("create comment on PR #%d in %s/%s: %w", index, owner, repo, err)
	}

	return nil
}

// CancelAutoMerge cancels the scheduled automerge for a pull request.
// DELETE /repos/{owner}/{repo}/pulls/{index}/merge
func (c *HTTPClient) CancelAutoMerge(ctx context.Context, owner, repo string, index int64) error {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, index)

	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}

	if err := c.expectStatus(resp, http.StatusNoContent); err != nil {
		// 404 means automerge was already cancelled — treat as success.
		if IsNotFound(err) {
			slog.Debug("automerge already cancelled", "owner", owner, "repo", repo, "pr", index)

			return nil
		}

		return fmt.Errorf("cancel automerge on PR #%d in %s/%s: %w", index, owner, repo, err)
	}

	slog.Debug("cancelled automerge", "owner", owner, "repo", repo, "pr", index)

	return nil
}

// GetBranchProtection returns the branch protection rule for a branch.
// GET /repos/{owner}/{repo}/branch_protections/{name}
func (c *HTTPClient) GetBranchProtection(ctx context.Context, owner, repo, branch string) (*BranchProtection, error) {
	path := fmt.Sprintf("/repos/%s/%s/branch_protections/%s", owner, repo, branch)

	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var bp BranchProtection
	if err := c.decodeJSON(resp, &bp); err != nil {
		if IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get branch protection for %s in %s/%s: %w", branch, owner, repo, err)
	}

	return &bp, nil
}

// CreateBranch creates a new branch from a target ref.
// POST /repos/{owner}/{repo}/branches
func (c *HTTPClient) CreateBranch(ctx context.Context, owner, repo, name, target string) error {
	path := fmt.Sprintf("/repos/%s/%s/branches", owner, repo)

	payload := map[string]string{
		"new_branch_name": name,
		"old_ref_name":    target,
	}

	resp, err := c.do(ctx, http.MethodPost, path, payload)
	if err != nil {
		return err
	}

	if err := c.expectStatus(resp, http.StatusCreated); err != nil {
		return fmt.Errorf("create branch %s from %s in %s/%s: %w", name, target, owner, repo, err)
	}

	return nil
}

// DeleteBranch deletes a branch.
// DELETE /repos/{owner}/{repo}/branches/{branch}
func (c *HTTPClient) DeleteBranch(ctx context.Context, owner, repo, name string) error {
	path := fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, name)

	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}

	if err := c.expectStatus(resp, http.StatusNoContent); err != nil {
		// 404 means branch already deleted — treat as success.
		if IsNotFound(err) {
			slog.Debug("branch already deleted", "owner", owner, "repo", repo, "branch", name)

			return nil
		}

		return fmt.Errorf("delete branch %s in %s/%s: %w", name, owner, repo, err)
	}

	return nil
}

// MergeBranches creates a merge of head into base and pushes it as branch
// mq/<head-short>. It shells out to git because Gitea has no API to merge
// two arbitrary refs into a new branch.
//
// Steps:
//  1. Shallow-clone the repo (base branch only)
//  2. Fetch the head SHA
//  3. git merge --no-ff the head SHA into base
//  4. Push the result as mq/<head-short>
//
// On conflict git merge exits non-zero and we return a MergeConflictError.
func (c *HTTPClient) MergeBranches(ctx context.Context, owner, repo, base, head, branchName string) (*MergeResult, error) {
	tmpDir, err := os.MkdirTemp("", "gitea-mq-merge-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			slog.Warn("failed to remove temp dir", "path", tmpDir, "error", err)
		}
	}()

	cloneURL := fmt.Sprintf("%s/%s/%s.git", c.baseURL, owner, repo)

	// Use token auth via URL for git push.
	authedURL := fmt.Sprintf("%s://gitea-mq:%s@%s",
		cloneURL[:strings.Index(cloneURL, "://")],
		c.token,
		cloneURL[strings.Index(cloneURL, "://")+3:],
	)

	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = tmpDir
		cmd.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=gitea-mq",
			"GIT_AUTHOR_EMAIL=gitea-mq@localhost",
			"GIT_COMMITTER_NAME=gitea-mq",
			"GIT_COMMITTER_EMAIL=gitea-mq@localhost",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return out, nil
	}

	// Clone base branch only (shallow for speed).
	if _, err := run("git", "clone", "--depth=1", "--branch", base, authedURL, "."); err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}

	// Fetch the PR head SHA so we can merge it.
	if _, err := run("git", "fetch", "origin", head); err != nil {
		return nil, fmt.Errorf("fetch head: %w", err)
	}

	// Merge head into base. --no-ff ensures a merge commit even if fast-forward
	// is possible, so CI always sees the combined result.
	mergeOut, mergeErr := run("git", "merge", "--no-ff", "-m", "mq: merge "+head+" into "+base, "FETCH_HEAD")
	if mergeErr != nil {
		// Check if this is a merge conflict.
		if strings.Contains(string(mergeOut), "CONFLICT") || strings.Contains(string(mergeOut), "Automatic merge failed") {
			return nil, &MergeConflictError{Base: base, Head: head, Message: string(mergeOut)}
		}
		return nil, fmt.Errorf("merge: %w", mergeErr)
	}

	// Push as the requested branch name.
	if _, err := run("git", "push", "origin", "HEAD:refs/heads/"+branchName); err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}

	// Read the merge commit SHA.
	shaOut, err := run("git", "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse: %w", err)
	}
	sha := strings.TrimSpace(string(shaOut))

	slog.Debug("created merge branch", "branch", branchName, "sha", sha[:8])

	return &MergeResult{SHA: sha}, nil
}

// MergeConflictError indicates a merge conflict when creating the merge branch.
type MergeConflictError struct {
	Base    string
	Head    string
	Message string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge conflict: cannot merge %s into %s: %s", e.Head, e.Base, e.Message)
}

// IsMergeConflict returns true if the error is a merge conflict.
func IsMergeConflict(err error) bool {
	_, ok := err.(*MergeConflictError)
	return ok
}

// ListBranchProtections lists all branch protection rules for a repository.
// Handles pagination.
func (c *HTTPClient) ListBranchProtections(ctx context.Context, owner, repo string) ([]BranchProtection, error) {
	var allBPs []BranchProtection

	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/branch_protections?page=%d&limit=50", owner, repo, page)

		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}

		var bps []BranchProtection
		if err := c.decodeJSON(resp, &bps); err != nil {
			return nil, fmt.Errorf("list branch protections for %s/%s: %w", owner, repo, err)
		}

		allBPs = append(allBPs, bps...)

		if len(bps) < 50 {
			break
		}

		page++
	}

	return allBPs, nil
}

// EditBranchProtection updates a branch protection rule.
// PATCH /repos/{owner}/{repo}/branch_protections/{name}
func (c *HTTPClient) EditBranchProtection(ctx context.Context, owner, repo, name string, opts EditBranchProtectionOpts) error {
	path := fmt.Sprintf("/repos/%s/%s/branch_protections/%s", owner, repo, name)

	resp, err := c.do(ctx, http.MethodPatch, path, opts)
	if err != nil {
		return err
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)

		return fmt.Errorf("edit branch protection %s in %s/%s: status %d: %s",
			name, owner, repo, resp.StatusCode, string(bodyBytes))
	}

	return nil
}

// ListWebhooks lists all webhooks for a repository. Handles pagination.
func (c *HTTPClient) ListWebhooks(ctx context.Context, owner, repo string) ([]Webhook, error) {
	var allHooks []Webhook

	page := 1

	for {
		path := fmt.Sprintf("/repos/%s/%s/hooks?page=%d&limit=50", owner, repo, page)

		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}

		var hooks []Webhook
		if err := c.decodeJSON(resp, &hooks); err != nil {
			return nil, fmt.Errorf("list webhooks for %s/%s: %w", owner, repo, err)
		}

		allHooks = append(allHooks, hooks...)

		if len(hooks) < 50 {
			break
		}

		page++
	}

	return allHooks, nil
}

// CreateWebhook creates a webhook on a repository.
// POST /repos/{owner}/{repo}/hooks
func (c *HTTPClient) CreateWebhook(ctx context.Context, owner, repo string, opts CreateWebhookOpts) error {
	path := fmt.Sprintf("/repos/%s/%s/hooks", owner, repo)

	resp, err := c.do(ctx, http.MethodPost, path, opts)
	if err != nil {
		return err
	}

	if err := c.expectStatus(resp, http.StatusCreated); err != nil {
		return fmt.Errorf("create webhook in %s/%s: %w", owner, repo, err)
	}

	return nil
}

// Ensure HTTPClient implements Client at compile time.
var _ Client = (*HTTPClient)(nil)
