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
	if err := os.MkdirAll(filepath.Join(info.Path, ".detent"), 0o700); err != nil {
		t.Fatalf("mkdir .detent: %v", err)
	}
	for _, name := range []string{"notes.md", "lessons.md"} {
		if err := os.WriteFile(filepath.Join(info.Path, ".detent", name), []byte("handoff\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(info.Path, ".detent", "tmp"), 0o700); err != nil {
		t.Fatalf("mkdir worker scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, ".detent", "tmp", "scratch"), []byte("temporary\n"), 0o600); err != nil {
		t.Fatalf("write worker scratch: %v", err)
	}

	got, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("DiffStat() error = %v", err)
	}
	if got.Files != 2 || got.Added != 2 || got.Removed != 1 || got.Fingerprint == "" {
		t.Fatalf("DiffStat() = %+v, want 2 files, 2 added, 1 removed, and a fingerprint", got)
	}
	originalFingerprint := got.Fingerprint

	if err := os.WriteFile(filepath.Join(info.Path, "added.txt"), []byte("third\nfourth\n"), 0o600); err != nil {
		t.Fatalf("rewrite added file: %v", err)
	}
	contentChanged, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("content-changed DiffStat() error = %v", err)
	}
	if contentChanged.Files != got.Files || contentChanged.Added != got.Added || contentChanged.Removed != got.Removed {
		t.Fatalf("content-changed DiffStat() = %+v, want unchanged counts from %+v", contentChanged, got)
	}
	if contentChanged.Fingerprint == originalFingerprint {
		t.Fatalf("content-changed fingerprint = %q, want different from original", contentChanged.Fingerprint)
	}

	if err := os.Remove(filepath.Join(info.Path, "added.txt")); err != nil {
		t.Fatalf("remove added file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "other.txt"), []byte("third\nfourth\n"), 0o600); err != nil {
		t.Fatalf("write other file: %v", err)
	}
	fileSetChanged, err := backend.DiffStat(context.Background(), info, Issue{Identifier: "DD-DIFF"})
	if err != nil {
		t.Fatalf("file-set-changed DiffStat() error = %v", err)
	}
	if fileSetChanged.Files != contentChanged.Files || fileSetChanged.Added != contentChanged.Added || fileSetChanged.Removed != contentChanged.Removed {
		t.Fatalf("file-set-changed DiffStat() = %+v, want unchanged counts from %+v", fileSetChanged, contentChanged)
	}
	if fileSetChanged.Fingerprint == contentChanged.Fingerprint {
		t.Fatalf("file-set-changed fingerprint = %q, want different from content-changed fingerprint", fileSetChanged.Fingerprint)
	}

	status := runGit(t, info.Path, "status", "--short")
	if !strings.Contains(status, "?? other.txt") {
		t.Fatalf("git status = %q, want other.txt to remain untracked", status)
	}
	if strings.Contains(status, ".detent/notes.md") || strings.Contains(status, ".detent/lessons.md") || strings.Contains(status, ".detent/tmp") {
		t.Fatalf("git status = %q, want Detent runtime files ignored", status)
	}
	for _, path := range []string{".detent/notes.md", ".detent/lessons.md", ".detent/tmp/scratch"} {
		if ignored := runGit(t, info.Path, "check-ignore", path); strings.TrimSpace(ignored) != path {
			t.Fatalf("check-ignore %s = %q, want %s", path, ignored, path)
		}
	}
}

func TestLocalGitRecoveryStateDetectsStrandedWork(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	root := filepath.Join(t.TempDir(), "workspaces")

	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       root,
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	provider, ok := backend.(RecoveryStateProvider)
	if !ok {
		t.Fatal("NewBackend() did not return a RecoveryStateProvider")
	}
	issue := Issue{Identifier: "DD-RECOVERY"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	issue.PullRequestHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(info.Path, "committed.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatalf("write committed file: %v", err)
	}
	runGit(t, info.Path, "add", "committed.txt")
	runGit(t, info.Path, "commit", "-m", "test: add committed work")
	commitSHA := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(info.Path, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	runGit(t, info.Path, "add", "dirty.txt")

	got, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() error = %v", err)
	}
	if got.UnpushedCommits != 1 || got.DiffStat.Files != 1 || got.DiffStat.Added != 1 {
		t.Fatalf("RecoveryState() = %+v, want one unpushed commit and one dirty file", got)
	}
	if len(got.TrackedPaths) != 1 || got.TrackedPaths[0] != "dirty.txt" {
		t.Fatalf("RecoveryState().TrackedPaths = %v, want [dirty.txt]", got.TrackedPaths)
	}
	if !got.PullRequestComparisonAvailable || len(got.CommitsNotInPullRequest) != 1 ||
		!strings.Contains(got.CommitsNotInPullRequest[0], commitSHA) ||
		!strings.Contains(got.CommitsNotInPullRequest[0], "test: add committed work") {
		t.Fatalf("RecoveryState() pull request commit evidence = %v, available=%t, want %s and subject", got.CommitsNotInPullRequest, got.PullRequestComparisonAvailable, commitSHA)
	}
	if len(got.UnpushedCommitRefs) != 1 || !strings.Contains(got.UnpushedCommitRefs[0], commitSHA) {
		t.Fatalf("RecoveryState().UnpushedCommitRefs = %v, want %s", got.UnpushedCommitRefs, commitSHA)
	}
	if want := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got.HeadSHA != want {
		t.Fatalf("RecoveryState().HeadSHA = %q, want %q", got.HeadSHA, want)
	}
	runGit(t, info.Path, "push", "-u", "origin", "HEAD:"+info.Branch)
	pushed, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() after push error = %v", err)
	}
	if pushed.UnpushedCommits != 0 || pushed.DiffStat != got.DiffStat {
		t.Fatalf("RecoveryState() after push = %+v, want no unpushed commits and unchanged dirty diff %+v", pushed, got.DiffStat)
	}
}

func TestLocalGitRecoveryStateExcludesUntrackedPathsFromStrandedEvidence(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	provider := backend.(RecoveryStateProvider)
	issue := Issue{Identifier: "DD-UNTRACKED"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	issue.PullRequestHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(info.Path, "session-marker.ses"), []byte("1722600000000 01234567-89ab-cdef-0123-456789abcdef\n"), 0o600); err != nil {
		t.Fatalf("write untracked session marker: %v", err)
	}

	got, err := provider.RecoveryState(t.Context(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() error = %v", err)
	}
	if got.DiffStat.Files != 1 || got.UnpushedCommits != 0 {
		t.Fatalf("RecoveryState() = %+v, want one aggregate diff file and no unpushed commits", got)
	}
	if len(got.TrackedPaths) != 0 || len(got.CommitsNotInPullRequest) != 0 || !got.PullRequestComparisonAvailable {
		t.Fatalf("RecoveryState() stranded evidence = tracked %v commits %v available=%t, want no evidence and available comparison", got.TrackedPaths, got.CommitsNotInPullRequest, got.PullRequestComparisonAvailable)
	}
}

func TestLocalGitDeliverableState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		push               bool
		advanceRemoteBase  bool
		deleteRemoteBranch bool
		wantCommitsAhead   int
		wantRemoteBranch   bool
	}{
		{name: "zero ahead without remote branch"},
		{name: "pushed commit", push: true, wantCommitsAhead: 1, wantRemoteBranch: true},
		{name: "pushed commit after remote base advances", push: true, advanceRemoteBase: true, wantCommitsAhead: 1, wantRemoteBranch: true},
		{name: "pushed then deleted remote branch", push: true, deleteRemoteBranch: true, wantCommitsAhead: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
			})
			if err != nil {
				t.Fatalf("NewBackend() error = %v", err)
			}
			provider := backend.(DeliverableStateProvider)
			issue := Issue{Identifier: "DD-DELIVERABLE-" + tt.name}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if tt.push {
				path := filepath.Join(info.Path, "delivered.txt")
				if err := os.WriteFile(path, []byte("delivered\n"), 0o600); err != nil {
					t.Fatalf("write delivered file: %v", err)
				}
				runGit(t, info.Path, "add", "delivered.txt")
				runGit(t, info.Path, "commit", "-m", "test: add delivered work")
				runGit(t, info.Path, "push", "-u", "origin", "HEAD:"+info.Branch)
			}
			if tt.advanceRemoteBase {
				path := filepath.Join(source, "remote-base.txt")
				if err := os.WriteFile(path, []byte("remote base\n"), 0o600); err != nil {
					t.Fatalf("write remote base file: %v", err)
				}
				runGit(t, source, "add", "remote-base.txt")
				runGit(t, source, "commit", "-m", "test: advance remote base")
				runGit(t, source, "push", "origin", "main")
			}
			if tt.deleteRemoteBranch {
				runGit(t, info.Path, "push", "origin", "--delete", info.Branch)
			}

			got, err := provider.DeliverableState(t.Context(), info, issue)
			if err != nil {
				t.Fatalf("DeliverableState() error = %v", err)
			}
			if got.CommitsAhead != tt.wantCommitsAhead || got.RemoteBranchExists != tt.wantRemoteBranch {
				t.Fatalf("DeliverableState() = %+v, want commits ahead=%d remote branch=%t", got, tt.wantCommitsAhead, tt.wantRemoteBranch)
			}
			localHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if got.Remote != "origin" || got.RemoteRef != "refs/heads/"+info.Branch || got.LocalHeadSHA != localHead {
				t.Fatalf("DeliverableState() ref evidence = %+v, want origin refs/heads/%s local %s", got, info.Branch, localHead)
			}
			if tt.wantRemoteBranch {
				remoteFields := strings.Fields(runGit(t, info.Path, "ls-remote", "origin", "refs/heads/"+info.Branch))
				if len(remoteFields) < 1 || got.RemoteHeadSHA != remoteFields[0] {
					t.Fatalf("DeliverableState().RemoteHeadSHA = %q, want ls-remote fields %v", got.RemoteHeadSHA, remoteFields)
				}
			} else if got.RemoteHeadSHA != "" {
				t.Fatalf("DeliverableState().RemoteHeadSHA = %q, want empty", got.RemoteHeadSHA)
			}
		})
	}
}

