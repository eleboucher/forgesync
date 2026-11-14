// Package github is a SourceProvider that reads issues, PRs and comments
// from github.com.
package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

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

// ListIssues returns both issues and PRs (PRs get a "[PR #N]" title prefix, a
// link to the actual PR and status labels). Real PR sync — pushing branches and
// creating real Forgejo PRs — is a separate path that lands with gitops
// support; this exposes PR conversations as issues in the meantime.
func (p *Provider) ListIssues(ctx context.Context, repo source.Repo, opts source.ListOpts) ([]source.Issue, error) {
	out := []source.Issue{}
	var prs []int64
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
			if i.PullRequestLinks != nil {
				prs = append(prs, int64(i.GetNumber()))
			}
		}
		if len(batch) < pageSize {
			break
		}
	}
	return p.withPRStatus(ctx, repo, "", prTitle, out, prs, opts.Since)
}

func (p *Provider) ListPullRequests(_ context.Context, _ source.Repo, _ source.ListOpts) ([]source.PullRequest, error) {
	return nil, nil
}

// Parent returns the repo that repo was forked from, and false if it isn't a
// fork.
func (p *Provider) Parent(ctx context.Context, repo source.Repo) (source.Repo, bool, error) {
	r, _, err := p.client.Repositories.Get(ctx, repo.Owner, repo.Name)
	if err != nil {
		return source.Repo{}, false, fmt.Errorf("get repo: %w", err)
	}
	parent := r.GetParent()
	if parent == nil {
		return source.Repo{}, false, nil
	}
	return source.Repo{Owner: parent.GetOwner().GetLogin(), Name: parent.GetName()}, true, nil
}

// UpstreamPullRequests returns a read-only view of the pull requests author
// opened on another repo, typically the parent of their fork. Its ListIssues
// yields only those PRs, titled "[upstream PR #N]"; ListComments is their
// conversation.
func (p *Provider) UpstreamPullRequests(author string) source.Provider {
	return &upstreamPRs{Provider: p, author: author}
}

type upstreamPRs struct {
	*Provider
	author string
}

func (u *upstreamPRs) ListIssues(ctx context.Context, repo source.Repo, opts source.ListOpts) ([]source.Issue, error) {
	out := []source.Issue{}
	var prs []int64
	listOpts := &gh.IssueListByRepoOptions{
		State:       "all",
		Creator:     u.author,
		ListOptions: gh.ListOptions{PerPage: pageSize},
	}
	if !opts.Since.IsZero() {
		listOpts.Since = opts.Since
	}
	for page := 1; ; page++ {
		listOpts.ListOptions.Page = page //nolint:staticcheck // ListCursorOptions also has Page; explicit selector avoids ambiguity
		batch, _, err := u.client.Issues.ListByRepo(ctx, repo.Owner, repo.Name, listOpts)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, i := range batch {
			if i.PullRequestLinks == nil {
				continue
			}
			out = append(out, mapUpstreamPR(i))
			prs = append(prs, int64(i.GetNumber()))
		}
		if len(batch) < pageSize {
			break
		}
	}
	return u.withPRStatus(ctx, repo, u.author, upstreamPRTitle, out, prs, opts.Since)
}

// mapUpstreamPR maps like mapIssue but with its own title prefix, so the
// shadow is never mistaken for a "[PR #N]" shadow that /sync can promote.
func mapUpstreamPR(i *gh.Issue) source.Issue {
	iss := mapIssue(i)
	iss.Title = upstreamPRTitle(int64(i.GetNumber()), i.GetTitle())
	return iss
}

func prTitle(number int64, title string) string {
	return fmt.Sprintf("[PR #%d] %s", number, title)
}

func upstreamPRTitle(number int64, title string) string {
	return fmt.Sprintf("[upstream PR #%d] %s", number, title)
}

// GetPullRequest fetches a single PR and maps it to source.PullRequest,
// including the head ref/SHA and merge state needed to promote it into Forgejo.
func (p *Provider) GetPullRequest(ctx context.Context, repo source.Repo, number int64) (source.PullRequest, error) {
	pr, _, err := p.client.PullRequests.Get(ctx, repo.Owner, repo.Name, int(number))
	if err != nil {
		return source.PullRequest{}, fmt.Errorf("fetch source PR: %w", err)
	}
	user := pr.GetUser()
	return source.PullRequest{
		Issue: source.Issue{
			Number: int64(pr.GetNumber()),
			Title:  pr.GetTitle(),
			Body:   pr.GetBody(),
			State:  pr.GetState(),
			Author: source.User{
				Login:     user.GetLogin(),
				AvatarURL: user.GetAvatarURL(),
				HTMLURL:   user.GetHTMLURL(),
			},
			HTMLURL:   pr.GetHTMLURL(),
			CreatedAt: pr.GetCreatedAt().Time,
			UpdatedAt: pr.GetUpdatedAt().Time,
			ClosedAt:  closedAtPtr(pr.ClosedAt),
		},
		BaseBranch: pr.GetBase().GetRef(),
		HeadBranch: pr.GetHead().GetRef(),
		HeadSHA:    pr.GetHead().GetSHA(),
		Merged:     pr.GetMerged(),
		MergedAt:   closedAtPtr(pr.MergedAt),
	}, nil
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
		title = prTitle(int64(i.GetNumber()), title)
		if u := i.PullRequestLinks.GetHTMLURL(); u != "" {
			htmlURL = u
		}
		if i.PullRequestLinks.MergedAt != nil {
			labels = append(labels, source.LabelMerged)
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
