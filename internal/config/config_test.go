package config

import (
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
)

func TestParseRepos_TagsForgeKind(t *testing.T) {
	got, err := parseRepos("org/app, org/lib", forge.KindGitea)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, r := range got {
		if r.Forge != forge.KindGitea {
			t.Errorf("%s: Forge = %q, want gitea", r, r.Forge)
		}
	}
	if got[0].String() != "gitea:org/app" {
		t.Errorf("String() = %q, want gitea:org/app", got[0].String())
	}
}

func TestParseRepos_Invalid(t *testing.T) {
	if _, err := parseRepos("noslash", forge.KindGitea); err == nil {
		t.Fatal("expected error for missing slash")
	}
}
