package syncloop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"code.gitea.io/sdk/gitea"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/config"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

// recorder captures the order of side effects across fakes so tests can assert
// the close-ordering invariant: the GitHub PR must be closed before the
// canonical shadow, and the shadow must not be closed while the PR is open.
type recorder struct{ events []string }

// fakeFJClient implements forgejoClient.
type fakeFJClient struct {
	rec        *recorder
	openIssues []*gitea.Issue
	comments   []gitea.CreateIssueCommentOption
}

func (f *fakeFJClient) SetContext(context.Context) {}

func (f *fakeFJClient) SearchRepos(gitea.SearchRepoOptions) ([]*gitea.Repository, *gitea.Response, error) {
	return nil, nil, nil
}

func (f *fakeFJClient) ListPushMirrors(string, string, gitea.ListOptions) ([]*gitea.PushMirrorResponse, *gitea.Response, error) {
	return nil, nil, nil
}

func (f *fakeFJClient) ListRepoIssues(string, string, gitea.ListIssueOption) ([]*gitea.Issue, *gitea.Response, error) {
	return f.openIssues, nil, nil
}

func (f *fakeFJClient) CreateIssueComment(_, _ string, _ int64, opt gitea.CreateIssueCommentOption) (*gitea.Comment, *gitea.Response, error) {
	f.comments = append(f.comments, opt)
	return &gitea.Comment{}, nil, nil
}

func (f *fakeFJClient) EditIssue(_, _ string, _ int64, opt gitea.EditIssueOption) (*gitea.Issue, *gitea.Response, error) {
	if opt.State != nil && *opt.State == gitea.StateClosed {
		f.rec.events = append(f.rec.events, "shadow-close")
	}
	return &gitea.Issue{}, nil, nil
}

// fakeCanonicalSink implements canonicalPRSink.
type fakeCanonicalSink struct {
	hasShadow   bool
	upsertNum   int64
	upsertCalls int
}

func (f *fakeCanonicalSink) Kind() string { return "forgejo" }

func (f *fakeCanonicalSink) UpsertIssue(context.Context, source.Repo, source.Issue, marker.Marker) (int64, error) {
	return 0, nil
}

func (f *fakeCanonicalSink) UpsertComment(context.Context, source.Repo, int64, source.Comment, marker.Marker) error {
	return nil
}

func (f *fakeCanonicalSink) HasPRShadow(context.Context, source.Repo, marker.Marker) (int64, bool) {
	return f.upsertNum, f.hasShadow
}

func (f *fakeCanonicalSink) UpsertPullRequest(context.Context, source.Repo, source.PullRequest, marker.Marker, string, string) (int64, error) {
	f.upsertCalls++
	return f.upsertNum, nil
}

// fakeGHSink implements githubPRSink.
type fakeGHSink struct {
	rec        *recorder
	closeErr   error
	closeCalls int
	lastNum    int64
	lastBody   string
}

func (f *fakeGHSink) Kind() string { return "github" }

func (f *fakeGHSink) UpsertIssue(context.Context, source.Repo, source.Issue, marker.Marker) (int64, error) {
	return 0, nil
}

func (f *fakeGHSink) UpsertComment(context.Context, source.Repo, int64, source.Comment, marker.Marker) error {
	return nil
}

func (f *fakeGHSink) CommentAndClosePullRequest(_ context.Context, _ source.Repo, number int64, comment string, _ marker.Marker) error {
	f.closeCalls++
	f.lastNum = number
	f.lastBody = comment
	f.rec.events = append(f.rec.events, "gh-close")
	return f.closeErr
}

// fakePRSource implements pullRequestSource.
type fakePRSource struct {
	pr source.PullRequest
}

func (f *fakePRSource) GetPullRequest(context.Context, source.Repo, int64) (source.PullRequest, error) {
	return f.pr, nil
}

