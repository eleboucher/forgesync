package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v75/github"
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
