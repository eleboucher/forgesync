// Package forgejoapi constructs a Gitea/Forgejo SDK client wired with our
// retryable HTTP transport, plus a small helper for git push URLs.
package forgejoapi

import (
	"net/url"
	"time"

	"code.gitea.io/sdk/gitea"
	"github.com/hashicorp/go-retryablehttp"
)

// Client wraps *gitea.Client and retains the base URL/token so we can build
// authenticated git push URLs (the SDK doesn't expose either).
type Client struct {
	*gitea.Client
	BaseURL string
	Token   string
}

// New returns a configured Forgejo SDK client. The Forgejo API is
// Gitea-compatible.
func New(baseURL, token string) (*Client, error) {
	rc := retryablehttp.NewClient()
	rc.RetryMax = 3
	rc.RetryWaitMin = 500 * time.Millisecond
	rc.RetryWaitMax = 30 * time.Second
	rc.Logger = nil
	rc.HTTPClient.Timeout = 30 * time.Second

	g, err := gitea.NewClient(baseURL,
		gitea.SetToken(token),
		gitea.SetHTTPClient(rc.StandardClient()),
	)
	if err != nil {
		return nil, err
	}
	return &Client{Client: g, BaseURL: baseURL, Token: token}, nil
}

// AuthGitURL returns the canonical git clone URL for a repo with basic-auth
// credentials embedded, suitable for `git push` / `git fetch`.
func (c *Client) AuthGitURL(owner, repo string) string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return c.BaseURL
	}
	u.Path = "/" + owner + "/" + repo + ".git"
	if c.Token != "" {
		u.User = url.UserPassword("oauth2", c.Token)
	}
	return u.String()
}
