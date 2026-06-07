package sink

import (
	"testing"
	"time"
)

func TestShadowDrifted(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		shadow, src time.Time
		want        bool
		name        string
	}{
		{now, now, false, "equal"},
		{now.Add(-time.Hour), now, false, "shadow older"},
		{now.Add(time.Second), now, false, "within threshold"},
		{now.Add(2 * time.Minute), now, true, "drifted"},
		{time.Time{}, now, false, "shadow zero"},
		{now, time.Time{}, false, "src zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShadowDrifted(tc.shadow, tc.src); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestPropagateState(t *testing.T) {
	const open, closed = "open", "closed"
	cases := []struct {
		existing, src string
		allowClose    bool
		wantNil       bool
		wantState     string
		name          string
	}{
		{closed, open, false, false, open, "reopen propagates without allowClose"},
		{closed, open, true, false, open, "reopen propagates with allowClose"},
		{open, closed, false, true, "", "close suppressed without allowClose"},
		{open, closed, true, false, closed, "close propagates with allowClose"},
		{open, open, true, true, "", "equal open"},
		{closed, closed, true, true, "", "equal closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PropagateState(tc.existing, tc.src, tc.allowClose)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %q", *got)
				}
				return
			}
			if got == nil || *got != tc.wantState {
				t.Errorf("got %v, want %q", got, tc.wantState)
			}
		})
	}
}
