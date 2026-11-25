//go:build !windows

package detent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMakeLintIgnoresAmbientBinary(t *testing.T) {

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Fatal(err)
	}
	for _, cached := range []bool{false, true} {
		name := "install"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, content string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "Makefile"), readNormalizedFile(t, "Makefile"))
			write(filepath.Join(root, "scripts", "runner-setup.sh"), readNormalizedFile(t, "scripts/runner-setup.sh"))
			write(filepath.Join(root, ".golangci-version"), "v2.9.0\n")
			write(filepath.Join(root, "go.mod"), "module example.com/test\n\ngo 1.26\n\ntoolchain go1.26.6\n")
			bin := filepath.Join(root, "bin")
			write(filepath.Join(bin, "golangci-lint"), "#!/bin/sh\necho incompatible ambient linter >&2\nexit 99\n")
			linter := "#!/bin/sh\nprintf 'pinned:%s:%s\\n' \"$GOTOOLCHAIN\" \"$*\"\n"
			fixture := filepath.Join(root, "pinned-linter")
			write(fixture, linter)
			write(filepath.Join(bin, "go"), "#!/bin/sh\nset -eu\n[ \"$GOTOOLCHAIN\" = go1.26.6 ]\ncase \"$*\" in 'install -p '*' github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0') ;; *) exit 1 ;; esac\ncp \"$DETENT_TEST_PINNED_LINTER\" \"$GOBIN/golangci-lint\"\n")
			if cached {
				cachedPath := filepath.Join(root, "tmp/tools/golangci-lint/v2.9.0/go1.26.6/golangci-lint")
				write(cachedPath, linter)
				old := time.Unix(1, 0)
				if err := os.Chtimes(cachedPath, old, old); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), makePath, "lint")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "DETENT_TEST_PINNED_LINTER="+fixture)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make lint: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "pinned:go1.26.6:run --allow-parallel-runners ") || !strings.Contains(string(output), "--timeout=15m") {
				t.Fatalf("make lint did not invoke the pinned toolchain: %s", output)
			}
			if installed := strings.Contains(string(output), "Installing github.com/golangci/golangci-lint"); installed == cached {
				t.Fatalf("make lint installed = %v, cached = %v: %s", installed, cached, output)
			}
		})
	}
}
