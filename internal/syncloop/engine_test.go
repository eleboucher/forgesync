package syncloop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/config"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const (
	tForgejo  = "forgejo"
	tGithub   = "github"
	tGHHost   = "github.com"
	tFJHost   = "git.erwanleboucher.dev"
	tRepoFork = "fork"
	tRepoSrc  = "src"
	tOwner    = "me"
	tUpstream = "up/parent"
)

func forkRepo() source.Repo { return source.Repo{Owner: tOwner, Name: tRepoFork} }
func srcRepo() source.Repo  { return source.Repo{Owner: tOwner, Name: tRepoSrc} }

// fakeSource is an in-memory source.Provider for engine tests.
type fakeSource struct {
	kind, host string
	issues     []source.Issue
	comments   map[int64][]source.Comment
}

func (f *fakeSource) Kind() string { return f.kind }
func (f *fakeSource) Host() string { return f.host }
func (f *fakeSource) ListIssues(_ context.Context, _ source.Repo, _ source.ListOpts) ([]source.Issue, error) {
	return f.issues, nil
}

func (f *fakeSource) ListPullRequests(_ context.Context, _ source.Repo, _ source.ListOpts) ([]source.PullRequest, error) {
	return nil, nil
}

func (f *fakeSource) ListComments(_ context.Context, _ source.Repo, n int64, _ source.ListOpts) ([]source.Comment, error) {
	return f.comments[n], nil
}

// fakeSink records the markers and parent issue numbers it was asked to upsert.
type fakeSink struct {
	kind            string
	issueMarkers    []marker.Marker
	commentMarkers  []marker.Marker
	commentDestNums []int64
	failIssue       int64 // UpsertIssue fails for this source id
}

func (f *fakeSink) Kind() string { return f.kind }
func (f *fakeSink) UpsertIssue(_ context.Context, _ source.Repo, _ source.Issue, m marker.Marker) (int64, error) {
	if f.failIssue != 0 && m.ID == f.failIssue {
		return 0, errors.New("upsert failed")
	}
	f.issueMarkers = append(f.issueMarkers, m)
	return m.ID, nil // mirror the source id
}

func (f *fakeSink) UpsertComment(_ context.Context, _ source.Repo, destNum int64, _ source.Comment, m marker.Marker) error {
	f.commentMarkers = append(f.commentMarkers, m)
	f.commentDestNums = append(f.commentDestNums, destNum)
	return nil
}

