package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestAdmissionWorkspaceCleanupIsScopedToCreatedPath(t *testing.T) {
	t.Parallel()

	backend := &admissionWorkspace{}
	info, err := backend.Create(context.Background(), workspace.Issue{ID: "detent/admission"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(info.Path)
	})

	if err := backend.Cleanup(context.Background(), filepath.Join(info.Path, "other")); err == nil {
		t.Fatal("Cleanup(other) error = nil")
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("created workspace stat error = %v", err)
	}
	backend.AfterRun(context.Background(), info, workspace.Issue{})
	if _, err := os.Stat(info.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace stat error = %v, want os.ErrNotExist", err)
	}
}
