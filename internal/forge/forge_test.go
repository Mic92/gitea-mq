package forge

import "testing"

func TestRepoRefString(t *testing.T) {
	cases := []struct {
		name string
		ref  RepoRef
		want string
	}{
		{"gitea", RepoRef{Forge: KindGitea, Owner: "o", Name: "n"}, "gitea:o/n"},
		{"github", RepoRef{Forge: KindGithub, Owner: "o", Name: "n"}, "github:o/n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.String(); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestParseRepoRef(t *testing.T) {
	cases := []struct {
		in   string
		want RepoRef
		ok   bool
	}{
		{"gitea:o/n", RepoRef{KindGitea, "o", "n"}, true},
		{"github:o/n", RepoRef{KindGithub, "o", "n"}, true},
		{"github:acme/hello-world", RepoRef{KindGithub, "acme", "hello-world"}, true},
		{"o/n", RepoRef{}, false},
		{"unknown:o/n", RepoRef{}, false},
		{"gitea:o", RepoRef{}, false},
		{"gitea:/n", RepoRef{}, false},
		{"gitea:o/", RepoRef{}, false},
		{"", RepoRef{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseRepoRef(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestKindValid(t *testing.T) {
	if !KindGitea.Valid() || !KindGithub.Valid() {
		t.Fatal("known kinds must be valid")
	}
	if Kind("bogus").Valid() {
		t.Fatal("unknown kind must be invalid")
	}
}
