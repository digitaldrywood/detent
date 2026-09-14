//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestDoctorInvariantsCancelsDescendants(t *testing.T) {
	root := t.TempDir()
	controlPath := filepath.Join(root, "startup.control")
	if err := syscall.Mkfifo(controlPath, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := os.OpenFile(controlPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	// The fake Go wrapper delays descendant startup on an explicit control
	// condition, then leaves the descendant holding its output pipe open.
	script := "#!/bin/sh\nprintf started > wrapper.started\nread -r release < startup.control\nsleep 120 &\nchild=$!\ntrap 'wait \"$child\"; exit 0' TERM\necho $child > child.pid\nwait\n"
	if err := os.WriteFile(filepath.Join(root, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- runDoctorInvariants(ctx, root)
	}()
	var pid int
	t.Cleanup(func() {
		cancel()
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-joined:
		case <-time.After(testenv.SubprocessWaitTimeout):
			t.Error("doctor invariant command did not join during cleanup")
		}
	})

	waitForDoctorFixtureFile(t, filepath.Join(root, "wrapper.started"), done)
	if _, err := os.Stat(filepath.Join(root, "child.pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant started before release, stat error = %v", err)
	}
	if _, err := release.WriteString("release\n"); err != nil {
		t.Fatal(err)
	}
	data := waitForDoctorFixtureFile(t, filepath.Join(root, "child.pid"), done)
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("descendant pid = %q, error = %v", data, err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation passed")
		}
	case <-time.After(testenv.SubprocessWaitTimeout):
		t.Fatal("cancellation did not return")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("descendant survived cancellation")
	}
}

func waitForDoctorFixtureFile(t *testing.T, path string, done <-chan error) []byte {
	t.Helper()

	deadline := time.NewTimer(testenv.SubprocessWaitTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if len(data) > 0 {
				return data
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read fixture file %s: %v", path, err)
		}
		select {
		case err := <-done:
			t.Fatalf("doctor invariant command exited before %s: %v", filepath.Base(path), err)
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", path)
		case <-tick.C:
		}
	}
}
