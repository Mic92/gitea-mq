package github_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/github/ghfake"
)

// A pre-existing "require pull request" ruleset rejects the queue's
// fast-forward unless the App is a bypass actor there too.
func TestForge_EnsureRepoSetup_bypassesExistingRulesets(t *testing.T) {
	srv, f := newTestForge(t)
	ctx := context.Background()
	repo := srv.Repo("org", "app")
	repo.Rulesets = []*ghfake.Ruleset{
		{
			ID: 10, Name: "default", Target: "branch", SourceType: "Repository", Enforcement: "active",
			Rules: []ghfake.RulesetRule{{Type: "pull_request"}},
		},
		{
			ID: 11, Name: "has-it", Target: "branch", SourceType: "Repository", Enforcement: "active",
			BypassActors: []ghfake.BypassActor{{ActorID: 1, ActorType: "Integration", BypassMode: "always"}},
		},
		{ID: 12, Name: "tags", Target: "tag", SourceType: "Repository", Enforcement: "active"},
		{ID: 13, Name: "org", Target: "branch", SourceType: "Organization", Enforcement: "active"},
	}

	for range 2 {
		if err := f.EnsureRepoSetup(ctx, "org", "app", forge.SetupConfig{}); err != nil {
			t.Fatalf("EnsureRepoSetup: %v", err)
		}
	}

	want := map[string]int{"default": 1, "has-it": 1, "tags": 0, "org": 0, forge.MQContext: 2}
	for _, rs := range repo.Rulesets {
		if got := len(rs.BypassActors); got != want[rs.Name] {
			t.Errorf("ruleset %q: %d bypass actors, want %d: %+v", rs.Name, got, want[rs.Name], rs.BypassActors)
		}
	}
	if repo.Rulesets[0].BypassActors[0] != (ghfake.BypassActor{ActorID: 1, ActorType: "Integration", BypassMode: "always"}) {
		t.Errorf("default bypass = %+v", repo.Rulesets[0].BypassActors)
	}
	if len(repo.Rulesets[0].Rules) != 1 {
		t.Errorf("update dropped rules: %+v", repo.Rulesets[0].Rules)
	}
}

func TestForge_EnsureRepoSetup(t *testing.T) {
	srv, f := newTestForge(t)
	ctx := context.Background()

	if err := f.EnsureRepoSetup(ctx, "org", "app", forge.SetupConfig{}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	repo := srv.Repo("org", "app")
	if repo.Settings["allow_auto_merge"] != true {
		t.Errorf("allow_auto_merge = %v, want true", repo.Settings["allow_auto_merge"])
	}

	if len(repo.Rulesets) != 1 || repo.Rulesets[0].Name != forge.MQContext {
		t.Fatalf("rulesets = %+v", repo.Rulesets)
	}
	rs := repo.Rulesets[0]
	if rs.Enforcement != "active" || rs.Target != "branch" {
		t.Errorf("ruleset meta = %+v", rs)
	}
	var conds struct {
		RefName struct{ Include, Exclude []string } `json:"ref_name"`
	}
	if err := json.Unmarshal(rs.Conditions, &conds); err != nil {
		t.Fatalf("decode conditions: %v", err)
	}
	if got := conds.RefName.Include; len(got) != 1 || got[0] != "~DEFAULT_BRANCH" {
		t.Errorf("include = %v, want [~DEFAULT_BRANCH]", got)
	}
	if len(rs.Rules) != 1 || rs.Rules[0].Type != "required_status_checks" {
		t.Fatalf("rules = %+v", rs.Rules)
	}
	// The default (false) would also gate branch *creation* on the check,
	// breaking normal pushes for everyone except the bypass actor.
	var params struct {
		DoNotEnforceOnCreate bool `json:"do_not_enforce_on_create"`
	}
	if err := json.Unmarshal(rs.Rules[0].Parameters, &params); err != nil {
		t.Fatalf("decode rule params: %v", err)
	}
	if !params.DoNotEnforceOnCreate {
		t.Error("do_not_enforce_on_create=false: ruleset would block all branch creation")
	}

	// Second run is a no-op: must not create a duplicate ruleset.
	if err := f.EnsureRepoSetup(ctx, "org", "app", forge.SetupConfig{}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(repo.Rulesets) != 1 {
		t.Errorf("idempotency: got %d rulesets", len(repo.Rulesets))
	}
}
