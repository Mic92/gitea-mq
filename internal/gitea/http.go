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
	"path/filepath"
	"strings"
	"time"
)

// HTTPClient implements Client using Gitea's REST API over HTTP.
type HTTPClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
	gitCache   *gitCache
}

// NewHTTPClient creates a new HTTP-based Gitea API client.
func NewHTTPClient(baseURL, token string) *HTTPClient {
	c := &HTTPClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{},
	}
	// Git auth via extraHeader; see gitcache.go for why.
	var authFlags []string
	if token != "" {
		authFlags = []string{"-c", "http." + c.baseURL + "/.extraheader=Authorization: token " + token}
	}
	c.gitCache = newGitCache(filepath.Join(os.TempDir(), "gitea-mq-cache"), authFlags, c.redact)
	return c
}

// SetGitCacheDir points the persistent git cache at dir. Call before any
// merge/push operation; the default is a directory under os.TempDir().
func (c *HTTPClient) SetGitCacheDir(dir string) {
	c.gitCache.baseDir = dir
}

// CleanupGitCache removes cached repositories not used within maxAge, e.g.
// repositories removed from the configuration. Call at startup.
func (c *HTTPClient) CleanupGitCache(maxAge time.Duration) {
	c.gitCache.cleanupStale(maxAge)
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

// MergeBranches creates a merge commit of head into base and pushes it as
// branchName, entirely inside the persistent git cache (Gitea has no API to
// merge two arbitrary refs into a new branch). Conflicts are returned as
// MergeConflictError.
func (c *HTTPClient) MergeBranches(ctx context.Context, owner, repo, base, head, branchName string) (*MergeResult, error) {
	refs := []string{"+refs/heads/" + base + ":refs/heads/" + base, head}
	var result *MergeResult
	err := c.gitCache.withRepo(ctx, c.cloneURL(owner, repo), owner, repo, refs, func(run gitRunFunc) error {
		sha, conflictOut, err := mergeCommit(run, "refs/heads/"+base, head, "mq: merge "+head+" into "+base)
		if err != nil {
			return fmt.Errorf("merge: %w", err)
		}
		if sha == "" {
			return &MergeConflictError{Base: base, Head: head, Message: conflictOut}
		}
		if _, err := run("push", "--quiet", "origin", sha+":refs/heads/"+branchName); err != nil {
			return fmt.Errorf("push: %w", err)
		}
		result = &MergeResult{SHA: sha}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slog.Debug("created merge branch", "branch", branchName, "sha", shortSHA(result.SHA))
	return result, nil
}

// gitRunFunc executes one git command inside the cached repository.
type gitRunFunc = func(args ...string) (string, error)

// mergeCommit merges head into base in-memory (merge-tree + commit-tree) and
// returns the merge commit SHA; a conflict returns an empty SHA plus
// merge-tree's report. A merge commit is created even when fast-forward would
// be possible, so CI always sees the combined result (like merge --no-ff).
func mergeCommit(run gitRunFunc, base, head, msg string) (sha, conflictOut string, err error) {
	out, err := run("merge-tree", "--write-tree", base, head)
	if err != nil {
		// merge-tree exits 1 on content conflicts and >1 on real errors.
		if gitExitCode(err) == 1 {
			return "", out, nil
		}
		return "", "", err
	}
	tree := strings.TrimSpace(out)
	commit, err := run("commit-tree", tree, "-p", base, "-p", head, "-m", msg)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(commit), "", nil
}

// gitExitCode extracts the git process exit code from a runner error, or -1.
func gitExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// StackStep is the per-head outcome of StackMerges.
type StackStep struct {
	Conflict bool
	Err      error
}

// StackMerges merges each head onto base in order inside the cached repo and
// pushes the result as branch. On a per-head conflict or fetch failure the
// step is marked and subsequent heads merge onto the pre-failure tip. A
// cache or final-push failure is returned as err so the caller can retry the
// whole build instead of mis-attributing a transient error to one PR.
func (c *HTTPClient) StackMerges(ctx context.Context, owner, repo, base string, heads []string, branch string) (string, []StackStep, error) {
	steps := make([]StackStep, len(heads))
	refs := []string{"+refs/heads/" + base + ":refs/heads/" + base}
	var tip string
	err := c.gitCache.withRepo(ctx, c.cloneURL(owner, repo), owner, repo, refs, func(run gitRunFunc) error {
		current := "refs/heads/" + base
		for i, head := range heads {
			// Heads are fetched one by one so a vanished head SHA fails only
			// its own step, not the whole batch.
			if _, err := run("fetch", "--quiet", "--filter=blob:none", "origin", head); err != nil {
				steps[i].Err = fmt.Errorf("fetch %s: %w", shortSHA(head), err)
				continue
			}
			sha, _, err := mergeCommit(run, current, head,
				fmt.Sprintf("mq: merge %s into %s", shortSHA(head), base))
			if err != nil {
				steps[i].Err = fmt.Errorf("merge %s: %w", shortSHA(head), err)
				continue
			}
			if sha == "" {
				steps[i].Conflict = true
				continue
			}
			current, tip = sha, sha
		}
		if tip == "" {
			return nil
		}
		if _, err := run("push", "--quiet", "origin", tip+":refs/heads/"+branch); err != nil {
			return fmt.Errorf("push %s: %w", branch, err)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if tip != "" {
		slog.Debug("stack-merge built", "branch", branch, "heads", len(heads), "tip", shortSHA(tip))
	}
	return tip, steps, nil
}

// cloneURL returns the repo's plain HTTPS clone URL; authentication is
// injected per git invocation via an extraHeader, never stored in the URL.
func (c *HTTPClient) cloneURL(owner, repo string) string {
	return fmt.Sprintf("%s/%s/%s.git", c.baseURL, owner, repo)
}

// FastForwardRef pushes sha to refs/heads/branch with a non-force refspec
// straight from the cached repo. git's client-side fast-forward check needs
// sha's ancestry back to the current branch tip, so both are fetched first;
// the push pack itself is empty because the server already has sha.
func (c *HTTPClient) FastForwardRef(ctx context.Context, owner, repo, branch, sha string) error {
	refs := []string{"+refs/heads/" + branch + ":refs/heads/" + branch, sha}
	return c.gitCache.withRepo(ctx, c.cloneURL(owner, repo), owner, repo, refs, func(run gitRunFunc) error {
		out, err := run("push", "--porcelain", "origin", sha+":refs/heads/"+branch)
		if err == nil {
			return nil
		}
		return classifyPushFailure(branch, sha, c.redact(out), err)
	})
}

// classifyPushFailure maps a failed `git push --porcelain` to a typed error
// by parsing its rejection line: "! <from>:<to> <summary> (<reason>)".
// Client-side ancestry rejections carry fixed reasons; "[remote rejected]"
// carries the server hook's message, i.e. branch protection denied the push.
// Output without a rejection line stays a generic error.
func classifyPushFailure(branch, sha, out string, err error) error {
	for line := range strings.Lines(out) {
		flag, rest, ok := strings.Cut(line, "\t")
		if !ok || flag != "!" {
			continue
		}
		_, result, ok := strings.Cut(rest, "\t")
		if !ok {
			continue
		}
		summary, reason, _ := strings.Cut(strings.TrimSpace(result), " (")
		reason = strings.TrimSuffix(reason, ")")
		if summary == "[remote rejected]" {
			return &ProtectedBranchError{Branch: branch, Message: reason}
		}
		switch reason {
		case "non-fast-forward", "fetch first", "needs force", "stale info":
			return &NotFastForwardError{Branch: branch, SHA: sha}
		}
	}
	return fmt.Errorf("git push %s: %w", branch, err)
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

// ServerVersion returns the Gitea/Forgejo server version string.
// GET /version
func (c *HTTPClient) ServerVersion(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := c.decodeJSON(resp, &v); err != nil {
		return "", fmt.Errorf("get server version: %w", err)
	}
	return v.Version, nil
}

// Ensure HTTPClient implements Client at compile time.
var _ Client = (*HTTPClient)(nil)
