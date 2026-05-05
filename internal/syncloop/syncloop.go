// Package syncloop drives the periodic two-way reconciliation. For each repo
// in the canonical Forgejo:
//
//	Flow A: each push_mirror target → canonical (issues/comments filed on the
//	        mirror flow back into the source-of-truth)
//	Flow B: canonical → each push_mirror target (issues/comments filed in
//	        the canonical Forgejo flow out to the mirror)
//
// Loop prevention: items whose body contains a forgesync marker are shadows
// (forgesync wrote them); they are filtered out at read time on every side.
package syncloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"code.gitea.io/sdk/gitea"
	gh "github.com/google/go-github/v75/github"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/config"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/forgejoapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/githubapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/gitops"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/sink"
	fjsink "git.erwanleboucher.dev/eleboucher/forgesync/internal/sink/forgejo"
	ghsink "git.erwanleboucher.dev/eleboucher/forgesync/internal/sink/github"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
	fjsource "git.erwanleboucher.dev/eleboucher/forgesync/internal/source/forgejo"
	ghsource "git.erwanleboucher.dev/eleboucher/forgesync/internal/source/github"
)

const (
	githubHost          = "github.com"
	forgejoTokenEnvBase = "FORGESYNC_FORGEJO_TOKEN_" //nolint:gosec // env var name, not a secret
	kindIssue           = "issue"
	kindComment         = "comment"
	kindPullRequest     = "pull_request"
	syncCommand         = "/sync"
	prTitlePrefix       = "[PR #"
)

// Engine is single-goroutine: tick is invoked sequentially from Run, and all
// per-repo work happens inside that single tick. The lazy provider/sink maps
// are therefore written without a mutex. If you ever fan out repo work across
// goroutines, guard them.
type Engine struct {
	cfg       *config.Config
	srcClient *forgejoapi.Client // typed SDK client for the canonical Forgejo
	ghClient  *gh.Client         // for fetching full PR data when promoting
	log       *slog.Logger

	// Canonical Forgejo as both source (for Flow B reads) and sink (for Flow A writes).
	canonicalSrc  *fjsource.Provider
	canonicalSink *fjsink.Sink

	// Outbound (Flow B) sinks per host, lazily populated.
	github       *ghsink.Sink
	forgejoSinks map[string]*fjsink.Sink

	// Inbound (Flow A) sources per host, lazily populated.
	githubSrc   *ghsource.Provider
	forgejoSrcs map[string]*fjsource.Provider
}

func New(cfg *config.Config, log *slog.Logger) (*Engine, error) {
	srcClient, err := forgejoapi.New(cfg.Source.URL, cfg.Source.Token)
	if err != nil {
		return nil, fmt.Errorf("forgejo client: %w", err)
	}
	canonicalHost := hostFromURL(cfg.Source.URL)

	ghClient := githubapi.New(cfg.Targets.GitHub.Token, "")

	return &Engine{
		cfg:           cfg,
		srcClient:     srcClient,
		ghClient:      ghClient,
		log:           log,
		canonicalSrc:  fjsource.NewWithClient(srcClient, canonicalHost),
		canonicalSink: fjsink.New(srcClient, cfg.Bot.Username, log),
		github:        ghsink.New(ghClient, log),
		githubSrc:     ghsource.NewWithClient(ghClient),
		forgejoSinks:  map[string]*fjsink.Sink{},
		forgejoSrcs:   map[string]*fjsource.Provider{},
	}, nil
}

func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(e.cfg.PollInterval)
	defer t.Stop()

	// First tick uses the wider InitialBackfill window so we catch activity
	// from before the daemon started.
	initialSince := time.Now().Add(-e.cfg.InitialBackfill)
	if err := e.tick(ctx, initialSince); err != nil && !errors.Is(err, context.Canceled) {
		e.log.Error("initial tick failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			since := time.Now().Add(-e.cfg.Window())
			if err := e.tick(ctx, since); err != nil && !errors.Is(err, context.Canceled) {
				e.log.Error("tick failed", "err", err)
			}
		}
	}
}

func (e *Engine) tick(parent context.Context, since time.Time) error {
	ctx, cancel := context.WithTimeout(parent, e.cfg.Window())
	defer cancel()

	e.log.Info("tick start", "since", since)

	const repoPageSize = 50
	for page := 1; ; page++ {
		repos, _, err := e.srcClient.SearchRepos(gitea.SearchRepoOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: repoPageSize},
		})
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			break
		}
		for _, repo := range repos {
			if err := e.safeSyncRepo(ctx, repo, since); err != nil {
				e.log.Error("sync repo failed", "repo", repo.FullName, "err", err)
			}
		}
		if len(repos) < repoPageSize {
			break
		}
	}
	e.log.Info("tick done")
	return nil
}

