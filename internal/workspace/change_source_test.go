package workspace

import (
	"errors"
	"os"
	"path/filepath"
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
		name string
		edit func(*ChangeSource)
	}{
		{"corrupt bundle", func(s *ChangeSource) { s.Bundle = []byte("changed") }},
		{"wrong diff", func(s *ChangeSource) { s.Version.Source.DiffSHA256 = strings.Repeat("a", 64) }},
		{"wrong repository", func(s *ChangeSource) { s.Version.Repository = "https://github.com/other/source" }},
		{"missing legacy source", func(s *ChangeSource) { s.Version.Source = nil; s.Bundle = nil }},
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
			test.edit(retained)
			issue := Issue{Identifier: "native#584", NativeRework: true, Source: retained}
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
