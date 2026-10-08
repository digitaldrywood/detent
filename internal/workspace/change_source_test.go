package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalGitChangeSourceRefusesUnverifiedRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	source := initSourceRepo(t)
	remote := initBareRemote(t)
	repository := "https://github.com/example/source"
	runGit(t, source, "remote", "add", "origin", repository)
	runGit(t, source, "config", "url."+remote+".insteadOf", repository)
	runGit(t, source, "push", "-u", "origin", "main")
	base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("reviewed source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "source.txt")
	runGit(t, source, "commit", "-m", "unpublished source")
	head := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	capture, err := CaptureChangeSource(t.Context(), source, base, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		edit      func(*ChangeSource)
		pending   string
		published bool
	}{
		{name: "corrupt bundle", edit: func(s *ChangeSource) { s.Bundle = []byte("changed") }},
		{name: "wrong diff", edit: func(s *ChangeSource) { s.Version.Source.DiffSHA256 = strings.Repeat("a", 64) }},
		{name: "wrong repository", edit: func(s *ChangeSource) { s.Version.Repository = "https://github.com/other/source" }},
		{name: "missing legacy source", edit: func(s *ChangeSource) { s.Version.Source = nil; s.Bundle = nil }},
		{name: "staged divergence survives restoration", pending: "index"},
		{name: "mixed staged working and untracked source survives restoration", pending: "mixed"},
		{name: "staged addition with missing working file survives restoration", pending: "addition"},
		{name: "staged deletion with restored working file survives restoration", pending: "deletion"},
		{name: "working source survives restoration", pending: "working"},
		{name: "untracked source survives restoration", pending: "untracked"},
		{name: "newer clean published head survives older source restoration", published: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "second")
			runGit(t, source, "clone", "--no-local", remote, dir)
			runGit(t, dir, "remote", "set-url", "origin", repository)
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: dir, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			metadata := capture.Source
			retained := &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: head, Repository: repository, Source: &metadata}}, Bundle: capture.Bundle}
			if test.edit != nil {
				test.edit(retained)
			}
			issue := Issue{Identifier: "native#584", NativeRework: true, Source: retained, BaseRef: base}
			if test.published {
				runGit(t, dir, "config", "url."+remote+".insteadOf", repository)
				initial := issue
				initial.Source = nil
				info, err := backend.Create(t.Context(), initial)
				if err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "push", "origin", head+":refs/heads/retained-work")
				runGit(t, dir, "fetch", "origin")
				runGit(t, info.Path, "reset", "--hard", head)
				writeFileDiffFile(t, info.Path, "source.txt", "newer completed work\n")
				runGit(t, info.Path, "add", "source.txt")
				runGit(t, info.Path, "-c", "user.name=Recovery Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "newer completed work")
				newer := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				runGit(t, info.Path, "push", "origin", "HEAD:refs/heads/published-work")
				runGit(t, dir, "fetch", "origin")
				before, err := backend.(RecoveryStateProvider).RecoveryState(t.Context(), info, initial)
				if err != nil || before.HeadSHA != newer || before.UnpushedCommits != 0 || len(before.TrackedPaths) != 0 || len(before.UntrackedPaths) != 0 {
					t.Fatalf("fixture lacks a clean newer published head: %+v, %v", before, err)
				}
				_, restoreErr := backend.Create(t.Context(), issue)
				var refusal *LandRefusal
				if !errors.As(restoreErr, &refusal) || refusal.Kind != LandRefusalHeadMoved {
					t.Errorf("older retained source must require explicit reconciliation: %v", restoreErr)
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != newer {
					t.Errorf("source restoration replaced newer published head: got=%s want=%s", got, newer)
				}
				if got := readFile(t, filepath.Join(info.Path, "source.txt")); got != "newer completed work\n" {
					t.Errorf("source restoration replaced newer published bytes: %q", got)
				}
				return
			}
			if test.pending != "" {
				runGit(t, dir, "config", "url."+remote+".insteadOf", repository)
				initial := issue
				initial.Source = nil
				info, err := backend.Create(t.Context(), initial)
				if err != nil {
					t.Fatal(err)
				}
				provider := backend.(RecoveryStateProvider)
				clean, err := provider.RecoveryState(t.Context(), info, initial)
				if err != nil {
					t.Fatal(err)
				}
				if clean.HeadSHA != base || clean.UnpushedCommits != 0 || len(clean.TrackedPaths) != 0 || len(clean.UntrackedPaths) != 0 {
					t.Fatalf("initial source is not the clean published base: %+v", clean)
				}
				wantFiles := map[string]string{"README.md": "source repo\n"}
				wantTracked := []string{"README.md"}
				var wantUntracked []string
				switch test.pending {
				case "index", "mixed":
					writeFileDiffFile(t, info.Path, "README.md", "staged source B\n")
					runGit(t, info.Path, "add", "README.md")
					if test.pending == "mixed" {
						wantFiles["README.md"] = "working source C\n"
						wantFiles["pending.go"] = "untracked sentinel\n"
						wantUntracked = []string{"pending.go"}
						writeFileDiffFile(t, info.Path, "pending.go", wantFiles["pending.go"])
					}
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
					if got := runGit(t, info.Path, "show", ":README.md"); got != "staged source B\n" {
						t.Fatalf("fixture lacks staged divergence: %q", got)
					}
				case "addition":
					writeFileDiffFile(t, info.Path, "added.go", "staged addition\n")
					runGit(t, info.Path, "add", "added.go")
					if err := os.Remove(filepath.Join(info.Path, "added.go")); err != nil {
						t.Fatal(err)
					}
					wantTracked = []string{"added.go"}
				case "deletion":
					runGit(t, info.Path, "rm", "README.md")
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
					wantUntracked = []string{"README.md"}
				case "working":
					wantFiles["README.md"] = "working source C\n"
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
				case "untracked":
					wantTracked = nil
					wantUntracked = []string{"pending.go"}
					wantFiles["pending.go"] = "untracked sentinel\n"
					writeFileDiffFile(t, info.Path, "pending.go", wantFiles["pending.go"])
				}
				if test.pending == "index" || test.pending == "addition" {
					if got := runGit(t, info.Path, "diff", "HEAD", "--"); got != "" {
						t.Fatalf("fixture working bytes differ from HEAD: %q", got)
					}
				}
				staged := runGit(t, info.Path, "ls-files", "--stage", "-z")
				observed, err := provider.RecoveryState(t.Context(), info, initial)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(observed.TrackedPaths, wantTracked) || !slices.Equal(observed.UntrackedPaths, wantUntracked) || observed.DiffStat.IsEmpty() || observed.WorkspaceFingerprint == clean.WorkspaceFingerprint {
					t.Errorf("pending source was not observed: %+v; want tracked=%v untracked=%v", observed, wantTracked, wantUntracked)
				}
				if got := runGit(t, info.Path, "ls-files", "--stage", "-z"); got != staged {
					t.Errorf("observation changed staged entries: got=%q want=%q", got, staged)
				}
				if test.pending == "index" {
					writeFileDiffFile(t, info.Path, "README.md", "alternate staged source\n")
					runGit(t, info.Path, "add", "README.md")
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
					changed, err := provider.RecoveryState(t.Context(), info, initial)
					if err != nil || changed.WorkspaceFingerprint == observed.WorkspaceFingerprint {
						t.Errorf("staged content changed without changing recovery identity: %+v, %v", changed, err)
					}
					writeFileDiffFile(t, info.Path, "README.md", "staged source B\n")
					runGit(t, info.Path, "add", "README.md")
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
				}
				_, restoreErr := backend.Create(t.Context(), issue)
				if restoreErr != nil {
					t.Logf("source restoration refused: %v", restoreErr)
				}
				if got := runGit(t, info.Path, "ls-files", "--stage", "-z"); got != staged {
					t.Errorf("source restoration discarded staged entries: got=%q want=%q", got, staged)
				}
				for path, want := range wantFiles {
					if got := readFile(t, filepath.Join(info.Path, path)); got != want {
						t.Errorf("source restoration changed %s working bytes: got=%q want=%q", path, got, want)
					}
				}
				if test.pending == "addition" {
					if _, err := os.Stat(filepath.Join(info.Path, "added.go")); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("source restoration changed missing working file: %v", err)
					}
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != base {
					t.Errorf("source restoration moved pending-source head: got=%s want=%s", got, base)
				}
				return
			}
			if _, err := backend.Create(t.Context(), issue); err == nil {
				t.Fatal("unverified source started a worker checkout")
			} else if test.name == "missing legacy source" && !strings.Contains(err.Error(), "source-owning runner") {
				t.Fatalf("legacy recovery lacks a concrete supported action: %v", err)
			}
			if _, err := backend.(*LocalGit).Existing(issue); !errors.Is(err, ErrMissingWorkspace) {
				t.Fatalf("refused recovery issued a fresh base checkout: %v", err)
			}
		})
	}
	for _, path := range []string{".detent/environment.md", ".env.local", "keys/certificate.p12"} {
		t.Run("capture excludes "+path, func(t *testing.T) {
			runGit(t, source, "reset", "--hard", head)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(source, path)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, path), []byte("local host setup\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "--force", path)
			runGit(t, source, "commit", "-m", "private fixture")
			privateHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			capture, err := CaptureChangeSource(t.Context(), source, base, privateHead)
			if !errors.Is(err, ErrCheckpointUnsafe) || len(capture.Bundle) != 0 {
				t.Fatalf("source capture uploaded host-local material: bytes=%d, %v", len(capture.Bundle), err)
			}
		})
	}
}
