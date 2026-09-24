package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/githubapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const (
	tOpen        = "OPEN"
	tClosed      = "CLOSED"
	tRepo        = "me/fork"
	tForkName    = "fork"
	tGraphQLPath = "/graphql"
	tNodes       = "nodes"
	tNumber      = "number"
	tState       = "state"
	tIssuesPath  = "/repos/me/fork/issues"
)

// fakeGraphQL answers the three query shapes forgesync sends: the repo's open
// PRs, a search for one author's open PRs, and aliased pullRequest lookups.
type fakeGraphQL struct {
	open         []int64
	prs          map[int64]map[string]any
	detailsCalls int
	search       string // the last search query string sent
}

func (f *fakeGraphQL) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string
		Variables struct{ Q string }
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Variables.Q != "" {
		f.search = req.Variables.Q
	}
	numbers := make([]map[string]any, 0, len(f.open))
	for _, n := range f.open {
		numbers = append(numbers, map[string]any{tNumber: n})
	}
	var data map[string]any
	switch {
	case strings.Contains(req.Query, "search("):
		data = map[string]any{"search": map[string]any{tNodes: numbers}}
	case strings.Contains(req.Query, "pullRequests(states"):
		data = map[string]any{"repository": map[string]any{"pullRequests": map[string]any{tNodes: numbers}}}
	default:
		f.detailsCalls++
		repo := map[string]any{}
		for n, pr := range f.prs {
			if strings.Contains(req.Query, fmt.Sprintf("pr%d:", n)) {
				repo[fmt.Sprintf("pr%d", n)] = pr
			}
		}
		data = map[string]any{"repository": repo}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func newProvider(t *testing.T, h http.HandlerFunc) *Provider {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c, err := githubapi.New("test-token", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return NewWithClient(c)
}

func prJSON(number int64, state string, rollupState string, checkAt time.Time) map[string]any {
	pr := map[string]any{
		tNumber: number, "title": "pr title", "body": "pr body",
		"url":       fmt.Sprintf("https://github.com/%s/pull/%d", tRepo, number),
		tState:      state,
		"createdAt": time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		"updatedAt": time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC),
		"author":    map[string]any{"login": "alice"},
	}
	if rollupState != "" {
		pr["commits"] = map[string]any{tNodes: []any{map[string]any{"commit": map[string]any{
			"statusCheckRollup": map[string]any{
				tState:     rollupState,
				"contexts": map[string]any{tNodes: []any{map[string]any{"completedAt": checkAt}}},
			},
		}}}}
	}
	return pr
}

func TestChecksLabel(t *testing.T) {
	cases := map[string]string{
		"SUCCESS":  source.LabelChecksPassing,
		"FAILURE":  source.LabelChecksFailing,
		"ERROR":    source.LabelChecksFailing,
		"PENDING":  source.LabelChecksPending,
		"EXPECTED": source.LabelChecksPending,
		"":         "",
	}
	for state, want := range cases {
		if got := checksLabel(state); got != want {
			t.Errorf("%q: got %q want %q", state, got, want)
		}
	}
}

func TestMapPRNode(t *testing.T) {
	repo := source.Repo{Owner: "me", Name: tForkName}
	notice := func(id int64) string {
		return marker.WithMarker("Promoted to the canonical Forgejo repository (PR #28).",
			marker.Marker{Type: "github", Host: "github.com", Repo: tRepo, Kind: promotedMarkerKind, ID: id})
	}
	checkAt := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)

	cases := []struct {
		name     string
		node     prNode
		want     []string
		wantOpen bool
	}{
		{"merged", prNode{Number: 3, State: "MERGED", Merged: true}, []string{source.LabelMerged}, false},
		{"promoted by /sync", prNode{Number: 3, State: tClosed, Comments: comments(notice(3))}, []string{source.LabelPromoted}, false},
		{"notice for another PR", prNode{Number: 3, State: tClosed, Comments: comments(notice(4))}, nil, false},
		{"closed, not merged", prNode{Number: 3, State: tClosed}, nil, false},
		{"open with passing checks", prNode{Number: 3, State: tOpen, Commits: commits("SUCCESS", checkAt)}, []string{source.LabelChecksPassing}, true},
		{"no checks", prNode{Number: 3, State: tOpen}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapPRNode(&tc.node, repo, prTitle)
			if !slices.Equal(got.Labels, tc.want) {
				t.Errorf("labels = %v, want %v", got.Labels, tc.want)
			}
			if (got.State == "open") != tc.wantOpen {
				t.Errorf("state = %q", got.State)
			}
			if got.Title != "[PR #3] " {
				t.Errorf("title = %q", got.Title)
			}
		})
	}
}

func TestMapPRNode_CheckResultBumpsUpdatedAt(t *testing.T) {
	updated := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	checkAt := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)
	n := prNode{Number: 3, State: tOpen, UpdatedAt: updated, Commits: commits("SUCCESS", checkAt)}
	got := mapPRNode(&n, source.Repo{Owner: "me", Name: tForkName}, prTitle)
	if !got.UpdatedAt.Equal(checkAt) {
		t.Errorf("UpdatedAt = %v, want the check time %v", got.UpdatedAt, checkAt)
	}
}