func (e *Engine) safeSyncRepo(ctx context.Context, repo *gitea.Repository, since time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic syncing %s: %v", repo.FullName, r)
		}
	}()
	return e.syncRepo(ctx, repo, since)
}

func (e *Engine) syncRepo(ctx context.Context, repo *gitea.Repository, since time.Time) error {
	_ = ctx
	owner, name := splitFullName(repo.FullName)
	mirrors, _, err := e.srcClient.ListPushMirrors(owner, name, gitea.ListOptions{})
	if err != nil {
		return err
	}
	if len(mirrors) == 0 {
		return nil
	}

	canonical := source.Repo{Owner: owner, Name: name}
	for _, m := range mirrors {
		host, target, err := parseRemoteRepo(m.RemoteAddress)
		if err != nil {
			e.log.Warn("skip mirror: bad remote", "repo", repo.FullName, "remote", m.RemoteAddress, "err", err)
			continue
		}

		// Flow A: target → canonical
		if err := e.syncInbound(ctx, canonical, host, target, since); err != nil {
			e.log.Error("flow A failed", "repo", repo.FullName, "remote", m.RemoteAddress, "err", err)
		}
		// Flow B: canonical → target
		if err := e.syncOutbound(ctx, canonical, host, target, since); err != nil {
			e.log.Error("flow B failed", "repo", repo.FullName, "remote", m.RemoteAddress, "err", err)
		}
		// /sync command flow: promote PR-shadow issues to real Forgejo PRs.
		if err := e.detectAndPromotePRs(ctx, canonical, host, target, since); err != nil {
			e.log.Error("PR promotion pass failed", "repo", repo.FullName, "remote", m.RemoteAddress, "err", err)
		}
	}
	return nil
}

// detectAndPromotePRs looks for [PR #N] shadow issues with a /sync comment and
// promotes them to real Forgejo PRs. Only runs for github.com targets.
// /sync must be posted on canonical Forgejo (trust check: user owns that forge).
func (e *Engine) detectAndPromotePRs(ctx context.Context, canonical source.Repo, host string, target source.Repo, since time.Time) error {
	if host != githubHost {
		return nil
	}

	issues, err := e.canonicalSrc.ListIssues(ctx, canonical, source.ListOpts{Since: since})
	if err != nil {
		return err
	}

	for _, iss := range issues {
		if !strings.HasPrefix(iss.Title, prTitlePrefix) {
			continue
		}
		m, ok := marker.Parse(iss.Body)
		if !ok || m.Host != githubHost || m.Kind != kindIssue {
			continue
		}

		comments, err := e.canonicalSrc.ListComments(ctx, canonical, iss.Number, source.ListOpts{})
		if err != nil {
			e.log.Error("list canonical comments failed", "iss", iss.Number, "err", err)
			continue
		}
		if !hasSyncCommand(comments, e.cfg.Bot.Username) {
			continue
		}

		prMarker := marker.Marker{
			Type: m.Type, Host: m.Host, Repo: m.Repo,
			Kind: kindPullRequest, ID: m.ID,
		}
		if e.canonicalSink.HasPRShadow(ctx, canonical, prMarker) {
			e.log.Debug("PR already promoted, skip",
				"canonical_iss", iss.Number, "src_pr", m.ID)
			continue
		}

		if err := e.promotePR(ctx, canonical, target, iss, m, prMarker); err != nil {
			e.log.Error("promote PR failed",
				"canonical_iss", iss.Number, "src_pr", m.ID, "err", err)
		}
	}
	return nil
}

