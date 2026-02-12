package web

import (
	"testing"
	"time"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 2, 12, 14, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		// Past times
		{"just now", now.Add(-2 * time.Second), "just now"},
		{"seconds", now.Add(-30 * time.Second), "30 seconds ago"},
		{"5 seconds", now.Add(-5 * time.Second), "5 seconds ago"},
		{"1 minute", now.Add(-1 * time.Minute), "1 minute ago"},
		{"minutes", now.Add(-7 * time.Minute), "7 minutes ago"},
		{"1 hour", now.Add(-1 * time.Hour), "1 hour ago"},
		{"hours", now.Add(-5 * time.Hour), "5 hours ago"},
		{"1 day", now.Add(-24 * time.Hour), "1 day ago"},
		{"days", now.Add(-3 * 24 * time.Hour), "3 days ago"},
		{"1 month", now.Add(-35 * 24 * time.Hour), "1 month ago"},
		{"months", now.Add(-90 * 24 * time.Hour), "3 months ago"},
		{"1 year", now.Add(-400 * 24 * time.Hour), "1 year ago"},
		{"years", now.Add(-800 * 24 * time.Hour), "2 years ago"},
		{"zero time", time.Time{}, pluralize(int(now.Sub(time.Time{}).Hours()/(24*365)), "year") + " ago"},

		// Future times
		{"future just now", now.Add(2 * time.Second), "just now"},
		{"future seconds", now.Add(30 * time.Second), "in 30 seconds"},
		{"future 1 minute", now.Add(1 * time.Minute), "in 1 minute"},
		{"future minutes", now.Add(7 * time.Minute), "in 7 minutes"},
		{"future 1 hour", now.Add(1 * time.Hour), "in 1 hour"},
		{"future hours", now.Add(5 * time.Hour), "in 5 hours"},
		{"future 1 day", now.Add(24 * time.Hour), "in 1 day"},
		{"future days", now.Add(3 * 24 * time.Hour), "in 3 days"},
		{"future 1 month", now.Add(35 * 24 * time.Hour), "in 1 month"},
		{"future months", now.Add(90 * 24 * time.Hour), "in 3 months"},
		{"future 1 year", now.Add(400 * 24 * time.Hour), "in 1 year"},
		{"future years", now.Add(800 * 24 * time.Hour), "in 2 years"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RelativeTime(tt.t, now)
			if got != tt.want {
				t.Errorf("RelativeTime(%v, %v) = %q, want %q", tt.t, now, got, tt.want)
			}
		})
	}
}
