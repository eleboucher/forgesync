// Package github is a SourceProvider that reads issues, PRs and comments
// from github.com.
package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v75/github"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const pageSize = 100

type Provider struct {
	client *gh.Client
}

func NewWithClient(c *gh.Client) *Provider {
	return &Provider{client: c}
}

func (p *Provider) Kind() string { return "github" }
func (p *Provider) Host() string { return "github.com" }

// ListIssues returns both issues and PRs (PRs get a "[PR #N]" title prefix and
// a link to the actual PR). Real PR sync — pushing branches and creating real
// Forgejo PRs — is a separate path that lands with gitops support; this exposes
// PR conversations as issues in the meantime.
func (p *Provider) ListIssues(ctx context.Context, repo source.Repo, opts source.ListOpts) ([]source.Issue, error) {
	out := []source.Issue{}
	listOpts := &gh.IssueListByRepoOptions{
		State:       "all",
		ListOptions: gh.ListOptions{PerPage: pageSize},
	}
	if !opts.Since.IsZero() {
		listOpts.Since = opts.Since
	}
	for page := 1; ; page++ {
		listOpts.ListOptions.Page = page //nolint:staticcheck // ListCursorOptions also has Page; explicit selector avoids ambiguity
		batch, _, err := p.client.Issues.ListByRepo(ctx, repo.Owner, repo.Name, listOpts)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, i := range batch {
			out = append(out, mapIssue(i))
		}
		if len(batch) < pageSize {
			break
		}
	}
	return out, nil
}

func (p *Provider) ListPullRequests(_ context.Context, _ source.Repo, _ source.ListOpts) ([]source.PullRequest, error) {
	return nil, nil
}

func (p *Provider) ListComments(ctx context.Context, repo source.Repo, issueNumber int64, opts source.ListOpts) ([]source.Comment, error) {
	out := []source.Comment{}
	listOpts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: pageSize},
	}
	if !opts.Since.IsZero() {
		since := opts.Since
		listOpts.Since = &since
	}
	for page := 1; ; page++ {
		listOpts.Page = page
		batch, _, err := p.client.Issues.ListComments(ctx, repo.Owner, repo.Name, int(issueNumber), listOpts)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			out = append(out, mapComment(c, issueNumber))
		}
		if len(batch) < pageSize {
			break
		}
	}
	return out, nil
}

func mapIssue(i *gh.Issue) source.Issue {
	labels := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, l.GetName())
	}
	title := i.GetTitle()
	htmlURL := i.GetHTMLURL()
	if i.PullRequestLinks != nil {
		title = fmt.Sprintf("[PR #%d] %s", i.GetNumber(), title)
		if u := i.PullRequestLinks.GetHTMLURL(); u != "" {
			htmlURL = u
		}
	}
	user := i.GetUser()
	return source.Issue{
		Number: int64(i.GetNumber()),
		Title:  title,
		Body:   strings.TrimSpace(i.GetBody()),
		State:  i.GetState(),
		Labels: labels,
		Author: source.User{
			Login:     user.GetLogin(),
			AvatarURL: user.GetAvatarURL(),
			HTMLURL:   user.GetHTMLURL(),
		},
		HTMLURL:   htmlURL,
		CreatedAt: i.GetCreatedAt().Time,
		UpdatedAt: i.GetUpdatedAt().Time,
		ClosedAt:  closedAtPtr(i.ClosedAt),
	}
}

func mapComment(c *gh.IssueComment, issueNumber int64) source.Comment {
	user := c.GetUser()
	return source.Comment{
		ID:          c.GetID(),
		IssueNumber: issueNumber,
		Body:        strings.TrimSpace(c.GetBody()),
		Author: source.User{
			Login:     user.GetLogin(),
			AvatarURL: user.GetAvatarURL(),
			HTMLURL:   user.GetHTMLURL(),
		},
		HTMLURL:   c.GetHTMLURL(),
		CreatedAt: c.GetCreatedAt().Time,
		UpdatedAt: c.GetUpdatedAt().Time,
	}
}

func closedAtPtr(t *gh.Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	tm := t.Time
	return &tm
}
