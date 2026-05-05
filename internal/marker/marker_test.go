package marker

import "testing"

func TestRoundTrip(t *testing.T) {
	m := Marker{Type: "github", Host: "github.com", Repo: "octocat/hello", Kind: "issue", ID: 42}
	body := WithMarker("hello world", m)

	got, ok := Parse(body)
	if !ok {
		t.Fatalf("expected to parse marker from %q", body)
	}
	if got != m {
		t.Fatalf("marker mismatch: got %+v want %+v", got, m)
	}
}

func TestParseMissing(t *testing.T) {
	if _, ok := Parse("just some body text"); ok {
		t.Fatalf("did not expect to parse marker")
	}
}

func TestSearchToken(t *testing.T) {
	m := Marker{Type: "github", Host: "github.com", Repo: "octocat/hello", Kind: "comment", ID: 7}
	want := "forgesync:src=github;host=github.com;repo=octocat/hello;kind=comment;id=7"
	if got := m.SearchToken(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHas(t *testing.T) {
	cases := map[string]bool{
		"plain body": false,
		"<!-- forgesync:src=github;host=x;repo=y;kind=issue;id=1 -->":     true,
		"forgesync:src=foo body without comment wrapping is also flagged": true,
		"":     false,
		"\n\n": false,
	}
	for body, want := range cases {
		if got := Has(body); got != want {
			t.Errorf("Has(%q): got %v want %v", body, got, want)
		}
	}
}
