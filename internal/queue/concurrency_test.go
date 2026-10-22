package queue_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestEnqueueSurfacesInsertError(t *testing.T) {
	svc, ctx, _ := testutil.TestQueueService(t)

	_, err := svc.Enqueue(ctx, 999999, 1, "sha", "main", false)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("want foreign key violation, got %v", err)
	}
	if strings.Contains(err.Error(), "25P02") {
		t.Fatalf("error masked by lookup on aborted transaction: %v", err)
	}
}