// promotePR creates a real Forgejo PR from a [PR #N] shadow issue.
//
// Side-effect: pushing forgesync/pr-N to Forgejo triggers push-mirror, which
// runs git push --mirror and deletes refs absent locally. If the PR head branch
// only exists on GitHub (external contributor), push-mirror will delete it and
// GitHub will auto-close the PR. We don't pre-mirror it because that lands
// untrusted code on the canonical forge as a normal branch.
func (e *Engine) promotePR(ctx context.Context, canonical, target source.Repo, iss source.Issue, issMarker, prMarker marker.Marker) error {
	pr, _, err := e.ghClient.PullRequests.Get(ctx, target.Owner, target.Name, int(issMarker.ID))
	if err != nil {
		return fmt.Errorf("fetch source PR: %w", err)
	}

	user := pr.GetUser()
	srcPR := source.PullRequest{
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
			ClosedAt:  timestampPtr(pr.ClosedAt),
		},
		BaseBranch: pr.GetBase().GetRef(),
		HeadBranch: pr.GetHead().GetRef(),
		HeadSHA:    pr.GetHead().GetSHA(),
		Merged:     pr.GetMerged(),
		MergedAt:   timestampPtr(pr.MergedAt),
	}

	srcGitURL := fmt.Sprintf("https://%s/%s/%s.git", githubHost, target.Owner, target.Name)
	if e.cfg.Targets.GitHub.Token != "" {
		if authed, err := gitops.AuthURL(srcGitURL, "oauth2", e.cfg.Targets.GitHub.Token); err == nil {
			srcGitURL = authed
		}
	}
	srcRef := fmt.Sprintf("refs/pull/%d/head", issMarker.ID)

	destNum, err := e.canonicalSink.UpsertPullRequest(ctx, canonical, srcPR, prMarker, srcGitURL, srcRef)
	if err != nil {
		return fmt.Errorf("upsert PR: %w", err)
	}

	// Post a "Promoted to #X" notice on the original issue and close it.
	notice := fmt.Sprintf("Promoted to #%d. Future updates will sync to that PR.", destNum)
	if _, _, err := e.srcClient.CreateIssueComment(canonical.Owner, canonical.Name, iss.Number, gitea.CreateIssueCommentOption{Body: notice}); err != nil {
		e.log.Warn("post promotion notice failed", "iss", iss.Number, "err", err)
	}
	closed := gitea.StateClosed
	if _, _, err := e.srcClient.EditIssue(canonical.Owner, canonical.Name, iss.Number, gitea.EditIssueOption{
		State: &closed,
	}); err != nil {
		e.log.Warn("close promoted issue failed", "iss", iss.Number, "err", err)
	}
	e.log.Info("promoted PR",
		"canonical_iss", iss.Number, "src_pr", issMarker.ID, "canonical_pr", destNum)
	return nil
}

// hasSyncCommand reports whether any non-bot comment is the `/sync` slash
// command, optionally followed by arguments.
func hasSyncCommand(comments []source.Comment, botUser string) bool {
	for _, c := range comments {
		if c.Author.Login == botUser {
			continue
		}
		fields := strings.Fields(c.Body)
		if len(fields) > 0 && fields[0] == syncCommand {
			return true
		}
	}
	return false
}

// syncInbound pulls native items from the mirror target into the canonical
// Forgejo (Flow A).
func (e *Engine) syncInbound(ctx context.Context, canonical source.Repo, host string, target source.Repo, since time.Time) error {
	src, err := e.sourceForHost(host)
	if err != nil {
		return err
	}
	return e.syncOneWay(ctx, src, target, e.canonicalSink, canonical, since)
}

// syncOutbound pushes native items from the canonical Forgejo to the mirror
// target (Flow B).
func (e *Engine) syncOutbound(ctx context.Context, canonical source.Repo, host string, target source.Repo, since time.Time) error {
	dst, err := e.sinkForHost(host)
	if err != nil {
		return err
	}
	return e.syncOneWay(ctx, e.canonicalSrc, canonical, dst, target, since)
}

// syncOneWay applies the shared per-direction logic: list issues from `src`,
// upsert native ones to `dst`. For each issue (native OR a shadow pointing at
// dst), list its native comments and upsert them. Shadows pointing at a
// different forge are left alone entirely.
func (e *Engine) syncOneWay(ctx context.Context, src source.Provider, srcRepo source.Repo, dst sink.Sink, dstRepo source.Repo, since time.Time) error {
	e.log.Info("sync",
		"direction", src.Kind()+"→"+dst.Kind(),
		"src_host", src.Host(), "src_repo", srcRepo.Slug(),
		"dst_repo", dstRepo.Slug(),
		"since", since,
	)

	issues, err := src.ListIssues(ctx, srcRepo, source.ListOpts{Since: since})
	if err != nil {
		return err
	}
	e.log.Debug("listed issues",
		"direction", src.Kind()+"→"+dst.Kind(),
		"src_repo", srcRepo.Slug(), "count", len(issues))

	for _, iss := range issues {
		destNum, ok := e.routeIssue(ctx, src, srcRepo, dst, dstRepo, iss)
		if !ok {
			continue
		}

		comments, err := src.ListComments(ctx, srcRepo, iss.Number, source.ListOpts{Since: since})
		if err != nil {
			e.log.Error("list comments failed",
				"src_repo", srcRepo.Slug(), "src_num", iss.Number, "err", err)
			continue
		}
		var (
			nativeComments int
			shadowFiltered int
		)
		for _, c := range comments {
			if marker.Has(c.Body) {
				shadowFiltered++
				continue
			}
			nativeComments++
			commentMarker := marker.Marker{
				Type: src.Kind(),
				Host: src.Host(),
				Repo: srcRepo.Slug(),
				Kind: kindComment,
				ID:   c.ID,
			}
			if err := dst.UpsertComment(ctx, dstRepo, destNum, c, commentMarker); err != nil {
				e.log.Error("upsert comment failed",
					"dst_repo", dstRepo.Slug(), "dst_issue", destNum,
					"src_comment", c.ID, "err", err)
			}
		}
		if len(comments) > 0 {
			e.log.Debug("processed comments",
				"src_repo", srcRepo.Slug(), "src_num", iss.Number,
				"dst_issue", destNum,
				"total", len(comments), "native", nativeComments, "shadows", shadowFiltered)
		}
	}
	return nil
}

