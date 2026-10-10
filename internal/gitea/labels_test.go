package gitea_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
)

func newLabeler(t *testing.T, mock *gitea.MockClient) forge.Labeler {
	t.Helper()
	l, ok := gitea.NewForge(mock, "https://gitea.example.com").(forge.Labeler)
	if !ok {
		t.Fatal("gitea forge does not implement forge.Labeler")
	}
	return l
}

// labelServer is a minimal in-memory label store behind MockClient.
type labelServer struct {
	labels []gitea.Label
	nextID int64
	issue  map[int64]bool // label ids on the PR
}

func newLabelServer(mock *gitea.MockClient, existing ...gitea.Label) *labelServer {
	s := &labelServer{labels: existing, nextID: 100, issue: map[int64]bool{}}
	mock.ListRepoLabelsFn = func(context.Context, string, string) ([]gitea.Label, error) {
		return slices.Clone(s.labels), nil
	}
	mock.CreateRepoLabelFn = func(_ context.Context, _, _, name, _ string, exclusive bool) (*gitea.Label, error) {
		s.nextID++
		l := gitea.Label{ID: s.nextID, Name: name, Exclusive: exclusive}
		s.labels = append(s.labels, l)
		return &l, nil
	}
	mock.AddIssueLabelsFn = func(_ context.Context, _, _ string, _ int64, ids []int64) ([]gitea.Label, error) {
		var out []gitea.Label
		for _, l := range s.labels {
			if slices.Contains(ids, l.ID) {
				s.issue[l.ID] = true
			}
			if s.issue[l.ID] {
				out = append(out, l)
			}
		}
		return out, nil
	}
	return s
}

func TestApplyLabelsCreatesExclusiveScopedLabelAndAddsByID(t *testing.T) {
	mock := &gitea.MockClient{}
	newLabelServer(mock)
	l := newLabeler(t, mock)

	if err := l.ApplyLabels(t.Context(), "o", "r", 7, []string{"mq/queued"}, nil); err != nil {
		t.Fatal(err)
	}

	creates := mock.CallsTo("CreateRepoLabel")
	if len(creates) != 1 || creates[0].Args[2] != "mq/queued" || creates[0].Args[4] != true {
		t.Fatalf("CreateRepoLabel calls = %v, want one exclusive mq/queued", creates)
	}
	adds := mock.CallsTo("AddIssueLabels")
	if len(adds) != 1 || !slices.Equal(adds[0].Args[3].([]int64), []int64{101}) {
		t.Fatalf("AddIssueLabels calls = %v, want id 101", adds)
	}
}

