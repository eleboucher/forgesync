package forgejo

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

	"code.gitea.io/sdk/gitea"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/forgejoapi"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const (
	testRepoOwner = "me"
	testRepoName  = "fork"
)

func newSink(t *testing.T, h http.HandlerFunc) *Sink {
	t.Helper()
	// Gitea SDK probes /api/v1/version on construction. Wrap the handler so
	// that endpoint always returns a stub.
	wrapped := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v1/version") {
			// Stub a recent Gitea version so the SDK's min-version probe passes.
			_, _ = w.Write([]byte(`{"version":"1.21.0"}`))
			return
		}
		h(w, r)
	}
	ts := httptest.NewServer(http.HandlerFunc(wrapped))
	t.Cleanup(ts.Close)
	c, err := forgejoapi.New(ts.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	return New(c, "forgesync-bot", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testRepo() source.Repo { return source.Repo{Owner: testRepoOwner, Name: testRepoName} }

const (
	stateOpen   = gitea.StateOpen
	stateClosed = gitea.StateClosed
)

func testIssue(state gitea.StateType, updated time.Time) source.Issue {
	return source.Issue{
		Number:    42,
		Title:     "hello",
		Body:      "world",
		State:     string(state),
		Author:    source.User{Login: "octocat"},
		HTMLURL:   "https://github.com/octocat/hi/issues/42",
		CreatedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: updated,
	}
}

func testMarker() marker.Marker {
	return marker.Marker{Type: "github", Host: "github.com", Repo: "octocat/hi", Kind: "issue", ID: 42}
}

func TestUpsertIssue_Create(t *testing.T) {
	var createCalls atomic.Int32
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			createCalls.Add(1)
			var req gitea.CreateIssueOption
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Closed {
				t.Errorf("expected open create, got Closed=true")
			}
			_ = json.NewEncoder(w).Encode(&gitea.Issue{Index: 7, Title: req.Title, Body: req.Body})
		default:
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
	})

	num, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateOpen, time.Now()), testMarker())
	if err != nil {
		t.Fatal(err)
	}
	if num != 7 {
		t.Errorf("got num=%d want 7", num)
	}
	if createCalls.Load() != 1 {
		t.Errorf("expected 1 create call, got %d", createCalls.Load())
	}
}

func TestUpsertIssue_CreateClosed(t *testing.T) {
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost:
			var req gitea.CreateIssueOption
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !req.Closed {
				t.Errorf("expected Closed=true on create for closed source")
			}
			_ = json.NewEncoder(w).Encode(&gitea.Issue{Index: 9, State: stateClosed})
		case r.Method == http.MethodPatch:
			t.Errorf("did not expect a PATCH after closed-create; got %s", r.URL.Path)
		}
	})
	if _, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateClosed, time.Now()), testMarker()); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertIssue_EditWhenChanged(t *testing.T) {
	m := testMarker()
	now := time.Now()
	existing := &gitea.Issue{
		Index:   7,
		Title:   "old title",
		Body:    "old body\n\n" + m.String(),
		State:   stateOpen,
		Updated: now.Add(-1 * time.Hour),
	}
	var patchCalls atomic.Int32

	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_ = json.NewEncoder(w).Encode([]*gitea.Issue{existing})
		case r.Method == http.MethodPatch:
			patchCalls.Add(1)
			var req gitea.EditIssueOption
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Title != "hello" {
				t.Errorf("title not patched: %q", req.Title)
			}
			_ = json.NewEncoder(w).Encode(&gitea.Issue{Index: 7})
		}
	})
	if _, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateOpen, now), m); err != nil {
		t.Fatal(err)
	}
	if patchCalls.Load() != 1 {
		t.Errorf("expected 1 PATCH, got %d", patchCalls.Load())
	}
}

