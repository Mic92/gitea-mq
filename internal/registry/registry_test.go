package registry_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jogman/gitea-mq/internal/config"
	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/registry"
	"github.com/jogman/gitea-mq/internal/testutil"
)

func newTestRegistry(t *testing.T) (*registry.RepoRegistry, context.Context) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pool := testutil.NewTestDB(t, testutil.Server())
	queueSvc := queue.NewService(pool)

	deps := &registry.Deps{
		Gitea:          &gitea.MockClient{},
		Queue:          queueSvc,
		PollInterval:   1 * time.Hour, // long interval so pollers don't fire during tests
		CheckTimeout:   1 * time.Hour,
		SuccessTimeout: 5 * time.Minute,
	}

	return registry.New(ctx, deps), ctx
}

func TestAddAndLookup(t *testing.T) {
	reg, ctx := newTestRegistry(t)
	ref := config.RepoRef{Owner: "org", Name: "app"}

	if err := reg.Add(ctx, ref); err != nil {
		t.Fatalf("Add: %v", err)
	}

	m, ok := reg.Lookup("org/app")
	if !ok {
		t.Fatal("expected repo to be found after Add")
	}
	if m.Ref != ref {
		t.Errorf("expected ref %v, got %v", ref, m.Ref)
	}
	if m.RepoID == 0 {
		t.Error("expected non-zero RepoID")
	}
}

func TestAddIdempotent(t *testing.T) {
	reg, ctx := newTestRegistry(t)
	ref := config.RepoRef{Owner: "org", Name: "app"}

	if err := reg.Add(ctx, ref); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := reg.Add(ctx, ref); err != nil {
		t.Fatalf("second Add: %v", err)
	}

	refs := reg.List()
	if len(refs) != 1 {
		t.Errorf("expected 1 repo after double Add, got %d", len(refs))
	}
}

func TestRemove(t *testing.T) {
	reg, ctx := newTestRegistry(t)
	ref := config.RepoRef{Owner: "org", Name: "app"}

	if err := reg.Add(ctx, ref); err != nil {
		t.Fatalf("Add: %v", err)
	}

	reg.Remove(ref)

	_, ok := reg.Lookup("org/app")
	if ok {
		t.Error("expected repo to be gone after Remove")
	}
	if reg.Contains("org/app") {
		t.Error("Contains should return false after Remove")
	}
}

func TestRemoveNonExistent(t *testing.T) {
	reg, _ := newTestRegistry(t)
	// Should not panic.
	reg.Remove(config.RepoRef{Owner: "org", Name: "nope"})
}

func TestConcurrentAccess(t *testing.T) {
	reg, ctx := newTestRegistry(t)

	var wg sync.WaitGroup

	// Concurrent adds of different repos.
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ref := config.RepoRef{Owner: "org", Name: fmt.Sprintf("repo-%d", n)}
			_ = reg.Add(ctx, ref)
		}(i)
	}

	// Concurrent reads while adds are happening.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.List()
			_ = reg.Contains("org/repo-0")
			_, _ = reg.Lookup("org/repo-0")
		}()
	}

	wg.Wait()

	if len(reg.List()) != 10 {
		t.Errorf("expected 10 repos, got %d", len(reg.List()))
	}
}
