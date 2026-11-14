package githubapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/metrics"
)

func TestRecordsRateLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Resource", "search")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	c, err := New("test-token", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Repositories.Get(context.Background(), "me", "fork"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metrics.GitHubRateLimitRemaining.WithLabelValues("search")); got != 4321 {
		t.Errorf("rate limit remaining = %v, want 4321", got)
	}
}
