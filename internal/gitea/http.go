package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// decodeJSON reads the response body, checks for non-2xx status codes, and
// optionally decodes JSON into v. If v is nil the body is drained and discarded.
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
	} else {
		// Drain the body so the HTTP client can reuse the connection.
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	return nil
}

// doDiscard executes an HTTP request, checks for errors, and discards the
// response body. Used by write-only endpoints (POST/PATCH/DELETE) that don't
// need the response payload.
func (c *HTTPClient) doDiscard(ctx context.Context, method, path string, body any, errLabel string) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}

	if err := c.decodeJSON(resp, nil); err != nil {
		return fmt.Errorf("%s: %w", errLabel, err)
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

// shortSHA truncates a SHA to 8 characters for display purposes.
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// IsNotFound returns true if the error is a 404 response.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// paginate fetches all pages of a Gitea API endpoint that returns a bare
// JSON array. pathFmt must contain a single %d verb for the page number
// (e.g. "/user/repos?page=%d&limit=50"). A short page (< 50 items) is
// treated as the last page, saving a final empty-page request when the
// data divides evenly. Use this for endpoints whose page size exactly
// matches the SQL LIMIT — i.e. most of Gitea's list endpoints.
func paginate[T any](ctx context.Context, c *HTTPClient, pathFmt, errLabel string) ([]T, error) {
	return paginateWrapped(ctx, c, pathFmt, errLabel, func(s *[]T) []T { return *s })
}

// paginateUntilEmpty is like paginate but keeps fetching until a page
// comes back empty. Use this for endpoints where Gitea applies a
// post-decode filter after the SQL LIMIT (the timeline endpoint hides
// CommentTypeCode entries this way), so a non-final page can legitimately
// be shorter than the limit and "short page = last page" would skip data.
// Costs one extra HTTP request per call.
func paginateUntilEmpty[T any](ctx context.Context, c *HTTPClient, pathFmt, errLabel string) ([]T, error) {
	return paginateWrappedUntilEmpty(ctx, c, pathFmt, errLabel, func(s *[]T) []T { return *s })
}

// paginateWrapped is paginate for endpoints whose JSON response is a
// wrapping object instead of a bare array (e.g. /repos/search returns
// {ok, data: [...]}). extract pulls the page items out of the decoded
// wrapper. Same EOF contract as paginate.
func paginateWrapped[Wrapper, Item any](ctx context.Context, c *HTTPClient, pathFmt, errLabel string, extract func(*Wrapper) []Item) ([]Item, error) {
	return paginatePages(ctx, c, pathFmt, errLabel, extract, false)
}

// paginateWrappedUntilEmpty is paginateUntilEmpty for wrapped responses.
// Same EOF contract as paginateUntilEmpty.
func paginateWrappedUntilEmpty[Wrapper, Item any](ctx context.Context, c *HTTPClient, pathFmt, errLabel string, extract func(*Wrapper) []Item) ([]Item, error) {
	return paginatePages(ctx, c, pathFmt, errLabel, extract, true)
}

// paginatePages fetches pages until the end-of-data condition: with
// untilEmpty the loop stops on the first empty page, otherwise a short page
// (< 50 items) is treated as the last one.
func paginatePages[Wrapper, Item any](ctx context.Context, c *HTTPClient, pathFmt, errLabel string, extract func(*Wrapper) []Item, untilEmpty bool) ([]Item, error) {
	var all []Item

	for page := 1; ; page++ {
		path := fmt.Sprintf(pathFmt, page)

		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}

		var w Wrapper
		if err := c.decodeJSON(resp, &w); err != nil {
			return nil, fmt.Errorf("%s: %w", errLabel, err)
		}

		items := extract(&w)
		all = append(all, items...)

		if (untilEmpty && len(items) == 0) || (!untilEmpty && len(items) < 50) {
			return all, nil
		}
	}
}

// repoSearchResponse is the wrapping object returned by /repos/search.
type repoSearchResponse struct {
	Data []Repo `json:"data"`
}

// SearchReposByTopic returns all repositories with the given topic.
// Uses the search endpoint which, for site admins, returns repos across the
// entire instance — not just repos the user owns or collaborates on.
func (c *HTTPClient) SearchReposByTopic(ctx context.Context, topic string) ([]Repo, error) {
	return paginateWrappedUntilEmpty(ctx, c,
		fmt.Sprintf("/repos/search?q=%s&topic=true&page=%%d&limit=50", topic),
		fmt.Sprintf("search repos by topic %s", topic),
		func(r *repoSearchResponse) []Repo { return r.Data })
}

