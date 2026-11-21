package gitexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestScratchGitKeepsSourceWorktree(t *testing.T) {
	for _, test := range []struct {
		name     string
		workTree bool
		run      func(context.Context, string, string) error
	}{
		{name: "bare init with repository selection", run: func(ctx context.Context, _, scratch string) error {
			_, err := Run(ctx, scratch, "init", "--bare", "--template=")
			return err
		}},
		{name: "bare init with empty directory", run: func(ctx context.Context, _, _ string) error {
			if _, err := ReadPublicationSource(ctx, "", "init", "--bare", "--template="); err == nil {
				return errors.New("scratch initialization accepted an empty path")
			}
			return nil
		}},
		{name: "live diff with source", workTree: true, run: func(ctx context.Context, source, _ string) error {
			return WithDiffInspection(ctx, source, filepath.Join(source, ".git", "index"), "", func(context.Context) error { return nil })
		}},
		{name: "live diff with empty source", workTree: true, run: func(ctx context.Context, source, _ string) error {
			return WithDiffInspection(ctx, "", filepath.Join(source, ".git", "index"), "", func(context.Context) error { return nil })
		}},
		{name: "publication without source", workTree: true, run: func(ctx context.Context, _, _ string) error {
			return withPublicationGit(ctx, "", "", func(string, publicationSource) error { return nil })
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, scratch := t.TempDir(), t.TempDir()
			if _, err := Run(t.Context(), source, "init", "--template="); err != nil {
				t.Fatal(err)
			}
			t.Chdir(source)
			t.Setenv("TMPDIR", scratch)
			t.Setenv("TMP", scratch)
			t.Setenv("TEMP", scratch)
			gitDir := filepath.Join(source, ".git")
			t.Setenv("GIT_DIR", gitDir)
			t.Setenv("GIT_COMMON_DIR", gitDir)
			if test.workTree {
				t.Setenv("GIT_WORK_TREE", source)
			}
			t.Setenv("GIT_INDEX_FILE", filepath.Join(gitDir, "index"))
			t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(gitDir, "objects"))
			err := test.run(t.Context(), source, scratch)
			bare, queryErr := ReadPublicationSource(t.Context(), source, "config", "--file", filepath.Join(gitDir, "config"), "--get", "core.bare")
			if queryErr != nil || bare != "false\n" {
				t.Errorf("source core.bare = %q, want false: %v", bare, queryErr)
			}
			if err != nil {
				t.Error(err)
			}
			if _, err := os.Stat(filepath.Join(source, "HEAD")); !os.IsNotExist(err) {
				t.Errorf("scratch init created HEAD at the source worktree root: %v", err)
			}
		})
	}
}
