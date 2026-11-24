package validationcmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent"
)

func TestLandingRecipeInputsMatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, file, helper string
		want               bool
	}{
		{name: "embedded recipe", helper: "unchanged", want: true},
		{name: "modified PostgreSQL helper rejected", file: "scripts/check-landing.py", helper: "modified"},
		{name: "missing PostgreSQL helper rejected", file: "scripts/check-landing.py", helper: "missing"},
		{name: "modified linter makefile rejected", file: "scripts/check-landing-lint.mk", helper: "modified"},
		{name: "missing linter makefile rejected", file: "scripts/check-landing-lint.mk", helper: "missing"},
		{name: "modified linter build script rejected", file: "scripts/build-custom-linter.sh", helper: "modified"},
		{name: "missing linter build script rejected", file: "scripts/build-custom-linter.sh", helper: "missing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for _, name := range []string{"Makefile", "scripts/check-landing.sh", "scripts/check-evidence.sh", "scripts/check-landing.py", "scripts/check-landing-lint.mk", "scripts/build-custom-linter.sh"} {
				if name == tt.file && tt.helper == "missing" {
					continue
				}
				data, err := fs.ReadFile(detent.ValidationFS(), name)
				if err != nil {
					t.Fatal(err)
				}
				if name == tt.file && tt.helper == "modified" {
					data = []byte("raise SystemExit(0)\n")
				}
				file := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := landingRecipeInputsMatch(root); got != tt.want {
				t.Fatalf("recipe applicability = %t, want %t", got, tt.want)
			}
		})
	}
}