// ListOpenPRs returns all open pull requests for a repository.
// Handles pagination to get all results.
func (c *HTTPClient) ListOpenPRs(ctx context.Context, owner, repo string) ([]PR, error) {
	return paginate[PR](ctx, c,
		fmt.Sprintf("/repos/%s/%s/pulls?state=open&page=%%d&limit=50", owner, repo),
		fmt.Sprintf("list open PRs for %s/%s", owner, repo))
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
	return paginateUntilEmpty[TimelineComment](ctx, c,
		fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?page=%%d&limit=50", owner, repo, index),
		fmt.Sprintf("get PR #%d timeline in %s/%s", index, owner, repo))
}

// GetCombinedCommitStatus returns the latest status per context for a commit
// ref by paginating GET /repos/{owner}/{repo}/commits/{ref}/status.
func (c *HTTPClient) GetCombinedCommitStatus(ctx context.Context, owner, repo, ref string) (*CombinedStatus, error) {
	statuses, err := paginateWrapped(ctx, c,
		fmt.Sprintf("/repos/%s/%s/commits/%s/status?page=%%d&limit=50", owner, repo, ref),
		fmt.Sprintf("get combined status for %s in %s/%s", shortSHA(ref), owner, repo),
		func(cs *CombinedStatus) []CommitStatusResult { return cs.Statuses })
	if err != nil {
		return nil, err
	}
	return &CombinedStatus{SHA: ref, Statuses: statuses}, nil
}

// CreateCommitStatus posts a commit status on a specific SHA.
// POST /repos/{owner}/{repo}/statuses/{sha}
func (c *HTTPClient) CreateCommitStatus(ctx context.Context, owner, repo, sha string, status CommitStatus) error {
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s", owner, repo, sha)

	if err := c.doDiscard(ctx, http.MethodPost, path, status,
		fmt.Sprintf("create commit status on %s in %s/%s", shortSHA(sha), owner, repo)); err != nil {
		return err
	}

	slog.Debug("created commit status", "owner", owner, "repo", repo, "sha", shortSHA(sha), "context", status.Context, "state", status.State)

	return nil
}

// CreateComment posts a comment on a pull request.
// POST /repos/{owner}/{repo}/issues/{index}/comments
func (c *HTTPClient) CreateComment(ctx context.Context, owner, repo string, index int64, body string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, index)

	return c.doDiscard(ctx, http.MethodPost, path, map[string]string{"body": body},
		fmt.Sprintf("create comment on PR #%d in %s/%s", index, owner, repo))
}

// CancelAutoMerge cancels the scheduled automerge for a pull request.
// DELETE /repos/{owner}/{repo}/pulls/{index}/merge
func (c *HTTPClient) CancelAutoMerge(ctx context.Context, owner, repo string, index int64) error {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, index)

	if err := c.doDiscard(ctx, http.MethodDelete, path, nil,
		fmt.Sprintf("cancel automerge on PR #%d in %s/%s", index, owner, repo)); err != nil {
		// 404 means automerge was already cancelled — treat as success.
		if IsNotFound(err) {
			slog.Debug("automerge already cancelled", "owner", owner, "repo", repo, "pr", index)

			return nil
		}

		return err
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

// ListBranches returns all branches for a repository. Handles pagination.
func (c *HTTPClient) ListBranches(ctx context.Context, owner, repo string) ([]Branch, error) {
	return paginate[Branch](ctx, c,
		fmt.Sprintf("/repos/%s/%s/branches?page=%%d&limit=50", owner, repo),
		fmt.Sprintf("list branches for %s/%s", owner, repo))
}

// CreateBranch creates a new branch from a target ref.
// POST /repos/{owner}/{repo}/branches
func (c *HTTPClient) CreateBranch(ctx context.Context, owner, repo, name, target string) error {
	path := fmt.Sprintf("/repos/%s/%s/branches", owner, repo)

	return c.doDiscard(ctx, http.MethodPost, path,
		map[string]string{"new_branch_name": name, "old_ref_name": target},
		fmt.Sprintf("create branch %s from %s in %s/%s", name, target, owner, repo))
}

// CompareCommits returns the commit count reachable from head but not base.
// GET /repos/{owner}/{repo}/compare/{base}...{head}
func (c *HTTPClient) CompareCommits(ctx context.Context, owner, repo, base, head string) (*Compare, error) {
	resp, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/compare/%s...%s", owner, repo, base, head), nil)
	if err != nil {
		return nil, err
	}

	var cmp Compare
	if err := c.decodeJSON(resp, &cmp); err != nil {
		return nil, fmt.Errorf("compare %s...%s: %w", base, head, err)
	}

	return &cmp, nil
}

// DeleteBranch deletes a branch.
// DELETE /repos/{owner}/{repo}/branches/{branch}
func (c *HTTPClient) DeleteBranch(ctx context.Context, owner, repo, name string) error {
	path := fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, name)

	if err := c.doDiscard(ctx, http.MethodDelete, path, nil,
		fmt.Sprintf("delete branch %s in %s/%s", name, owner, repo)); err != nil {
		// 404 means branch already deleted — treat as success.
		if IsNotFound(err) {
			slog.Debug("branch already deleted", "owner", owner, "repo", repo, "branch", name)

			return nil
		}

		return err
	}

	return nil
}

