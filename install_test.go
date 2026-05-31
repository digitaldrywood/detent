package symphony_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallScriptInstallsBinaryAndRefusesExistingLock(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source-symphony")
	if err := os.WriteFile(source, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	lockPath := filepath.Join(stateDir, "install.lock")
	env := append(os.Environ(),
		"HOME="+tmp,
		"SYMPHONY_INSTALL_SOURCE="+source,
		"SYMPHONY_INSTALL_DIR="+installDir,
		"SYMPHONY_STATE_DIR="+stateDir,
		"SYMPHONY_INSTALL_LOCK="+lockPath,
	)

	first := runInstall(t, root, env)
	if first.err != nil {
		t.Fatalf("first install error = %v\nstdout:\n%s\nstderr:\n%s", first.err, first.stdout, first.stderr)
	}
	if _, err := os.Stat(filepath.Join(installDir, "symphony")); err != nil {
		t.Fatalf("installed binary stat error = %v", err)
	}
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile(lock) error = %v", err)
	}
	if !strings.Contains(string(lock), filepath.Join(installDir, "symphony")) {
		t.Fatalf("lock file = %q, want installed binary path", string(lock))
	}

	second := runInstall(t, root, env)
	if second.err == nil {
		t.Fatal("second install error = nil, want lock failure")
	}
	if !strings.Contains(second.stderr, "already installed") {
		t.Fatalf("second install stderr = %q, want already installed", second.stderr)
	}
}

type installRun struct {
	stdout string
	stderr string
	err    error
}

func runInstall(t *testing.T, root string, env []string) installRun {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "install.sh")
	cmd.Dir = root
	cmd.Env = env

	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return installRun{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}
