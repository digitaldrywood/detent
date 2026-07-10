package intake

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStaleTODOScannerFindsSourceTodosAndSkipsExcludedTrees(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\n// TODO: handle retries\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "vendor", "example"), 0o700); err != nil {
		t.Fatalf("MkdirAll(vendor) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "example", "dep.go"), []byte("// TODO: vendored\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(dep.go) error = %v", err)
	}

	scanner, err := DefaultScannerFactory().New("stale-todos", root)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	events, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v, want one source TODO", events)
	}
	if events[0].Fields["path"] != "main.go" || events[0].Fields["line"] != "3" {
		t.Fatalf("event location = %#v, want main.go:3", events[0].Fields)
	}
}
