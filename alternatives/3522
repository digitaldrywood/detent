package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestLocalGitCleanupChecksEveryWorkspace(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, record := range []string{"none", "ordinary", "preserved"} {
		for _, work := range []string{"clean", "tracked", "staged", "untracked", "unpushed", "detached", "missing", "broken", "different branch", "no remote"} {
			t.Run(record+"/"+work, func(t *testing.T) {
				t.Parallel()
				source := initSourceRepo(t)
				if work != "no remote" {
					runGit(t, source, "remote", "add", "origin", initBareRemote(t))
					runGit(t, source, "push", "-u", "origin", "main")
				}
				backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				issue := Issue{Identifier: "cleanup-safety"}
				info, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if record != "none" {
					if err := backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
						t.Fatal(err)
					}
				}
				if record == "preserved" {
					if _, err := backend.PreserveIssue(t.Context(), issue); err != nil {
						t.Fatal(err)
					}
				}
				if work == "detached" {
					runGit(t, info.Path, "checkout", "--detach")
				}
				if work != "clean" && work != "no remote" && work != "broken" {
					name := "README.md"
					if work == "untracked" {
						name = "implementation.go"
					}
					if err := os.WriteFile(filepath.Join(info.Path, name), []byte("unique work\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if work != "tracked" && work != "untracked" {
						runGit(t, info.Path, "add", name)
						if work != "staged" {
							runGit(t, info.Path, "commit", "-m", "unique work")
						}
					}
				}
				if work == "different branch" {
					runGit(t, info.Path, "checkout", "--detach", "origin/main")
				}
				head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				status := runGit(t, info.Path, "status", "--porcelain")
				if work == "missing" {
					if err := os.Rename(info.Path, filepath.Join(t.TempDir(), "moved")); err != nil {
						t.Fatal(err)
					}
				}
				if work == "broken" {
					if err := os.Rename(filepath.Join(info.Path, ".git"), filepath.Join(info.Path, "saved-git")); err != nil {
						t.Fatal(err)
					}
				}
				result, err := backend.CleanupIssue(t.Context(), issue)
				if work == "clean" {
					if err != nil || result.Worktrees != 1 || result.Branches != 1 {
						t.Fatalf("cleanup = %+v, %v; want removed workspace and branch", result, err)
					}
					return
				}
				if !errors.Is(err, ErrWorkspacePreserved) || result.Worktrees != 0 || result.Branches != 0 {
					t.Fatalf("cleanup = %+v, %v; want retained work", result, err)
				}
				if !branchExists(t, source, info.Branch) {
					t.Fatal("cleanup deleted branch holding retained work")
				}
				if work != "missing" && work != "broken" {
					if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
						t.Fatalf("head = %q, want %q", got, head)
					}
					if got := runGit(t, info.Path, "status", "--porcelain"); got != status {
						t.Fatalf("status = %q, want %q", got, status)
					}
				}
			})
		}
	}
}

