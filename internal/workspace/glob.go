package workspace

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Patterns use '/' on all platforms. '*' and '?' match within one path
// segment; character classes use path.Match's '[abc]' / '[^abc]' syntax.
// A standalone '**' segment matches zero or more
// complete segments. A pattern without '/' matches a basename at any depth.
// Patterns containing a slash are anchored at the monitored root. Directory
// exclusion prunes descendants. Include never prunes directories. Exclusion
// wins, and VCS directories .git/.hg/.svn are always excluded. Absolute paths,
// backslash escapes, empty segments, '.', '..', and '!' negation are rejected.
func validateGlob(pattern string) error {
	bad := func() error {
		return fmt.Errorf("invalid monitoring glob %q: use root-relative slash paths, *, ?, classes, and standalone **", pattern)
	}
	if pattern == "" || strings.Contains(pattern, "\\") || strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "!") || (len(pattern) > 1 && pattern[1] == ':') {
		return bad()
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return bad()
		}
		if strings.Contains(segment, "**") && segment != "**" {
			return bad()
		}
		if _, err := path.Match(segment, ""); err != nil {
			return bad()
		}
	}
	return nil
}

func globMatch(pattern, name string) bool {
	if !strings.Contains(pattern, "/") {
		if pattern == "**" {
			return true
		}
		matched, _ := path.Match(pattern, path.Base(name))
		return matched
	}
	parts, names := strings.Split(pattern, "/"), strings.Split(name, "/")
	// Dynamic programming bounds matching by pattern-segments × path-segments,
	// including patterns with repeated '**' segments.
	matched := make([]bool, len(names)+1)
	matched[0] = true
	for _, part := range parts {
		next := make([]bool, len(names)+1)
		if part == "**" {
			next[0] = matched[0]
			for i := 1; i <= len(names); i++ {
				next[i] = matched[i] || next[i-1]
			}
		} else {
			for i := 1; i <= len(names); i++ {
				ok, _ := path.Match(part, names[i-1])
				next[i] = matched[i-1] && ok
			}
		}
		matched = next
	}
	return matched[len(names)]
}

func (t *Tracker) excluded(absolute, relative string) bool {
	for _, segment := range strings.Split(relative, "/") {
		if slices.Contains([]string{".git", ".hg", ".svn"}, segment) {
			return true
		}
	}
	for _, dir := range t.cfg.ExcludeDirs {
		if within(absolute, dir) {
			return true
		}
	}
	for _, pattern := range t.cfg.Exclude {
		// Apply exclusions to each ancestor too: a queued event for a child of
		// an excluded directory has the same scope rules as a full traversal.
		for rel := relative; rel != "."; rel = path.Dir(rel) {
			if globMatch(pattern, rel) {
				return true
			}
		}
	}
	return false
}

func (t *Tracker) included(relative string) bool {
	if len(t.cfg.Include) == 0 {
		return true
	}
	for _, pattern := range t.cfg.Include {
		if globMatch(pattern, filepath.ToSlash(relative)) {
			return true
		}
	}
	return false
}
