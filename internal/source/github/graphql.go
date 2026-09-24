package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

// PR status comes from GitHub's GraphQL API: the REST issue list has no check
// results, and a check finishing doesn't bump a PR's updated time, so the poll
// window alone would never see it.

const (
	// prBatch caps the aliased pullRequest fields per GraphQL request.
	prBatch = 50
	// promotedMarkerKind is the marker kind on the "Promoted to" notice
	// forgesync posts on a GitHub PR it moved into the canonical Forgejo.
	promotedMarkerKind = "pull_request"
)

const prFields = `number title body url state merged createdAt updatedAt closedAt
author { login avatarUrl url }
labels(first: 50) { nodes { name } }
comments(last: 100) { nodes { body } }
commits(last: 1) { nodes { commit { statusCheckRollup { state
  contexts(first: 100) { nodes { ... on CheckRun { startedAt completedAt } ... on StatusContext { createdAt } } } } } } }`

type prNode struct {
	Number    int64
	Title     string
	Body      string
	URL       string
	State     string // OPEN, CLOSED or MERGED
	Merged    bool
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  *time.Time
	Author    *struct {
		Login     string
		AvatarURL string
		URL       string
	}
	Labels struct {
		Nodes []struct{ Name string }
	}
	Comments struct {
		Nodes []struct{ Body string }
	}
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *rollup
			}
		}
	}
}

type rollup struct {
	State    string // SUCCESS, FAILURE, ERROR, PENDING or EXPECTED
	Contexts struct {
		Nodes []struct {
			StartedAt   *time.Time // check runs; set on re-runs before they finish
			CompletedAt *time.Time // check runs
			CreatedAt   *time.Time // commit statuses
		}
	}
}

// withPRStatus replaces the PRs in issues with their GraphQL view, which
// carries the status labels, and appends the open PRs the poll window missed
// whose checks changed since since. prs are the PR numbers already in issues;
// author, when set, limits the open PRs to that author's; title renders a
// shadow title.
//
// Only a check change inside the window brings an otherwise idle PR in: that
// keeps its updated time ahead of the shadow's last write, so the sinks'
// drift guard lets it through, and spares every other open PR a round trip.
func (p *Provider) withPRStatus(ctx context.Context, repo source.Repo, author string, title func(int64, string) string, issues []source.Issue, prs []int64, since time.Time) ([]source.Issue, error) {
	open, err := p.openPRNumbers(ctx, repo, author)
	if err != nil {
		return nil, err
	}
	want := slices.Compact(slices.Sorted(slices.Values(slices.Concat(prs, open))))
	if len(want) == 0 {
		return issues, nil
	}
	nodes, err := p.pullRequests(ctx, repo, want)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	for i, iss := range issues {
		if n, ok := nodes[iss.Number]; ok {
			issues[i] = mapPRNode(n, repo, title)
			seen[iss.Number] = true
		}
	}
	for _, num := range open {
		n, ok := nodes[num]
		if !ok || seen[num] {
			continue
		}
		iss := mapPRNode(n, repo, title)
		if !since.IsZero() && iss.UpdatedAt.Before(since) {
			continue
		}
		issues = append(issues, iss)
		seen[num] = true
	}
	return issues, nil
}

// openPRNumbers lists the repo's open PRs, or only author's when set. GitHub
// caps both lists at 100.
func (p *Provider) openPRNumbers(ctx context.Context, repo source.Repo, author string) ([]int64, error) {
	type numbers struct {
		Nodes []struct{ Number int64 }
	}
	var list numbers
	if author == "" {
		var data struct {
			Repository struct{ PullRequests numbers }
		}
		q := `query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {
  pullRequests(states: OPEN, first: 100) { nodes { number } } } }`
		if err := p.graphql(ctx, q, map[string]any{"owner": repo.Owner, "name": repo.Name}, &data); err != nil {
			return nil, err
		}
		list = data.Repository.PullRequests
	} else {
		var data struct{ Search numbers }
		q := `query($q: String!) { search(query: $q, type: ISSUE, first: 100) { nodes { ... on PullRequest { number } } } }`
		search := fmt.Sprintf("repo:%s is:pr is:open author:%s", repo.Slug(), author)
		if err := p.graphql(ctx, q, map[string]any{"q": search}, &data); err != nil {
			return nil, err
		}
		list = data.Search
	}
	out := make([]int64, 0, len(list.Nodes))
	for _, n := range list.Nodes {
		out = append(out, n.Number)
	}
	return out, nil
}

