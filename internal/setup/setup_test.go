package setup_test

import (
	"context"
	"testing"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/setup"
)

func TestEnsureBranchProtection_AddsMissing(t *testing.T) {
	mock := &gitea.MockClient{
		ListBranchProtectionsFn: func(_ context.Context, _, _ string) ([]gitea.BranchProtection, error) {
			return []gitea.BranchProtection{
				{
					RuleName:            "main",
					BranchName:          "main",
					EnableStatusCheck:   true,
					StatusCheckContexts: []string{"ci/build"},
				},
			}, nil
		},
	}

	if err := setup.EnsureBranchProtection(context.Background(), mock, "org", "app"); err != nil {
		t.Fatal(err)
	}

	calls := mock.CallsTo("EditBranchProtection")
	if len(calls) != 1 {
		t.Fatalf("expected 1 EditBranchProtection call, got %d", len(calls))
	}

	opts := calls[0].Args[3].(gitea.EditBranchProtectionOpts)
	found := false
	for _, c := range opts.StatusCheckContexts {
		if c == "gitea-mq" {
			found = true
		}
	}
	if !found {
		t.Error("expected gitea-mq in status check contexts")
	}
	// Original check should be preserved.
	foundCI := false
	for _, c := range opts.StatusCheckContexts {
		if c == "ci/build" {
			foundCI = true
		}
	}
	if !foundCI {
		t.Error("expected ci/build preserved in status check contexts")
	}
}

func TestEnsureBranchProtection_AlreadyPresent(t *testing.T) {
	mock := &gitea.MockClient{
		ListBranchProtectionsFn: func(_ context.Context, _, _ string) ([]gitea.BranchProtection, error) {
			return []gitea.BranchProtection{
				{
					RuleName:            "main",
					BranchName:          "main",
					EnableStatusCheck:   true,
					StatusCheckContexts: []string{"ci/build", "gitea-mq"},
				},
			}, nil
		},
	}

	if err := setup.EnsureBranchProtection(context.Background(), mock, "org", "app"); err != nil {
		t.Fatal(err)
	}

	calls := mock.CallsTo("EditBranchProtection")
	if len(calls) != 0 {
		t.Fatalf("expected no EditBranchProtection calls when already present, got %d", len(calls))
	}
}

func TestEnsureBranchProtection_NoBranchProtection(t *testing.T) {
	mock := &gitea.MockClient{
		ListBranchProtectionsFn: func(_ context.Context, _, _ string) ([]gitea.BranchProtection, error) {
			return nil, nil
		},
	}

	// Should not error, just warn.
	if err := setup.EnsureBranchProtection(context.Background(), mock, "org", "app"); err != nil {
		t.Fatal(err)
	}

	calls := mock.CallsTo("EditBranchProtection")
	if len(calls) != 0 {
		t.Fatalf("expected no EditBranchProtection calls, got %d", len(calls))
	}
}

func TestEnsureWebhook_CreatesMissing(t *testing.T) {
	mock := &gitea.MockClient{
		ListWebhooksFn: func(_ context.Context, _, _ string) ([]gitea.Webhook, error) {
			return nil, nil
		},
	}

	if err := setup.EnsureWebhook(context.Background(), mock, "org", "app", "https://mq.example.com/webhook", "secret123"); err != nil {
		t.Fatal(err)
	}

	calls := mock.CallsTo("CreateWebhook")
	if len(calls) != 1 {
		t.Fatalf("expected 1 CreateWebhook call, got %d", len(calls))
	}

	opts := calls[0].Args[2].(gitea.CreateWebhookOpts)
	if opts.Config["url"] != "https://mq.example.com/webhook" {
		t.Errorf("expected webhook URL, got %q", opts.Config["url"])
	}
	if opts.Config["secret"] != "secret123" {
		t.Error("expected secret in webhook config")
	}
	if len(opts.Events) != 1 || opts.Events[0] != "status" {
		t.Errorf("expected [status] events, got %v", opts.Events)
	}
}

func TestEnsureWebhook_AlreadyExists(t *testing.T) {
	mock := &gitea.MockClient{
		ListWebhooksFn: func(_ context.Context, _, _ string) ([]gitea.Webhook, error) {
			return []gitea.Webhook{
				{
					ID:     1,
					Type:   "gitea",
					Config: map[string]string{"url": "https://mq.example.com/webhook"},
					Events: []string{"status"},
					Active: true,
				},
			}, nil
		},
	}

	if err := setup.EnsureWebhook(context.Background(), mock, "org", "app", "https://mq.example.com/webhook", "secret123"); err != nil {
		t.Fatal(err)
	}

	calls := mock.CallsTo("CreateWebhook")
	if len(calls) != 0 {
		t.Fatalf("expected no CreateWebhook calls when webhook exists, got %d", len(calls))
	}
}
