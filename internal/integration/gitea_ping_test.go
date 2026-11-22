package integration_test

import (
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// Readiness relies on Ping failing for a revoked token, not just a down server.
func TestGiteaPing(t *testing.T) {
	giteaServer := testutil.GiteaInstance()
	if giteaServer == nil {
		t.Skip("gitea server not available")
	}
	api := testutil.NewGiteaAPI(giteaServer.URL)
	api.CreateToken(t, "write:repository", "write:issue")

	if err := gitea.NewHTTPClient(giteaServer.URL, api.Token).Ping(t.Context()); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if err := gitea.NewHTTPClient(giteaServer.URL, "not-a-token").Ping(t.Context()); err == nil {
		t.Fatal("invalid token: expected error")
	}
}
