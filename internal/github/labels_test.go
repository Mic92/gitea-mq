package github_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
)

func newLabeler(t *testing.T) (*ghfake.Server, forge.Labeler) {
	t.Helper()
	srv, f := newTestForge(t)
	l, ok := f.(forge.Labeler)
	if !ok {
		t.Fatal("github forge does not implement forge.Labeler")
	}
	srv.AddPR("org", "app", ghfake.PR{Number: 7, HeadSHA: "h7", BaseRef: "main"})
	return srv, l
}

func prLabels(srv *ghfake.Server) []string {
	return slices.Clone(srv.Repo("org", "app").PRs[7].Labels)
}

func TestApplyLabelsCreatesLabelAndSwapsState(t *testing.T) {
	srv, l := newLabeler(t)
	ctx := t.Context()

	if err := l.ApplyLabels(ctx, "org", "app", 7, []string{"mq/queued"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := prLabels(srv); !slices.Equal(got, []string{"mq/queued"}) {
		t.Fatalf("labels = %v", got)
	}
	if !srv.Repo("org", "app").Labels["mq/queued"] {
		t.Fatal("repo label mq/queued was not created")
	}

	// GitHub has no exclusive scopes: the old state is removed explicitly.
	if err := l.ApplyLabels(ctx, "org", "app", 7, []string{"mq/testing"}, []string{"mq/queued"}); err != nil {
		t.Fatal(err)
	}
	if got := prLabels(srv); !slices.Equal(got, []string{"mq/testing"}) {
		t.Fatalf("labels = %v", got)
	}
}

func TestApplyLabelsCreatesEachLabelOnce(t *testing.T) {
	srv, l := newLabeler(t)
	for range 3 {
		if err := l.ApplyLabels(t.Context(), "org", "app", 7, []string{"mq/queued"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.Repo("org", "app").LabelCreates; n != 1 {
		t.Fatalf("label creates = %d, want 1", n)
	}
}

func TestApplyLabelsToleratesExistingLabelAndMissingRemoval(t *testing.T) {
	srv, l := newLabeler(t)
	srv.Repo("org", "app").Labels["mq/queued"] = true

	if err := l.ApplyLabels(t.Context(), "org", "app", 7, []string{"mq/queued"}, []string{"mq/gone"}); err != nil {
		t.Fatal(err)
	}
	if got := prLabels(srv); !slices.Equal(got, []string{"mq/queued"}) {
		t.Fatalf("labels = %v", got)
	}
}

func TestApplyLabelsForbiddenMapsToErrLabelsForbidden(t *testing.T) {
	srv, l := newLabeler(t)
	srv.Repo("org", "app").LabelsForbidden = true

	err := l.ApplyLabels(t.Context(), "org", "app", 7, []string{"mq/queued"}, nil)
	if !errors.Is(err, forge.ErrLabelsForbidden) {
		t.Fatalf("err = %v, want ErrLabelsForbidden", err)
	}
}

func TestDeleteLabelDetachesFromPRsAndIgnoresMissing(t *testing.T) {
	srv, l := newLabeler(t)
	if err := l.ApplyLabels(t.Context(), "org", "app", 7, []string{"mq/batch-3"}, nil); err != nil {
		t.Fatal(err)
	}

	if err := l.DeleteLabel(t.Context(), "org", "app", "mq/batch-3"); err != nil {
		t.Fatal(err)
	}
	if got := prLabels(srv); len(got) != 0 {
		t.Fatalf("labels = %v, want none", got)
	}
	if err := l.DeleteLabel(t.Context(), "org", "app", "mq/batch-3"); err != nil {
		t.Fatalf("second delete: %v", err)
	}

	// The label is gone server-side, so the next use must recreate it.
	if err := l.ApplyLabels(t.Context(), "org", "app", 7, []string{"mq/batch-3"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := prLabels(srv); !slices.Equal(got, []string{"mq/batch-3"}) {
		t.Fatalf("labels = %v", got)
	}
}
