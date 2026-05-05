package syncloop

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
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
}

func (f *fakeSink) Kind() string { return f.kind }
func (f *fakeSink) UpsertIssue(_ context.Context, _ source.Repo, _ source.Issue, m marker.Marker) (int64, error) {
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
