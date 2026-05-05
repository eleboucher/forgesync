// Package marker defines the hidden HTML-comment forgesync embeds in every
// item it writes. Markers are how forgesync stays stateless: search the
// destination for the marker to find an existing shadow; presence of any
// marker on the *source* side identifies a shadow we wrote and must not loop.
package marker

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Marker uniquely identifies the source item a shadow points back at.
type Marker struct {
	Type string // "github" | "forgejo"
	Host string // e.g. "github.com", "git.erwanleboucher.dev"
	Repo string // "owner/name" on the source side
	Kind string // "issue" | "comment"
	ID   int64  // numeric id on the source side
}

func (m Marker) String() string {
	return fmt.Sprintf("<!-- forgesync:src=%s;host=%s;repo=%s;kind=%s;id=%d -->",
		m.Type, m.Host, m.Repo, m.Kind, m.ID)
}

// SearchToken returns the substring used in destination full-text search.
// Forgejo and GitHub both index issue bodies; the angle-bracket comment
// itself doesn't always tokenize cleanly, but this prefix does.
func (m Marker) SearchToken() string {
	return fmt.Sprintf("forgesync:src=%s;host=%s;repo=%s;kind=%s;id=%d",
		m.Type, m.Host, m.Repo, m.Kind, m.ID)
}

var markerRe = regexp.MustCompile(`<!--\s*forgesync:src=([^;]+);host=([^;]+);repo=([^;]+);kind=([^;]+);id=(\d+)\s*-->`)

// Parse returns the marker embedded in body, if any.
func Parse(body string) (Marker, bool) {
	m := markerRe.FindStringSubmatch(body)
	if m == nil {
		return Marker{}, false
	}
	id, err := strconv.ParseInt(m[5], 10, 64)
	if err != nil {
		return Marker{}, false
	}
	return Marker{
		Type: m[1],
		Host: m[2],
		Repo: m[3],
		Kind: m[4],
		ID:   id,
	}, true
}

// Has reports whether body contains any forgesync marker. Used to filter
// shadows out of the source-read path so we don't loop.
func Has(body string) bool {
	return strings.Contains(body, "forgesync:src=")
}

// WithMarker appends a marker to body, separated by a blank line.
func WithMarker(body string, m Marker) string {
	return body + "\n\n" + m.String()
}
