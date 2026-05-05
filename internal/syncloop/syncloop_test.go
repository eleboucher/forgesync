package syncloop

import (
	"testing"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

const (
	testForgejo = "forgejo"
	testHost    = "git.erwanleboucher.dev"
)

func TestParseRemoteRepo(t *testing.T) {
	const (
		gh    = githubHost
		owner = "octocat"
	)
	cases := []struct {
		in          string
		host        string
		owner, name string
		wantErr     bool
	}{
		{"https://" + gh + "/" + owner + "/hello-world.git", gh, owner, "hello-world", false},
		{"https://" + gh + "/" + owner + "/hello-world", gh, owner, "hello-world", false},
		{"https://codeberg.org/" + testForgejo + "/" + testForgejo + ".git", "codeberg.org", testForgejo, testForgejo, false},
		{"https://example.com/just-one", "", "", "", true},
		{"::not a url::", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			h, r, err := parseRemoteRepo(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h != tc.host || r.Owner != tc.owner || r.Name != tc.name {
				t.Errorf("got (%q,%q,%q) want (%q,%q,%q)", h, r.Owner, r.Name, tc.host, tc.owner, tc.name)
			}
		})
	}
}

func TestNormalizeHostForEnv(t *testing.T) {
	cases := map[string]string{
		"codeberg.org":    "CODEBERG_ORG",
		"git.example.com": "GIT_EXAMPLE_COM",
		"GIT.EXAMPLE.COM": "GIT_EXAMPLE_COM",
		"example-1.io":    "EXAMPLE_1_IO",
	}
	for in, want := range cases {
		if got := normalizeHostForEnv(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestSplitFullName(t *testing.T) {
	cases := []struct{ in, owner, name string }{
		{"octocat/hello", "octocat", "hello"},
		{"single", "single", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		o, n := splitFullName(tc.in)
		if o != tc.owner || n != tc.name {
			t.Errorf("%q: got (%q,%q) want (%q,%q)", tc.in, o, n, tc.owner, tc.name)
		}
	}
}

func TestHasSyncCommand(t *testing.T) {
	const (
		bot   = "forgesync-bot"
		human = "alice"
	)
	cases := []struct {
		name     string
		comments []source.Comment
		want     bool
	}{
		{"plain", []source.Comment{{Author: source.User{Login: human}, Body: "/sync"}}, true},
		{"whitespace", []source.Comment{{Author: source.User{Login: human}, Body: "  /sync\n"}}, true},
		{"with args", []source.Comment{{Author: source.User{Login: human}, Body: "/sync now"}}, true},
		{"from bot ignored", []source.Comment{{Author: source.User{Login: bot}, Body: "/sync"}}, false},
		{"no command", []source.Comment{{Author: source.User{Login: human}, Body: "lgtm"}}, false},
		{"prefix mismatch", []source.Comment{{Author: source.User{Login: human}, Body: "/synced"}}, false},
		{"empty list", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSyncCommand(tc.comments, bot); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestHostFromURL(t *testing.T) {
	cases := map[string]string{
		"https://" + testHost:                testHost,
		"https://Forgejo.example.com:3000/x": "forgejo.example.com:3000",
		"":                                   "",
	}
	for in, want := range cases {
		if got := hostFromURL(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
