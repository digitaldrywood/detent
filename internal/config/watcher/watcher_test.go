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
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestWatchDebouncesWorkflowWrites(t *testing.T) {
	t.Parallel()

	debounce := 150 * time.Millisecond
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 60000, "initial")

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

	writeWorkflow(t, path, 61000, "first")
	writeWorkflow(t, path, 62000, "second")

	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Config.Polling.IntervalMS != 62000 {
		t.Fatalf("Polling.IntervalMS = %d, want 62000", update.Workflow.Config.Polling.IntervalMS)
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
	writeWorkflow(t, path, 60000, "initial")

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

	writeWorkflow(t, path, 61000, "second")
	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Prompt != "second\n" {
		t.Fatalf("Prompt = %q, want second", update.Workflow.Prompt)
	}

	writeWorkflow(t, path, 61000, "second")
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
	writeWorkflow(t, path, 60000, "initial")

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
	writeWorkflow(t, tmp, 63000, "renamed")
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}

	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Config.Polling.IntervalMS != 63000 {
		t.Fatalf("Polling.IntervalMS = %d, want 63000", update.Workflow.Config.Polling.IntervalMS)
	}
	if update.Workflow.Prompt != "renamed\n" {
		t.Fatalf("Prompt = %q, want renamed", update.Workflow.Prompt)
	}
}

func TestWatchRetriesTransientReloadErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 60000, "initial")

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

	writeWorkflow(t, path, 62000, "second")

	update := receiveUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Workflow.Config.Polling.IntervalMS != 62000 {
		t.Fatalf("Polling.IntervalMS = %d, want 62000", update.Workflow.Config.Polling.IntervalMS)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("loader attempts = %d, want at least 2", got)
	}
}

func TestWatchReportsInvalidReload(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	writeWorkflow(t, path, 60000, "initial")

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

func TestFileWatcherDebouncesGlobalConfigWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "global.yaml")
	writeGlobalConfig(t, path, 2)

	w, err := NewFile(path, func(path string) (globalconfig.Config, error) {
		return globalconfig.Read(path)
	}, WithFileDebounce(150*time.Millisecond))
	if err != nil {
		t.Fatalf("NewFile() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	updates, err := w.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	writeGlobalConfig(t, path, 3)
	writeGlobalConfig(t, path, 4)

	update := receiveFileUpdate(t, updates)
	if update.Err != nil {
		t.Fatalf("update error = %v", update.Err)
	}
	if update.Value.Global.MaxConcurrentAgents != 4 {
		t.Fatalf("MaxConcurrentAgents = %d, want 4", update.Value.Global.MaxConcurrentAgents)
	}
	if update.Value.Path != path {
		t.Fatalf("Path = %q, want %q", update.Value.Path, path)
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

func receiveFileUpdate[T any](t *testing.T, updates <-chan FileUpdate[T]) FileUpdate[T] {
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

	return FileUpdate[T]{}
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

func writeGlobalConfig(t *testing.T, path string, maxConcurrentAgents int) {
	t.Helper()

	raw := []byte(`apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: ` + strconv.Itoa(maxConcurrentAgents) + `
  scheduling: weighted
projects: []
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