func TestLocalGitCleanupRecordedLanding(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	tests := []struct {
		name          string
		method        string
		terminal      bool
		wrongHead     bool
		verifiedPR    bool
		recorded      bool
		extraCommit   bool
		dirty         bool
		wantPreserved bool
	}{
		{name: "verified merged PR", method: "squash", terminal: true, verifiedPR: true},
		{name: "mismatched PR head", method: "squash", terminal: true, verifiedPR: true, wrongHead: true, wantPreserved: true},
		{name: "verified PR later commit", method: "squash", terminal: true, verifiedPR: true, extraCommit: true, wantPreserved: true},
		{name: "verified PR dirty", method: "squash", terminal: true, verifiedPR: true, dirty: true, wantPreserved: true},
		{name: "active PR proof", method: "squash", verifiedPR: true, wantPreserved: true},
		{name: "squash landed", method: "squash", terminal: true, recorded: true},
		{name: "merge landed", method: "merge", terminal: true, recorded: true},
		{name: "fast-forward landed", method: "fast-forward", terminal: true, recorded: true},
		{name: "nonterminal squash", method: "squash", recorded: true, wantPreserved: true},
		{name: "terminal without landing", method: "squash", terminal: true, wantPreserved: true},
		{name: "work after landing", method: "squash", terminal: true, recorded: true, extraCommit: true, wantPreserved: true},
		{name: "uncommitted work after landing", method: "squash", terminal: true, recorded: true, dirty: true, wantPreserved: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			publishCleanupSource(t, source)
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ProjectID: "native", ID: "wi_1", Identifier: "native#1", Terminal: tt.terminal}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(info.Path, "native.txt"), []byte("delivered work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, info.Path, "add", "native.txt")
			runGit(t, info.Path, "commit", "-m", "native work")
			landedHead := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			runGit(t, info.Path, "push", "origin", info.Branch)
			switch tt.method {
			case "squash":
				runGit(t, source, "merge", "--squash", info.Branch)
				runGit(t, source, "commit", "-m", "squash native work")
			case "merge":
				runGit(t, source, "merge", "--no-ff", "-m", "merge native work", info.Branch)
			case "fast-forward":
				runGit(t, source, "merge", "--ff-only", info.Branch)
			}
			runGit(t, source, "push", "origin", "main")
			runGit(t, source, "push", "origin", "--delete", info.Branch)
			if tt.method == "squash" {
				count, err := retainedGitCommitCount(t.Context(), info.Path, "HEAD")
				if err != nil || count == 0 {
					t.Fatalf("original head should fail live branch proof: count %d, %v", count, err)
				}
			}
			if tt.verifiedPR {
				issue.CleanupDeliveredHeadSHA = landedHead
				if tt.wrongHead {
					issue.CleanupDeliveredHeadSHA = strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
				}
			}
			if tt.recorded {
				issue.LandedHeadSHA = landedHead
			}
			if tt.extraCommit {
				runGit(t, info.Path, "commit", "--allow-empty", "-m", "later work")
			}
			if tt.dirty {
				if err := os.WriteFile(filepath.Join(info.Path, "later.txt"), []byte("later work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := backend.CleanupIssue(t.Context(), issue)
			if got := errors.Is(err, ErrWorkspacePreserved); got != tt.wantPreserved {
				t.Fatalf("CleanupIssue() = %+v, %v; want preserved %t", result, err, tt.wantPreserved)
			}
			if !tt.wantPreserved && err != nil {
				t.Fatalf("CleanupIssue() error = %v", err)
			}
			_, statErr := os.Stat(info.Path)
			if tt.wantPreserved != (statErr == nil) {
				t.Fatalf("workspace present = %t, want %t", statErr == nil, tt.wantPreserved)
			}
			if branchExists(t, source, info.Branch) != tt.wantPreserved {
				t.Fatalf("branch present = %t, want %t", branchExists(t, source, info.Branch), tt.wantPreserved)
			}
		})
	}
}

func TestLocalGitCleanupChecksRemovalHooks(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	skipWindows(t)
	for _, dirty := range []bool{false, true} {
		t.Run(strconv.FormatBool(dirty), func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			publishCleanupSource(t, source)
			trace := filepath.Join(t.TempDir(), "hook-ran")
			cacheRoot := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cacheRoot, "README"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(cacheRoot, "00", fmt.Sprintf("%064x-d", 1))
			if err := os.Mkdir(filepath.Dir(entry), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entry, make([]byte, 2048), 0600); err != nil {
				t.Fatal(err)
			}
			pin := filepath.Join(t.TempDir(), "active-output")
			if err := os.Link(entry, pin); err != nil {
				t.Fatal(err)
			}
			ctx := cache.WithContext(t.Context())
			reserved, registered, finished := false, false, false
			ctx = WithValidationProcessOwner(ctx, func(processCtx context.Context, _ Info, _ Issue, _ string) (ValidationProcess, error) {
				reserved = true
				return validationProcessFixture{
					started: func(procgroup.Identity) error {
						registered = true
						if report, err := cache.Maintain(processCtx, toolcache.Policy{MaxBytes: 1024}, time.Now()); err != nil || report.SizeEvictedBytes != 0 || report.RetainedBytes < 2048 {
							t.Errorf("maintenance did not yield to the running hook: %+v, %v", report, err)
						}
						if data, err := os.ReadFile(entry); err != nil || len(data) != 2048 {
							t.Errorf("running hook's cache entry changed: bytes=%d err=%v", len(data), err)
						}
						return nil
					},
					finished: func(gate.CommandResult, error) error { finished = true; return nil },
				}, nil
			})
			shell := filepath.Join(t.TempDir(), "hook-shell")
			probe := fmt.Sprintf(`{"GOHOSTOS":%q,"GOHOSTARCH":%q,"GOVERSION":"go1.26.0"}`, runtime.GOOS, runtime.GOARCH)
			script := "#!/bin/sh\nif [ \"$2\" = 'go env -json' ]; then\n  printf '%s\\n' " + shellQuote(probe) + "\n  exit 0\nfi\nexec /bin/sh \"$@\"\n"
			if err := os.WriteFile(shell, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			backend, err := NewLocalGit(LocalGitOptions{
				Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true,
				Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
				Hooks:  Hooks{Shell: shell, BeforeRemove: "test \"$GOCACHE\" = " + shellQuote(cacheRoot) + " && touch " + shellQuote(trace) + " && printf 'hook work' > README.md"},
			})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "cleanup-hook", Terminal: true}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			issue.CleanupDeliveredHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			if dirty {
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("worker work"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := backend.CleanupIssue(ctx, issue)
			if !errors.Is(err, ErrWorkspacePreserved) || result.Worktrees != 0 {
				t.Fatalf("cleanup = %+v, %v; want preservation", result, err)
			}
			_, err = os.Stat(trace)
			if dirty != errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("hook trace stat = %v, dirty = %t", err, dirty)
			}
			if reserved != !dirty || registered != !dirty || finished != !dirty {
				t.Fatalf("cleanup hook ownership: reserved=%t registered=%t finished=%t dirty=%t", reserved, registered, finished, dirty)
			}
			if err := os.Remove(pin); err != nil {
				t.Fatal(err)
			}
			if report, err := cache.Maintain(t.Context(), toolcache.Policy{MaxBytes: 1024}, time.Now()); err != nil || report.SizeEvictedBytes < 2048 || report.RetainedBytes > 1024 {
				t.Fatalf("hook retained cache protection after cleanup: %#v, %v", report, err)
			}
			if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unprotected hook cache entry retained: %v", err)
			}
			want := "hook work"
			if dirty {
				want = "worker work"
			}
			if got := readFile(t, filepath.Join(info.Path, "README.md")); got != want {
				t.Fatalf("retained work = %q, want %q", got, want)
			}
		})
	}
}

func TestLocalGitPreservesRevokedWorkAcrossCleanupAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()

	for _, kind := range []string{"tracked", "staged", "untracked", "unpushed", "pushed", "local only"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			if kind != "local only" {
				runGit(t, source, "remote", "add", "origin", remote)
				runGit(t, source, "push", "-u", "origin", "main")
			}
			opts := LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true}
			backend, err := NewLocalGit(opts)
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ProjectID: "detent", ID: "2138", Identifier: "digitaldrywood/detent#2138"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			name := "README.md"
			if kind == "untracked" {
				name = "implementation.go"
			}
			content := []byte("completed worker implementation\n")
			if err := os.WriteFile(filepath.Join(info.Path, name), content, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "staged" || kind == "unpushed" || kind == "pushed" || kind == "local only" {
				runGit(t, info.Path, "add", name)
			}
			if kind == "unpushed" || kind == "pushed" || kind == "local only" {
				runGit(t, info.Path, "commit", "-m", "completed implementation")
			}
			if kind == "pushed" {
				runGit(t, info.Path, "push", "-u", "origin", info.Branch)
			}
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			status := runGit(t, info.Path, "status", "--porcelain")
			preserved, err := backend.PreserveIssue(t.Context(), issue)
			if err != nil || !preserved.Preserved || preserved.Path != info.Path || preserved.HeadSHA != head {
				t.Fatalf("preservation = %#v, error = %v", preserved, err)
			}
			backend, err = NewLocalGit(opts)
			if err != nil {
				t.Fatal(err)
			}
			_, cleanupErr := backend.CleanupIssue(t.Context(), issue)
			if kind != "pushed" {
				if !errors.Is(cleanupErr, ErrWorkspacePreserved) {
					t.Fatalf("cleanup error = %v, want retained workspace", cleanupErr)
				}
				actual, err := os.ReadFile(filepath.Join(info.Path, name))
				if err != nil || string(actual) != string(content) {
					t.Fatalf("retained file = %q, error = %v", actual, err)
				}
				if got := runGit(t, info.Path, "status", "--porcelain"); got != status {
					t.Fatalf("retained index/status = %q, want %q", got, status)
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
					t.Fatalf("retained head = %q, want %q", got, head)
				}
				residual, err := backend.ReconcileResiduals(t.Context(), nil)
				if err != nil || residual.Removed != 0 || residual.PreservedSkipped != 1 {
					t.Fatalf("residual cleanup = %#v, error = %v", residual, err)
				}
				resumed, err := backend.Create(t.Context(), issue)
				if err != nil || resumed.Path != info.Path || resumed.Created {
					t.Fatalf("resumed workspace = %#v, error = %v", resumed, err)
				}
				if kind != "unpushed" && kind != "local only" {
					runGit(t, info.Path, "add", name)
					runGit(t, info.Path, "commit", "-m", "publish recovered implementation")
				}
				if kind == "local only" {
					runGit(t, source, "remote", "add", "origin", remote)
				}
				runGit(t, info.Path, "push", "-u", "origin", info.Branch)
				_, cleanupErr = backend.CleanupIssue(t.Context(), issue)
			}
			if cleanupErr != nil {
				t.Fatalf("cleanup after delivery: %v", cleanupErr)
			}
			if _, err := os.Stat(info.Path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("delivered workspace remains: %v", err)
			}
		})
	}
}

func TestLocalGitPreservationVerifiesWorkEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, kind := range []string{"clean base", "dirty", "local commit", "pushed commit", "base branch pushed", "remote branch deleted"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "evidence", BaseRef: base}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			changed := kind == "dirty" || kind == "local commit" || kind == "pushed commit" || kind == "remote branch deleted"
			if changed {
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("worker implementation\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if kind != "dirty" {
					runGit(t, info.Path, "add", "README.md")
					runGit(t, info.Path, "commit", "-m", "implement change")
				}
			}
			if kind == "pushed commit" || kind == "base branch pushed" || kind == "remote branch deleted" {
				runGit(t, info.Path, "push", "origin", info.Branch)
			}
			if kind == "remote branch deleted" {
				runGit(t, remote, "update-ref", "-d", "refs/heads/"+info.Branch)
			}
			preserved, err := backend.PreserveIssue(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if !preserved.LocalChangesVerified {
				t.Fatalf("unverified local evidence: %#v", preserved)
			}
			localWork := preserved.UnpushedCommits > 0 || len(preserved.TrackedPaths) > 0 || len(preserved.UntrackedPaths) > 0
			if localWork != (kind == "dirty" || kind == "local commit" || kind == "remote branch deleted") {
				t.Fatalf("local evidence = %#v", preserved)
			}
			delivery := preserved.Delivery
			delivered := delivery != nil && delivery.RemoteBranchExists && delivery.CommitsAhead > 0 && delivery.LocalHeadSHA == delivery.RemoteHeadSHA
			if delivered != (kind == "pushed commit") {
				t.Fatalf("delivery evidence = %#v", preserved)
			}
		})
	}
}

