// Package gitops shells out to the system `git` to fetch a ref from one remote
// and push it to another. Used for promoting an inbound PR's head ref into the
// canonical Forgejo as a forgesync/pr-N branch.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

var ErrGitNotFound = errors.New("git binary not found in PATH")

// MirrorRef fetches srcRef from srcURL and (force-)pushes it to dstURL as
// dstBranch. Force-push because PR head refs get rewritten on rebase, and the
// forgesync/pr-* branch namespace is owned by forgesync.
func MirrorRef(ctx context.Context, srcURL, srcRef, dstURL, dstBranch string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return ErrGitNotFound
	}
	dir, err := os.MkdirTemp("", "forgesync-git-*")
	if err != nil {
		return fmt.Errorf("tmpdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	runs := [][]string{
		{"init", "--bare", "-q"},
		{"fetch", "--no-tags", "-q", srcURL, srcRef + ":refs/heads/__forgesync_tmp"},
		{"push", "-q", "--force", dstURL, "refs/heads/__forgesync_tmp:refs/heads/" + dstBranch},
	}
	for _, args := range runs {
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // controlled args
		cmd.Dir = dir
		// Fail fast on credential prompts; never block waiting for stdin.
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", args[0], err, redact(string(out), srcURL, dstURL))
		}
	}
	return nil
}

// AuthURL injects basic auth into a URL for use with git fetch/push.
func AuthURL(rawURL, username, password string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(username, password)
	return u.String(), nil
}

// redact replaces full URLs (which may contain user:pass) in text with a
// censored form so credentials don't leak into error logs.
func redact(s string, urls ...string) string {
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			continue
		}
		censored := *u
		censored.User = url.UserPassword("***", "***")
		s = strings.ReplaceAll(s, raw, censored.String())
	}
	return s
}
