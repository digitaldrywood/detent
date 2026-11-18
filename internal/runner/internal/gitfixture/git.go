package gitfixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	ctx := testenv.WaitContext(t, t.Context())
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	procgroup.Configure(ctx, cmd)
	outputFile, err := os.CreateTemp(t.TempDir(), "git-output-*")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = outputFile, outputFile
	runErr := cmd.Run()
	if err := outputFile.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(cmd.Args[1:], " "), runErr, output)
	}
	return string(output)
}

func AddOrigin(t *testing.T, source string) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "origin.git")
	Git(t, source, "init", "--bare", "-b", "main", remote)
	Git(t, source, "remote", "add", "origin", remote)
	Git(t, source, "push", "-u", "origin", "main")
	return remote
}
