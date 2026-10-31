package pathsafe

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRelativeResolvesCleanPath(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	got, err := WorkspaceRelative(" "+workspace+" ", " .detent/skills/. ")
	if err != nil {
		t.Fatalf("WorkspaceRelative() error = %v", err)
	}

	want := filepath.Join(workspace, ".detent", "skills")
	if got != want {
		t.Fatalf("WorkspaceRelative() = %q, want %q", got, want)
	}
}

func TestWorkspaceRelativeRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	tests := []string{
		"",
		"~/skills",
		"/tmp/skills",
		`\tmp\skills`,
		`C:\tmp\skills`,
		"../skills",
		"skills/../skills",
		`skills\..\skills`,
	}

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			_, err := WorkspaceRelative(workspace, path)
			if err == nil {
				t.Fatal("WorkspaceRelative() error = nil, want error")
			}
			if IsWorkspaceRelative(path) {
				t.Fatal("IsWorkspaceRelative() = true, want false")
			}
			if !strings.Contains(err.Error(), "relative path inside the workspace") {
				t.Fatalf("WorkspaceRelative() error = %v, want workspace-relative path error", err)
			}
		})
	}
}

func TestWorkspaceRelativeRejectsMissingWorkspace(t *testing.T) {
	t.Parallel()

	_, err := WorkspaceRelative("", ".detent/skills")
	if err == nil {
		t.Fatal("WorkspaceRelative() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "workspace path is required") {
		t.Fatalf("WorkspaceRelative() error = %v, want workspace path error", err)
	}
}
