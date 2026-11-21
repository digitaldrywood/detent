package signing

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

func TestSigningProbeKeepsSourceObjects(t *testing.T) {
	source := t.TempDir()
	if _, err := gitexec.Run(t.Context(), source, "init", "--template="); err != nil {
		t.Fatal(err)
	}
	t.Chdir(source)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	gitDir := filepath.Join(source, ".git")
	t.Setenv("GIT_DIR", gitDir)
	t.Setenv("GIT_COMMON_DIR", gitDir)
	t.Setenv("GIT_WORK_TREE", source)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(gitDir, "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(gitDir, "objects"))

	policy := Policy{Report: Report{Identity: Identity{Name: "Example Runner", Email: "runner@example.invalid"}, Signing: "unsigned"}}
	attempted, err := policy.VerifySigning(t.Context())
	if !attempted || err != nil {
		t.Errorf("signing probe attempted = %v, error = %v", attempted, err)
	}
	objects, err := gitexec.ReadPublicationSource(t.Context(), source, "count-objects")
	if err != nil || !strings.HasPrefix(objects, "0 objects,") {
		t.Errorf("signing probe wrote to source object store: %q, %v", objects, err)
	}
	bare, err := gitexec.ReadPublicationSource(t.Context(), source, "config", "--local", "--get", "core.bare")
	if err != nil || bare != "false\n" {
		t.Errorf("source core.bare = %q, want false: %v", bare, err)
	}
}
