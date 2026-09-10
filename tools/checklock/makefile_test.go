package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMakeCheckGeneratedPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Makefile requires POSIX shell commands")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Fatal(err)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		stale     bool
		change    bool
		wantTrace string
		wantError bool
	}{
		{"stale before admission", true, false, "migrations\ngenerated\n", true},
		{"changed during admission", false, true, "migrations\ngenerated\nlock\nmigrations\ngenerated\n", true},
		{"unchanged inputs", false, false, "migrations\ngenerated\nlock\nmigrations\ngenerated\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			write("Makefile", string(makefile), 0o600)
			write(".golangci-version", "test", 0o600)
			write("go.mod", "module fixture\n", 0o600)
			write("input", "current", 0o600)
			document := "current"
			if tt.stale {
				document = "stale"
			}
			write("reference", document, 0o600)
			if tt.change {
				write("change-at-admission", "", 0o600)
			}
			write("git", "#!/bin/sh\npwd\n", 0o700)
			write("go", `#!/bin/sh
set -eu
case "$*" in
  'run ./tools/migrationcheck')
    echo migrations >> trace
    ;;
  'run ./internal/config/cmd/configdoc -root . -check')
    echo generated >> trace
    cmp input reference
    ;;
  'run ./tools/checklock '*)
    echo lock >> trace
    if [ -f change-at-admission ]; then
      echo changed > input
    fi
    while [ "$1" != '--' ]; do shift; done
    shift
    exec "$@"
    ;;
  *) exit 99 ;;
esac
`, 0o700)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("MAKEFLAGS", "")
			t.Setenv("MFLAGS", "")
			t.Setenv("MAKEOVERRIDES", "")
			ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "make", "check", "VERSION=test", "COMMIT=test", "DATE=test", "GOLANGCI_LINT_VERSION=test", "MAKE=make -o build -o lint -o vet -o nilaway-audit -o test-race-cover")
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			if (err != nil) != tt.wantError {
				t.Fatalf("make check error = %v, want error %v\n%s", err, tt.wantError, output)
			}
			trace, err := os.ReadFile(filepath.Join(dir, "trace"))
			if err != nil {
				t.Fatal(err)
			}
			if string(trace) != tt.wantTrace {
				t.Errorf("trace = %q, want %q\n%s", trace, tt.wantTrace, output)
			}
			if got := strings.Contains(string(output), "All checks passed."); got == tt.wantError {
				t.Errorf("success message = %v, want %v\n%s", got, !tt.wantError, output)
			}
		})
	}
}
