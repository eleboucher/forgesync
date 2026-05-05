// Package forgejo is the Forgejo destination writer. Stateless: every item
// carries a marker, and the marker drives idempotent upsert via search.
package forgejo

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"code.gitea.io/sdk/gitea"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/forgejoapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/gitops"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/sink"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

// Forgejo's default body limit is configurable but typically generous; cap
// defensively so we never POST something the server will reject.
const bodyLimit = 1_000_000

type Sink struct {
	client *forgejoapi.Client
	bot    string
	log    *slog.Logger
}

func New(client *forgejoapi.Client, botUsername string, log *slog.Logger) *Sink {
	return &Sink{client: client, bot: botUsername, log: log}
}

func (s *Sink) Kind() string { return "forgejo" }

// UpsertIssue creates the issue if no marker match exists, otherwise PATCHes
// it. Skips the PATCH if the rendered body+title+state is unchanged, or if
// the shadow looks user-edited (updated_at significantly newer than source).
func (s *Sink) UpsertIssue(ctx context.Context, dest source.Repo, src source.Issue, m marker.Marker) (int64, error) {
	_ = ctx
	body := renderIssueBody(src, m)

	existing, err := s.findIssueByMarker(dest, m, gitea.IssueTypeIssue)
	if err != nil {
		return 0, err
	}

	if existing == nil {
		created, _, err := s.client.CreateIssue(dest.Owner, dest.Name, gitea.CreateIssueOption{
			Title:  src.Title,
			Body:   body,
			Closed: src.State == "closed",
		})
		if err != nil {
			return 0, fmt.Errorf("create issue: %w", err)
		}
		s.log.Debug("forgejo sink: created issue",
			"dest", dest.Slug(), "dest_num", created.Index, "marker_id", m.ID)
		return created.Index, nil
	}

	reopen := sink.PropagateReopen(string(existing.State), src.State)
	if existing.Body == body && existing.Title == src.Title && reopen == nil {
		s.log.Debug("forgejo sink: issue unchanged, skip",
			"dest", dest.Slug(), "dest_num", existing.Index)
		return existing.Index, nil
	}

	if sink.ShadowDrifted(existing.Updated, src.UpdatedAt) {
		s.log.Warn("skipping issue PATCH: shadow appears user-edited",
			"dest", dest.Slug(),
			"dest_num", existing.Index,
			"shadow_updated", existing.Updated,
			"source_updated", src.UpdatedAt)
		return existing.Index, nil
	}

	editOpt := gitea.EditIssueOption{
		Title: src.Title,
		Body:  &body,
	}
	if reopen != nil {
		st := gitea.StateType(*reopen)
		editOpt.State = &st
	}
	if _, _, err := s.client.EditIssue(dest.Owner, dest.Name, existing.Index, editOpt); err != nil {
		return 0, fmt.Errorf("edit issue: %w", err)
	}
	if reopen != nil {
		s.log.Info("forgejo sink: reopened issue", "dest", dest.Slug(), "dest_num", existing.Index)
	} else {
		s.log.Debug("forgejo sink: patched issue (title/body)",
			"dest", dest.Slug(), "dest_num", existing.Index)
	}
	return existing.Index, nil
}

func (s *Sink) UpsertComment(ctx context.Context, dest source.Repo, destIssueNumber int64, src source.Comment, m marker.Marker) error {
	_ = ctx
	body := renderCommentBody(src, m)

	existing, err := s.findCommentByMarker(dest, destIssueNumber, m)
	if err != nil {
		return err
	}

	if existing == nil {
		if _, _, err := s.client.CreateIssueComment(dest.Owner, dest.Name, destIssueNumber, gitea.CreateIssueCommentOption{Body: body}); err != nil {
			return fmt.Errorf("create comment: %w", err)
		}
		s.log.Debug("forgejo sink: created comment",
			"dest", dest.Slug(), "dest_issue", destIssueNumber, "marker_id", m.ID)
		return nil
	}

	if existing.Body == body {
		s.log.Debug("forgejo sink: comment unchanged, skip",
			"dest", dest.Slug(), "dest_issue", destIssueNumber, "comment_id", existing.ID)
		return nil
	}
	if sink.ShadowDrifted(existing.Updated, src.UpdatedAt) {
		s.log.Warn("skipping comment PATCH: shadow appears user-edited",
			"dest", dest.Slug(),
			"dest_issue", destIssueNumber,
			"comment_id", existing.ID)
		return nil
	}
	if _, _, err := s.client.EditIssueComment(dest.Owner, dest.Name, existing.ID, gitea.EditIssueCommentOption{Body: body}); err != nil {
		return fmt.Errorf("edit comment: %w", err)
	}
	s.log.Debug("forgejo sink: patched comment",
		"dest", dest.Slug(), "dest_issue", destIssueNumber, "comment_id", existing.ID)
	return nil
}

