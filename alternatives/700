package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDoctorExternalBranchWorktrees(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	source := filepath.Join(base, "source")
	root := filepath.Join(base, "managed")
	external := filepath.Join(base, "review-pr-1917")
	tests := []struct {
		name       string
		worktrees  []doctorGitWorktree
		listErr    error
		wantStatus doctorStatus
		wantDetail []string
		wantHint   string
	}{
		{
			name: "source managed and detached worktrees are healthy",
			worktrees: []doctorGitWorktree{
				{Path: source, Branch: "main"},
				{Path: filepath.Join(root, "issue-1"), Branch: "detent/issue-1"},
				{Path: external},
			},
			wantStatus: doctorOK,
			wantDetail: []string{"all branch worktrees", root},
		},
		{
			name: "external review checkout warns with holder",
			worktrees: []doctorGitWorktree{
				{Path: source, Branch: "main"},
				{Path: external, Branch: "detent/issue-1838"},
			},
			wantStatus: doctorWarn,
			wantDetail: []string{"1 branch is held", "detent/issue-1838", external},
			wantHint:   "Active PR review checkouts are safe to keep",
		},
		{
			name:       "git listing failure warns",
			listErr:    errors.New("corrupt worktree registry"),
			wantStatus: doctorWarn,
			wantDetail: []string{"cannot list worktrees", "corrupt worktree registry"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := checkDoctorExternalBranchWorktrees(t.Context(), "pyroapex", source, root, doctorDeps{
				gitWorktrees: func(context.Context, string) ([]doctorGitWorktree, error) {
					return append([]doctorGitWorktree(nil), tt.worktrees...), tt.listErr
				},
			})
			if check.Status != tt.wantStatus {
				t.Fatalf("Status = %s, want %s: %#v", check.Status, tt.wantStatus, check)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
			if tt.wantHint != "" && !strings.Contains(check.Hint, tt.wantHint) {
				t.Fatalf("Hint = %q, want containing %q", check.Hint, tt.wantHint)
			}
		})
	}
}

func TestParseDoctorGitWorktrees(t *testing.T) {
	t.Parallel()

	output := "worktree /repo\x00HEAD abc\x00branch refs/heads/main\x00\x00" +
		"worktree /managed/issue\x00HEAD def\x00branch refs/heads/detent/issue\x00\x00" +
		"worktree /review/detached\x00HEAD 123\x00detached\x00\x00"
	got := parseDoctorGitWorktrees(output)
	want := []doctorGitWorktree{
		{Path: "/repo", Branch: "main"},
		{Path: "/managed/issue", Branch: "detent/issue"},
		{Path: "/review/detached"},
	}
	if len(got) != len(want) {
		t.Fatalf("worktrees = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("worktrees[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestCheckDoctorWorkspaceGrowthThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		count             int
		registered        int
		listErr           error
		sourceMatchesRoot bool
		wantCheck         bool
		wantStatus        doctorStatus
		wantDetail        []string
		wantHint          string
	}{
		{
			name:  "below threshold is not surfaced",
			count: doctorWorkspaceCountWarningThreshold - 1,
		},
		{
			name:       "threshold reports unregistered directories",
			count:      doctorWorkspaceCountWarningThreshold,
			registered: doctorWorkspaceCountWarningThreshold - 2,
			wantCheck:  true,
			wantStatus: doctorWarn,
			wantDetail: []string{"50 retained workspace directories", "2 are not registered with the source repository"},
			wantHint:   "Confirm workspace cleanup is running",
		},
		{
			name:       "registration failure still reports growth",
			count:      doctorWorkspaceCountWarningThreshold,
			listErr:    errors.New("corrupt worktree registry"),
			wantCheck:  true,
			wantStatus: doctorWarn,
			wantDetail: []string{"50 retained workspace directories", "cannot classify unregistered directories", "corrupt worktree registry"},
			wantHint:   "Confirm workspace cleanup is running",
		},
		{
			name:              "source checkout root is not counted as retained workspaces",
			count:             doctorWorkspaceCountWarningThreshold,
			sourceMatchesRoot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			sourceRoot := filepath.Join(t.TempDir(), "source")
			if tt.sourceMatchesRoot {
				sourceRoot = root
			}
			if err := os.Mkdir(filepath.Join(root, ".detent"), 0o700); err != nil {
				t.Fatalf("Mkdir(.detent) error = %v", err)
			}
			for index := range tt.count {
				if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("issue-%d", index)), 0o700); err != nil {
					t.Fatalf("Mkdir(workspace) error = %v", err)
				}
			}

			worktrees := []doctorGitWorktree{{Path: sourceRoot, Branch: "main"}}
			for index := range tt.registered {
				worktrees = append(worktrees, doctorGitWorktree{
					Path:   filepath.Join(root, fmt.Sprintf("issue-%d", index)),
					Branch: fmt.Sprintf("detent/issue-%d", index),
				})
			}
			check, ok := checkDoctorWorkspaceGrowth(t.Context(), "pyroapex", root, sourceRoot, doctorDeps{
				gitWorktrees: func(context.Context, string) ([]doctorGitWorktree, error) {
					return worktrees, tt.listErr
				},
			})
			if ok != tt.wantCheck {
				t.Fatalf("check present = %t, want %t: %#v", ok, tt.wantCheck, check)
			}
			if !tt.wantCheck {
				return
			}
			if check.Status != tt.wantStatus {
				t.Fatalf("Status = %s, want %s: %#v", check.Status, tt.wantStatus, check)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
			if tt.wantHint != "" && !strings.Contains(check.Hint, tt.wantHint) {
				t.Fatalf("Hint = %q, want containing %q", check.Hint, tt.wantHint)
			}
		})
	}
}