func TestUpsertIssue_SkipWhenEqual(t *testing.T) {
	m := testMarker()
	src := testIssue(stateOpen, time.Now())
	expectedBody := renderIssueBody(src, m)

	existing := &gitea.Issue{
		Index:   7,
		Title:   src.Title,
		Body:    expectedBody,
		State:   gitea.StateType(src.State),
		Updated: src.UpdatedAt,
	}
	var patchCalls atomic.Int32

	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalls.Add(1)
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues") {
			_ = json.NewEncoder(w).Encode([]*gitea.Issue{existing})
			return
		}
	})
	if _, err := sink.UpsertIssue(context.Background(), testRepo(), src, m); err != nil {
		t.Fatal(err)
	}
	if patchCalls.Load() != 0 {
		t.Errorf("expected 0 PATCHes when body equal, got %d", patchCalls.Load())
	}
}

func TestUpsertIssue_SkipWhenShadowDrifted(t *testing.T) {
	m := testMarker()
	srcUpdate := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	shadowUpdate := srcUpdate.Add(2 * time.Minute)

	existing := &gitea.Issue{
		Index:   7,
		Title:   "user-edited title",
		Body:    "user-edited body\n\n" + m.String(),
		State:   stateOpen,
		Updated: shadowUpdate,
	}
	var patchCalls atomic.Int32

	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalls.Add(1)
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues") {
			_ = json.NewEncoder(w).Encode([]*gitea.Issue{existing})
			return
		}
	})

	num, err := sink.UpsertIssue(context.Background(), testRepo(), testIssue(stateOpen, srcUpdate), m)
	if err != nil {
		t.Fatal(err)
	}
	if num != existing.Index {
		t.Errorf("expected to return existing num %d, got %d", existing.Index, num)
	}
	if patchCalls.Load() != 0 {
		t.Errorf("expected 0 PATCHes when shadow drifted, got %d", patchCalls.Load())
	}
}

func TestUpsertComment_FindsMarkerOnSecondPage(t *testing.T) {
	// Regression: findCommentByMarker must paginate. Otherwise an issue with
	// many comments would never see the existing shadow, and we'd POST a new
	// duplicate every tick.
	m := testMarker()
	const pageSize = 50
	page1 := make([]*gitea.Comment, pageSize)
	for i := range page1 {
		page1[i] = &gitea.Comment{ID: int64(i + 1), Body: "noise"}
	}
	page2 := []*gitea.Comment{
		{ID: 999, Body: "the existing shadow\n\n" + m.String()},
	}

	var posts atomic.Int32
	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/comments"):
			switch r.URL.Query().Get("page") {
			case "1", "":
				_ = json.NewEncoder(w).Encode(page1)
			case "2":
				_ = json.NewEncoder(w).Encode(page2)
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments"):
			posts.Add(1)
			_ = json.NewEncoder(w).Encode(&gitea.Comment{ID: 1})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(&gitea.Comment{ID: 999})
		}
	})

	c := source.Comment{
		ID: 999, IssueNumber: 7, Body: "the existing shadow",
		Author: source.User{Login: "alice"}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := sink.UpsertComment(context.Background(), testRepo(), 7, c, m); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 {
		t.Errorf("expected 0 POSTs (shadow found on page 2), got %d", posts.Load())
	}
}

func TestUpsertComment_Create(t *testing.T) {
	m := testMarker()
	var creates atomic.Int32

	sink := newSink(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments"):
			creates.Add(1)
			_ = json.NewEncoder(w).Encode(&gitea.Comment{ID: 1})
		}
	})

	c := source.Comment{
		ID: 99, IssueNumber: 7, Body: "lgtm",
		Author: source.User{Login: "alice"}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := sink.UpsertComment(context.Background(), testRepo(), 7, c, m); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 {
		t.Errorf("expected 1 create, got %d", creates.Load())
	}
}

func TestRenderBody_EmptySourceBody(t *testing.T) {
	src := source.Issue{
		Author:    source.User{Login: "ghost"},
		HTMLURL:   "https://example.com/x",
		Body:      "   ",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	body := renderIssueBody(src, testMarker())
	if strings.Contains(body, "   ") {
		t.Errorf("whitespace-only body should be omitted, got %q", body)
	}
	if !strings.Contains(body, "Originally by **ghost**") {
		t.Errorf("attribution missing: %q", body)
	}
	if !strings.Contains(body, testMarker().String()) {
		t.Errorf("marker missing")
	}
}