// redact strips the API token from git output before it reaches logs or PR
// comments.
func (c *HTTPClient) redact(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "***")
}

// MergeBranches creates a merge of head into base and pushes it as branch
// gitea-mq/<head-short>. It shells out to git because Gitea has no API to merge
// two arbitrary refs into a new branch.
//
// Steps:
//  1. Shallow-clone the repo (base branch only)
//  2. Fetch the head SHA
//  3. git merge --no-ff the head SHA into base
//  4. Push the result as gitea-mq/<head-short>
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
	authedURL := fmt.Sprintf(
		"%s://gitea-mq:%s@%s",
		cloneURL[:strings.Index(cloneURL, "://")],
		c.token,
		cloneURL[strings.Index(cloneURL, "://")+3:],
	)

	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = tmpDir
		cmd.Env = append(
			os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=gitea-mq",
			"GIT_AUTHOR_EMAIL=gitea-mq@localhost",
			"GIT_COMMITTER_NAME=gitea-mq",
			"GIT_COMMITTER_EMAIL=gitea-mq@localhost",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("%s: %w\n%s", c.redact(strings.Join(args, " ")), err, c.redact(string(out)))
		}
		return out, nil
	}

	// Full clone of the base branch. We need enough history for git to
	// find the common ancestor with the PR head, so shallow clones don't
	// work reliably here.
	if _, err := run("git", "clone", "--single-branch", "--branch", base, authedURL, "."); err != nil {
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

	slog.Debug("created merge branch", "branch", branchName, "sha", shortSHA(sha))

	return &MergeResult{SHA: sha}, nil
}

// StackStep is the per-head outcome of StackMerges.
type StackStep struct {
	Conflict bool
	Err      error
}

// StackMerges clones base once and merges each head onto it in order, pushing
// the result as branch. On a per-head conflict the merge is aborted and the
// step marked; subsequent heads merge onto the pre-conflict tip. A clone or
// final-push failure is returned as err so the caller can retry the whole
// build instead of mis-attributing a transient network error to one PR.
func (c *HTTPClient) StackMerges(ctx context.Context, owner, repo, base string, heads []string, branch string) (string, []StackStep, error) {
	tmpDir, err := os.MkdirTemp("", "gitea-mq-stack-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	url := c.authedCloneURL(owner, repo)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = tmpDir
		cmd.Env = append(os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=gitea-mq", "GIT_AUTHOR_EMAIL=gitea-mq@localhost",
			"GIT_COMMITTER_NAME=gitea-mq", "GIT_COMMITTER_EMAIL=gitea-mq@localhost")
		return cmd.CombinedOutput()
	}

	if out, err := run("clone", "--single-branch", "--branch", base, url, "."); err != nil {
		return "", nil, fmt.Errorf("clone %s: %w\n%s", base, err, c.redact(string(out)))
	}

	steps := make([]StackStep, len(heads))
	var tip string
	for i, head := range heads {
		if out, err := run("fetch", "-q", "origin", head); err != nil {
			steps[i].Err = fmt.Errorf("fetch %s: %s", shortSHA(head), c.redact(string(out)))
			continue
		}
		out, err := run("merge", "--no-ff", "-m",
			fmt.Sprintf("mq: merge %s into %s", shortSHA(head), base), "FETCH_HEAD")
		if err != nil {
			s := string(out)
			if strings.Contains(s, "CONFLICT") || strings.Contains(s, "Automatic merge failed") {
				steps[i].Conflict = true
			} else {
				steps[i].Err = fmt.Errorf("merge %s: %s", shortSHA(head), c.redact(string(out)))
			}
			_, _ = run("merge", "--abort")
			continue
		}
		sha, _ := run("rev-parse", "HEAD")
		tip = strings.TrimSpace(string(sha))
	}

	if tip == "" {
		return "", steps, nil
	}
	if out, err := run("push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return "", nil, fmt.Errorf("push %s: %w\n%s", branch, err, c.redact(string(out)))
	}
	slog.Debug("stack-merge built", "branch", branch, "heads", len(heads), "tip", shortSHA(tip))
	return tip, steps, nil
}

