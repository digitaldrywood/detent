package tracker

import (
	"path"
	"strings"
)

// diffDeniedSegments are path segments refused wherever they appear.
var diffDeniedSegments = []string{"node_modules"}

// diffDeniedPairs are two-segment sequences refused wherever they appear, so a
// nested checkout's .git is filtered as well as the root one.
var diffDeniedPairs = [][2]string{{".git", "objects"}, {".git", "config"}}

// diffDeniedSuffixes are the file-name suffixes of the project's secret
// patterns: "*.pem", "*.key", "*.p12".
var diffDeniedSuffixes = []string{".pem", ".key", ".p12"}

// diffDeniedPrefixes are the file-name prefixes of the project's secret
// patterns: ".env*", "id_rsa*".
var diffDeniedPrefixes = []string{".env", "id_rsa"}

// DiffPathDenied reports whether value matches the files denylist of section
// 18.4. It works on the path string alone, because a deleted or renamed file
// has no current canonical target to resolve against, and it is applied to
// both a file's path and its old_path.
//
// It is the fixed half of the filter. workspacesession.Denylist adds
// project-configured globs on top of it for the live files and diff surfaces.
func DiffPathDenied(value string) bool {
	segments := diffPathSegments(value)
	if len(segments) == 0 {
		return false
	}
	for index, segment := range segments {
		for _, denied := range diffDeniedSegments {
			if segment == denied {
				return true
			}
		}
		if index+1 >= len(segments) {
			continue
		}
		for _, pair := range diffDeniedPairs {
			if segment == pair[0] && segments[index+1] == pair[1] {
				return true
			}
		}
	}
	name := path.Base(segments[len(segments)-1])
	for _, suffix := range diffDeniedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	for _, prefix := range diffDeniedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// diffPathSegments lowercases and splits a reported path. Backslashes are
// treated as separators too, so a Windows-style path cannot slip a denied
// segment past a comparison that only knows about "/".
func diffPathSegments(value string) []string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return nil
	}
	var segments []string
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == "." {
			continue
		}
		segments = append(segments, segment)
	}
	return segments
}
