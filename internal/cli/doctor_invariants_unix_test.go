//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDoctorInvariantsCancelsDescendants(t *testing.T) {
	root := t.TempDir()
	// The fake Go wrapper leaves a descendant holding its output pipe open.
	script := "#!/bin/sh\nsleep 120 &\nchild=$!\ntrap 'wait \"$child\"; exit 0' TERM\necho $child > child.pid\nwait\n"
	if err := os.WriteFile(filepath.Join(root, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runDoctorInvariants(ctx, root) }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var pid int
	for pid == 0 {
		select {
		case <-deadline.C:
			t.Fatal("descendant did not start")
		case err := <-done:
			t.Fatalf("early exit: %v", err)
		case <-tick.C:
			data, err := os.ReadFile(filepath.Join(root, "child.pid"))
			if err == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			}
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation passed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not return")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("descendant survived cancellation")
	}
}
