package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadMissingNotesReturnsEmpty(t *testing.T) {
	t.Parallel()

	content, err := Read(filepath.Join(t.TempDir(), ".detent", "notes.md"), ReadOptions{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if content != "" {
		t.Fatalf("Read() = %q, want empty", content)
	}
}

func TestAppendTimestampsAndDropsOldestEntriesFirst(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".detent", "notes.md")
	at := time.Date(2026, 7, 2, 21, 45, 0, 0, time.UTC)
	for _, entry := range []Entry{
		{Title: "first", Body: strings.Repeat("one ", 16)},
		{Title: "second", Body: strings.Repeat("two ", 16)},
		{Title: "third", Body: strings.Repeat("three ", 16)},
	} {
		if err := Append(path, entry, AppendOptions{Now: at, MaxBytes: 260}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	content, err := Read(path, ReadOptions{MaxBytes: 260})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(content) > 260 {
		t.Fatalf("content len = %d, want <= 260:\n%s", len(content), content)
	}
	for _, want := range []string{
		"## 2026-07-02T21:45:00Z - second",
		"## 2026-07-02T21:45:00Z - third",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("notes missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, " - first") {
		t.Fatalf("oldest entry was not dropped:\n%s", content)
	}
}

func TestTailKeepsBoundedSuffix(t *testing.T) {
	t.Parallel()

	got := Tail("prefix-keep", 4)
	if got != "[truncated to last 4 bytes]\nkeep" {
		t.Fatalf("Tail() = %q", got)
	}
}

func TestAppendPreservesUntimestampedNotes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".detent", "notes.md")
	if err := Append(path, Entry{Title: "first structured", Body: "structured"}, AppendOptions{
		Now: time.Date(2026, 7, 2, 21, 45, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	content := "manual note without detent timestamp\n\n- key file: internal/runner/prompt.go\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write manual notes: %v", err)
	}

	if err := Append(path, Entry{Title: "second structured", Body: "new structured note"}, AppendOptions{
		Now: time.Date(2026, 7, 2, 22, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := Read(path, ReadOptions{})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	for _, want := range []string{
		"manual note without detent timestamp",
		"- key file: internal/runner/prompt.go",
		"## 2026-07-02T22:00:00Z - second structured",
		"new structured note",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("notes missing %q:\n%s", want, got)
		}
	}
}
