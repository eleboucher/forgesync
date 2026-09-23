// Package sink defines the destination-writer interface. Forgejo and GitHub
// both implement this; the engine dispatches by host.
package sink

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
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

// PropagateState returns the state the shadow should be set to so it matches
// its source ("open" or "closed"), or nil if they already agree. Both opens and
// closes flow. This is loop-safe because the engine only ever upserts a native
// item into its shadow (routeIssue never treats a shadow as a source), so the
// authoritative side's state is never overwritten by its own mirror.
func PropagateState(existingState, srcState string) *string {
	if existingState == srcState {
		return nil
	}
	if srcState != "open" && srcState != "closed" {
		return nil
	}
	s := srcState
	return &s
}

// LabelColor is the color given to labels a sink has to create on the
// destination, since the source only tells us the label's name.
const LabelColor = "ededed"

var syncedLabelsRe = regexp.MustCompile(`<!-- forgesync:labels=(\S*) -->`)

// WithSyncedLabels appends a hidden note listing the labels forgesync set on a
// shadow, so a later sync can tell them apart from labels people added on the
// destination. Nothing is appended when there are none.
func WithSyncedLabels(body string, labels []string) string {
	labels = uniqueLabels(labels)
	if len(labels) == 0 {
		return body
	}
	escaped := make([]string, len(labels))
	for i, l := range labels {
		escaped[i] = url.QueryEscape(l)
	}
	return body + "\n<!-- forgesync:labels=" + strings.Join(escaped, ",") + " -->"
}

// SyncedLabels returns the labels recorded in body by WithSyncedLabels.
func SyncedLabels(body string) []string {
	m := syncedLabelsRe.FindStringSubmatch(body)
	if m == nil || m[1] == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(m[1], ",") {
		if l, err := url.QueryUnescape(part); err == nil && l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ShadowLabels returns the labels a shadow should carry: its current labels,
// minus the ones forgesync set earlier that the source has since dropped, plus
// the source's. Labels people added on the destination are left alone.
func ShadowLabels(current, previouslySynced, src []string) []string {
	src = uniqueLabels(src)
	wanted := foldSet(src)
	dropped := foldSet(previouslySynced)
	var out []string
	seen := map[string]bool{}
	for _, l := range slices.Concat(current, src) {
		k := strings.ToLower(l)
		if seen[k] || (dropped[k] && !wanted[k]) {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return out
}

// LabelsEqual reports whether a and b hold the same label names, ignoring
// order, case and duplicates (GitHub treats label names case-insensitively).
func LabelsEqual(a, b []string) bool {
	sa, sb := foldSet(a), foldSet(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

func foldSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, l := range labels {
		set[strings.ToLower(l)] = true
	}
	return set
}

// uniqueLabels drops case-insensitive duplicates, keeping the first spelling.
func uniqueLabels(labels []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range labels {
		if k := strings.ToLower(l); !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	return out
}

// RenderBody composes the destination body for an issue or comment: the
// attribution header, the (possibly truncated) source body, and the marker.
// bodyLimit caps the final character count to fit the destination's limit.
func RenderBody(author source.User, sourceURL string, at time.Time, body string, m marker.Marker, bodyLimit int) string {
	attribution := authorAttribution(author, sourceURL, at)
	markerStr := m.String()
	body = strings.TrimSpace(body)
	notice := fmt.Sprintf("\n\n_… body truncated; view full source at %s_", sourceURL)

	body, truncated := truncateForLimit(body, bodyLimit, attribution, markerStr, notice)

	var b strings.Builder
	b.WriteString(attribution)
	if body != "" {
		b.WriteString("\n\n")
		b.WriteString(body)
	}
	if truncated {
		b.WriteString(notice)
	}
	b.WriteString("\n\n")
	b.WriteString(markerStr)
	return b.String()
}

// truncateForLimit shrinks body so the final composed body (attribution +
// body + truncation notice + marker) fits under limit. Truncates on rune
// boundaries so we don't split a multi-byte character. The reserve accounts for
// the actual notice (which embeds the source URL) plus the two "\n\n"
// separators the layout adds around the body and before the marker.
func truncateForLimit(body string, limit int, attribution, markerStr, notice string) (string, bool) {
	const separators = 4 // "\n\n" before body + "\n\n" before marker
	overhead := utf8.RuneCountInString(attribution) +
		utf8.RuneCountInString(markerStr) +
		utf8.RuneCountInString(notice) + separators
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
