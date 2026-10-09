package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestRunnerProjectCheckoutSelection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		secondClone bool
		wantName    string
		wantErr     string
	}{
		{name: "symlinked workspace root reaches the retained checkout once", wantName: "detent"},
		{name: "two distinct checkouts of one repository are ambiguous", secondClone: true, wantErr: "require selecting the retained source"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			real := filepath.Join(base, "code", "detent-runner")
			gitClone(t, filepath.Join(real, "detent"))
			if test.secondClone {
				gitClone(t, filepath.Join(real, "copy"))
			}
			root := filepath.Join(base, "detent-runner")
			if err := os.Symlink(real, root); err != nil {
				t.Fatal(err)
			}
			retained := []globalconfig.Project{{ID: "detent", Workdir: filepath.Join(real, "detent")}}
			workdir, name, err := runnerProjectCheckout(t.Context(), root, "prj_detent", "digitaldrywood/detent", retained)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || name != test.wantName || workdir != filepath.Join(real, "detent") {
				t.Fatalf("checkout = %q, %q, %v", workdir, name, err)
			}
		})
	}
}

func gitClone(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q", dir}, {"-C", dir, "remote", "add", "origin", "git@github.com:digitaldrywood/detent.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}
