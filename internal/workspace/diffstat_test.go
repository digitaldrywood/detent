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
