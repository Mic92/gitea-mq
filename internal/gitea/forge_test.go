package gitea_test

import (
	"context"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

func newForge(mock *gitea.MockClient) forge.Forge {
	return gitea.NewForge(mock, "https://gitea.example.com")
}

func TestForge_ListOpenPRs_FoldsTimeline(t *testing.T) {
	// PR 1 scheduled → AutoMergeEnabled; PR 2 cancelled, PR 3 no events → not.
	mock := &gitea.MockClient{
		ListOpenPRsFn: func(_ context.Context, _, _ string) ([]gitea.PR, error) {
			return []gitea.PR{
				{Index: 1, Title: "one", State: "open", Head: &gitea.PRRef{Ref: "feat-1", Sha: "sha1"}, Base: &gitea.PRRef{Ref: "main"}, User: &gitea.User{Login: "alice"}},
				{Index: 2, Title: "two", State: "open", Head: &gitea.PRRef{Ref: "feat-2", Sha: "sha2"}, Base: &gitea.PRRef{Ref: "main"}},
				{Index: 3, Title: "three", State: "open", Head: &gitea.PRRef{Ref: "feat-3", Sha: "sha3"}, Base: &gitea.PRRef{Ref: "main"}},
			}, nil
		},
		GetPRTimelineFn: func(_ context.Context, _, _ string, index int64) ([]gitea.TimelineComment, error) {
			switch index {
			case 1:
				return []gitea.TimelineComment{
					{Type: "comment"},
					{Type: "pull_scheduled_merge"},
				}, nil
			case 2:
				return []gitea.TimelineComment{
					{Type: "pull_scheduled_merge"},
					{Type: "pull_cancel_scheduled_merge"},
				}, nil
			default:
				return nil, nil
			}
		},
	}

	f := newForge(mock)
	prs, err := f.ListOpenPRs(context.Background(), "org", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 3 {
		t.Fatalf("got %d PRs, want 3: %+v", len(prs), prs)
	}
	if !prs[0].AutoMergeEnabled || prs[1].AutoMergeEnabled || prs[2].AutoMergeEnabled {
		t.Errorf("AutoMergeEnabled mapping wrong: %+v", prs)
	}
	got := prs[0]
	if got.Number != 1 || got.HeadSHA != "sha1" || got.HeadBranch != "feat-1" || got.BaseBranch != "main" {
		t.Errorf("ref mapping wrong: %+v", got)
	}
	if got.AuthorLogin != "alice" {
		t.Errorf("AuthorLogin = %q, want alice", got.AuthorLogin)
	}
}

func TestForge_GetRequiredChecks_StripsSelf(t *testing.T) {
	mock := &gitea.MockClient{
		GetBranchProtectionFn: func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
			return &gitea.BranchProtection{
				EnableStatusCheck:   true,
				StatusCheckContexts: []string{"ci/build", "gitea-mq", "ci/test"},
			}, nil
		},
	}

	f := newForge(mock)
	checks, err := f.GetRequiredChecks(context.Background(), "org", "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ci/build", "ci/test"}
	if len(checks) != len(want) {
		t.Fatalf("got %v, want %v", checks, want)
	}
	for i := range want {
		if checks[i] != want[i] {
			t.Fatalf("got %v, want %v", checks, want)
		}
	}
}

func TestForge_GetRequiredChecks_NoProtection(t *testing.T) {
	mock := &gitea.MockClient{
		GetBranchProtectionFn: func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
			return nil, nil
		},
	}
	f := newForge(mock)
	checks, err := f.GetRequiredChecks(context.Background(), "org", "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if checks != nil {
		t.Fatalf("got %v, want nil", checks)
	}
}

func TestForge_CreateMergeBranch_ConflictMapped(t *testing.T) {
	mock := &gitea.MockClient{
		MergeBranchesFn: func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
			return nil, &gitea.MergeConflictError{Base: "main", Head: "sha1", Message: "CONFLICT"}
		},
	}
	f := newForge(mock)
	sha, conflict, err := f.CreateMergeBranch(context.Background(), "org", "app", "main", "sha1", "gitea-mq/1")
	if err != nil {
		t.Fatalf("err = %v, want nil (conflict should not be an error)", err)
	}
	if !conflict {
		t.Error("conflict = false, want true")
	}
	if sha != "" {
		t.Errorf("sha = %q, want empty", sha)
	}
}

func TestForge_CreateMergeBranch_Success(t *testing.T) {
	f := newForge(&gitea.MockClient{
		MergeBranchesFn: func(_ context.Context, _, _, _, _, _ string) (*gitea.MergeResult, error) {
			return &gitea.MergeResult{SHA: "mergesha"}, nil
		},
	})
	sha, conflict, err := f.CreateMergeBranch(context.Background(), "org", "app", "main", "sha1", "gitea-mq/1")
	if err != nil || conflict || sha != "mergesha" {
		t.Fatalf("got (%q, %v, %v), want (mergesha, false, nil)", sha, conflict, err)
	}
}

