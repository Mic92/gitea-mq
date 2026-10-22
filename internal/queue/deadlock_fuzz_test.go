package queue_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/jackc/pgx/v5/pgconn"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

type fuzzOp struct {
	name string
	run  func(ctx context.Context, r *rand.Rand) error
}

// DEADLOCK_FUZZ=1 [FUZZ_WORKERS=16] [FUZZ_SECONDS=20] go test -run DeadlockFuzz -v ./internal/queue/
func TestDeadlockFuzz(t *testing.T) {
	if os.Getenv("DEADLOCK_FUZZ") == "" {
		t.Skip("set DEADLOCK_FUZZ=1 to run")
	}
	workers := envInt("FUZZ_WORKERS", 16)
	duration := time.Duration(envInt("FUZZ_SECONDS", 20)) * time.Second

	pool := jitterPool(t, int32(workers+2), 3*time.Millisecond, map[string]string{
		"deadlock_timeout":  "100ms",
		"lock_timeout":      "5s",
		"statement_timeout": "10s",
	})
	svc := queue.NewService(pool)
	ctx := t.Context()

	type repoKey struct{ forge, owner, name string }
	repoKeys := []repoKey{{"gitea", "org", "a"}, {"github", "org", "b"}}
	var repoIDs []int64
	for _, k := range repoKeys {
		r, err := svc.GetOrCreateRepo(ctx, k.forge, k.owner, k.name)
		if err != nil {
			t.Fatal(err)
		}
		repoIDs = append(repoIDs, r.ID)
	}
	branches := []string{"main", "release"}
	const prs = 6

	repo := func(r *rand.Rand) int64 { return repoIDs[r.IntN(len(repoIDs))] }
	branch := func(r *rand.Rand) string { return branches[r.IntN(len(branches))] }
	pr := func(r *rand.Rand) int64 { return 1 + r.Int64N(prs) }
	entry := func(ctx context.Context, r *rand.Rand) (*pg.QueueEntry, error) {
		return svc.GetEntry(ctx, repo(r), pr(r))
	}
	// Mirrors batch.Engine's per-branch lock.
	var engineLocks sync.Map
	engine := func(r *rand.Rand) (int64, string, func()) {
		id, br := repo(r), branch(r)
		m, _ := engineLocks.LoadOrStore(fmt.Sprint(id, br), &sync.Mutex{})
		m.(*sync.Mutex).Lock()
		return id, br, m.(*sync.Mutex).Unlock
	}
	subset := func(r *rand.Rand, ids []int64) []int64 {
		var out []int64
		for _, id := range ids {
			if r.IntN(2) == 0 {
				out = append(out, id)
			}
		}
		return out
	}
	states := []pg.EntryState{pg.EntryStateQueued, pg.EntryStateTesting, pg.EntryStateSuccess}

	ops := []fuzzOp{
		{"Enqueue", func(ctx context.Context, r *rand.Rand) error {
			_, err := svc.Enqueue(ctx, repo(r), pr(r), "sha", branch(r), r.IntN(3) == 0)
			return err
		}},
		{"Dequeue", func(ctx context.Context, r *rand.Rand) error {
			return svc.Dequeue(ctx, repo(r), pr(r))
		}},
		{"SetPriority", func(ctx context.Context, r *rand.Rand) error {
			return svc.SetPriority(ctx, repo(r), pr(r), r.IntN(2) == 0)
		}},
		{"UpdateState", func(ctx context.Context, r *rand.Rand) error {
			return svc.UpdateState(ctx, repo(r), pr(r), states[r.IntN(len(states))])
		}},
		{"SetMergeBranch", func(ctx context.Context, r *rand.Rand) error {
			return svc.SetMergeBranch(ctx, repo(r), pr(r), "b", "s")
		}},
		{"SetError", func(ctx context.Context, r *rand.Rand) error {
			return svc.SetError(ctx, repo(r), pr(r), "boom")
		}},
		{"SaveCheckStatus", func(ctx context.Context, r *rand.Rand) error {
			e, err := entry(ctx, r)
			if err != nil || e == nil {
				return err
			}
			return svc.SaveCheckStatus(ctx, e.ID, fmt.Sprintf("ci/%d", r.IntN(2)), pg.CheckStatePending, "")
		}},
		{"FormBatch", func(ctx context.Context, r *rand.Rand) error {
			id, br, unlock := engine(r)
			defer unlock()
			if b, err := svc.GetLiveBatch(ctx, id, br); err != nil || b != nil {
				return err
			}
			_, err := svc.FormBatch(ctx, id, br, r.IntN(4))
			return err
		}},
		{"SaveBatch", func(ctx context.Context, r *rand.Rand) error {
			id, br, unlock := engine(r)
			defer unlock()
			b, err := svc.GetLiveBatch(ctx, id, br)
			if err != nil || b == nil {
				return err
			}
			b.Builds++
			if r.IntN(3) == 0 {
				b.State = pg.BatchStateDone
			}
			return svc.SaveBatch(ctx, b, subset(r, b.MemberIds)...)
		}},
		{"ClearBatchMembers", func(ctx context.Context, r *rand.Rand) error {
			id, br, unlock := engine(r)
			defer unlock()
			b, err := svc.GetLiveBatch(ctx, id, br)
			if err != nil || b == nil {
				return err
			}
			if err := svc.ClearMergeBranch(ctx, b.MemberIds); err != nil {
				return err
			}
			return svc.ClearCheckStatuses(ctx, b.MemberIds)
		}},
		{"CancelLiveBatches", func(ctx context.Context, r *rand.Rand) error {
			return svc.CancelLiveBatches(ctx, repo(r))
		}},
		{"DequeueAll", func(ctx context.Context, r *rand.Rand) error {
			if r.IntN(10) != 0 {
				return nil
			}
			return svc.DequeueAll(ctx, repo(r))
		}},
		{"GetOrCreateRepo", func(ctx context.Context, r *rand.Rand) error {
			k := repoKeys[r.IntN(len(repoKeys))]
			_, err := svc.GetOrCreateRepo(ctx, k.forge, k.owner, k.name)
			return err
		}},
		{"Reads", func(ctx context.Context, r *rand.Rand) error {
			id, br := repo(r), branch(r)
			if _, err := svc.Head(ctx, id, br); err != nil {
				return err
			}
			if _, err := svc.Position(ctx, id, br, pr(r)); err != nil {
				return err
			}
			_, err := svc.ListActiveEntries(ctx, id)
			return err
		}},
	}

	type key struct{ code, op string }
	var (
		mu       sync.Mutex
		failures = map[key]int{}
		samples  = map[key]string{}
		calls    = map[string]int{}
	)
	record := func(op string, err error) {
		mu.Lock()
		defer mu.Unlock()
		calls[op]++
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		code := "go"
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			code = pgErr.Code
			if pgErr.Detail != "" {
				err = fmt.Errorf("%w\n      detail: %s", err, pgErr.Detail)
			}
		}
		k := key{code, op}
		failures[k]++
		if _, ok := samples[k]; !ok {
			samples[k] = err.Error()
		}
	}

	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("FUZZ_SEED"); s != "" {
		seed, _ = strconv.ParseUint(s, 10, 64)
	}
	t.Logf("seed=%d workers=%d duration=%s", seed, workers, duration)

	runCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(seed, uint64(w)))
			for runCtx.Err() == nil {
				op := ops[r.IntN(len(ops))]
				record(op.name, op.run(runCtx, r))
			}
		})
	}
	wg.Wait()

	var total int
	for _, n := range calls {
		total += n
	}
	t.Logf("%d operations", total)

	keys := make([]key, 0, len(failures))
	for k := range failures {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return failures[keys[i]] > failures[keys[j]] })
	for _, k := range keys {
		t.Logf("%-6s %-18s x%-5d %s", k.code, k.op, failures[k], samples[k])
	}
	for _, k := range keys {
		if k.code == "40P01" || k.code == "55P03" || k.code == "57014" {
			t.Errorf("lock problem in %s (%s)", k.op, k.code)
		}
	}
}
