package batch_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// A dependency added while the batch was testing: the forge would refuse to
// merge that PR, and the fast-forward would not. It is held back with its merge
// intent intact, and the rest is tested again instead of landing.
func TestHandlePass_HoldsMemberWithNewDependency(t *testing.T) {
	e, f, svc, ctx := setup(t, 10, 20, 30)
	b, err := e.FormAndBuild(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	f.OpenDependenciesFn = func(_ context.Context, _, _ string, n int64) ([]forge.Dependency, error) {
		if n == 20 {
			return []forge.Dependency{{Number: 7}}, nil
		}
		return nil, nil
	}

	if err := e.HandlePass(ctx, b); err != nil {
		t.Fatal(err)
	}
	if f.target != "main0" {
		t.Fatalf("target moved to %q although #20 has an open dependency", f.target)
	}
	if want := "m(m(main0,sha10),sha30)"; b.State != pg.BatchStateTesting || b.BranchSha.String != want {
		t.Fatalf("want the rest rebuilt as %q and testing, got %+v", want, b)
	}
	if ent, _ := svc.GetEntry(ctx, e.RepoID, 20); ent != nil {
		t.Fatalf("#20 still queued: %+v", ent)
	}
	if n := len(f.CallsTo("CancelAutoMerge")) + len(f.CallsTo("RemoveLabel")); n != 0 {
		t.Fatalf("merge intent withdrawn %d time(s); it must stay scheduled", n)
	}
	if n := len(f.CallsTo("Comment")); n != 1 {
		t.Fatalf("Comment calls = %d, want 1", n)
	}
}

// A failed lookup lands nothing and changes nothing: the pass is retried with
// the next check instead of landing a PR whose dependencies are unknown.
func TestHandlePass_DependencyLookupErrorLandsNothing(t *testing.T) {
	e, f, _, ctx := setup(t, 10)
	b, err := e.FormAndBuild(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	f.OpenDependenciesFn = func(context.Context, string, string, int64) ([]forge.Dependency, error) {
		return nil, errors.New("forge down")
	}

	if err := e.HandlePass(ctx, b); err == nil {
		t.Fatal("want the lookup error back")
	}
	if f.target != "main0" || b.State != pg.BatchStateTesting || len(b.CurrentIds) != 1 {
		t.Fatalf("batch changed on a failed lookup: target=%q batch=%+v", f.target, b)
	}
}