func TestListIssues_AddsStatusAndOpenPRs(t *testing.T) {
	// The window holds issue #1 and closed PR #2. PR #5 is open but hasn't
	// changed, so only the open-PR list finds it; its check finished later.
	checkAt := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)
	fg := &fakeGraphQL{
		open: []int64{5},
		prs: map[int64]map[string]any{
			2: prJSON(2, tClosed, "FAILURE", checkAt),
			5: prJSON(5, tOpen, "PENDING", checkAt),
		},
	}
	p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tGraphQLPath:
			fg.handle(w, r)
		case tIssuesPath:
			_ = json.NewEncoder(w).Encode([]*gh.Issue{
				{Number: gh.Ptr(1), Title: gh.Ptr("a bug"), State: gh.Ptr("open")},
				{
					Number: gh.Ptr(2), Title: gh.Ptr("pr title"), State: gh.Ptr("closed"),
					PullRequestLinks: &gh.PullRequestLinks{HTMLURL: gh.Ptr("https://github.com/me/fork/pull/2")},
				},
			})
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	})

	got, err := p.ListIssues(context.Background(), source.Repo{Owner: "me", Name: tForkName}, source.ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected issue 1, PR 2 and open PR 5, got %d items", len(got))
	}
	if got[0].Title != "a bug" || len(got[0].Labels) != 0 {
		t.Errorf("plain issue changed: %+v", got[0])
	}
	if !slices.Contains(got[1].Labels, source.LabelChecksFailing) {
		t.Errorf("PR 2 labels = %v, want checks: failing", got[1].Labels)
	}
	if got[2].Number != 5 || !slices.Contains(got[2].Labels, source.LabelChecksPending) {
		t.Errorf("open PR 5 = %+v, want it appended with checks: pending", got[2])
	}
	if !got[2].UpdatedAt.Equal(checkAt) {
		t.Errorf("PR 5 UpdatedAt = %v, want the check time", got[2].UpdatedAt)
	}
	if fg.detailsCalls != 1 {
		t.Errorf("expected both PRs in one details request, got %d", fg.detailsCalls)
	}
}

func TestListIssues_IdleOpenPRsNotAppended(t *testing.T) {
	// With a poll window, an open PR outside it only comes in when one of its
	// checks changed inside it; otherwise the drift guard would skip it anyway.
	since := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	fg := &fakeGraphQL{
		open: []int64{5, 6},
		prs: map[int64]map[string]any{
			5: prJSON(5, tOpen, "SUCCESS", since.Add(-time.Hour)),
			6: prJSON(6, tOpen, "FAILURE", since.Add(10*time.Minute)),
		},
	}
	p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tGraphQLPath:
			fg.handle(w, r)
		case tIssuesPath:
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	})
	got, err := p.ListIssues(context.Background(), source.Repo{Owner: "me", Name: tForkName}, source.ListOpts{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 6 {
		t.Errorf("expected only PR 6 (check changed in the window), got %+v", got)
	}
}

func TestPullRequests_Batches(t *testing.T) {
	fg := &fakeGraphQL{}
	p := newProvider(t, fg.handle)
	numbers := make([]int64, 0, 60)
	for n := range int64(60) {
		numbers = append(numbers, n+1)
	}
	if _, err := p.pullRequests(context.Background(), source.Repo{Owner: "me", Name: tForkName}, numbers); err != nil {
		t.Fatal(err)
	}
	if fg.detailsCalls != 2 {
		t.Errorf("60 PRs should take 2 requests of up to %d, got %d", prBatch, fg.detailsCalls)
	}
}

func TestMapPRNode_RerunStartBumpsUpdatedAt(t *testing.T) {
	// A re-run check has started but not finished: its start time is the
	// newest change, so the pending state gets past the drift guard.
	updated := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	started := time.Date(2026, 9, 1, 12, 45, 0, 0, time.UTC)
	n := prNode{Number: 3, State: tOpen, UpdatedAt: updated}
	n.Commits = commits("PENDING", updated)
	n.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes[0].StartedAt = &started
	got := mapPRNode(&n, source.Repo{Owner: "me", Name: tForkName}, prTitle)
	if !got.UpdatedAt.Equal(started) {
		t.Errorf("UpdatedAt = %v, want the re-run start %v", got.UpdatedAt, started)
	}
}

func TestListIssues_NoPRsSkipsDetails(t *testing.T) {
	fg := &fakeGraphQL{}
	p := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tGraphQLPath:
			fg.handle(w, r)
		case tIssuesPath:
			_ = json.NewEncoder(w).Encode([]*gh.Issue{{Number: gh.Ptr(1), Title: gh.Ptr("a bug")}})
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := p.ListIssues(context.Background(), source.Repo{Owner: "me", Name: tForkName}, source.ListOpts{}); err != nil {
		t.Fatal(err)
	}
	if fg.detailsCalls != 0 {
		t.Errorf("no PRs means no details request, got %d", fg.detailsCalls)
	}
}

func TestGraphQLErrorIsReturned(t *testing.T) {
	p := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"rate limited"}]}`))
	})
	_, err := p.openPRNumbers(context.Background(), source.Repo{Owner: "me", Name: tForkName}, "")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("expected the GraphQL error, got %v", err)
	}
}

func comments(bodies ...string) (c struct{ Nodes []struct{ Body string } }) {
	for _, b := range bodies {
		c.Nodes = append(c.Nodes, struct{ Body string }{b})
	}
	return c
}

func commits(state string, at time.Time) (c struct {
	Nodes []struct {
		Commit struct{ StatusCheckRollup *rollup }
	}
}) {
	r := &rollup{State: state}
	r.Contexts.Nodes = append(r.Contexts.Nodes, struct {
		StartedAt   *time.Time
		CompletedAt *time.Time
		CreatedAt   *time.Time
	}{CompletedAt: &at})
	c.Nodes = append(c.Nodes, struct {
		Commit struct{ StatusCheckRollup *rollup }
	}{})
	c.Nodes[0].Commit.StatusCheckRollup = r
	return c
}
