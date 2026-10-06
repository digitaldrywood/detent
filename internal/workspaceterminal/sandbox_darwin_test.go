package workspaceterminal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestSandboxRootAuthority(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSandboxRoot(root); err != nil {
		t.Fatal(err)
	}
	t.Run("canceled sandbox construction", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		service, err := newService(ctx, root, "/bin/sh", workspacesession.IsolationSandbox, nil)
		if service != nil || !errors.Is(err, ErrSandboxIsolation) || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled construction = %v, %v; want sandbox refusal wrapping cancellation", service, err)
		}
	})
	if err := validateSandboxRoot("/"); !errors.Is(err, ErrSandboxIsolation) {
		t.Fatalf("filesystem root accepted: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSandboxRoot(home); !errors.Is(err, ErrSandboxIsolation) {
		t.Fatalf("runner home accepted: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateSandboxRoot(alias); !errors.Is(err, ErrSandboxIsolation) {
		t.Fatalf("replaced root accepted: %v", err)
	}
}

func TestSandboxMountContainment(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "worktree")
	nested := filepath.Join(root, "nested")
	sibling := filepath.Join(base, "worktree-sibling")
	for _, directory := range []string{nested, sibling} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "mount-alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		path   string
		within bool
	}{
		{name: "root mount", path: root, within: true},
		{name: "nested mount", path: nested, within: true},
		{name: "mountpoint alias resolves into worktree", path: alias, within: true},
		{name: "sibling prefix is outside", path: sibling},
		{name: "containing filesystem is outside", path: base},
	} {
		t.Run(test.name, func(t *testing.T) {
			within, err := mountWithinRoot(root, test.path)
			if err != nil || within != test.within {
				t.Fatalf("mount containment = %v, %v; want %v", within, err, test.within)
			}
		})
	}
}

func TestSandboxChildCannotDetach(t *testing.T) {
	if AvailableIsolation() != workspacesession.IsolationSandbox {
		t.Skip("actual host enforcement is unavailable; this is not sandbox acceptance evidence")
	}
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	helper := filepath.Join(root, "helper")
	destination, err := os.OpenFile(helper, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	service, err := New(t.Context(), root, "/bin/sh", workspacesession.IsolationSandbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := service.Open(t.Context(), workspacesession.TerminalOpen{}, (&collector{}).emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(terminal.Close)
	command := shellQuote(helper) + " -test.run='^TestSandboxChildHelper$' -- terminal-sandbox-helper; exit $?\n"
	if err := terminal.Write([]byte(command)); err != nil {
		t.Fatal(err)
	}
	if result := terminal.Wait(); result.ExitCode != 0 {
		t.Fatalf("child escaped terminal lifetime confinement: %+v", result)
	}
}

func TestSandboxChildHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "terminal-sandbox-helper" {
		return
	}
	if _, err := unix.Setsid(); !errors.Is(err, unix.EPERM) {
		t.Fatalf("setsid = %v, want sandbox refusal", err)
	}
	if err := unix.Setpgid(0, 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("setpgid = %v, want sandbox refusal", err)
	}
	const syscallPosixSpawn = 244
	if _, _, err := unix.Syscall6(syscallPosixSpawn, 0, 0, 0, 0, 0, 0); !errors.Is(err, unix.EPERM) {
		t.Fatalf("posix_spawn = %v, want sandbox refusal", err)
	}
	if _, err := unix.SysctlRaw("kern.procargs2", os.Getppid()); err == nil {
		t.Fatal("sandbox exposed another process's arguments and environment")
	}
}
