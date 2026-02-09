package poller_test

import (
	"testing"
	"time"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/poller"
)

func tc(typ string, id int64) gitea.TimelineComment {
	return gitea.TimelineComment{
		ID:        id,
		Type:      typ,
		CreatedAt: time.Now(),
	}
}

// The latest automerge-related comment determines the state.
func TestHasAutomergeScheduled(t *testing.T) {
	tests := []struct {
		name     string
		timeline []gitea.TimelineComment
		want     bool
	}{
		{
			name: "latest is pull_scheduled_merge",
			timeline: []gitea.TimelineComment{
				tc("comment", 1),
				tc("pull_scheduled_merge", 2),
			},
			want: true,
		},
		{
			name: "cancelled after scheduling",
			timeline: []gitea.TimelineComment{
				tc("pull_scheduled_merge", 1),
				tc("comment", 2),
				tc("pull_cancel_scheduled_merge", 3),
			},
			want: false,
		},
		{
			name:     "empty timeline",
			timeline: nil,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := poller.HasAutomergeScheduled(tt.timeline)
			if got != tt.want {
				t.Fatalf("HasAutomergeScheduled() = %v, want %v", got, tt.want)
			}
		})
	}
}
