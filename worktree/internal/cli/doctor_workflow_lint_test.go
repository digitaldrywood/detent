package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDoctorWindowsCommandUsesConfiguredPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	for _, name := range []string{"project-check", "project-check.local"} {
		want := filepath.Join(binDir, name+".EXE")
		if err := os.WriteFile(want, []byte("example"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		got, err := resolveDoctorWindowsCommand(root, []string{
			`Path=C:\\Windows\\System32`,
			"PATH=bin",
			"PATHEXT=.EXE;.CMD",
		}, name)
		if err != nil {
			t.Fatalf("resolveDoctorWindowsCommand(%q) error = %v", name, err)
		}
		if got != want {
			t.Fatalf("resolveDoctorWindowsCommand(%q) = %q, want %q", name, got, want)
		}
	}
}
