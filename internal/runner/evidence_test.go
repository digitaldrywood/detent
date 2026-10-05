package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestWorkspaceEvidenceSource(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	for _, file := range []string{filepath.Join(root, "verification.log"), filepath.Join(outside, "secret.log")} {
		if err := os.WriteFile(file, []byte("browser verification passed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Create(filepath.Join(root, "large.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Truncate(attachment.MaxBytes+1), file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "page.html"), []byte("<html>page</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("verification.log", filepath.Join(root, "inside.log")); err != nil {
		t.Fatal(err)
	}
	source := workspaceEvidenceSource(root)
	for _, test := range []struct {
		name     string
		path     string
		allowed  bool
		tooLarge bool
	}{
		{name: "log", path: "verification.log", allowed: true},
		{name: "symlink within workspace", path: "inside.log", allowed: true},
		{name: "absolute", path: filepath.Join(outside, "secret.log")},
		{name: "parent traversal", path: "../secret.log"},
		{name: "symlink escape", path: "escape/secret.log"},
		{name: "oversize", path: "large.log", tooLarge: true},
		{name: "directory", path: "."},
		{name: "active content", path: "page.html"},
		{name: "missing", path: "missing.log"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := source(t.Context(), test.path)
			if (err == nil) != test.allowed || test.tooLarge && !errors.Is(err, attachment.ErrTooLarge) {
				t.Fatalf("read %q: %+v, %v", test.path, result, err)
			}
			if test.allowed && (result.ContentType != "text/plain" || string(result.Content) != "browser verification passed\n") {
				t.Fatalf("log evidence = %+v", result)
			}
		})
	}
	remote := SSHExecutionSources{evidence: source, directory: outside}
	path, err := json.Marshal("verification.log")
	if err != nil {
		t.Fatal(err)
	}
	result, err := remote.Handle(t.Context(), "source.evidence", []json.RawMessage{path})
	if err != nil || string(result.(ValidationEvidence).Content) != "browser verification passed\n" {
		t.Fatalf("remote workspace evidence = %+v, %v", result, err)
	}
}
