package gitea

import (
	"context"
	"fmt"
	"sync"
)

// MockCall records a single method call made to the mock client.
type MockCall struct {
	Method string
	Args   []any
}

// MockClient is a test double for Client that records all calls and returns
// configurable responses. Safe for concurrent use.
type MockClient struct {
	mu    sync.Mutex
	Calls []MockCall

	// Response configurators. Set these before calling the method under test.
	// Each returns (result, error). If nil, the method returns zero value + nil.

	ListUserReposFn         func(ctx context.Context) ([]Repo, error)
	GetRepoTopicsFn         func(ctx context.Context, owner, repo string) ([]string, error)
	ListOpenPRsFn           func(ctx context.Context, owner, repo string) ([]PR, error)
	GetPRFn                 func(ctx context.Context, owner, repo string, index int64) (*PR, error)
	GetPRTimelineFn         func(ctx context.Context, owner, repo string, index int64) ([]TimelineComment, error)
	CreateCommitStatusFn    func(ctx context.Context, owner, repo, sha string, status CommitStatus) error
	CreateCommentFn         func(ctx context.Context, owner, repo string, index int64, body string) error
	CancelAutoMergeFn       func(ctx context.Context, owner, repo string, index int64) error
	GetBranchProtectionFn   func(ctx context.Context, owner, repo, branch string) (*BranchProtection, error)
	ListBranchesFn          func(ctx context.Context, owner, repo string) ([]Branch, error)
	CreateBranchFn          func(ctx context.Context, owner, repo, name, target string) error
	DeleteBranchFn          func(ctx context.Context, owner, repo, name string) error
	MergeBranchesFn         func(ctx context.Context, owner, repo, base, head, branchName string) (*MergeResult, error)
	ListBranchProtectionsFn func(ctx context.Context, owner, repo string) ([]BranchProtection, error)
	EditBranchProtectionFn  func(ctx context.Context, owner, repo, name string, opts EditBranchProtectionOpts) error
	ListWebhooksFn          func(ctx context.Context, owner, repo string) ([]Webhook, error)
	CreateWebhookFn         func(ctx context.Context, owner, repo string, opts CreateWebhookOpts) error
}

// Ensure MockClient implements Client at compile time.
var _ Client = (*MockClient)(nil)

func (m *MockClient) record(method string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Calls = append(m.Calls, MockCall{Method: method, Args: args})
}

// CallsTo returns all recorded calls to the named method.
func (m *MockClient) CallsTo(method string) []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()

	var result []MockCall
	for _, c := range m.Calls {
		if c.Method == method {
			result = append(result, c)
		}
	}

	return result
}

// Reset clears all recorded calls.
func (m *MockClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Calls = nil
}

func (m *MockClient) ListUserRepos(ctx context.Context) ([]Repo, error) {
	m.record("ListUserRepos")

	if m.ListUserReposFn != nil {
		return m.ListUserReposFn(ctx)
	}

	return nil, nil
}

func (m *MockClient) GetRepoTopics(ctx context.Context, owner, repo string) ([]string, error) {
	m.record("GetRepoTopics", owner, repo)

	if m.GetRepoTopicsFn != nil {
		return m.GetRepoTopicsFn(ctx, owner, repo)
	}

	return nil, nil
}

func (m *MockClient) ListOpenPRs(ctx context.Context, owner, repo string) ([]PR, error) {
	m.record("ListOpenPRs", owner, repo)

	if m.ListOpenPRsFn != nil {
		return m.ListOpenPRsFn(ctx, owner, repo)
	}

	return nil, nil
}

func (m *MockClient) GetPR(ctx context.Context, owner, repo string, index int64) (*PR, error) {
	m.record("GetPR", owner, repo, index)

	if m.GetPRFn != nil {
		return m.GetPRFn(ctx, owner, repo, index)
	}

	return nil, fmt.Errorf("PR #%d not found", index)
}

