package changesource

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/internal/gitfixture"
)

func TestCaptureChangeSourceCompressesRevisions(t *testing.T) {
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "packed"
		}
		t.Run(name, func(t *testing.T) {
			source := gitfixture.SourceRepo(t)
			gitfixture.Git(t, source, "config", "gc.auto", "0")
			gitfixture.WriteFile(t, source, ".gitattributes", "*.bin -diff\n")
			gitfixture.WriteFile(t, source, "nested/.gitattributes", "*.bin -text\n")
			gitfixture.Git(t, source, "add", ".gitattributes", "nested/.gitattributes")
			gitfixture.Git(t, source, "commit", "-m", "inherited attributes")
			data := make([]byte, 5<<20)
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSpace(gitfixture.Git(t, source, "rev-parse", "HEAD"))
			for i := range 5 {
				data[i] ^= 1
				if err := os.WriteFile(filepath.Join(source, "nested/blob.bin"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				gitfixture.Git(t, source, "add", "nested/blob.bin")
				if i == 4 {
					gitfixture.Git(t, source, "rm", ".gitattributes")
				}
				gitfixture.Git(t, source, "commit", "-m", "blob revision")
			}
			if packed {
				gitfixture.Git(t, source, "repack", "-ad")
			}
			head := strings.TrimSpace(gitfixture.Git(t, source, "rev-parse", "HEAD"))
			capture, err := Capture(t.Context(), source, base, head)
			if err != nil {
				t.Fatal(err)
			}
			if len(capture.Bundle) >= 2*len(data) {
				t.Fatalf("repeated blob revisions were not compressed: bundle bytes=%d", len(capture.Bundle))
			}
			if err := Verify(t.Context(), capture.Source, capture.Bundle); err != nil {
				t.Fatalf("captured pack cannot be verified without repository objects: %v", err)
			}
		})
	}
}
func TestVerifyChangeSourceKeepsSourceWorktree(t *testing.T) {
	source := gitfixture.SourceRepo(t)
	base := strings.TrimSpace(gitfixture.Git(t, source, "rev-parse", "HEAD"))
	gitfixture.WriteFile(t, source, "README.md", "edited source\n")
	gitfixture.Git(t, source, "commit", "-am", "edit source")
	head := strings.TrimSpace(gitfixture.Git(t, source, "rev-parse", "HEAD"))
	capture, err := Capture(t.Context(), source, base, head)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(source)
	gitDir := filepath.Join(source, ".git")
	t.Setenv("GIT_DIR", gitDir)
	t.Setenv("GIT_COMMON_DIR", gitDir)
	t.Setenv("GIT_WORK_TREE", source)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(gitDir, "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(gitDir, "objects"))
	if err := Verify(t.Context(), capture.Source, capture.Bundle); err != nil {
		t.Error(err)
	}
	if bare := strings.TrimSpace(gitfixture.Git(t, source, "config", "--file", filepath.Join(gitDir, "config"), "--get", "core.bare")); bare != "false" {
		t.Errorf("source core.bare = %q, want false", bare)
	}
}

func TestDescribeUsesPublishedGitObjects(t *testing.T) {
	source := gitfixture.SourceRepo(t)
	base := strings.TrimSpace(gitfixture.Git(t, source, "rev-parse", "HEAD"))
	gitfixture.WriteFile(t, source, "README.md", "edited source\n")
	gitfixture.Git(t, source, "commit", "-am", "edit source")
	head := strings.TrimSpace(gitfixture.Git(t, source, "rev-parse", "HEAD"))
	before, err := Describe(t.Context(), source, base, head)
	if err != nil || !strings.Contains(before.Patch, "+edited source") {
		t.Fatalf("describe original source = %q, %v", before.Patch, err)
	}
	gitfixture.Git(t, source, "replace", head, base)
	after, err := Describe(t.Context(), source, base, head)
	if err != nil || before.Patch != after.Patch || before.Source != after.Source {
		t.Fatalf("local Git replacement changed the review source: patch = %q, digest = %q, error = %v", after.Patch, after.Source.DiffSHA256, err)
	}
	digest, err := Diff(t.Context(), source, base, head)
	if err != nil || digest != before.Source.DiffSHA256 {
		t.Fatalf("local Git replacement changed the recovered source digest: %q, %v", digest, err)
	}
}

func TestMain(m *testing.M) { os.Exit(gitfixture.RunTests(m)) }
