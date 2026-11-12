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
func TestMain(m *testing.M) { os.Exit(gitfixture.RunTests(m)) }