func TestForge_GetCheckStates_MapsCombinedStatus(t *testing.T) {
	mock := &gitea.MockClient{
		GetCombinedCommitStatusFn: func(_ context.Context, _, _, _ string) (*gitea.CombinedStatus, error) {
			return &gitea.CombinedStatus{
				Statuses: []gitea.CommitStatusResult{
					{Context: "ci/build", Status: "success"},
					{Context: "ci/test", Status: "failure"},
					{Context: "gitea-mq", Status: "pending"},
				},
			}, nil
		},
	}
	f := newForge(mock)
	states, err := f.GetCheckStates(context.Background(), "org", "app", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if states["ci/build"].State != pg.CheckStateSuccess {
		t.Errorf("ci/build = %q, want success", states["ci/build"].State)
	}
	if states["ci/test"].State != pg.CheckStateFailure {
		t.Errorf("ci/test = %q, want failure", states["ci/test"].State)
	}
	if _, ok := states["gitea-mq"]; ok {
		t.Error("gitea-mq should be excluded from check states")
	}
}

func TestForge_SetMQStatus(t *testing.T) {
	mock := &gitea.MockClient{}
	f := newForge(mock)
	err := f.SetMQStatus(context.Background(), "org", "app", "abc", forge.MQStatus{
		State:       pg.CheckStatePending,
		Description: "Queued (#1)",
		TargetURL:   "https://mq/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := mock.CallsTo("CreateCommitStatus")
	if len(calls) != 1 {
		t.Fatalf("got %d CreateCommitStatus calls, want 1", len(calls))
	}
	st := calls[0].Args[3].(gitea.CommitStatus)
	if st.Context != "gitea-mq" {
		t.Errorf("Context = %q, want gitea-mq", st.Context)
	}
	if st.State != "pending" {
		t.Errorf("State = %q, want pending", st.State)
	}
	if st.Description != "Queued (#1)" || st.TargetURL != "https://mq/x" {
		t.Errorf("status fields not passed through: %+v", st)
	}
}

func TestForge_EnsureRepoSetup_BuildsWebhookURL(t *testing.T) {
	mock := &gitea.MockClient{
		ListBranchProtectionsFn: func(_ context.Context, _, _ string) ([]gitea.BranchProtection, error) {
			return []gitea.BranchProtection{{RuleName: "main", StatusCheckContexts: []string{"ci"}}}, nil
		},
	}
	f := newForge(mock)
	err := f.EnsureRepoSetup(context.Background(), "org", "app", forge.SetupConfig{
		ExternalURL:   "https://mq.example.com/",
		WebhookSecret: "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(mock.CallsTo("EditBranchProtection")) != 1 {
		t.Error("expected branch protection to be patched")
	}

	hooks := mock.CallsTo("CreateWebhook")
	if len(hooks) != 1 {
		t.Fatalf("got %d CreateWebhook calls, want 1", len(hooks))
	}
	opts := hooks[0].Args[2].(gitea.CreateWebhookOpts)
	if got := opts.Config["url"]; got != "https://mq.example.com/webhook/gitea" {
		t.Errorf("webhook url = %q, want https://mq.example.com/webhook/gitea", got)
	}
	if opts.Config["secret"] != "s3cret" {
		t.Error("webhook secret not passed through")
	}
}

func TestForge_EnsureRepoSetup_NoExternalURLSkipsWebhook(t *testing.T) {
	mock := &gitea.MockClient{}
	f := newForge(mock)
	err := f.EnsureRepoSetup(context.Background(), "org", "app", forge.SetupConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.CallsTo("ListWebhooks")) != 0 || len(mock.CallsTo("CreateWebhook")) != 0 {
		t.Error("webhook ops should be skipped when ExternalURL is empty")
	}
	if len(mock.CallsTo("ListBranchProtections")) != 1 {
		t.Error("branch protection should still run")
	}
}

func TestForge_URLHelpers(t *testing.T) {
	f := gitea.NewForge(&gitea.MockClient{}, "https://gitea.example.com/")
	if got := f.RepoHTMLURL("org", "app"); got != "https://gitea.example.com/org/app" {
		t.Errorf("RepoHTMLURL = %q", got)
	}
	if got := f.BranchHTMLURL("org", "app", "gitea-mq/1"); got != "https://gitea.example.com/org/app/src/branch/gitea-mq/1" {
		t.Errorf("BranchHTMLURL = %q", got)
	}
}
