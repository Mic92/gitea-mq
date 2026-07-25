package gitea

import (
	"fmt"
	"strings"
)

// supportsStatusWebhook reports whether a Gitea server at the given version
// delivers commit-status webhook events (added in Gitea 1.24). Forgejo
// versions carry a "+gitea-x.y.z" compatibility suffix; that compat version
// is what counts.
func supportsStatusWebhook(version string) bool {
	base, suffix, _ := strings.Cut(version, "+")
	if compat, ok := strings.CutPrefix(suffix, "gitea-"); ok {
		base = compat
	}
	var major, minor int
	if _, err := fmt.Sscanf(base, "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major > 1 || (major == 1 && minor >= 24)
}