// pullRequests fetches the given PRs, prBatch at a time, keyed by number.
func (p *Provider) pullRequests(ctx context.Context, repo source.Repo, numbers []int64) (map[int64]*prNode, error) {
	out := make(map[int64]*prNode, len(numbers))
	for chunk := range slices.Chunk(numbers, prBatch) {
		var b strings.Builder
		b.WriteString("query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {")
		for _, n := range chunk {
			fmt.Fprintf(&b, " pr%d: pullRequest(number: %d) { ...pr }", n, n)
		}
		b.WriteString(" } } fragment pr on PullRequest { " + prFields + " }")

		var data struct{ Repository map[string]*prNode }
		if err := p.graphql(ctx, b.String(), map[string]any{"owner": repo.Owner, "name": repo.Name}, &data); err != nil {
			return nil, err
		}
		for _, n := range data.Repository {
			if n != nil {
				out[n.Number] = n
			}
		}
	}
	return out, nil
}

func (p *Provider) graphql(ctx context.Context, query string, vars map[string]any, data any) error {
	req, err := p.client.NewRequest(ctx, http.MethodPost, "graphql", map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("graphql request: %w", err)
	}
	var resp struct {
		Data   json.RawMessage
		Errors []struct{ Message string }
	}
	if _, err := p.client.Do(req, &resp); err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	if len(resp.Errors) > 0 {
		return errors.New("graphql: " + resp.Errors[0].Message)
	}
	return json.Unmarshal(resp.Data, data)
}

// mapPRNode maps a PR to its shadow issue, adding the status labels. The
// updated time is the later of the PR's and its newest check result, so a
// check finishing gets past the sinks' shadow-drift guard.
func mapPRNode(n *prNode, repo source.Repo, title func(int64, string) string) source.Issue {
	labels := make([]string, 0, len(n.Labels.Nodes)+2)
	for _, l := range n.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	state := "open"
	if n.State != "OPEN" {
		state = "closed"
	}
	switch {
	case n.Merged:
		labels = append(labels, source.LabelMerged)
	case state == "closed" && promoted(n, repo):
		labels = append(labels, source.LabelPromoted)
	}

	updated := n.UpdatedAt
	if len(n.Commits.Nodes) > 0 {
		if r := n.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil {
			if l := checksLabel(r.State); l != "" {
				labels = append(labels, l)
			}
			for _, c := range r.Contexts.Nodes {
				for _, t := range []*time.Time{c.StartedAt, c.CompletedAt, c.CreatedAt} {
					if t != nil && t.After(updated) {
						updated = *t
					}
				}
			}
		}
	}

	var author source.User
	if n.Author != nil {
		author = source.User{Login: n.Author.Login, AvatarURL: n.Author.AvatarURL, HTMLURL: n.Author.URL}
	}
	return source.Issue{
		Number:    n.Number,
		Title:     title(n.Number, n.Title),
		Body:      strings.TrimSpace(n.Body),
		State:     state,
		Labels:    labels,
		Author:    author,
		HTMLURL:   n.URL,
		CreatedAt: n.CreatedAt,
		UpdatedAt: updated,
		ClosedAt:  n.ClosedAt,
	}
}

// promoted reports whether forgesync closed this PR after moving it into a
// canonical Forgejo with /sync: its "Promoted to" notice carries a
// pull_request marker for this very PR.
func promoted(n *prNode, repo source.Repo) bool {
	for _, c := range n.Comments.Nodes {
		m, ok := marker.Parse(c.Body)
		if ok && m.Type == "github" && m.Kind == promotedMarkerKind && m.Repo == repo.Slug() && m.ID == n.Number {
			return true
		}
	}
	return false
}

func checksLabel(state string) string {
	switch state {
	case "SUCCESS":
		return source.LabelChecksPassing
	case "FAILURE", "ERROR":
		return source.LabelChecksFailing
	case "PENDING", "EXPECTED":
		return source.LabelChecksPending
	}
	return ""
}
