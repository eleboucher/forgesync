// Package githubapi constructs a github.com SDK client wired with our
// retryable HTTP transport, auth, and a custom User-Agent.
package githubapi

import (
	"net/url"
	"time"

	"github.com/google/go-github/v75/github"
	"github.com/hashicorp/go-retryablehttp"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/version"
)

// New returns a configured *github.Client. baseURL is optional; pass an
// httptest URL for tests, "" for the public API.
func New(token, baseURL string) *github.Client {
	rc := retryablehttp.NewClient()
	rc.RetryMax = 3
	rc.RetryWaitMin = 500 * time.Millisecond
	rc.RetryWaitMax = 30 * time.Second
	rc.Logger = nil
	rc.HTTPClient.Timeout = 30 * time.Second

	gh := github.NewClient(rc.StandardClient())
	if token != "" {
		gh = gh.WithAuthToken(token)
	}
	gh.UserAgent = version.UserAgent()
	if baseURL != "" {
		if u, err := url.Parse(baseURL + "/"); err == nil {
			gh.BaseURL = u
		}
	}
	return gh
}