func TestLocalGitRecoveryStateDetectsAmendedCommit(t *testing.T) {
	t.Parallel()

	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{
		Root:       filepath.Join(t.TempDir(), "workspaces"),
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	provider := backend.(RecoveryStateProvider)
	issue := Issue{Identifier: "DD-RECOVERY-AMEND"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	path := filepath.Join(info.Path, "work.txt")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("write work file: %v", err)
	}
	runGit(t, info.Path, "add", "work.txt")
	runGit(t, info.Path, "commit", "-m", "test: add work")
	first, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() error = %v", err)
	}

	if err := os.WriteFile(path, []byte("other\n"), 0o600); err != nil {
		t.Fatalf("update work file: %v", err)
	}
	runGit(t, info.Path, "add", "work.txt")
	runGit(t, info.Path, "commit", "--amend", "--no-edit")
	amended, err := provider.RecoveryState(context.Background(), info, issue)
	if err != nil {
		t.Fatalf("RecoveryState() after amend error = %v", err)
	}
	if first.UnpushedCommits != amended.UnpushedCommits || first.DiffStat != amended.DiffStat {
		t.Fatalf("amended recovery counts = %+v, want unchanged from %+v", amended, first)
	}
	if first.WorkspaceFingerprint == "" || first.WorkspaceFingerprint == amended.WorkspaceFingerprint {
		t.Fatalf("amended workspace fingerprint = %q, want change from %q", amended.WorkspaceFingerprint, first.WorkspaceFingerprint)
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

func TestGitDiffStopErrorIgnoresCompletedProcess(t *testing.T) {
	t.Parallel()

	waitErr := errors.New("wait")

	tests := []struct {
		name    string
		killErr error
		waitErr error
		wantErr bool
	}{
		{
			name: "no kill error",
		},
		{
			name:    "already done",
			killErr: os.ErrProcessDone,
			waitErr: waitErr,
		},
		{
			name:    "kill denied after successful wait",
			killErr: os.ErrPermission,
		},
		{
			name:    "kill denied before failed wait",
			killErr: os.ErrPermission,
			waitErr: waitErr,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := gitDiffStopError(tt.killErr, tt.waitErr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("gitDiffStopError() error = nil, want error")
				}
				if !errors.Is(err, tt.killErr) {
					t.Fatalf("gitDiffStopError() error = %v, want wrapped %v", err, tt.killErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("gitDiffStopError() error = %v, want nil", err)
			}
		})
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

func TestGitRecoveryBaseFingerprintUsesConfiguredBase(t *testing.T) {
	t.Parallel()

	if got := gitRecoveryBaseFingerprint(t.Context(), "", "base-sha"); got != "base-sha" {
		t.Fatalf("gitRecoveryBaseFingerprint() = %q, want base-sha", got)
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