func testEngine(fj *fakeFJClient, cs *fakeCanonicalSink, ghSink *fakeGHSink, prSrc *fakePRSource, canonicalSrc source.Provider) *Engine {
	return &Engine{
		cfg:           &config.Config{Bot: config.Bot{Username: tBot}},
		fjClient:      fj,
		ghPRs:         prSrc,
		canonicalSink: cs,
		github:        ghSink,
		canonicalSrc:  canonicalSrc,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

const tBot = "forgesync-bot"

func TestPromotePR_CloseOrdering(t *testing.T) {
	canonical := source.Repo{Owner: tOwner, Name: tRepoSrc}
	target := source.Repo{Owner: tOwner, Name: tRepoSrc}
	iss := source.Issue{Number: 14, Title: "[PR #7] thing"}
	issMarker := marker.Marker{Type: tGithub, Host: tGHHost, Repo: target.Slug(), Kind: kindIssue, ID: 7}
	prMarker := marker.Marker{Type: tGithub, Host: tGHHost, Repo: target.Slug(), Kind: kindPullRequest, ID: 7}

	cases := []struct {
		name        string
		prState     string
		ghCloseErr  error
		wantGHClose int
		wantEvents  []string
	}{
		// Open PR: close GitHub first, then the canonical shadow.
		{"open", stateOpen, nil, 1, []string{"gh-close", "shadow-close"}},
		// Already merged/closed on GitHub: skip the GH close, still close the shadow.
		{"already closed", "closed", nil, 0, []string{"shadow-close"}},
		// GH close fails: leave the shadow OPEN so Flow A won't fight it next tick.
		{"gh close fails", stateOpen, errors.New("boom"), 1, []string{"gh-close"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			fj := &fakeFJClient{rec: rec}
			ghSink := &fakeGHSink{rec: rec, closeErr: tc.ghCloseErr}
			cs := &fakeCanonicalSink{upsertNum: 15}
			prSrc := &fakePRSource{pr: source.PullRequest{Issue: source.Issue{Number: 7, State: tc.prState}}}
			e := testEngine(fj, cs, ghSink, prSrc, nil)

			if err := e.promotePR(context.Background(), canonical, target, iss, issMarker, prMarker); err != nil {
				t.Fatal(err)
			}
			if cs.upsertCalls != 1 {
				t.Errorf("UpsertPullRequest calls = %d, want 1", cs.upsertCalls)
			}
			if ghSink.closeCalls != tc.wantGHClose {
				t.Errorf("GH close calls = %d, want %d", ghSink.closeCalls, tc.wantGHClose)
			}
			if !slices.Equal(rec.events, tc.wantEvents) {
				t.Errorf("side-effect order = %v, want %v", rec.events, tc.wantEvents)
			}
			// The canonical notice is always posted, and must carry a marker so
			// Flow B doesn't echo it onto the GitHub PR.
			if len(fj.comments) != 1 {
				t.Fatalf("expected 1 canonical notice, got %d", len(fj.comments))
			}
			if !marker.Has(fj.comments[0].Body) {
				t.Errorf("canonical notice must carry a marker, got %q", fj.comments[0].Body)
			}
		})
	}
}

func TestDetectAndPromotePRs(t *testing.T) {
	canonical := source.Repo{Owner: tOwner, Name: tRepoSrc}
	target := source.Repo{Owner: tOwner, Name: tRepoSrc}
	prShadowMarker := marker.Marker{Type: tGithub, Host: tGHHost, Repo: target.Slug(), Kind: kindIssue, ID: 7}

	prShadowIssue := func() *gitea.Issue {
		return &gitea.Issue{Index: 14, Title: "[PR #7] thing", Body: "body\n\n" + prShadowMarker.String()}
	}
	syncComment := map[int64][]source.Comment{
		14: {{Author: source.User{Login: "alice"}, Body: syncCommand}},
	}

	cases := []struct {
		name        string
		host        string
		issues      []*gitea.Issue
		comments    map[int64][]source.Comment
		hasShadow   bool
		wantPromote bool
		wantGHClose int
	}{
		{"promotes on /sync", githubHost, []*gitea.Issue{prShadowIssue()}, syncComment, false, true, 1},
		{"no /sync comment", githubHost, []*gitea.Issue{prShadowIssue()}, nil, false, false, 0},
		// Already promoted but the shadow is still open: don't re-upsert, but DO
		// retry the pending close of the source GitHub PR.
		{"already promoted retries close", githubHost, []*gitea.Issue{prShadowIssue()}, syncComment, true, false, 1},
		{"non-github host skipped", tFJHost, []*gitea.Issue{prShadowIssue()}, syncComment, false, false, 0},
		{"non-PR issue ignored", githubHost, []*gitea.Issue{{Index: 1, Title: "regular bug"}}, nil, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			fj := &fakeFJClient{rec: rec, openIssues: tc.issues}
			ghSink := &fakeGHSink{rec: rec}
			cs := &fakeCanonicalSink{hasShadow: tc.hasShadow, upsertNum: 15}
			prSrc := &fakePRSource{pr: source.PullRequest{Issue: source.Issue{Number: 7, State: stateOpen}}}
			src := &fakeSource{kind: tForgejo, host: tFJHost, comments: tc.comments}
			e := testEngine(fj, cs, ghSink, prSrc, src)

			if err := e.detectAndPromotePRs(context.Background(), canonical, tc.host, target); err != nil {
				t.Fatal(err)
			}
			gotPromote := cs.upsertCalls > 0
			if gotPromote != tc.wantPromote {
				t.Errorf("promoted = %v, want %v (upsertCalls=%d)", gotPromote, tc.wantPromote, cs.upsertCalls)
			}
			if ghSink.closeCalls != tc.wantGHClose {
				t.Errorf("GH close calls = %d, want %d", ghSink.closeCalls, tc.wantGHClose)
			}
		})
	}
}
