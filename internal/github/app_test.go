package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"

	githubpkg "github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
)

// testKey generates a small RSA key so ghinstallation can sign real JWTs;
// ghfake never verifies them but the transport refuses to construct without
// valid PEM.
func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func newTestApp(t *testing.T, srv *ghfake.Server) *githubpkg.App {
	t.Helper()
	app, err := githubpkg.NewApp(1, testKey(t), srv.URL+"/api/v3")
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	if err := app.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return app
}

func TestApp_RefreshRoutesByInstallation(t *testing.T) {
	srv := ghfake.New()
	defer srv.Close()

	srv.AddRepo("orgA", "app")
	srv.AddRepo("orgB", "lib")
	srv.AddInstallation(100, "orgA/app")
	srv.AddInstallation(200, "orgB/lib")

	app := newTestApp(t, srv)

	repos := app.Repos()
	slices.Sort(repos)
	if !slices.Equal(repos, []string{"orgA/app", "orgB/lib"}) {
		t.Fatalf("Repos() = %v", repos)
	}

	// Distinct installations must yield distinct clients (different tokens).
	cA, err := app.ClientForRepo("orgA", "app")
	if err != nil {
		t.Fatalf("ClientForRepo orgA: %v", err)
	}
	cB, err := app.ClientForRepo("orgB", "lib")
	if err != nil {
		t.Fatalf("ClientForRepo orgB: %v", err)
	}
	if cA == cB {
		t.Error("expected distinct clients for distinct installations")
	}

	if _, err := app.ClientForRepo("orgC", "nope"); err == nil {
		t.Error("expected error for repo without installation")
	}
}
