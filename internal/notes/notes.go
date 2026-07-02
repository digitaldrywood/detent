package notes

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/pathsafe"
)

const (
	DefaultPath      = ".detent/notes.md"
	DefaultMaxBytes  = 64 * 1024
	DefaultTailBytes = 16 * 1024
)

var entryHeadingPattern = regexp.MustCompile(`^## \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z - `)

type Entry struct {
	Title string
	Body  string
}

type AppendOptions struct {
	Now      time.Time
	MaxBytes int
}

type ReadOptions struct {
	MaxBytes int
}

func WorkspacePath(workspacePath string) (string, error) {
	return pathsafe.WorkspaceRelative(workspacePath, DefaultPath)
}

func Append(path string, entry Entry, opts AppendOptions) error {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	entries, err := ReadAll(path)
	if err != nil {
		return err
	}
	entries = append(entries, renderEntry(entry, now.UTC()))
	entries = compactEntries(entries, maxBytes(opts.MaxBytes))

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(entries, "\n\n")+"\n"), 0o600)
}

func Read(path string, opts ReadOptions) (string, error) {
	entries, err := ReadAll(path)
	if err != nil {
		return "", err
	}
	entries = compactEntries(entries, maxBytes(opts.MaxBytes))
	return strings.Join(entries, "\n\n"), nil
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

func Tail(text string, limit int) string {
	if limit <= 0 || text == "" {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	return "[truncated to last " + tailLimitLabel(limit) + "]\n" + text[len(text)-limit:]
}

func tailLimitLabel(limit int) string {
	if limit >= 1024 && limit%1024 == 0 {
		return strconv.Itoa(limit/1024) + " KiB"
	}
	return strconv.Itoa(limit) + " bytes"
}

func renderEntry(entry Entry, now time.Time) string {
	title := strings.Join(strings.Fields(entry.Title), " ")
	if title == "" {
		title = "Handoff note"
	}
	title = strings.ReplaceAll(title, "\n", " ")

	body := normalizeBody(entry.Body)
	if body == "" {
		body = "_No details recorded._"
	}

	return "## " + now.Format("2006-01-02T15:04:05Z") + " - " + title + "\n\n" + body
}

func normalizeBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.TrimSpace(body)
}

func parseEntries(content string) []string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	entries := make([]string, 0)
	var current []string

	for _, line := range lines {
		if entryHeadingPattern.MatchString(line) {
			if entry := strings.TrimSpace(strings.Join(current, "\n")); entry != "" {
				entries = append(entries, entry)
			}
			current = []string{line}
			continue
		}
		current = append(current, line)
	}
	if entry := strings.TrimSpace(strings.Join(current, "\n")); entry != "" {
		entries = append(entries, entry)
	}

	return entries
}

func compactEntries(entries []string, limit int) []string {
	if len(entries) == 0 {
		return []string{}
	}
	for len(entries) > 1 && entriesSize(entries) > limit {
		entries = entries[1:]
	}
	if entriesSize(entries) <= limit {
		return entries
	}
	return []string{trimEntry(entries[len(entries)-1], limit)}
}

func trimEntry(entry string, limit int) string {
	if limit <= 0 || len(entry) <= limit {
		return entry
	}
	heading, body, ok := strings.Cut(entry, "\n\n")
	if !ok {
		return Tail(entry, limit)
	}
	prefix := heading + "\n\n"
	remaining := limit - len(prefix)
	if remaining <= 0 {
		return Tail(entry, limit)
	}
	return prefix + Tail(body, remaining)
}

func entriesSize(entries []string) int {
	if len(entries) == 0 {
		return 0
	}
	size := len(strings.Join(entries, "\n\n")) + 1
	return size
}

func maxBytes(value int) int {
	if value <= 0 {
		return DefaultMaxBytes
	}
	return value
}
