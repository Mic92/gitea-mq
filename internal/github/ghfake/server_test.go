package ghfake_test

import (
	"context"
	"testing"

	"github.com/Mic92/gitea-mq/internal/github/ghfake"
)

// Sanity check that go-github can talk to the fake at all; deeper behaviour
// is exercised by the adapter tests.
func TestClient_GetRepo(t *testing.T) {
	srv := ghfake.New()
	defer srv.Close()
	srv.AddRepo("org", "app")

	repo, _, err := srv.Client().Repositories.Get(context.Background(), "org", "app")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if repo.GetFullName() != "org/app" {
		t.Errorf("FullName = %q", repo.GetFullName())
	}
}