// routeIssue decides what destination issue number to use for an item read
// from src. It returns (destNum, true) when comments under this issue should
// be processed.
//
//   - Native issue (no marker): upsert to dst, return the new dest number.
//   - Shadow whose marker points at the current dst: don't upsert (loop), but
//     return the marker's ID so native comments can be parented correctly.
//   - Shadow pointing somewhere else: skip — not our concern in this direction.
func (e *Engine) routeIssue(ctx context.Context, src source.Provider, srcRepo source.Repo, dst sink.Sink, dstRepo source.Repo, iss source.Issue) (int64, bool) {
	if m, isShadow := marker.Parse(iss.Body); isShadow {
		if m.Type != dst.Kind() || m.Repo != dstRepo.Slug() || m.Kind != kindIssue {
			e.log.Debug("foreign shadow skipped",
				"src_repo", srcRepo.Slug(), "src_num", iss.Number,
				"marker_type", m.Type, "marker_repo", m.Repo)
			return 0, false
		}
		e.log.Debug("shadow routed for comment-only sync",
			"src_repo", srcRepo.Slug(), "src_num", iss.Number,
			"dst_repo", dstRepo.Slug(), "dst_num", m.ID)
		return m.ID, true
	}
	issueMarker := marker.Marker{
		Type: src.Kind(),
		Host: src.Host(),
		Repo: srcRepo.Slug(),
		Kind: kindIssue,
		ID:   iss.Number,
	}
	destNum, err := dst.UpsertIssue(ctx, dstRepo, iss, issueMarker)
	if err != nil {
		e.log.Error("upsert issue failed",
			"src_repo", srcRepo.Slug(), "src_num", iss.Number,
			"dst_repo", dstRepo.Slug(), "err", err)
		return 0, false
	}
	return destNum, true
}

func (e *Engine) sourceForHost(host string) (source.Provider, error) {
	if host == githubHost {
		if e.cfg.Targets.GitHub.Token == "" {
			return nil, fmt.Errorf("github mirror found but FORGESYNC_GITHUB_TOKEN is not set")
		}
		return e.githubSrc, nil
	}
	if p, ok := e.forgejoSrcs[host]; ok {
		return p, nil
	}
	envName := forgejoTokenEnvBase + normalizeHostForEnv(host)
	token := os.Getenv(envName)
	if token == "" {
		return nil, fmt.Errorf("no source token for host %q (set %s)", host, envName)
	}
	client, err := forgejoapi.New("https://"+host, token)
	if err != nil {
		return nil, fmt.Errorf("forgejo client for %s: %w", host, err)
	}
	p := fjsource.NewWithClient(client, host)
	e.forgejoSrcs[host] = p
	return p, nil
}

func (e *Engine) sinkForHost(host string) (sink.Sink, error) {
	if host == githubHost {
		if e.cfg.Targets.GitHub.Token == "" {
			return nil, fmt.Errorf("github mirror found but FORGESYNC_GITHUB_TOKEN is not set")
		}
		return e.github, nil
	}
	if s, ok := e.forgejoSinks[host]; ok {
		return s, nil
	}
	envName := forgejoTokenEnvBase + normalizeHostForEnv(host)
	token := os.Getenv(envName)
	if token == "" {
		return nil, fmt.Errorf("no sink token for host %q (set %s)", host, envName)
	}
	client, err := forgejoapi.New("https://"+host, token)
	if err != nil {
		return nil, fmt.Errorf("forgejo client for %s: %w", host, err)
	}
	s := fjsink.New(client, e.cfg.Bot.Username, e.log)
	e.forgejoSinks[host] = s
	return s, nil
}

// parseRemoteRepo pulls (host, owner/name) out of a push_mirror remote_address.
func parseRemoteRepo(remote string) (host string, repo source.Repo, err error) {
	u, err := url.Parse(remote)
	if err != nil {
		return "", source.Repo{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", source.Repo{}, fmt.Errorf("invalid remote address: %s", remote)
	}
	return strings.ToLower(u.Host), source.Repo{
		Owner: parts[0],
		Name:  strings.TrimSuffix(parts[1], ".git"),
	}, nil
}

func splitFullName(full string) (string, string) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok {
		return full, ""
	}
	return owner, name
}

func hostFromURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func timestampPtr(t *gh.Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	tm := t.Time
	return &tm
}

func normalizeHostForEnv(host string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		default:
			return '_'
		}
	}, host)
}
