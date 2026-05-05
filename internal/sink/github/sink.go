// Package github is the GitHub destination writer. Symmetrical with the
// Forgejo sink: marker-search-then-PATCH-or-POST, with a since= fallback
// for index lag and a shadow-drift guard to avoid clobbering user edits.
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	gh "github.com/google/go-github/v75/github"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/sink"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const (
	stateClosed = "closed"
	// GitHub rejects issue/comment bodies > 65536 chars. Stay under that with
	// headroom for attribution, marker, separators, and the truncation notice.
	bodyLimit = 60000
)

type Sink struct {
	client *gh.Client
	log    *slog.Logger
}

func New(client *gh.Client, log *slog.Logger) *Sink {
	return &Sink{client: client, log: log}
}

func (s *Sink) Kind() string { return "github" }

func (s *Sink) UpsertIssue(ctx context.Context, dest source.Repo, src source.Issue, m marker.Marker) (int64, error) {
	body := renderIssueBody(src, m)

	existing, err := s.findByMarker(ctx, dest, m)
	if err != nil {
		return 0, err
	}

	if existing == nil {
		created, _, err := s.client.Issues.Create(ctx, dest.Owner, dest.Name, &gh.IssueRequest{
			Title: gh.Ptr(src.Title),
			Body:  gh.Ptr(body),
		})
		if err != nil {
			return 0, fmt.Errorf("create issue: %w", err)
		}
		num := int64(created.GetNumber())
		s.log.Debug("github sink: created issue",
			"dest", dest.Slug(), "dest_num", num, "marker_id", m.ID)
		// GitHub's create endpoint always opens the issue. If the source is
		// already closed, send a follow-up PATCH.
		if src.State == stateClosed {
			if _, _, err := s.client.Issues.Edit(ctx, dest.Owner, dest.Name, created.GetNumber(), &gh.IssueRequest{
				State: gh.Ptr(stateClosed),
			}); err != nil {
				return 0, fmt.Errorf("close newly created issue: %w", err)
			}
			s.log.Debug("github sink: closed newly created issue", "dest_num", num)
		}
		return num, nil
	}

	existingNum := int64(existing.GetNumber())
	reopen := sink.PropagateReopen(existing.GetState(), src.State)
	if existing.GetBody() == body && existing.GetTitle() == src.Title && reopen == nil {
		s.log.Debug("github sink: issue unchanged, skip",
			"dest", dest.Slug(), "dest_num", existingNum)
		return existingNum, nil
	}

	if sink.ShadowDrifted(existing.GetUpdatedAt().Time, src.UpdatedAt) {
		s.log.Warn("skipping issue PATCH: shadow appears user-edited",
			"dest", dest.Slug(),
			"dest_num", existingNum,
			"shadow_updated", existing.GetUpdatedAt().Time,
			"source_updated", src.UpdatedAt)
		return existingNum, nil
	}

	// Asymmetric state policy: propagate reopens only. Never close from PATCH.
	editReq := &gh.IssueRequest{
		Title: gh.Ptr(src.Title),
		Body:  gh.Ptr(body),
	}
	if reopen != nil {
		editReq.State = reopen
	}
	if _, _, err := s.client.Issues.Edit(ctx, dest.Owner, dest.Name, existing.GetNumber(), editReq); err != nil {
		return 0, fmt.Errorf("edit issue: %w", err)
	}
	if reopen != nil {
		s.log.Info("github sink: reopened issue", "dest", dest.Slug(), "dest_num", existingNum)
	} else {
		s.log.Debug("github sink: patched issue (title/body)",
			"dest", dest.Slug(), "dest_num", existingNum)
	}
	return existingNum, nil
}

