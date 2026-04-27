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