func TestLocalGitPreservationInspectionFailureKeepsFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: initSourceRepo(t), AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "detent#2138"}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PreserveIssue(t.Context(), issue); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(info.Path, ".git"), filepath.Join(info.Path, "saved-git-pointer")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
		t.Fatalf("cleanup error = %v, want preservation on failed inspection", err)
	}
	result, err := backend.ReconcileResiduals(t.Context(), nil)
	if err != nil || result.PreservedSkipped != 1 || result.Removed != 0 {
		t.Fatalf("residual cleanup = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(info.Path, "README.md")); err != nil {
		t.Fatalf("retained file missing: %v", err)
	}
}

func TestFilesystemPreservationSurvivesRestartAndResumption(t *testing.T) {
	t.Parallel()
	for _, artifact := range []bool{false, true} {
		name := "empty"
		if artifact {
			name = "artifact"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := FilesystemOptions{Root: filepath.Join(t.TempDir(), "workspaces")}
			backend, err := NewFilesystem(opts)
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ProjectID: "video", ID: "2138", Identifier: "video#2138"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			artifactPath := filepath.Join(info.Path, "artifacts", "render.mp4")
			if artifact {
				if err := os.WriteFile(artifactPath, []byte("completed render"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := backend.DiffStat(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			preserver, ok := any(backend).(IssuePreserver)
			if !ok {
				t.Fatal("filesystem backend does not support preservation")
			}
			preserved, err := preserver.PreserveIssue(t.Context(), issue)
			if err != nil || !preserved.Preserved || preserved.Path != info.Path {
				t.Fatalf("preservation = %#v, error = %v", preserved, err)
			}
			backend, err = NewFilesystem(opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, resumed := range []bool{false, true} {
				if resumed {
					resumedInfo, err := backend.Create(t.Context(), issue)
					if err != nil || resumedInfo.Created || resumedInfo.Path != info.Path {
						t.Fatalf("resumption = %#v, error = %v", resumedInfo, err)
					}
				}
				if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
					t.Fatalf("cleanup error = %v, want preservation", err)
				}
			}
			after, err := backend.DiffStat(t.Context(), info, issue)
			if err != nil || after != before {
				t.Fatalf("retained artifact evidence = %#v, error = %v, want %#v", after, err, before)
			}
			if artifact {
				content, err := os.ReadFile(artifactPath)
				if err != nil || string(content) != "completed render" {
					t.Fatalf("retained artifact = %q, error = %v", content, err)
				}
			}
		})
	}
}
