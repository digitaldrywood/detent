//go:build windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

func TestRunWindowsRetainsInheritedOutputOwnership(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelParent), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
			defer cancel()
			commandCtx, stop := context.WithCancel(ctx)
			defer stop()
			path := filepath.Join(t.TempDir(), "validation.lock")
			criticalPath := filepath.Join(t.TempDir(), "critical.lock")
			release := filepath.Join(t.TempDir(), "release")
			t.Setenv("GOCOVERDIR", t.TempDir())
			t.Setenv("DETENT_CHECKLOCK_PARENT_RELEASE", release)
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			ready := newNotifyingWriter()
			var stderr bytes.Buffer
			done := make(chan struct{})
			var code int
			go func() {
				code = run(commandCtx, []string{"-lock", path, "--", os.Args[0], "--", "checklock-inherited-output", criticalPath}, reader, ready, &stderr)
				close(done)
			}()
			t.Cleanup(func() {
				writer.Close()
				stop()
				select {
				case <-done:
				case <-time.After(validationIntegrationTimeout):
					t.Error("validation did not stop after releasing descendant input")
				}
				reader.Close()
			})
			select {
			case <-ready.notified:
			case <-done:
				t.Fatalf("parent exited before readiness: %d: %s", code, &stderr)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			ready.mu.Lock()
			pid, err := strconv.Atoi(strings.TrimSpace(ready.data.String()))
			ready.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			parent, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Release()
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := parent.Wait(); err != nil {
				t.Fatal(err)
			}
			if cancelParent {
				stop()
			}
			select {
			case <-done:
				t.Fatalf("gate released while descendant retained output: %d: %s", code, &stderr)
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for _, heldPath := range []string{path, criticalPath} {
				inspection, err := instancelock.Inspect(heldPath)
				if err != nil || inspection.Status != instancelock.StatusHeld {
					t.Fatalf("descendant ownership lost: %+v, %v", inspection, err)
				}
			}
			if _, err := writer.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			wantCode := 0
			if cancelParent {
				wantCode = 1
			}
			if code != wantCode {
				t.Fatalf("released descendant: code=%d want=%d stderr=%s", code, wantCode, &stderr)
			}
			acquireTestLock(t, path)
			acquireTestLock(t, criticalPath)
		})
	}
}