func (m *MockClient) GetPRTimeline(ctx context.Context, owner, repo string, index int64) ([]TimelineComment, error) {
	m.record("GetPRTimeline", owner, repo, index)

	if m.GetPRTimelineFn != nil {
		return m.GetPRTimelineFn(ctx, owner, repo, index)
	}

	return nil, nil
}

func (m *MockClient) CreateCommitStatus(ctx context.Context, owner, repo, sha string, status CommitStatus) error {
	m.record("CreateCommitStatus", owner, repo, sha, status)

	if m.CreateCommitStatusFn != nil {
		return m.CreateCommitStatusFn(ctx, owner, repo, sha, status)
	}

	return nil
}

func (m *MockClient) CreateComment(ctx context.Context, owner, repo string, index int64, body string) error {
	m.record("CreateComment", owner, repo, index, body)

	if m.CreateCommentFn != nil {
		return m.CreateCommentFn(ctx, owner, repo, index, body)
	}

	return nil
}

func (m *MockClient) CancelAutoMerge(ctx context.Context, owner, repo string, index int64) error {
	m.record("CancelAutoMerge", owner, repo, index)

	if m.CancelAutoMergeFn != nil {
		return m.CancelAutoMergeFn(ctx, owner, repo, index)
	}

	return nil
}

func (m *MockClient) GetBranchProtection(ctx context.Context, owner, repo, branch string) (*BranchProtection, error) {
	m.record("GetBranchProtection", owner, repo, branch)

	if m.GetBranchProtectionFn != nil {
		return m.GetBranchProtectionFn(ctx, owner, repo, branch)
	}

	return nil, nil
}

func (m *MockClient) ListBranches(ctx context.Context, owner, repo string) ([]Branch, error) {
	m.record("ListBranches", owner, repo)

	if m.ListBranchesFn != nil {
		return m.ListBranchesFn(ctx, owner, repo)
	}

	return nil, nil
}

func (m *MockClient) CreateBranch(ctx context.Context, owner, repo, name, target string) error {
	m.record("CreateBranch", owner, repo, name, target)

	if m.CreateBranchFn != nil {
		return m.CreateBranchFn(ctx, owner, repo, name, target)
	}

	return nil
}

func (m *MockClient) DeleteBranch(ctx context.Context, owner, repo, name string) error {
	m.record("DeleteBranch", owner, repo, name)

	if m.DeleteBranchFn != nil {
		return m.DeleteBranchFn(ctx, owner, repo, name)
	}

	return nil
}

func (m *MockClient) MergeBranches(ctx context.Context, owner, repo, base, head, branchName string) (*MergeResult, error) {
	m.record("MergeBranches", owner, repo, base, head, branchName)

	if m.MergeBranchesFn != nil {
		return m.MergeBranchesFn(ctx, owner, repo, base, head, branchName)
	}

	return &MergeResult{SHA: "mock-merge-sha"}, nil
}

func (m *MockClient) ListBranchProtections(ctx context.Context, owner, repo string) ([]BranchProtection, error) {
	m.record("ListBranchProtections", owner, repo)

	if m.ListBranchProtectionsFn != nil {
		return m.ListBranchProtectionsFn(ctx, owner, repo)
	}

	return nil, nil
}

func (m *MockClient) EditBranchProtection(ctx context.Context, owner, repo, name string, opts EditBranchProtectionOpts) error {
	m.record("EditBranchProtection", owner, repo, name, opts)

	if m.EditBranchProtectionFn != nil {
		return m.EditBranchProtectionFn(ctx, owner, repo, name, opts)
	}

	return nil
}

func (m *MockClient) ListWebhooks(ctx context.Context, owner, repo string) ([]Webhook, error) {
	m.record("ListWebhooks", owner, repo)

	if m.ListWebhooksFn != nil {
		return m.ListWebhooksFn(ctx, owner, repo)
	}

	return nil, nil
}

func (m *MockClient) CreateWebhook(ctx context.Context, owner, repo string, opts CreateWebhookOpts) error {
	m.record("CreateWebhook", owner, repo, opts)

	if m.CreateWebhookFn != nil {
		return m.CreateWebhookFn(ctx, owner, repo, opts)
	}

	return nil
}
