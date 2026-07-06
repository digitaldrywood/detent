package lessons

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultPath       = ".detent/lessons.md"
	DefaultMaxEntries = 50
)

type Entry struct {
	IssueNumber string
	IssueRef    string
	Title       string
	FailureKind string
	Symptom     string
	Hypothesis  string
	Hint        string
}

type AppendOptions struct {
	Date       time.Time
	MaxEntries int
}

type FailureKindPattern struct {
	FailureKind string
	Count       int
	Examples    []string
}

func Append(path string, entry Entry, opts AppendOptions) error {
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}

	date := opts.Date
	if date.IsZero() {
		date = time.Now().UTC()
	}

	entries, err := ReadAll(path)
	if err != nil {
		return err
	}

	rendered := renderEntry(entry, date)
	entries = append([]string{rendered}, entries...)
	if len(entries) > maxEntries {
		entries = entries[:maxEntries]
	}

	return writeEntries(path, entries)
}

func Recent(path string, count int) ([]string, error) {
	if count <= 0 {
		return []string{}, nil
	}

	entries, err := ReadAll(path)
	if err != nil {
		return nil, err
	}
	if len(entries) > count {
		entries = entries[:count]
	}

	return entries, nil
}

func FailureKindPatterns(path string, threshold int) ([]FailureKindPattern, error) {
	if threshold <= 0 {
		threshold = 1
	}

	entries, err := ReadAll(path)
	if err != nil {
		return nil, err
	}

	patternsByKind := map[string]*FailureKindPattern{}
	keys := []string{}
	for _, entry := range entries {
		failureKind := renderedEntryField(entry, "Failure kind")
		if failureKind == "" || strings.HasPrefix(failureKind, "<") {
			continue
		}
		key := strings.ToLower(failureKind)
		pattern, ok := patternsByKind[key]
		if !ok {
			pattern = &FailureKindPattern{FailureKind: failureKind}
			patternsByKind[key] = pattern
			keys = append(keys, key)
		}
		pattern.Count++
		if example := renderedEntryHeading(entry); example != "" && len(pattern.Examples) < 3 {
			pattern.Examples = append(pattern.Examples, example)
		}
	}

	sort.Slice(keys, func(i, j int) bool {
		left := patternsByKind[keys[i]]
		right := patternsByKind[keys[j]]
		if left.Count != right.Count {
			return left.Count > right.Count
		}
		return left.FailureKind < right.FailureKind
	})

	patterns := make([]FailureKindPattern, 0, len(keys))
	for _, key := range keys {
		pattern, ok := patternsByKind[key]
		if !ok || pattern == nil {
			continue
		}
		if pattern.Count < threshold {
			continue
		}
		patterns = append(patterns, *pattern)
	}
	return patterns, nil
}

func ReadAll(path string) ([]string, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}

	return parseEntries(string(content)), nil
}

func renderEntry(entry Entry, date time.Time) string {
	lines := []string{
		"## " + date.Format("2006-01-02") + " - " + issueRef(entry) + " - \"" + headingTitle(entry) + "\"",
		"- **Failure kind:** " + field(entry.FailureKind, "<unknown>"),
		"- **Symptom:** " + field(entry.Symptom, "<unavailable>"),
		"- **Hypothesis (Detent):** " + field(entry.Hypothesis, "<unavailable>"),
		"- **Hint for next time:** " + field(entry.Hint, "<unavailable>"),
	}

	return strings.Join(lines, "\n")
}

func issueRef(entry Entry) string {
	if strings.TrimSpace(entry.IssueNumber) != "" {
		return "issue #" + strings.TrimLeft(strings.TrimSpace(entry.IssueNumber), "#")
	}
	if strings.TrimSpace(entry.IssueRef) != "" {
		return strings.TrimSpace(entry.IssueRef)
	}
	return "issue <unknown>"
}

func headingTitle(entry Entry) string {
	return strings.ReplaceAll(field(entry.Title, "Untitled"), `"`, `\"`)
}

func field(value string, fallback string) string {
	normalized := strings.Join(strings.Fields(value), " ")
	if normalized == "" {
		return fallback
	}
	return normalized
}

func parseEntries(content string) []string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	entries := make([]string, 0)
	var current []string

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if len(current) > 0 {
				entries = append(entries, strings.TrimSpace(strings.Join(current, "\n")))
			}
			current = []string{line}
			continue
		}
		if len(current) > 0 {
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		entries = append(entries, strings.TrimSpace(strings.Join(current, "\n")))
	}

	return entries
}

func renderedEntryField(entry string, label string) string {
	prefix := "- **" + label + ":** "
	for _, line := range strings.Split(entry, "\n") {
		line = strings.TrimSpace(line)
		value, ok := strings.CutPrefix(line, prefix)
		if ok {
			return field(value, "")
		}
	}
	return ""
}

func renderedEntryHeading(entry string) string {
	for _, line := range strings.Split(entry, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "## "))
		}
	}
	return ""
}

func writeEntries(path string, entries []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return os.WriteFile(path, []byte(strings.Join(entries, "\n\n")+"\n"), 0o600)
}