func TestApplyLabelsCachesLabelIDs(t *testing.T) {
	mock := &gitea.MockClient{}
	newLabelServer(mock, gitea.Label{ID: 5, Name: "mq/queued", Exclusive: true})
	l := newLabeler(t, mock)

	for range 3 {
		if err := l.ApplyLabels(t.Context(), "o", "r", 7, []string{"mq/queued"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(mock.CallsTo("ListRepoLabels")); n != 1 {
		t.Fatalf("ListRepoLabels calls = %d, want 1", n)
	}
	if n := len(mock.CallsTo("CreateRepoLabel")); n != 0 {
		t.Fatalf("CreateRepoLabel calls = %d, want 0", n)
	}
}

func TestApplyLabelsSkipsRemovalCoveredByExclusiveScope(t *testing.T) {
	mock := &gitea.MockClient{}
	newLabelServer(mock,
		gitea.Label{ID: 1, Name: "mq/queued", Exclusive: true},
		gitea.Label{ID: 2, Name: "mq/testing", Exclusive: true})
	l := newLabeler(t, mock)

	if err := l.ApplyLabels(t.Context(), "o", "r", 7, []string{"mq/testing"}, []string{"mq/queued"}); err != nil {
		t.Fatal(err)
	}
	if n := len(mock.CallsTo("RemoveIssueLabel")); n != 0 {
		t.Fatalf("RemoveIssueLabel calls = %d, want 0: Gitea swaps within an exclusive scope", n)
	}
}

func TestApplyLabelsRemovesExplicitlyWhenNotExclusive(t *testing.T) {
	mock := &gitea.MockClient{}
	newLabelServer(mock,
		gitea.Label{ID: 1, Name: "mq/queued"}, // created by hand, not exclusive
		gitea.Label{ID: 2, Name: "mq/testing", Exclusive: true})
	l := newLabeler(t, mock)

	if err := l.ApplyLabels(t.Context(), "o", "r", 7, []string{"mq/testing"}, []string{"mq/queued"}); err != nil {
		t.Fatal(err)
	}
	rm := mock.CallsTo("RemoveIssueLabel")
	if len(rm) != 1 || rm[0].Args[3] != int64(1) {
		t.Fatalf("RemoveIssueLabel calls = %v, want id 1", rm)
	}
}

func TestApplyLabelsRemoveOnlyDoesNotCreateLabels(t *testing.T) {
	mock := &gitea.MockClient{}
	newLabelServer(mock, gitea.Label{ID: 1, Name: "mq/queued", Exclusive: true})
	l := newLabeler(t, mock)

	if err := l.ApplyLabels(t.Context(), "o", "r", 7, nil, []string{"mq/queued", "mq/gone"}); err != nil {
		t.Fatal(err)
	}
	if n := len(mock.CallsTo("CreateRepoLabel")); n != 0 {
		t.Fatalf("CreateRepoLabel calls = %d, want 0", n)
	}
	if n := len(mock.CallsTo("AddIssueLabels")); n != 0 {
		t.Fatalf("AddIssueLabels calls = %d, want 0", n)
	}
	if rm := mock.CallsTo("RemoveIssueLabel"); len(rm) != 1 {
		t.Fatalf("RemoveIssueLabel calls = %v, want only the existing label", rm)
	}
}

func TestApplyLabelsForbiddenMapsToErrLabelsForbidden(t *testing.T) {
	mock := &gitea.MockClient{}
	newLabelServer(mock)
	mock.CreateRepoLabelFn = func(context.Context, string, string, string, string, bool) (*gitea.Label, error) {
		return nil, &gitea.APIError{StatusCode: 403, Body: "forbidden"}
	}
	l := newLabeler(t, mock)

	err := l.ApplyLabels(t.Context(), "o", "r", 7, []string{"mq/queued"}, nil)
	if !errors.Is(err, forge.ErrLabelsForbidden) {
		t.Fatalf("err = %v, want ErrLabelsForbidden", err)
	}
}

func TestApplyLabelsRefreshesStaleCache(t *testing.T) {
	mock := &gitea.MockClient{}
	srv := newLabelServer(mock, gitea.Label{ID: 1, Name: "mq/queued", Exclusive: true})
	l := newLabeler(t, mock)
	if err := l.ApplyLabels(t.Context(), "o", "r", 7, []string{"mq/queued"}, nil); err != nil {
		t.Fatal(err)
	}

	// Someone deleted and recreated the label: id 1 is now unknown to Gitea,
	// which silently drops it instead of failing.
	srv.labels = []gitea.Label{{ID: 9, Name: "mq/queued", Exclusive: true}}
	srv.issue = map[int64]bool{}
	if err := l.ApplyLabels(t.Context(), "o", "r", 7, []string{"mq/queued"}, nil); err != nil {
		t.Fatal(err)
	}
	if !srv.issue[9] {
		t.Fatalf("label 9 not applied after cache refresh: %v", srv.issue)
	}
}

func TestDeleteLabelResolvesIDAndIgnoresMissing(t *testing.T) {
	mock := &gitea.MockClient{}
	newLabelServer(mock, gitea.Label{ID: 4, Name: "mq/batch-3", Exclusive: true})
	l := newLabeler(t, mock)

	if err := l.DeleteLabel(t.Context(), "o", "r", "mq/batch-3"); err != nil {
		t.Fatal(err)
	}
	del := mock.CallsTo("DeleteRepoLabel")
	if len(del) != 1 || del[0].Args[2] != int64(4) {
		t.Fatalf("DeleteRepoLabel calls = %v, want id 4", del)
	}

	if err := l.DeleteLabel(t.Context(), "o", "r", "mq/batch-3"); err != nil {
		t.Fatal(err)
	}
	if n := len(mock.CallsTo("DeleteRepoLabel")); n != 1 {
		t.Fatalf("DeleteRepoLabel calls = %d, want 1", n)
	}
}
