package queue_test

import (
	"context"
	"testing"

	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// Migration 003 must backfill forge='gitea' for rows created under the old
// schema so existing deployments keep their queue history.
func TestMigration003_BackfillsForge(t *testing.T) {
	pool := testutil.NewRawTestDB(t, testutil.Server())
	ctx := context.Background()

	if err := pg.MigrateTo(pool, 2); err != nil {
		t.Fatalf("migrate to 002: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (owner, name) VALUES ('org', 'app')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := pg.MigrateTo(pool, 3); err != nil {
		t.Fatalf("migrate to 003: %v", err)
	}

	var forge string
	if err := pool.QueryRow(ctx, `SELECT forge FROM repos WHERE owner='org' AND name='app'`).Scan(&forge); err != nil {
		t.Fatalf("select: %v", err)
	}
	if forge != "gitea" {
		t.Fatalf("forge = %q, want gitea", forge)
	}
}

// Migration 005 keeps live entries as normal priority and purges terminal
// rows, which nothing filters out anymore.
func TestMigration005_PriorityAndPurge(t *testing.T) {
	pool := testutil.NewRawTestDB(t, testutil.Server())
	ctx := context.Background()

	if err := pg.MigrateTo(pool, 4); err != nil {
		t.Fatalf("migrate to 004: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO repos (owner, name) VALUES ('org', 'app');
		INSERT INTO queue_entries (repo_id, pr_number, pr_head_sha, target_branch, state)
		SELECT id, n, 'sha', 'main', s::entry_state FROM repos,
		  (VALUES (1, 'queued'), (2, 'failed'), (3, 'cancelled')) AS v(n, s)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := pg.MigrateTo(pool, 5); err != nil {
		t.Fatalf("migrate to 005: %v", err)
	}

	var n int64
	var priority bool
	if err := pool.QueryRow(ctx, `SELECT pr_number, priority FROM queue_entries`).Scan(&n, &priority); err != nil {
		t.Fatalf("select: %v", err)
	}
	if n != 1 || priority {
		t.Fatalf("got PR #%d priority=%v, want only PR #1 with priority=false", n, priority)
	}
}
