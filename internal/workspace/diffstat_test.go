package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDiffStat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		output  string
		want    DiffStat
		wantErr bool
	}{
		{name: "empty", output: "", want: DiffStat{}},
		{
			name: "full stat output",
			output: " README.md | 1 -\n added.txt | 2 ++\n" +
				" 2 files changed, 2 insertions(+), 1 deletion(-)\n",
			want: DiffStat{Files: 2, Added: 2, Removed: 1},
		},
		{
			name:   "insertions only",
			output: " 1 file changed, 5 insertions(+)\n",
			want:   DiffStat{Files: 1, Added: 5},
		},
		{
			name:   "deletions only",
			output: " 3 files changed, 8 deletions(-)\n",
			want:   DiffStat{Files: 3, Removed: 8},
		},
		{
			name:   "no line changes",
			output: " 1 file changed\n",
			want:   DiffStat{Files: 1},
		},
		{
			name:    "malformed",
			output:  "not a diff stat\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseDiffStat(tt.output)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseDiffStat() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDiffStat() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseDiffStat() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLocalGitDiffStat(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	clean, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("clean DiffStat() error = %v", err)
	}
	if clean != (DiffStat{}) {
		t.Fatalf("clean DiffStat() = %+v, want zero", clean)
	}

	if err := os.WriteFile(filepath.Join(info.Path, "added.txt"), []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatalf("write added file: %v", err)
	}
	if err := os.Remove(filepath.Join(info.Path, "README.md")); err != nil {
		t.Fatalf("remove README.md: %v", err)
	}

	got, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("DiffStat() error = %v", err)
	}
	want := DiffStat{Files: 2, Added: 2, Removed: 1}
	if got != want {
		t.Fatalf("DiffStat() = %+v, want %+v", got, want)
	}

	status := runGit(t, info.Path, "status", "--short")
	if !strings.Contains(status, "?? added.txt") {
		t.Fatalf("git status = %q, want added.txt to remain untracked", status)
	}
}

func TestLocalGitDiffIsBounded(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	provider, ok := backend.(DiffProvider)
	if !ok {
		t.Fatal("NewBackend() did not return a DiffProvider")
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-FULL-DIFF"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "added.txt"), []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatalf("write added file: %v", err)
	}
	if err := os.Remove(filepath.Join(info.Path, "README.md")); err != nil {
		t.Fatalf("remove README.md: %v", err)
	}

	tests := []struct {
		name          string
		maxBytes      int
		wantTruncated bool
		wantPatch     []string
		forbidden     []string
	}{
		{
			name:          "inline under limit",
			maxBytes:      4096,
			wantTruncated: false,
			wantPatch: []string{
				"diff --git a/README.md b/README.md",
				"diff --git a/added.txt b/added.txt",
				"+first",
				"+second",
			},
		},
		{
			name:          "stat only over limit",
			maxBytes:      1,
			wantTruncated: true,
			forbidden:     []string{"diff --git", "+first"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diff, err := provider.Diff(context.Background(), info, Issue{Identifier: "DD-FULL-DIFF"}, tt.maxBytes)
			if err != nil {
				t.Fatalf("Diff() error = %v", err)
			}
			if diff.Stat != (DiffStat{Files: 2, Added: 2, Removed: 1}) {
				t.Fatalf("Diff().Stat = %+v, want 2 files, 2 added, 1 removed", diff.Stat)
			}
			if diff.Truncated != tt.wantTruncated {
				t.Fatalf("Diff().Truncated = %v, want %v", diff.Truncated, tt.wantTruncated)
			}
			for _, want := range tt.wantPatch {
				if !strings.Contains(diff.Patch, want) {
					t.Fatalf("Diff().Patch missing %q:\n%s", want, diff.Patch)
				}
			}
			for _, forbidden := range tt.forbidden {
				if strings.Contains(diff.Patch, forbidden) {
					t.Fatalf("Diff().Patch contains %q:\n%s", forbidden, diff.Patch)
				}
			}
		})
	}

	status := runGit(t, info.Path, "status", "--short")
	if !strings.Contains(status, "?? added.txt") {
		t.Fatalf("git status = %q, want added.txt to remain untracked", status)
	}
}

func TestLocalGitDiffUsesBaseRefForCleanBranch(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	root := filepath.Join(t.TempDir(), "workspaces")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	provider, ok := backend.(DiffProvider)
	if !ok {
		t.Fatal("NewBackend() did not return a DiffProvider")
	}

	info, err := backend.Create(context.Background(), Issue{Identifier: "DD-PR-DIFF"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	baseRef := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("source repo\nvalidator diff\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, info.Path, "add", "README.md")
	runGit(t, info.Path, "commit", "-m", "change readme")

	cleanStatus := runGit(t, info.Path, "status", "--short")
	if cleanStatus != "" {
		t.Fatalf("git status = %q, want clean branch", cleanStatus)
	}

	withoutBase, err := provider.Diff(context.Background(), info, Issue{Identifier: "DD-PR-DIFF"}, 4096)
	if err != nil {
		t.Fatalf("Diff() without base error = %v", err)
	}
	if withoutBase.Stat != (DiffStat{}) || withoutBase.Patch != "" {
		t.Fatalf("Diff() without base = %+v, want clean HEAD diff", withoutBase)
	}

	withBase, err := provider.Diff(context.Background(), info, Issue{Identifier: "DD-PR-DIFF", BaseRef: baseRef}, 4096)
	if err != nil {
		t.Fatalf("Diff() with base error = %v", err)
	}
	if withBase.Stat != (DiffStat{Files: 1, Added: 1}) {
		t.Fatalf("Diff().Stat = %+v, want 1 file, 1 added", withBase.Stat)
	}
	if withBase.Truncated {
		t.Fatal("Diff().Truncated = true, want false")
	}
	if !strings.Contains(withBase.Patch, "+validator diff") {
		t.Fatalf("Diff().Patch missing committed branch change:\n%s", withBase.Patch)
	}
}

func TestGitDiffStatMissingWorkspaceIsClassified(t *testing.T) {
	t.Parallel()

	_, err := GitDiffStat(context.Background(), filepath.Join(t.TempDir(), "missing-worktree"))
	if err == nil {
		t.Fatal("GitDiffStat() error = nil, want missing workspace error")
	}
	if !IsMissingWorkspaceError(err) {
		t.Fatalf("IsMissingWorkspaceError(%v) = false, want true", err)
	}
	if !errors.Is(err, ErrMissingWorkspace) {
		t.Fatalf("GitDiffStat() error = %v, want ErrMissingWorkspace", err)
	}
}

func TestIsMissingWorkspaceErrorIgnoresUnmarkedNotExist(t *testing.T) {
	t.Parallel()

	err := &os.PathError{Op: "read", Path: filepath.Join(t.TempDir(), "index"), Err: os.ErrNotExist}
	if IsMissingWorkspaceError(err) {
		t.Fatalf("IsMissingWorkspaceError(%v) = true, want false", err)
	}
}
