package forge

import "testing"

func TestParseRepoRefRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want RepoRef
		ok   bool
	}{
		{"gitea:o/n", RepoRef{KindGitea, "o", "n"}, true},
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
			if !ok {
				return
			}
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
			if got.String() != tc.in {
				t.Errorf("String()=%q want %q", got.String(), tc.in)
			}
		})
	}
}
