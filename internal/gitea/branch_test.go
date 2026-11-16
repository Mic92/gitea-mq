package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// GetBranch reads the protection that applies to a branch from the branches
// endpoint, which a token without repository admin may call. The
// branch_protections endpoint answers such a token with 403.
func TestGetBranch_ReadsStatusChecksWithoutAdmin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/org/app/branches/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"main","protected":true,"enable_status_check":true,` +
				`"status_check_contexts":["ci/build","gitea-mq"],"effective_branch_protection_name":""}`))
		case "/api/v1/repos/org/app/branch_protections/main":
			http.Error(w, `{"message":"user should be an owner or a collaborator with admin write of a repository"}`,
				http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "tok")

	b, err := c.GetBranch(context.Background(), "org", "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Name != "main" || !b.EnableStatusCheck {
		t.Fatalf("got %+v, want main with status checks enabled", b)
	}
	if want := []string{"ci/build", "gitea-mq"}; !slices.Equal(b.StatusCheckContexts, want) {
		t.Fatalf("contexts = %v, want %v", b.StatusCheckContexts, want)
	}

	checks, err := NewForge(c, srv.URL).GetRequiredChecks(context.Background(), "org", "app", "main")
	if err != nil {
		t.Fatalf("GetRequiredChecks: %v", err)
	}
	if want := []string{"ci/build"}; !slices.Equal(checks, want) {
		t.Fatalf("required checks = %v, want %v", checks, want)
	}
}

func TestGetBranch_MissingBranchIsNil(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	b, err := NewHTTPClient(srv.URL, "tok").GetBranch(context.Background(), "org", "app", "gone")
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		t.Fatalf("got %+v, want nil", b)
	}
}
