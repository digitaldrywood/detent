package lessons

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadAllAndRecentTreatMissingFileAsEmpty(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".detent", "lessons.md")

	entries, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ReadAll() len = %d, want 0", len(entries))
	}

	recent, err := Recent(path, 3)
	if err != nil {
		t.Fatalf("Recent() error = %v", err)
	}
	if len(recent) != 0 {
		t.Fatalf("Recent() len = %d, want 0", len(recent))
	}
}

func TestAppendStoresNewestFirstAndCapsEntries(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".detent", "lessons.md")

	for index := 1; index <= 4; index++ {
		err := Append(path, Entry{
			IssueNumber: strconv.Itoa(index),
			Title:       "Issue " + strconv.Itoa(index),
			FailureKind: "kind " + strconv.Itoa(index),
			Symptom:     "symptom " + strconv.Itoa(index),
			Hypothesis:  "hypothesis " + strconv.Itoa(index),
			Hint:        "hint " + strconv.Itoa(index),
		}, AppendOptions{
			Date:       time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
			MaxEntries: 3,
		})
		if err != nil {
			t.Fatalf("Append(%d) error = %v", index, err)
		}
	}

	entries, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries len = %d, want 3", len(entries))
	}
	if !strings.Contains(entries[0], "issue #4") || !strings.Contains(entries[2], "issue #2") {
		t.Fatalf("entries not newest first with oldest capped: %#v", entries)
	}
	if strings.Contains(strings.Join(entries, "\n"), "issue #1") {
		t.Fatalf("oldest entry was not capped: %#v", entries)
	}

	recent, err := Recent(path, 2)
	if err != nil {
		t.Fatalf("Recent() error = %v", err)
	}
	if len(recent) != 2 || !strings.Contains(recent[0], "issue #4") || !strings.Contains(recent[1], "issue #3") {
		t.Fatalf("Recent() = %#v, want issue #4 then issue #3", recent)
	}
}

func TestAppendRendersFallbacksAndEscapesTitleQuotes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".detent", "lessons.md")

	err := Append(path, Entry{
		IssueRef:    "issue MT-9",
		Title:       `Needs "quotes" escaped`,
		FailureKind: "",
		Symptom:     "  command\nfailed\tbefore diff  ",
		Hypothesis:  "",
		Hint:        "",
	}, AppendOptions{Date: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	entries, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	entry := entries[0]
	for _, want := range []string{
		`## 2026-05-22 - issue MT-9 - "Needs \"quotes\" escaped"`,
		"- **Failure kind:** <unknown>",
		"- **Symptom:** command failed before diff",
		"- **Hypothesis (Detent):** <unavailable>",
		"- **Hint for next time:** <unavailable>",
	} {
		if !strings.Contains(entry, want) {
			t.Fatalf("entry missing %q:\n%s", want, entry)
		}
	}
}

func TestReadAllReturnsReadErrors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	_, err := ReadAll(root)
	if err == nil {
		t.Fatal("ReadAll() error = nil, want error")
	}
	_, err = Recent(root, 1)
	if err == nil {
		t.Fatal("Recent() error = nil, want error")
	}
	if err := Append(root, Entry{Title: "cannot append"}, AppendOptions{}); err == nil {
		t.Fatal("Append() error = nil, want error")
	}
}
