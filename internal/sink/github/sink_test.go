package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v75/github"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/githubapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const (
	stateOpen      = "open"
	searchPath     = "/search/issues"
	itemsKey       = "items"
	testRepoOwner  = "octocat"
	testRepoName   = "hi"
	testAuthor     = "alice"
	testSourceHTML = "https://git.example.com/me/proj/issues/11"
)

func newSink(t *testing.T, h http.HandlerFunc) *Sink {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := githubapi.New("test-token", ts.URL)
	return New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testRepo() source.Repo { return source.Repo{Owner: testRepoOwner, Name: testRepoName} }

func testMarker() marker.Marker {
	return marker.Marker{Type: "forgejo", Host: "git.example.com", Repo: "me/proj", Kind: "issue", ID: 11}
}

func testIssue(state string, updated time.Time) source.Issue {
	return source.Issue{
		Number: 11, Title: "hi", Body: "body",
		State:     state,
		Author:    source.User{Login: testAuthor},
		HTMLURL:   testSourceHTML,
		CreatedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: updated,
	}
}

func TestUpsertIssue_CreateOpen(t *testing.T) {
	var creates, patches atomic.Int32
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == searchPath:
			_ = json.NewEncoder(w).Encode(map[string]any{itemsKey: []any{}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			creates.Add(1)
			_ = json.NewEncoder(w).Encode(&gh.Issue{Number: gh.Ptr(5)})
		case r.Method == http.MethodPatch:
			patches.Add(1)
			_ = json.NewEncoder(w).Encode(&gh.Issue{Number: gh.Ptr(5)})
		}
	})
	num, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateOpen, time.Now()), testMarker())
	if err != nil {
		t.Fatal(err)
	}
	if num != 5 {
		t.Errorf("got %d want 5", num)
	}
	if creates.Load() != 1 || patches.Load() != 0 {
		t.Errorf("creates=%d patches=%d (want 1,0)", creates.Load(), patches.Load())
	}
}

func TestUpsertIssue_CreateClosedDoesPatch(t *testing.T) {
	// GitHub's create endpoint always opens, so a closed source needs a follow-up PATCH.
	var creates, patches atomic.Int32
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == searchPath:
			_ = json.NewEncoder(w).Encode(map[string]any{itemsKey: []any{}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			creates.Add(1)
			_ = json.NewEncoder(w).Encode(&gh.Issue{Number: gh.Ptr(5)})
		case r.Method == http.MethodPatch:
			patches.Add(1)
			var req gh.IssueRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.State == nil || *req.State != stateClosed {
				t.Errorf("expected state=closed in PATCH, got %+v", req.State)
			}
			_ = json.NewEncoder(w).Encode(&gh.Issue{Number: gh.Ptr(5), State: gh.Ptr(stateClosed)})
		}
	})
	if _, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateClosed, time.Now()), testMarker()); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || patches.Load() != 1 {
		t.Errorf("creates=%d patches=%d (want 1,1)", creates.Load(), patches.Load())
	}
}

func TestUpsertIssue_PATCHReopens(t *testing.T) {
	// Shadow is closed, source is open → propagate the reopen.
	m := testMarker()
	now := time.Now()
	existing := &gh.Issue{
		Number:    gh.Ptr(5),
		Title:     gh.Ptr("old title"),
		Body:      gh.Ptr("old body\n\n" + m.String()),
		State:     gh.Ptr(stateClosed),
		UpdatedAt: &gh.Timestamp{Time: now.Add(-time.Hour)},
	}
	var sentState string
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == searchPath:
			_ = json.NewEncoder(w).Encode(map[string]any{itemsKey: []*gh.Issue{existing}})
		case r.Method == http.MethodPatch:
			var req gh.IssueRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.State != nil {
				sentState = *req.State
			}
			_ = json.NewEncoder(w).Encode(&gh.Issue{Number: gh.Ptr(5)})
		}
	})
	if _, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateOpen, now), m); err != nil {
		t.Fatal(err)
	}
	if sentState != stateOpen {
		t.Errorf("PATCH must send state=open on reopen, got %q", sentState)
	}
}

func TestUpsertIssue_PATCHDoesNotClose(t *testing.T) {
	// Shadow is open, source is closed → DO NOT propagate the close.
	m := testMarker()
	now := time.Now()
	existing := &gh.Issue{
		Number:    gh.Ptr(5),
		Title:     gh.Ptr("hi"),                   // matches src title
		Body:      gh.Ptr("old\n\n" + m.String()), // body differs to force PATCH
		State:     gh.Ptr(stateOpen),
		UpdatedAt: &gh.Timestamp{Time: now.Add(-time.Hour)},
	}
	var stateField *string
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == searchPath:
			_ = json.NewEncoder(w).Encode(map[string]any{itemsKey: []*gh.Issue{existing}})
		case r.Method == http.MethodPatch:
			var req gh.IssueRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			stateField = req.State
			_ = json.NewEncoder(w).Encode(&gh.Issue{Number: gh.Ptr(5)})
		}
	})
	if _, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateClosed, now), m); err != nil {
		t.Fatal(err)
	}
	if stateField != nil {
		t.Errorf("PATCH must NOT send state on close transition, got %q", *stateField)
	}
}

func TestRenderBody_TruncatesOverLimit(t *testing.T) {
	src := source.Issue{
		Author:    source.User{Login: testAuthor},
		HTMLURL:   testSourceHTML,
		Body:      strings.Repeat("a", 70000),
		CreatedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	body := renderIssueBody(src, testMarker())

	if got := len([]rune(body)); got >= 65536 {
		t.Fatalf("rendered body has %d runes, must stay under 65536", got)
	}
	if !strings.Contains(body, "truncated") {
		t.Errorf("expected truncation notice")
	}
	if !strings.Contains(body, testMarker().String()) {
		t.Errorf("marker must be preserved at the end")
	}
}

func TestRenderBody_NoTruncationWhenFits(t *testing.T) {
	src := source.Issue{
		Author:    source.User{Login: testAuthor},
		HTMLURL:   testSourceHTML,
		Body:      "short and sweet",
		CreatedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	}
	body := renderIssueBody(src, testMarker())
	if strings.Contains(body, "truncated") {
		t.Errorf("did not expect truncation notice for short body, got: %q", body)
	}
}

func TestUpsertIssue_SkipWhenShadowDrifted(t *testing.T) {
	m := testMarker()
	srcUpdate := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	shadow := &gh.Issue{
		Number:    gh.Ptr(5),
		Title:     gh.Ptr("user edited"),
		Body:      gh.Ptr("user edited\n\n" + m.String()),
		State:     gh.Ptr(stateOpen),
		UpdatedAt: &gh.Timestamp{Time: srcUpdate.Add(5 * time.Minute)},
	}
	var patches atomic.Int32
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == searchPath:
			_ = json.NewEncoder(w).Encode(map[string]any{itemsKey: []*gh.Issue{shadow}})
		case r.Method == http.MethodPatch:
			patches.Add(1)
		}
	})
	if _, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateOpen, srcUpdate), m); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 0 {
		t.Errorf("expected 0 PATCH on drift, got %d", patches.Load())
	}
}
