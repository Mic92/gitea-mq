package forge

import (
	"context"
	"sync"
)

// MockCall records one invocation on a MockForge.
type MockCall struct {
	Method string
	Args   []any
}

// MockForge is a test double for Forge. Each method delegates to the matching
// *Fn field if set, else returns zero value and nil. Safe for concurrent use.
type MockForge struct {
	mu    sync.Mutex
	Calls []MockCall

	KindVal Kind

	RepoHTMLURLFn       func(owner, name string) string
	BranchHTMLURLFn     func(owner, name, branch string) string
	ListOpenPRsFn       func(ctx context.Context, owner, name string) ([]PR, error)
	GetPRFn             func(ctx context.Context, owner, name string, number int64) (*PR, error)
	SetMQStatusFn       func(ctx context.Context, owner, name, sha string, st MQStatus) error
	MirrorCheckFn       func(ctx context.Context, owner, name, sha, checkContext string, c Check) error
	GetRequiredChecksFn func(ctx context.Context, owner, name, branch string) ([]string, error)
	GetCheckStatesFn    func(ctx context.Context, owner, name, sha string) (map[string]Check, error)
	CreateMergeBranchFn func(ctx context.Context, owner, name, base, headSHA, branch string) (string, bool, error)
	DeleteBranchFn      func(ctx context.Context, owner, name, branch string) error
	ListBranchesFn      func(ctx context.Context, owner, name string) ([]string, error)
	CancelAutoMergeFn   func(ctx context.Context, owner, name string, number int64) error
	CommentFn           func(ctx context.Context, owner, name string, number int64, body string) error
	EnsureRepoSetupFn   func(ctx context.Context, owner, name string, cfg SetupConfig) error
}

var _ Forge = (*MockForge)(nil)

func (m *MockForge) record(method string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = append(m.Calls, MockCall{Method: method, Args: args})
}

// CallsTo returns all recorded calls to method, in order.
func (m *MockForge) CallsTo(method string) []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []MockCall
	for _, c := range m.Calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (m *MockForge) Kind() Kind {
	if m.KindVal == "" {
		return KindGitea
	}
	return m.KindVal
}

func (m *MockForge) RepoHTMLURL(owner, name string) string {
	m.record("RepoHTMLURL", owner, name)
	if m.RepoHTMLURLFn != nil {
		return m.RepoHTMLURLFn(owner, name)
	}
	return ""
}

func (m *MockForge) BranchHTMLURL(owner, name, branch string) string {
	m.record("BranchHTMLURL", owner, name, branch)
	if m.BranchHTMLURLFn != nil {
		return m.BranchHTMLURLFn(owner, name, branch)
	}
	return ""
}

func (m *MockForge) ListOpenPRs(ctx context.Context, owner, name string) ([]PR, error) {
	m.record("ListOpenPRs", owner, name)
	if m.ListOpenPRsFn != nil {
		return m.ListOpenPRsFn(ctx, owner, name)
	}
	return nil, nil
}

func (m *MockForge) GetPR(ctx context.Context, owner, name string, number int64) (*PR, error) {
	m.record("GetPR", owner, name, number)
	if m.GetPRFn != nil {
		return m.GetPRFn(ctx, owner, name, number)
	}
	return nil, nil
}

func (m *MockForge) SetMQStatus(ctx context.Context, owner, name, sha string, st MQStatus) error {
	m.record("SetMQStatus", owner, name, sha, st)
	if m.SetMQStatusFn != nil {
		return m.SetMQStatusFn(ctx, owner, name, sha, st)
	}
	return nil
}

func (m *MockForge) MirrorCheck(ctx context.Context, owner, name, sha, checkContext string, c Check) error {
	m.record("MirrorCheck", owner, name, sha, checkContext, c)
	if m.MirrorCheckFn != nil {
		return m.MirrorCheckFn(ctx, owner, name, sha, checkContext, c)
	}
	return nil
}

func (m *MockForge) GetRequiredChecks(ctx context.Context, owner, name, branch string) ([]string, error) {
	m.record("GetRequiredChecks", owner, name, branch)
	if m.GetRequiredChecksFn != nil {
		return m.GetRequiredChecksFn(ctx, owner, name, branch)
	}
	return nil, nil
}

func (m *MockForge) GetCheckStates(ctx context.Context, owner, name, sha string) (map[string]Check, error) {
	m.record("GetCheckStates", owner, name, sha)
	if m.GetCheckStatesFn != nil {
		return m.GetCheckStatesFn(ctx, owner, name, sha)
	}
	return nil, nil
}

func (m *MockForge) CreateMergeBranch(ctx context.Context, owner, name, base, headSHA, branch string) (string, bool, error) {
	m.record("CreateMergeBranch", owner, name, base, headSHA, branch)
	if m.CreateMergeBranchFn != nil {
		return m.CreateMergeBranchFn(ctx, owner, name, base, headSHA, branch)
	}
	return "", false, nil
}

func (m *MockForge) DeleteBranch(ctx context.Context, owner, name, branch string) error {
	m.record("DeleteBranch", owner, name, branch)
	if m.DeleteBranchFn != nil {
		return m.DeleteBranchFn(ctx, owner, name, branch)
	}
	return nil
}

func (m *MockForge) ListBranches(ctx context.Context, owner, name string) ([]string, error) {
	m.record("ListBranches", owner, name)
	if m.ListBranchesFn != nil {
		return m.ListBranchesFn(ctx, owner, name)
	}
	return nil, nil
}

func (m *MockForge) CancelAutoMerge(ctx context.Context, owner, name string, number int64) error {
	m.record("CancelAutoMerge", owner, name, number)
	if m.CancelAutoMergeFn != nil {
		return m.CancelAutoMergeFn(ctx, owner, name, number)
	}
	return nil
}

func (m *MockForge) Comment(ctx context.Context, owner, name string, number int64, body string) error {
	m.record("Comment", owner, name, number, body)
	if m.CommentFn != nil {
		return m.CommentFn(ctx, owner, name, number, body)
	}
	return nil
}

func (m *MockForge) EnsureRepoSetup(ctx context.Context, owner, name string, cfg SetupConfig) error {
	m.record("EnsureRepoSetup", owner, name, cfg)
	if m.EnsureRepoSetupFn != nil {
		return m.EnsureRepoSetupFn(ctx, owner, name, cfg)
	}
	return nil
}
