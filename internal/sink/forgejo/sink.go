// Package forgejo is the Forgejo destination writer. Stateless: every item
// carries a marker, and the marker drives idempotent upsert via search.
package forgejo

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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
	s.client.SetContext(ctx)
	body := renderIssueBody(src, m)

	existing, err := s.findIssueByMarker(dest, m, gitea.IssueTypeIssue)
	if err != nil {
		return 0, err
	}

	if existing == nil {
		labelIDs, err := s.labelIDs(dest, src.Labels, nil)
		if err != nil {
			// A label we can't create shouldn't hold back the issue itself.
			s.log.Warn("forgejo sink: creating issue without labels",
				"dest", dest.Slug(), "marker_id", m.ID, "err", err)
			labelIDs = nil
		}
		created, _, err := s.client.CreateIssue(dest.Owner, dest.Name, gitea.CreateIssueOption{
			Title:  src.Title,
			Body:   body,
			Closed: src.State == "closed",
			Labels: labelIDs,
		})
		if err != nil {
			return 0, fmt.Errorf("create issue: %w", err)
		}
		s.log.Debug("forgejo sink: created issue",
			"dest", dest.Slug(), "dest_num", created.Index, "marker_id", m.ID)
		return created.Index, nil
	}

	stateChange := sink.PropagateState(string(existing.State), src.State)
	contentChanged := existing.Body != body || existing.Title != src.Title || stateChange != nil
	current := labelNames(existing.Labels)
	wantLabels := sink.ShadowLabels(current, sink.SyncedLabels(existing.Body), src.Labels)
	labelsChanged := !sink.LabelsEqual(current, wantLabels)
	if !contentChanged && !labelsChanged {
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

	if contentChanged {
		editOpt := gitea.EditIssueOption{
			Title: src.Title,
			Body:  &body,
		}
		if stateChange != nil {
			st := gitea.StateType(*stateChange)
			editOpt.State = &st
		}
		if _, _, err := s.client.EditIssue(dest.Owner, dest.Name, existing.Index, editOpt); err != nil {
			return 0, fmt.Errorf("edit issue: %w", err)
		}
		if stateChange != nil {
			s.log.Info("forgejo sink: synced issue state",
				"dest", dest.Slug(), "dest_num", existing.Index, "state", *stateChange)
		} else {
			s.log.Debug("forgejo sink: patched issue (title/body)",
				"dest", dest.Slug(), "dest_num", existing.Index)
		}
	}
	if labelsChanged {
		labelIDs, err := s.labelIDs(dest, wantLabels, existing.Labels)
		if err == nil {
			_, _, err = s.client.ReplaceIssueLabels(dest.Owner, dest.Name, existing.Index, gitea.IssueLabelsOption{
				Labels: labelIDs,
			})
		}
		if err != nil {
			// Logged, not returned: a label problem shouldn't block the issue's comments.
			s.log.Warn("forgejo sink: label sync failed",
				"dest", dest.Slug(), "dest_num", existing.Index, "err", err)
		} else {
			s.log.Debug("forgejo sink: synced issue labels",
				"dest", dest.Slug(), "dest_num", existing.Index, "labels", wantLabels)
		}
	}
	return existing.Index, nil
}

// labelIDs resolves label names to label IDs, creating any label the repo
// doesn't have yet. known seeds the lookup with labels already on the issue,
// so an org-level label there is reused rather than duplicated in the repo.
// Names match case-insensitively.
func (s *Sink) labelIDs(dest source.Repo, names []string, known []*gitea.Label) ([]int64, error) {
	ids := make([]int64, 0, len(names))
	if len(names) == 0 {
		return ids, nil
	}
	byName, err := s.repoLabels(dest)
	if err != nil {
		return nil, err
	}
	for _, l := range known {
		byName[strings.ToLower(l.Name)] = l
	}
	for _, name := range names {
		if l, ok := byName[strings.ToLower(name)]; ok {
			ids = append(ids, l.ID)
			continue
		}
		created, _, err := s.client.CreateLabel(dest.Owner, dest.Name, gitea.CreateLabelOption{
			Name:  name,
			Color: "#" + sink.LabelColor,
		})
		if err != nil {
			return nil, fmt.Errorf("create label %q: %w", name, err)
		}
		s.log.Info("forgejo sink: created label", "dest", dest.Slug(), "label", name)
		byName[strings.ToLower(name)] = created
		ids = append(ids, created.ID)
	}
	return ids, nil
}

func (s *Sink) repoLabels(dest source.Repo) (map[string]*gitea.Label, error) {
	const pageSize = 50
	out := map[string]*gitea.Label{}
	for page := 1; ; page++ {
		batch, _, err := s.client.ListRepoLabels(dest.Owner, dest.Name, gitea.ListLabelsOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
		})
		if err != nil {
			return nil, fmt.Errorf("list labels: %w", err)
		}
		for _, l := range batch {
			out[strings.ToLower(l.Name)] = l
		}
		if len(batch) < pageSize {
			break
		}
	}
	return out, nil
}

func labelNames(labels []*gitea.Label) []string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return names
}

func (s *Sink) UpsertComment(ctx context.Context, dest source.Repo, destIssueNumber int64, src source.Comment, m marker.Marker) error {
	s.client.SetContext(ctx)
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
	s.client.SetContext(ctx)
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
// exists, returning its index. Used by the engine to skip duplicate promotions
// and to find the canonical PR when retrying a pending close.
func (s *Sink) HasPRShadow(ctx context.Context, dest source.Repo, m marker.Marker) (int64, bool) {
	s.client.SetContext(ctx)
	hit, _ := s.findIssueByMarker(dest, m, gitea.IssueTypePull)
	if hit == nil {
		return 0, false
	}
	return hit.Index, true
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
	body := sink.RenderBody(src.Author, src.HTMLURL, src.CreatedAt, src.Body, m, bodyLimit)
	return sink.WithSyncedLabels(body, src.Labels)
}

func renderCommentBody(src source.Comment, m marker.Marker) string {
	return sink.RenderBody(src.Author, src.HTMLURL, src.CreatedAt, src.Body, m, bodyLimit)
}