// authedCloneURL returns the repo's HTTPS clone URL with the API token
// embedded as basic-auth for non-interactive git operations.
func (c *HTTPClient) authedCloneURL(owner, repo string) string {
	plain := fmt.Sprintf("%s/%s/%s.git", c.baseURL, owner, repo)
	i := strings.Index(plain, "://")
	return fmt.Sprintf("%s://gitea-mq:%s@%s", plain[:i], c.token, plain[i+3:])
}

// FastForwardRef pushes sha to refs/heads/branch with a non-force refspec.
// git's client-side fast-forward check needs sha's ancestry back to the
// current branch tip, so we fetch sha without a depth limit. The server
// already has every object (sha is the tip of a branch we built there) so the
// resulting push pack is empty — cost is the fetch, same order as the full
// single-branch clone MergeBranches already does.
func (c *HTTPClient) FastForwardRef(ctx context.Context, owner, repo, branch, sha string) error {
	tmpDir, err := os.MkdirTemp("", "gitea-mq-ff-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	url := c.authedCloneURL(owner, repo)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = tmpDir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		return cmd.CombinedOutput()
	}

	if out, err := run("init", "--bare", "-q"); err != nil {
		return fmt.Errorf("git init: %s: %w", out, err)
	}
	if out, err := run("fetch", "-q", url, sha); err != nil {
		return fmt.Errorf("git fetch %s: %s: %w", shortSHA(sha), c.redact(string(out)), err)
	}
	out, err := run("push", url, sha+":refs/heads/"+branch)
	if err == nil {
		return nil
	}
	msg := c.redact(string(out))
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "non-fast-forward") || strings.Contains(low, "fetch first"):
		return &NotFastForwardError{Branch: branch, SHA: sha}
	case strings.Contains(low, "protected branch") || strings.Contains(low, "not allowed to push"):
		return &ProtectedBranchError{Branch: branch, Message: msg}
	default:
		return fmt.Errorf("git push %s: %w\n%s", branch, err, msg)
	}
}

// EditIssueState sets an issue/PR state via PATCH /repos/{o}/{r}/issues/{n}.
func (c *HTTPClient) EditIssueState(ctx context.Context, owner, repo string, index int64, state string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, index)
	return c.doDiscard(ctx, http.MethodPatch, path, map[string]string{"state": state},
		fmt.Sprintf("edit issue #%d state in %s/%s", index, owner, repo))
}

// NotFastForwardError indicates a rejected non-fast-forward push.
type NotFastForwardError struct {
	Branch, SHA string
}

func (e *NotFastForwardError) Error() string {
	return fmt.Sprintf("push %s to %s: non-fast-forward", shortSHA(e.SHA), e.Branch)
}

// ProtectedBranchError indicates branch protection denied the push.
type ProtectedBranchError struct {
	Branch, Message string
}

func (e *ProtectedBranchError) Error() string {
	return fmt.Sprintf("push to protected branch %s denied: %s", e.Branch, e.Message)
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
	var mergeErr *MergeConflictError
	return errors.As(err, &mergeErr)
}

// ListBranchProtections lists all branch protection rules for a repository.
// Handles pagination.
func (c *HTTPClient) ListBranchProtections(ctx context.Context, owner, repo string) ([]BranchProtection, error) {
	return paginate[BranchProtection](ctx, c,
		fmt.Sprintf("/repos/%s/%s/branch_protections?page=%%d&limit=50", owner, repo),
		fmt.Sprintf("list branch protections for %s/%s", owner, repo))
}

// EditBranchProtection updates a branch protection rule.
// PATCH /repos/{owner}/{repo}/branch_protections/{name}
func (c *HTTPClient) EditBranchProtection(ctx context.Context, owner, repo, name string, opts EditBranchProtectionOpts) error {
	path := fmt.Sprintf("/repos/%s/%s/branch_protections/%s", owner, repo, name)

	return c.doDiscard(ctx, http.MethodPatch, path, opts,
		fmt.Sprintf("edit branch protection %s in %s/%s", name, owner, repo))
}

// ListWebhooks lists all webhooks for a repository. Handles pagination.
func (c *HTTPClient) ListWebhooks(ctx context.Context, owner, repo string) ([]Webhook, error) {
	return paginate[Webhook](ctx, c,
		fmt.Sprintf("/repos/%s/%s/hooks?page=%%d&limit=50", owner, repo),
		fmt.Sprintf("list webhooks for %s/%s", owner, repo))
}

// CreateWebhook creates a webhook on a repository.
// POST /repos/{owner}/{repo}/hooks
func (c *HTTPClient) CreateWebhook(ctx context.Context, owner, repo string, opts CreateWebhookOpts) error {
	return c.doDiscard(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/hooks", owner, repo), opts,
		fmt.Sprintf("create webhook in %s/%s", owner, repo))
}

// Ensure HTTPClient implements Client at compile time.
var _ Client = (*HTTPClient)(nil)
