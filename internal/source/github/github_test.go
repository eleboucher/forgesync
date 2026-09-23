package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/githubapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

func TestMapIssue_PRGetsTitlePrefixAndPRURL(t *testing.T) {
	now := time.Now()
	i := &gh.Issue{
		Number:    gh.Ptr(419),
		Title:     gh.Ptr("Bump foo to v1.2.3"),
		Body:      gh.Ptr("x"),
		State:     gh.Ptr("open"),
		HTMLURL:   gh.Ptr("https://github.com/me/repo/issues/419"),
		CreatedAt: &gh.Timestamp{Time: now},
		UpdatedAt: &gh.Timestamp{Time: now},
		PullRequestLinks: &gh.PullRequestLinks{
			HTMLURL: gh.Ptr("https://github.com/me/repo/pull/419"),
		},
	}
	got := mapIssue(i)
	if got.Title != "[PR #419] Bump foo to v1.2.3" {
		t.Errorf("title: got %q", got.Title)
	}
	if got.HTMLURL != "https://github.com/me/repo/pull/419" {
		t.Errorf("HTMLURL: got %q", got.HTMLURL)
	}
}

func TestParentAndUpstreamPullRequests(t *testing.T) {
	var creator string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/me/fork":
			_ = json.NewEncoder(w).Encode(&gh.Repository{
				Name:   gh.Ptr("fork"),
				Parent: &gh.Repository{Name: gh.Ptr("proj"), Owner: &gh.User{Login: gh.Ptr("up")}},
			})
		case "/repos/me/solo":
			_ = json.NewEncoder(w).Encode(&gh.Repository{Name: gh.Ptr("solo")})
		case "/repos/up/proj/issues":
			creator = r.URL.Query().Get("creator")
			_ = json.NewEncoder(w).Encode([]*gh.Issue{
				{Number: gh.Ptr(3), Title: gh.Ptr("an issue I filed")},
				{
					Number: gh.Ptr(4), Title: gh.Ptr("add postgres"), State: gh.Ptr("open"),
					PullRequestLinks: &gh.PullRequestLinks{HTMLURL: gh.Ptr("https://github.com/up/proj/pull/4")},
				},
			})
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(ts.Close)
	c, err := githubapi.New("test-token", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := NewWithClient(c)
	ctx := context.Background()

	if _, ok, err := p.Parent(ctx, source.Repo{Owner: "me", Name: "solo"}); err != nil || ok {
		t.Errorf("non-fork: ok=%v err=%v, want no parent", ok, err)
	}
	parent, ok, err := p.Parent(ctx, source.Repo{Owner: "me", Name: "fork"})
	if err != nil || !ok || parent.Slug() != "up/proj" {
		t.Fatalf("parent = %v ok=%v err=%v, want up/proj", parent, ok, err)
	}

	prs, err := p.UpstreamPullRequests("me").ListIssues(ctx, parent, source.ListOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if creator != "me" {
		t.Errorf("creator filter = %q, want me", creator)
	}
	if len(prs) != 1 {
		t.Fatalf("expected only the PR, got %d items", len(prs))
	}
	if prs[0].Title != "[upstream PR #4] add postgres" {
		t.Errorf("title: got %q", prs[0].Title)
	}
	if prs[0].HTMLURL != "https://github.com/up/proj/pull/4" {
		t.Errorf("HTMLURL: got %q", prs[0].HTMLURL)
	}
}

func TestMapIssue_PlainIssueUnchanged(t *testing.T) {
	i := &gh.Issue{
		Number:  gh.Ptr(100),
		Title:   gh.Ptr("Something is broken"),
		HTMLURL: gh.Ptr("https://github.com/me/repo/issues/100"),
	}
	got := mapIssue(i)
	if got.Title != "Something is broken" {
		t.Errorf("title should not be prefixed for plain issues, got %q", got.Title)
	}
	if got.HTMLURL != "https://github.com/me/repo/issues/100" {
		t.Errorf("HTMLURL: got %q", got.HTMLURL)
	}
}
