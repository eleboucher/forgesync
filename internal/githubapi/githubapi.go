// Package githubapi constructs a github.com SDK client wired with our
// retryable HTTP transport, auth, and a custom User-Agent.
package githubapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/hashicorp/go-retryablehttp"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/metrics"
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
	// Inside the retries, so every attempt is recorded, including the 429s the
	// retry client swallows once it gives up.
	rc.HTTPClient.Transport = rateLimitTransport{next: rc.HTTPClient.Transport}

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

// rateLimitTransport records the rate limit GitHub reports on every response,
// per resource (core, search, graphql), as a metric.
type rateLimitTransport struct {
	next http.RoundTripper
}

func (t rateLimitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	if remaining, perr := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); perr == nil {
		resource := resp.Header.Get("X-RateLimit-Resource")
		if resource == "" {
			resource = "core"
		}
		metrics.GitHubRateLimitRemaining.WithLabelValues(resource).Set(float64(remaining))
	}
	return resp, nil
}
