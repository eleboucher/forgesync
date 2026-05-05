package forgejo

import (
	"context"
	"strings"

	"code.gitea.io/sdk/gitea"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/forgejoapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const pageSize = 50

type Provider struct {
	client *forgejoapi.Client
	host   string
}

func NewWithClient(c *forgejoapi.Client, host string) *Provider {
	return &Provider{client: c, host: host}
}

func (p *Provider) Kind() string { return "forgejo" }
func (p *Provider) Host() string { return p.host }

func (p *Provider) ListIssues(ctx context.Context, repo source.Repo, opts source.ListOpts) ([]source.Issue, error) {
	_ = ctx
	out := []source.Issue{}
	for page := 1; ; page++ {
		batch, _, err := p.client.ListRepoIssues(repo.Owner, repo.Name, gitea.ListIssueOption{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
			State:       gitea.StateAll,
			Type:        gitea.IssueTypeIssue,
			Since:       opts.Since,
		})
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
	_ = ctx
	out := []source.Comment{}
	for page := 1; ; page++ {
		batch, _, err := p.client.ListIssueComments(repo.Owner, repo.Name, issueNumber, gitea.ListIssueCommentOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
			Since:       opts.Since,
		})
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

func mapIssue(i *gitea.Issue) source.Issue {
	labels := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, l.Name)
	}
	var login string
	if i.Poster != nil {
		login = i.Poster.UserName
	}
	return source.Issue{
		Number:    i.Index,
		Title:     i.Title,
		Body:      strings.TrimSpace(i.Body),
		State:     string(i.State),
		Labels:    labels,
		Author:    source.User{Login: login},
		HTMLURL:   i.HTMLURL,
		CreatedAt: i.Created,
		UpdatedAt: i.Updated,
		ClosedAt:  i.Closed,
	}
}

func mapComment(c *gitea.Comment, issueNumber int64) source.Comment {
	var login string
	if c.Poster != nil {
		login = c.Poster.UserName
	}
	return source.Comment{
		ID:          c.ID,
		IssueNumber: issueNumber,
		Body:        strings.TrimSpace(c.Body),
		Author:      source.User{Login: login},
		HTMLURL:     c.HTMLURL,
		CreatedAt:   c.Created,
		UpdatedAt:   c.Updated,
	}
}