// UpsertPullRequest mirrors srcRef from srcGitURL into dest as
// forgesync/pr-{src.Number}, then creates or PATCHes a real Forgejo PR with a
// marker pointing back at the source. Returns the destination PR number.
func (s *Sink) UpsertPullRequest(ctx context.Context, dest source.Repo, src source.PullRequest, m marker.Marker, srcGitURL, srcRef string) (int64, error) {
	branchName := fmt.Sprintf("forgesync/pr-%d", src.Number)
	dstURL := s.client.AuthGitURL(dest.Owner, dest.Name)

	s.log.Info("forgejo sink: mirroring PR ref",
		"src_num", src.Number, "src_ref", srcRef, "dst_branch", branchName)
	if err := gitops.MirrorRef(ctx, srcGitURL, srcRef, dstURL, branchName); err != nil {
		return 0, fmt.Errorf("mirror ref: %w", err)
	}

	body := renderIssueBody(src.Issue, m)

	existing, err := s.findIssueByMarker(dest, m, gitea.IssueTypePull)
	if err != nil {
		return 0, err
	}

	if existing == nil {
		created, _, err := s.client.CreatePullRequest(dest.Owner, dest.Name, gitea.CreatePullRequestOption{
			Title: src.Title,
			Body:  body,
			Head:  branchName,
			Base:  src.BaseBranch,
		})
		if err != nil {
			return 0, fmt.Errorf("create PR: %w", err)
		}
		s.log.Info("forgejo sink: created PR",
			"dest", dest.Slug(), "dest_num", created.Index, "marker_id", m.ID)
		return created.Index, nil
	}

	if existing.Body == body && existing.Title == src.Title {
		s.log.Debug("forgejo sink: PR unchanged, skip",
			"dest", dest.Slug(), "dest_num", existing.Index)
		return existing.Index, nil
	}
	if _, _, err := s.client.EditPullRequest(dest.Owner, dest.Name, existing.Index, gitea.EditPullRequestOption{
		Title: src.Title,
		Body:  &body,
	}); err != nil {
		return 0, fmt.Errorf("edit PR: %w", err)
	}
	s.log.Debug("forgejo sink: patched PR (title/body only)",
		"dest", dest.Slug(), "dest_num", existing.Index)
	return existing.Index, nil
}

// HasPRShadow reports whether a real Forgejo PR matching the marker already
// exists. Used by the engine to skip duplicate promotions.
func (s *Sink) HasPRShadow(ctx context.Context, dest source.Repo, m marker.Marker) bool {
	_ = ctx
	hit, _ := s.findIssueByMarker(dest, m, gitea.IssueTypePull)
	return hit != nil
}

// findIssueByMarker searches for an issue or PR (per kind) carrying the
// marker, with a since= fallback for a freshly-created item the search index
// hasn't picked up yet.
func (s *Sink) findIssueByMarker(dest source.Repo, m marker.Marker, kind gitea.IssueType) (*gitea.Issue, error) {
	hits, _, err := s.client.ListRepoIssues(dest.Owner, dest.Name, gitea.ListIssueOption{
		ListOptions: gitea.ListOptions{PageSize: 50},
		State:       gitea.StateAll,
		Type:        kind,
		KeyWord:     m.SearchToken(),
	})
	if err != nil {
		return nil, fmt.Errorf("search by marker: %w", err)
	}
	if hit := matchMarker(hits, m); hit != nil {
		return hit, nil
	}

	// Forgejo's full-text search index is async on Elasticsearch backends, so a
	// freshly-created issue may not be searchable yet. Fall back to a since=
	// list (exact DB query) before declaring it absent.
	recent, _, err := s.client.ListRepoIssues(dest.Owner, dest.Name, gitea.ListIssueOption{
		ListOptions: gitea.ListOptions{PageSize: 50},
		State:       gitea.StateAll,
		Type:        kind,
		Since:       time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("list recent for marker fallback: %w", err)
	}
	return matchMarker(recent, m), nil
}

func matchMarker(issues []*gitea.Issue, m marker.Marker) *gitea.Issue {
	for _, hit := range issues {
		if found, ok := marker.Parse(hit.Body); ok && found == m {
			return hit
		}
	}
	return nil
}

func (s *Sink) findCommentByMarker(dest source.Repo, issueNumber int64, m marker.Marker) (*gitea.Comment, error) {
	const pageSize = 50
	for page := 1; ; page++ {
		batch, _, err := s.client.ListIssueComments(dest.Owner, dest.Name, issueNumber, gitea.ListIssueCommentOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
		})
		if err != nil {
			return nil, fmt.Errorf("list comments: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, c := range batch {
			if found, ok := marker.Parse(c.Body); ok && found == m {
				return c, nil
			}
		}
		if len(batch) < pageSize {
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
