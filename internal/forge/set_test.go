package forge

import (
	"errors"
	"testing"
)

func TestSetRoutesByKind(t *testing.T) {
	s := NewSet()
	gf := &MockForge{KindVal: KindGitea}
	hf := &MockForge{KindVal: KindGithub}
	s.Register(gf)
	s.Register(hf)

	if got, _ := s.For(RepoRef{Forge: KindGitea, Owner: "o", Name: "n"}); got != gf {
		t.Errorf("gitea: got %v want %v", got, gf)
	}
	if got, _ := s.For(RepoRef{Forge: KindGithub, Owner: "o", Name: "n"}); got != hf {
		t.Errorf("github: got %v want %v", got, hf)
	}
}

func TestSetForUnknownKind(t *testing.T) {
	s := NewSet()
	_, err := s.For(RepoRef{Forge: KindGithub, Owner: "o", Name: "n"})
	var uf *UnknownForgeError
	if !errors.As(err, &uf) || uf.Kind != KindGithub {
		t.Fatalf("want UnknownForgeError{github}, got %v", err)
	}
}
