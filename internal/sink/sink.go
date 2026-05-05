// Package sink defines the destination-writer interface. Forgejo and GitHub
// both implement this; the engine dispatches by host.
package sink

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"git.erwanleboucher.dev/eleboucher/forgesync/internal/marker"
	"git.erwanleboucher.dev/eleboucher/forgesync/internal/source"
)

// shadowDriftThreshold is how much later a shadow's updated_at can be vs its
// source before we treat it as "user-edited the shadow" and skip the PATCH
// to avoid silently overwriting their work.
const shadowDriftThreshold = 30 * time.Second

// Sink writes issues and comments to a destination forge, using markers for
// idempotent upsert.
type Sink interface {
	// Kind returns "github" or "forgejo".
	Kind() string

	// UpsertIssue creates or updates the issue. Returns the destination issue
	// number for use as a comment parent.
	UpsertIssue(ctx context.Context, dest source.Repo, src source.Issue, m marker.Marker) (int64, error)

	// UpsertComment creates or updates a comment under destIssueNumber.
	UpsertComment(ctx context.Context, dest source.Repo, destIssueNumber int64, src source.Comment, m marker.Marker) error
}

// ShadowDrifted reports whether the shadow has been modified noticeably after
// the source's last update — a sign a user edited the shadow directly.
func ShadowDrifted(shadowUpdated, sourceUpdated time.Time) bool {
	if shadowUpdated.IsZero() || sourceUpdated.IsZero() {
		return false
	}
	return shadowUpdated.Sub(sourceUpdated) > shadowDriftThreshold
}

// PropagateReopen returns "open" iff the source is open and the destination is
// closed — a reopen we should propagate. Returns nil otherwise: closes never
// flow, and equal states need no PATCH.
func PropagateReopen(existingState, srcState string) *string {
	if existingState == "closed" && srcState == "open" {
		s := "open"
		return &s
	}
	return nil
}

// RenderBody composes the destination body for an issue or comment: the
// attribution header, the (possibly truncated) source body, and the marker.
// bodyLimit caps the final character count to fit the destination's limit.
func RenderBody(author source.User, sourceURL string, at time.Time, body string, m marker.Marker, bodyLimit int) string {
	attribution := authorAttribution(author, sourceURL, at)
	markerStr := m.String()
	body = strings.TrimSpace(body)

	body, truncated := truncateForLimit(body, bodyLimit, attribution, markerStr)

	var b strings.Builder
	b.WriteString(attribution)
	if body != "" {
		b.WriteString("\n\n")
		b.WriteString(body)
	}
	if truncated {
		fmt.Fprintf(&b, "\n\n_… body truncated; view full source at %s_", sourceURL)
	}
	b.WriteString("\n\n")
	b.WriteString(markerStr)
	return b.String()
}

// truncateForLimit shrinks body so the final composed body (attribution +
// body + truncation notice + marker) fits under limit. Truncates on rune
// boundaries so we don't split a multi-byte character.
func truncateForLimit(body string, limit int, attribution, markerStr string) (string, bool) {
	const noticeReserve = 100
	overhead := utf8.RuneCountInString(attribution) + utf8.RuneCountInString(markerStr) + noticeReserve + 8
	budget := max(limit-overhead, 0)
	if utf8.RuneCountInString(body) <= budget {
		return body, false
	}
	runes := []rune(body)
	return string(runes[:budget]), true
}

func authorAttribution(u source.User, sourceURL string, at time.Time) string {
	who := u.Login
	if who == "" {
		who = "unknown"
	}
	return fmt.Sprintf("> _Originally by **%s** on %s — [view on source](%s)_",
		who, at.UTC().Format("2006-01-02 15:04 UTC"), sourceURL)
}
