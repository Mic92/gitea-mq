package gitea_test

import (
	"context"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
)

func TestForge_ReviewBlocker(t *testing.T) {
	approved := gitea.Review{State: "APPROVED", Official: true}
	tests := []struct {
		name    string
		bp      *gitea.BranchProtection
		reviews []gitea.Review
		want    string
	}{
		{name: "unprotected", bp: nil, want: ""},
		{name: "no review rules", bp: &gitea.BranchProtection{}, want: ""},
		{
			name: "missing approvals",
			bp:   &gitea.BranchProtection{RequiredApprovals: 2},
			reviews: []gitea.Review{
				approved,
				{State: "APPROVED"}, // not official: no approve right
				{State: "APPROVED", Official: true, Dismissed: true}, // dismissed
				{State: "COMMENT", Official: true},
			},
			want: "Waiting for approvals (1/2)",
		},
		{
			name:    "enough approvals",
			bp:      &gitea.BranchProtection{RequiredApprovals: 2},
			reviews: []gitea.Review{approved, approved},
			want:    "",
		},
		{
			name:    "stale approval kept",
			bp:      &gitea.BranchProtection{RequiredApprovals: 1},
			reviews: []gitea.Review{{State: "APPROVED", Official: true, Stale: true}},
			want:    "",
		},
		{
			name:    "stale approval dismissed by rule",
			bp:      &gitea.BranchProtection{RequiredApprovals: 1, DismissStaleApprovals: true},
			reviews: []gitea.Review{{State: "APPROVED", Official: true, Stale: true}},
			want:    "Waiting for approvals (0/1)",
		},
		{
			name:    "changes requested blocks",
			bp:      &gitea.BranchProtection{RequiredApprovals: 1, BlockOnRejectedReviews: true},
			reviews: []gitea.Review{approved, {State: "REQUEST_CHANGES", Official: true}},
			want:    "Changes requested",
		},
		{
			name:    "changes requested ignored without rule",
			bp:      &gitea.BranchProtection{RequiredApprovals: 1},
			reviews: []gitea.Review{approved, {State: "REQUEST_CHANGES", Official: true}},
			want:    "",
		},
		{
			name:    "pending official review request",
			bp:      &gitea.BranchProtection{BlockOnOfficialReviewRequests: true},
			reviews: []gitea.Review{{State: "REQUEST_REVIEW", Official: true}},
			want:    "Waiting for requested reviews",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &gitea.MockClient{
				GetBranchProtectionFn: func(_ context.Context, _, _, branch string) (*gitea.BranchProtection, error) {
					if branch != "main" {
						t.Errorf("protection looked up for %q, want main", branch)
					}
					return tt.bp, nil
				},
				ListPRReviewsFn: func(_ context.Context, _, _ string, _ int64) ([]gitea.Review, error) {
					return tt.reviews, nil
				},
			}
			got, err := forge.ReviewBlocker(context.Background(), newForge(mock), "org", "app", 42, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Repos without review rules must not pay for a reviews call every poll.
func TestForge_ReviewBlocker_SkipsReviewsWithoutRules(t *testing.T) {
	mock := &gitea.MockClient{
		GetBranchProtectionFn: func(_ context.Context, _, _, _ string) (*gitea.BranchProtection, error) {
			return &gitea.BranchProtection{EnableStatusCheck: true}, nil
		},
	}
	if _, err := forge.ReviewBlocker(context.Background(), newForge(mock), "org", "app", 42, "main"); err != nil {
		t.Fatal(err)
	}
	if n := len(mock.CallsTo("ListPRReviews")); n != 0 {
		t.Fatalf("ListPRReviews called %d time(s)", n)
	}
}
