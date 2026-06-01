package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestWatchDebouncesWorkflowWrites(t *testing.T) {
	t.Parallel()

	debounce := 150 * time.Millisecond
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 100, "initial")

	w, err := New(path, WithDebounce(debounce))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	writeWorkflow(t, path, 200, "first")
	writeWorkflow(t, path, 300, "second")

	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Config.Polling.IntervalMS != 300 {
		t.Fatalf("Polling.IntervalMS = %d, want 300", update.Workflow.Config.Polling.IntervalMS)
	}
	if update.Workflow.Prompt != "second\n" {
		t.Fatalf("Prompt = %q, want second", update.Workflow.Prompt)
	}

	select {
	case extra := <-updates:
		t.Fatalf("extra update after debounce = %#v", extra)
	case <-time.After(2 * debounce):
	}
}

func TestWatchSuppressesDuplicateWorkflowUpdates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 100, "initial")

	w, err := New(path, WithDebounce(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	writeWorkflow(t, path, 300, "second")
	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Prompt != "second\n" {
		t.Fatalf("Prompt = %q, want second", update.Workflow.Prompt)
	}

	writeWorkflow(t, path, 300, "second")
	select {
	case extra := <-updates:
		t.Fatalf("duplicate update = %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchHandlesAtomicSaveRename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 100, "initial")

	w, err := New(path, WithDebounce(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	tmp := filepath.Join(dir, ".WORKFLOW.md.tmp")
	writeWorkflow(t, tmp, 450, "renamed")
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}

	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Config.Polling.IntervalMS != 450 {
		t.Fatalf("Polling.IntervalMS = %d, want 450", update.Workflow.Config.Polling.IntervalMS)
	}
	if update.Workflow.Prompt != "renamed\n" {
		t.Fatalf("Prompt = %q, want renamed", update.Workflow.Prompt)
	}
}

func TestWatchRetriesTransientReloadErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 100, "initial")

	var attempts atomic.Int32
	w, err := New(path,
		WithDebounce(10*time.Millisecond),
		WithLoader(func(path string) (workflowconfig.Workflow, error) {
			if attempts.Add(1) == 1 {
				return workflowconfig.Workflow{}, errors.New("missing YAML frontmatter")
			}
			return workflowconfig.LoadWorkflow(path)
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	writeWorkflow(t, path, 300, "second")

	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Config.Polling.IntervalMS != 300 {
		t.Fatalf("Polling.IntervalMS = %d, want 300", update.Workflow.Config.Polling.IntervalMS)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("loader attempts = %d, want at least 2", got)
	}
}

func TestWatchReportsInvalidReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 100, "initial")

	w, err := New(path, WithDebounce(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	if err := os.WriteFile(path, []byte("---\ntracker: [\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	update := receiveUpdate(t, updates)
	if update.Err == nil {
		t.Fatal("update error = nil, want parse error")
	}
}

func receiveUpdate(t *testing.T, updates <-chan Update) Update {
	t.Helper()

	select {
	case update, ok := <-updates:
		if !ok {
			t.Fatal("updates channel closed")
		}
		return update
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watcher update")
	}

	return Update{}
}

func writeWorkflow(t *testing.T, path string, intervalMS int, prompt string) {
	t.Helper()

	raw := []byte(`---
tracker:
  kind: memory
polling:
  interval_ms: ` + strconv.Itoa(intervalMS) + `
---
` + prompt + `
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
