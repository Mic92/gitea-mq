package gitea

import "testing"

func TestSupportsStatusWebhook(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"1.24.0", true},
		{"1.25.1", true},
		{"2.0.0", true},
		{"1.24.0+dev-123-gabcdef", true},
		{"1.23.5", false},
		{"1.9.4", false},
		// Forgejo reports its own version with a Gitea compat suffix; the
		// compat version decides.
		{"11.0.1+gitea-1.22.0", false},
		{"12.0.0+gitea-1.24.0", true},
		{"", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		if got := supportsStatusWebhook(tc.version); got != tc.want {
			t.Errorf("supportsStatusWebhook(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}