func newEngine() *Engine {
	return &Engine{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestSyncOneWay_NativeIssueUpserted(t *testing.T) {
	// Shadow points at a different repo on dst — not ours, leave alone.
	foreignShadow := marker.Marker{Type: tForgejo, Host: "git.x", Repo: "a/b", Kind: kindIssue, ID: 1}
	src := &fakeSource{
		kind: tGithub, host: tGHHost,
		issues: []source.Issue{
			{Number: 1, Body: "native body, no marker", UpdatedAt: time.Now()},
			{Number: 2, Body: "foreign shadow\n\n" + foreignShadow.String(), UpdatedAt: time.Now()},
		},
	}
	sink := &fakeSink{kind: tForgejo}

	e := newEngine()
	if err := e.syncOneWay(context.Background(), src,
		forkRepo(), sink, srcRepo(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if len(sink.issueMarkers) != 1 {
		t.Fatalf("expected 1 upsert (native only), got %d", len(sink.issueMarkers))
	}
	if sink.issueMarkers[0].ID != 1 {
		t.Errorf("expected native issue (id=1) to be upserted, got id=%d", sink.issueMarkers[0].ID)
	}
}

func TestSyncOneWay_ShadowIssueRoutesCommentsToMarkerID(t *testing.T) {
	// User commented on a shadow on the source side. The shadow's marker points
	// at the dst (kind=tGithub, repo=me/src). Its native comment must flow to
	// dst issue #99 (the marker's ID), and the issue itself must NOT be re-upserted.
	dstRepo := srcRepo()
	shadowMarker := marker.Marker{Type: tGithub, Host: tGHHost, Repo: dstRepo.Slug(), Kind: kindIssue, ID: 99}
	src := &fakeSource{
		kind: tForgejo, host: tFJHost,
		issues: []source.Issue{
			{Number: 5, Body: "imported from github\n\n" + shadowMarker.String(), UpdatedAt: time.Now()},
		},
		comments: map[int64][]source.Comment{
			5: {{ID: 100, Body: "my response", UpdatedAt: time.Now()}},
		},
	}
	sink := &fakeSink{kind: tGithub}
	e := newEngine()

	if err := e.syncOneWay(context.Background(), src,
		forkRepo(), sink, dstRepo, time.Now()); err != nil {
		t.Fatal(err)
	}

	if len(sink.issueMarkers) != 0 {
		t.Errorf("expected no issue upsert for shadow, got %d", len(sink.issueMarkers))
	}
	if len(sink.commentMarkers) != 1 {
		t.Fatalf("expected 1 comment upsert, got %d", len(sink.commentMarkers))
	}
	if sink.commentDestNums[0] != 99 {
		t.Errorf("comment routed to dest %d, expected marker.ID=99", sink.commentDestNums[0])
	}
}

func TestSyncOneWay_ForeignShadowSkippedEntirely(t *testing.T) {
	// Shadow's marker points at a DIFFERENT repo than the dst — leave it alone.
	dstRepo := source.Repo{Owner: tOwner, Name: "actual"}
	foreignShadow := marker.Marker{Type: tGithub, Host: tGHHost, Repo: "someone/else", Kind: kindIssue, ID: 1}
	src := &fakeSource{
		kind: tForgejo, host: tFJHost,
		issues: []source.Issue{
			{Number: 7, Body: "x\n\n" + foreignShadow.String(), UpdatedAt: time.Now()},
		},
		comments: map[int64][]source.Comment{
			7: {{ID: 1, Body: "should not flow anywhere", UpdatedAt: time.Now()}},
		},
	}
	sink := &fakeSink{kind: tGithub}
	e := newEngine()

	if err := e.syncOneWay(context.Background(), src,
		forkRepo(), sink, dstRepo, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(sink.issueMarkers) != 0 || len(sink.commentMarkers) != 0 {
		t.Errorf("expected no upserts for foreign shadow, got issues=%d comments=%d",
			len(sink.issueMarkers), len(sink.commentMarkers))
	}
}

func TestSyncOneWay_FiltersShadowComments(t *testing.T) {
	shadowMarker := marker.Marker{Type: tGithub, Host: tGHHost, Repo: "x/y", Kind: kindComment, ID: 99}
	src := &fakeSource{
		kind: tForgejo, host: "git.example.com",
		issues: []source.Issue{{Number: 1, Body: "native", UpdatedAt: time.Now()}},
		comments: map[int64][]source.Comment{
			1: {
				{ID: 10, Body: "native comment", UpdatedAt: time.Now()},
				{ID: 11, Body: "shadow comment\n\n" + shadowMarker.String(), UpdatedAt: time.Now()},
			},
		},
	}
	sink := &fakeSink{kind: tGithub}

	e := newEngine()
	if err := e.syncOneWay(context.Background(), src,
		srcRepo(), sink,
		source.Repo{Owner: tOwner, Name: "dst"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(sink.commentMarkers) != 1 {
		t.Fatalf("expected 1 native comment, got %d", len(sink.commentMarkers))
	}
	if sink.commentMarkers[0].ID != 10 {
		t.Errorf("expected comment id=10, got %d", sink.commentMarkers[0].ID)
	}
}

func TestSyncRepo_SkipsReposWithoutAdmin(t *testing.T) {
	cases := []struct {
		name      string
		perms     *gitea.Permission
		wantCalls int
	}{
		{"no permissions reported", nil, 1},
		{"read only", &gitea.Permission{Pull: true}, 0},
		{"push without admin", &gitea.Permission{Pull: true, Push: true}, 0},
		{"admin", &gitea.Permission{Pull: true, Push: true, Admin: true}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fj := &fakeFJClient{}
			e := newEngine()
			e.fjClient = fj
			repo := &gitea.Repository{FullName: tOwner + "/" + tRepoSrc, Permissions: tc.perms}
			if err := e.syncRepo(context.Background(), repo, time.Now()); err != nil {
				t.Fatal(err)
			}
			if fj.pushMirrorCalls != tc.wantCalls {
				t.Errorf("ListPushMirrors calls = %d, want %d", fj.pushMirrorCalls, tc.wantCalls)
			}
		})
	}
}

func TestLocalOnlyFilter_KeepsLabelledNativeIssuesOnCanonical(t *testing.T) {
	// A shadow labelled local-only still routes its comments: the label only
	// keeps a canonical-native issue from being published.
	dstRepo := srcRepo()
	shadowMarker := marker.Marker{Type: tGithub, Host: tGHHost, Repo: dstRepo.Slug(), Kind: kindIssue, ID: 99}
	src := &fakeSource{
		kind: tForgejo, host: tFJHost,
		issues: []source.Issue{
			{Number: 1, Body: "public", UpdatedAt: time.Now()},
			{Number: 2, Body: "private", Labels: []string{"bug", localOnlyLabel}, UpdatedAt: time.Now()},
			{Number: 3, Body: "private too", Labels: []string{"Local-Only"}, UpdatedAt: time.Now()},
			{Number: 4, Body: "imported\n\n" + shadowMarker.String(), Labels: []string{localOnlyLabel}, UpdatedAt: time.Now()},
		},
		comments: map[int64][]source.Comment{
			2: {{ID: 20, Body: "stays here", UpdatedAt: time.Now()}},
			4: {{ID: 40, Body: "reply to the shadow", UpdatedAt: time.Now()}},
		},
	}
	sink := &fakeSink{kind: tGithub}
	e := newEngine()

	if err := e.syncOneWay(context.Background(), localOnlyFilter{src},
		forkRepo(), sink, dstRepo, time.Now()); err != nil {
		t.Fatal(err)
	}

	if len(sink.issueMarkers) != 1 || sink.issueMarkers[0].ID != 1 {
		t.Errorf("expected only issue 1 to be published, got %+v", sink.issueMarkers)
	}
	if len(sink.commentMarkers) != 1 || sink.commentMarkers[0].ID != 40 {
		t.Errorf("expected only the shadow's comment to flow, got %+v", sink.commentMarkers)
	}
	if len(src.issues) != 4 {
		t.Errorf("filter must not modify the provider's slice, got %d issues", len(src.issues))
	}
}

func TestSyncOneWay_ItemFailureFailsTheFlow(t *testing.T) {
	// One issue fails: the rest still sync, but the flow reports failure so its
	// resume point stays put and the next tick retries.
	src := &fakeSource{
		kind: tForgejo, host: tFJHost,
		issues: []source.Issue{
			{Number: 1, Body: "ok", UpdatedAt: time.Now()},
			{Number: 2, Body: "fails", UpdatedAt: time.Now()},
			{Number: 3, Body: "ok too", UpdatedAt: time.Now()},
		},
	}
	sink := &fakeSink{kind: tGithub, failIssue: 2}
	e := newEngine()
	err := e.syncOneWay(context.Background(), src, forkRepo(), sink, srcRepo(), time.Now())
	if err == nil {
		t.Fatal("expected the flow to report the failed item")
	}
	if len(sink.issueMarkers) != 2 {
		t.Errorf("expected the other 2 issues to sync, got %d", len(sink.issueMarkers))
	}
}

func TestSyncOneWay_CountsItems(t *testing.T) {
	src := &fakeSource{
		kind: tForgejo, host: tFJHost,
		issues: []source.Issue{
			{Number: 1, Body: "ok", UpdatedAt: time.Now()},
			{Number: 2, Body: "fails", UpdatedAt: time.Now()},
		},
		comments: map[int64][]source.Comment{
			1: {{ID: 10, Body: "a reply", UpdatedAt: time.Now()}},
		},
	}
	dst := &fakeSink{kind: tGithub, failIssue: 2}
	count := func(kind, result string) float64 {
		return testutil.ToFloat64(metrics.Items.WithLabelValues(kind, tForgejo, tGithub, result))
	}
	issuesOK, issuesErr, commentsOK := count(kindIssue, "ok"), count(kindIssue, "error"), count(kindComment, "ok")

	_ = newEngine().syncOneWay(context.Background(), src, forkRepo(), dst, srcRepo(), time.Now())

	if got := count(kindIssue, "ok") - issuesOK; got != 1 {
		t.Errorf("issues ok = %v, want 1", got)
	}
	if got := count(kindIssue, "error") - issuesErr; got != 1 {
		t.Errorf("issues error = %v, want 1", got)
	}
	if got := count(kindComment, "ok") - commentsOK; got != 1 {
		t.Errorf("comments ok = %v, want 1", got)
	}
}

func TestTick_RecordsResult(t *testing.T) {
	e := newEngine()
	e.cfg = &config.Config{}
	fj := &fakeFJClient{}
	e.fjClient = fj
	ok, failed := testutil.ToFloat64(metrics.Ticks.WithLabelValues("ok")), testutil.ToFloat64(metrics.Ticks.WithLabelValues("error"))

	if err := e.tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metrics.LastSuccess); time.Since(time.Unix(int64(got), 0)) > 5*time.Second {
		t.Errorf("last success = %v, want about now", got)
	}
	fj.searchErr = errors.New("forgejo is down")
	if err := e.tick(context.Background(), time.Now()); err == nil {
		t.Fatal("expected the search error")
	}

	if got := testutil.ToFloat64(metrics.Ticks.WithLabelValues("ok")) - ok; got != 1 {
		t.Errorf("ok ticks = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.Ticks.WithLabelValues("error")) - failed; got != 1 {
		t.Errorf("failed ticks = %v, want 1", got)
	}
}

func TestRunFlow_ResumesFromLastSuccess(t *testing.T) {
	f := flow{repo: tOwner + "/" + tRepoSrc, mirror: "github.com/" + tOwner + "/" + tRepoFork, direction: directionInbound}
	okBefore := testutil.ToFloat64(metrics.FlowRuns.WithLabelValues(f.repo, f.mirror, f.direction, "ok"))
	errBefore := testutil.ToFloat64(metrics.FlowRuns.WithLabelValues(f.repo, f.mirror, f.direction, "error"))
	e := newEngine()
	e.cfg = &config.Config{PollInterval: 5 * time.Minute, InitialBackfill: 24 * time.Hour}
	e.initialSince = time.Now().Add(-e.cfg.InitialBackfill)

	var got time.Time
	record := func(since time.Time) error { got = since; return nil }
	fail := func(since time.Time) error { got = since; return errors.New("github is down") }
	near := func(a, b time.Time) bool { return a.Sub(b).Abs() < time.Second }

	// Never succeeded: resume from the first tick's look-back.
	if err := e.runFlow(f, time.Now().Add(-e.cfg.Window()), record); err != nil {
		t.Fatal(err)
	}
	if !near(got, e.initialSince) {
		t.Errorf("first run since = %v, want the initial look-back %v", got, e.initialSince)
	}

	// Healthy: the usual window, nothing wider.
	window := time.Now().Add(-e.cfg.Window())
	if err := e.runFlow(f, window, record); err != nil {
		t.Fatal(err)
	}
	if !near(got, window) {
		t.Errorf("healthy run since = %v, want the window %v", got, window)
	}

	// Last success three hours ago: failed runs keep reaching back to it.
	lastOK := time.Now().Add(-3 * time.Hour)
	e.lastSynced[f.key()] = lastOK
	for range 2 {
		if err := e.runFlow(f, time.Now().Add(-e.cfg.Window()), fail); err == nil {
			t.Fatal("expected the failure to be returned")
		}
		if want := lastOK.Add(-e.cfg.PollInterval); !near(got, want) {
			t.Errorf("failed run since = %v, want %v", got, want)
		}
	}
	if !e.lastSynced[f.key()].Equal(lastOK) {
		t.Errorf("a failed run must not move the resume point")
	}

	// Older than InitialBackfill: capped, like a restart.
	e.lastSynced[f.key()] = time.Now().Add(-72 * time.Hour)
	if err := e.runFlow(f, time.Now().Add(-e.cfg.Window()), record); err != nil {
		t.Fatal(err)
	}
	if want := time.Now().Add(-e.cfg.InitialBackfill); !near(got, want) {
		t.Errorf("capped since = %v, want %v", got, want)
	}
	if time.Since(e.lastSynced[f.key()]) > time.Second {
		t.Errorf("a successful run must become the resume point")
	}

	// 3 successful runs and 2 failed ones, each counted once.
	if got := testutil.ToFloat64(metrics.FlowRuns.WithLabelValues(f.repo, f.mirror, f.direction, "ok")) - okBefore; got != 3 {
		t.Errorf("ok runs counted = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.FlowRuns.WithLabelValues(f.repo, f.mirror, f.direction, "error")) - errBefore; got != 2 {
		t.Errorf("failed runs counted = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.FlowLastSuccess.WithLabelValues(f.repo, f.mirror, f.direction)); int64(got) != e.lastSynced[f.key()].Unix() {
		t.Errorf("last-success gauge = %v, want the resume point %d", got, e.lastSynced[f.key()].Unix())
	}
}

// fakeUpstream implements upstreamSource.
type fakeUpstream struct {
	parent      source.Repo
	isFork      bool
	prs         *fakeSource
	author      string
	parentCalls int
}

func (f *fakeUpstream) Parent(context.Context, source.Repo) (source.Repo, bool, error) {
	f.parentCalls++
	return f.parent, f.isFork, nil
}

func (f *fakeUpstream) UpstreamPullRequests(author string) source.Provider {
	f.author = author
	return f.prs
}

func TestSyncUpstreamPRs(t *testing.T) {
	canonical := srcRepo()
	target := forkRepo()
	parent := source.Repo{Owner: "up", Name: "parent"}

	cases := []struct {
		name        string
		host        string
		isFork      bool
		mirrored    map[string]bool
		wantUpserts int
	}{
		{"fork on github", githubHost, true, nil, 1},
		{"not a fork", githubHost, false, nil, 0},
		{"non-github host skipped", tFJHost, true, nil, 0},
		// Flow A already imports the parent's PRs; don't fight it over shadows.
		{"parent is also a mirror", githubHost, true, map[string]bool{"github.com/up/parent": true}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeUpstream{
				parent: parent, isFork: tc.isFork,
				prs: &fakeSource{kind: tGithub, host: tGHHost, issues: []source.Issue{
					{Number: 4, Title: "[upstream PR #4] add postgres", State: stateOpen, UpdatedAt: time.Now()},
				}},
			}
			cs := &fakeCanonicalSink{}
			e := newEngine()
			e.cfg = &config.Config{
				PollInterval: 5 * time.Minute, InitialBackfill: time.Hour,
				Targets: config.Targets{GitHub: config.GitHubTarget{Token: "t"}},
			}
			e.upstream = up
			e.canonicalSink = cs

			if err := e.syncUpstreamPRs(context.Background(), canonical, tc.host, target, time.Now(), tc.mirrored); err != nil {
				t.Fatal(err)
			}
			if len(cs.issueMarkers) != tc.wantUpserts {
				t.Fatalf("upserts = %d, want %d", len(cs.issueMarkers), tc.wantUpserts)
			}
			if tc.wantUpserts == 0 {
				return
			}
			if up.author != tOwner {
				t.Errorf("PRs read for author %q, want the fork owner %q", up.author, tOwner)
			}
			want := marker.Marker{Type: tGithub, Host: tGHHost, Repo: parent.Slug(), Kind: kindIssue, ID: 4}
			if cs.issueMarkers[0] != want {
				t.Errorf("marker: got %+v want %+v", cs.issueMarkers[0], want)
			}
		})
	}
}

func TestUpstreamPRShadow_NeverFlowsOut(t *testing.T) {
	// The shadow and a reply to it sit in canonical. Flow B to the fork must
	// treat the shadow as foreign: nothing reaches the fork, or the parent.
	shadow := marker.Marker{Type: tGithub, Host: tGHHost, Repo: tUpstream, Kind: kindIssue, ID: 4}
	src := &fakeSource{
		kind: tForgejo, host: tFJHost,
		issues: []source.Issue{
			{Number: 9, Title: "[upstream PR #4] add postgres", Body: "x\n\n" + shadow.String(), UpdatedAt: time.Now()},
		},
		comments: map[int64][]source.Comment{
			9: {{ID: 90, Body: "note to self", UpdatedAt: time.Now()}},
		},
	}
	sink := &fakeSink{kind: tGithub}
	e := newEngine()
	if err := e.syncOneWay(context.Background(), localOnlyFilter{src},
		srcRepo(), sink, forkRepo(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(sink.issueMarkers) != 0 || len(sink.commentMarkers) != 0 {
		t.Errorf("expected nothing written out, got issues=%d comments=%d",
			len(sink.issueMarkers), len(sink.commentMarkers))
	}
}

func TestSyncOneWay_MarkersCarrySourceIdentity(t *testing.T) {
	src := &fakeSource{
		kind: tForgejo, host: tFJHost,
		issues: []source.Issue{{Number: 5, Body: "x", UpdatedAt: time.Now()}},
	}
	sink := &fakeSink{kind: tGithub}
	e := newEngine()
	if err := e.syncOneWay(context.Background(), src,
		source.Repo{Owner: tOwner, Name: "proj"}, sink,
		source.Repo{Owner: tOwner, Name: "proj"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := sink.issueMarkers[0]
	want := marker.Marker{Type: tForgejo, Host: tFJHost, Repo: "me/proj", Kind: kindIssue, ID: 5}
	if got != want {
		t.Errorf("marker: got %+v want %+v", got, want)
	}
}
