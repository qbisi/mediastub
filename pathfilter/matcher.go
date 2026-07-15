// Package pathfilter implements the media include matching shared by mount and sync.
package pathfilter

import (
	"fmt"
	"path"
	"strings"
)

const DefaultIncludes = "*.mkv,*.mka,*.mks,*.webm,*.mp4,*.m4v,*.mov"

// Matcher matches slash-separated paths using path.Match semantics.
type Matcher struct {
	patterns []string
}

// New validates patterns and constructs a Matcher.
func New(patterns []string) (*Matcher, error) {
	copyPatterns := append([]string(nil), patterns...)
	for _, pattern := range copyPatterns {
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid include pattern %q: %w", pattern, err)
		}
	}
	return &Matcher{patterns: copyPatterns}, nil
}

// ParseCommaSeparated splits and trims a comma-separated pattern list.
func ParseCommaSeparated(value string) []string {
	patterns := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			patterns = append(patterns, item)
		}
	}
	return patterns
}

// Match reports whether relativePath matches any configured pattern. Patterns
// containing a slash match the full path; all other patterns match its basename.
func (m *Matcher) Match(relativePath string) bool {
	for _, pattern := range m.patterns {
		target := path.Base(relativePath)
		if strings.Contains(pattern, "/") {
			target = relativePath
		}
		matched, _ := path.Match(pattern, target)
		if matched {
			return true
		}
	}
	return false
}

// Patterns returns a copy of the configured patterns.
func (m *Matcher) Patterns() []string { return append([]string(nil), m.patterns...) }
