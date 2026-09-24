package metrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResult(t *testing.T) {
	if Result(nil) != "ok" || Result(errors.New("boom")) != "error" {
		t.Error("Result must map nil to ok and an error to error")
	}
}

func TestHandlerServesForgesyncMetrics(t *testing.T) {
	Ticks.WithLabelValues("ok").Inc()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{"forgesync_ticks_total", "forgesync_last_success_timestamp_seconds", "go_goroutines"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
}
