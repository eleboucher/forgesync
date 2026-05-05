package gitops

import (
	"strings"
	"testing"
)

const testRepoURL = "https://example.com/me/repo.git"

func TestAuthURL(t *testing.T) {
	got, err := AuthURL(testRepoURL, "oauth2", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://oauth2:secret-token@example.com/me/repo.git" //nolint:gosec // test fixture
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRedactStripsCredentials(t *testing.T) {
	authed := "https://oauth2:supersecret@example.com/me/repo.git" //nolint:gosec // test fixture
	out := "fatal: failed to push to " + authed + " bla"
	got := redact(out, authed)
	if strings.Contains(got, "supersecret") {
		t.Errorf("token leaked in redacted output: %q", got)
	}
}

func TestRedactNoOpForUnauthURL(t *testing.T) {
	out := "some output mentioning " + testRepoURL
	if got := redact(out, testRepoURL); got != out {
		t.Errorf("unauth URL should not be touched: %q", got)
	}
}
