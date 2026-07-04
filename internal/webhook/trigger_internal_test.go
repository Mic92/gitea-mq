package webhook

import (
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// Only a green check can newly enqueue an auto-merge PR, so pending/failure
// events must not spend a poll (and its API calls).
func TestMaybeTriggerPoll_OnlyOnSuccess(t *testing.T) {
	for _, tc := range []struct {
		state pg.CheckState
		want  int
	}{
		{pg.CheckStateSuccess, 1},
		{pg.CheckStatePending, 0},
		{forge.ParseCheckState("failure"), 0},
	} {
		var polled int
		rm := &RepoMonitor{TriggerPoll: func() { polled++ }}
		maybeTriggerPoll(rm, tc.state)
		if polled != tc.want {
			t.Errorf("state %q: polled=%d want %d", tc.state, polled, tc.want)
		}
	}
}