func (s *Sink) UpsertComment(ctx context.Context, dest source.Repo, destIssueNumber int64, src source.Comment, m marker.Marker) error {
	body := renderCommentBody(src, m)

	existing, err := s.findCommentByMarker(ctx, dest, destIssueNumber, m)
	if err != nil {
		return err
	}

	if existing == nil {
		if _, _, err := s.client.Issues.CreateComment(ctx, dest.Owner, dest.Name, int(destIssueNumber), &gh.IssueComment{
			Body: gh.Ptr(body),
		}); err != nil {
			return fmt.Errorf("create comment: %w", err)
		}
		s.log.Debug("github sink: created comment",
			"dest", dest.Slug(), "dest_issue", destIssueNumber, "marker_id", m.ID)
		return nil
	}

	if existing.GetBody() == body {
		s.log.Debug("github sink: comment unchanged, skip",
			"dest", dest.Slug(), "dest_issue", destIssueNumber, "comment_id", existing.GetID())
		return nil
	}
	if sink.ShadowDrifted(existing.GetUpdatedAt().Time, src.UpdatedAt) {
		s.log.Warn("skipping comment PATCH: shadow appears user-edited",
			"dest", dest.Slug(),
			"dest_issue", destIssueNumber,
			"comment_id", existing.GetID())
		return nil
	}
	if _, _, err := s.client.Issues.EditComment(ctx, dest.Owner, dest.Name, existing.GetID(), &gh.IssueComment{
		Body: gh.Ptr(body),
	}); err != nil {
		return fmt.Errorf("edit comment: %w", err)
	}
	s.log.Debug("github sink: patched comment",
		"dest", dest.Slug(), "dest_issue", destIssueNumber, "comment_id", existing.GetID())
	return nil
}

func (s *Sink) findByMarker(ctx context.Context, dest source.Repo, m marker.Marker) (*gh.Issue, error) {
	q := fmt.Sprintf("%s repo:%s/%s", m.SearchToken(), dest.Owner, dest.Name)
	searchOpts := &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: 50}}
	result, _, err := s.client.Search.Issues(ctx, q, searchOpts)
	if err != nil {
		return nil, fmt.Errorf("search by marker: %w", err)
	}
	if hit := matchMarker(filterOutPRs(result.Issues), m); hit != nil {
		return hit, nil
	}

	// Search index can lag; fall back to recent list.
	listOpts := &gh.IssueListByRepoOptions{
		State:       "all",
		ListOptions: gh.ListOptions{PerPage: 50},
		Since:       time.Now().Add(-1 * time.Hour),
	}
	recent, _, err := s.client.Issues.ListByRepo(ctx, dest.Owner, dest.Name, listOpts)
	if err != nil {
		return nil, fmt.Errorf("list recent for marker fallback: %w", err)
	}
	return matchMarker(recent, m), nil
}

func filterOutPRs(in []*gh.Issue) []*gh.Issue {
	out := make([]*gh.Issue, 0, len(in))
	for _, i := range in {
		if i.PullRequestLinks != nil {
			continue
		}
		out = append(out, i)
	}
	return out
}

func matchMarker(issues []*gh.Issue, m marker.Marker) *gh.Issue {
	for _, hit := range issues {
		if found, ok := marker.Parse(hit.GetBody()); ok && found == m {
			return hit
		}
	}
	return nil
}

func (s *Sink) findCommentByMarker(ctx context.Context, dest source.Repo, issueNumber int64, m marker.Marker) (*gh.IssueComment, error) {
	listOpts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 1; ; page++ {
		listOpts.ListOptions.Page = page //nolint:staticcheck // explicit selector for clarity even when unambiguous
		batch, _, err := s.client.Issues.ListComments(ctx, dest.Owner, dest.Name, int(issueNumber), listOpts)
		if err != nil {
			// Treat 404 (issue gone) the same as "no shadow found" so the caller
			// can try again next tick rather than failing hard.
			var ghErr *gh.ErrorResponse
			if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("list comments: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			if found, ok := marker.Parse(c.GetBody()); ok && found == m {
				return c, nil
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	return nil, nil
}

func renderIssueBody(src source.Issue, m marker.Marker) string {
	return sink.RenderBody(src.Author, src.HTMLURL, src.CreatedAt, src.Body, m, bodyLimit)
}

func renderCommentBody(src source.Comment, m marker.Marker) string {
	return sink.RenderBody(src.Author, src.HTMLURL, src.CreatedAt, src.Body, m, bodyLimit)
}
