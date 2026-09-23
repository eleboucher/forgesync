package sink

import (
	"strings"
	"testing"
	"time"
)

const (
	tBug    = "bug"
	tBugCap = "Bug"
	tUI     = "ui"
	tTriage = "triage"
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

func TestLabelsEqual(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
		name string
	}{
		{nil, nil, true, "both empty"},
		{nil, []string{}, true, "nil and empty"},
		{[]string{tBug, tUI}, []string{tUI, tBug}, true, "order ignored"},
		{[]string{tBugCap}, []string{tBug}, true, "case ignored"},
		{[]string{tBug}, []string{tBug, tUI}, false, "extra label"},
		{[]string{tBug, tBug}, []string{tBug, tUI}, false, "different sets"},
		{[]string{tBug, tBugCap}, []string{tBug}, true, "duplicates ignored"},
		{[]string{tBug}, nil, false, "cleared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LabelsEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSyncedLabelsRoundTrip(t *testing.T) {
	labels := []string{tBug, "needs review", "a,b", "--> sneaky", "prio/high", tBugCap}
	body := WithSyncedLabels("text\n\n<!-- forgesync:src=x -->", labels)
	got := SyncedLabels(body)
	want := []string{tBug, "needs review", "a,b", "--> sneaky", "prio/high"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q want %q", got, want)
	}
	if WithSyncedLabels("text", nil) != "text" {
		t.Error("no labels must leave the body unchanged")
	}
	if SyncedLabels("no note here") != nil {
		t.Error("a body without a note has no synced labels")
	}
}

func TestShadowLabels(t *testing.T) {
	cases := []struct {
		current, prev, src []string
		want               string
		name               string
	}{
		{nil, nil, []string{tBug}, tBug, "new label added"},
		{[]string{tBug}, []string{tBug}, nil, "", "dropped at source is removed"},
		{[]string{tTriage}, []string{tBug}, nil, tTriage, "destination label kept"},
		{[]string{tTriage}, nil, []string{tBug}, "triage,bug", "no record keeps everything"},
		{[]string{tBugCap}, []string{tBug}, []string{tBug}, tBugCap, "still wanted, spelling kept"},
		{nil, nil, []string{tBug, "BUG"}, tBug, "source duplicates collapsed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(ShadowLabels(tc.current, tc.prev, tc.src), ","); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestPropagateState(t *testing.T) {
	const open, closed = "open", "closed"
	cases := []struct {
		existing, src string
		wantNil       bool
		wantState     string
		name          string
	}{
		{closed, open, false, open, "reopen propagates"},
		{open, closed, false, closed, "close propagates"},
		{open, open, true, "", "equal open"},
		{closed, closed, true, "", "equal closed"},
		{open, "", true, "", "unknown source state ignored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PropagateState(tc.existing, tc.src)
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
