package forge

import (
	"errors"
	"testing"
)

func TestSetRegisterAndFor(t *testing.T) {
	s := NewSet()
	gf := &MockForge{KindVal: KindGitea}
	hf := &MockForge{KindVal: KindGithub}
	s.Register(gf)
	s.Register(hf)

	got, err := s.For(RepoRef{Forge: KindGitea, Owner: "o", Name: "n"})
	if err != nil {
		t.Fatalf("For(gitea): %v", err)
	}
	if got != gf {
		t.Errorf("got %v want %v", got, gf)
	}

	got, err = s.For(RepoRef{Forge: KindGithub, Owner: "o", Name: "n"})
	if err != nil {
		t.Fatalf("For(github): %v", err)
	}
	if got != hf {
		t.Errorf("got %v want %v", got, hf)
	}
}

func TestSetForUnknown(t *testing.T) {
	s := NewSet()
	_, err := s.For(RepoRef{Forge: KindGithub, Owner: "o", Name: "n"})
	var uf *ErrUnknownForge
	if !errors.As(err, &uf) {
		t.Fatalf("want ErrUnknownForge, got %v", err)
	}
	if uf.Kind != KindGithub {
		t.Errorf("kind=%v want github", uf.Kind)
	}
}

func TestSetZeroValue(t *testing.T) {
	var s Set
	s.Register(&MockForge{KindVal: KindGitea})
	if _, err := s.For(RepoRef{Forge: KindGitea, Owner: "o", Name: "n"}); err != nil {
		t.Errorf("zero Set should accept Register: %v", err)
	}
}
