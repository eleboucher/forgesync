// Package githubapi constructs a github.com SDK client wired with our
// retryable HTTP transport, auth, and a custom User-Agent.
package githubapi

import (
	"time"

	"github.com/google/go-github/v91/github"
	"github.com/hashicorp/go-retryablehttp"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/version"
)

// New returns a configured *github.Client. baseURL is optional; pass an
// httptest URL for tests, "" for the public API.
func New(token, baseURL string) (*github.Client, error) {
	rc := retryablehttp.NewClient()
	rc.RetryMax = 3
	rc.RetryWaitMin = 500 * time.Millisecond
	rc.RetryWaitMax = 30 * time.Second
	rc.Logger = nil
	rc.HTTPClient.Timeout = 30 * time.Second

	opts := []github.ClientOptionsFunc{
		github.WithHTTPClient(rc.StandardClient()),
		github.WithUserAgent(version.UserAgent()),
	}
	if token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	if baseURL != "" {
		opts = append(opts, github.WithURLs(&baseURL, nil))
	}
	return github.NewClient(opts...)
}
