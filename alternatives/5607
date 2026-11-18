//go:build !windows

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunBoundsInheritedOutputDrain(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
	defer cancel()
	path := filepath.Join(t.TempDir(), "validation.lock")
	criticalPath := filepath.Join(t.TempDir(), "critical.lock")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	var stderr bytes.Buffer
	code := run(ctx, []string{
		"-lock", path, "--", "env", "GOCOVERDIR=" + t.TempDir(), os.Args[0],
		"--", "checklock-inherited-output", criticalPath,
	}, reader, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), exec.ErrWaitDelay.Error()) || ctx.Err() != nil {
		t.Fatalf("inherited output drain = %d, context=%v, stderr=%s", code, ctx.Err(), &stderr)
	}
	acquireTestLock(t, path)
	acquireTestLock(t, criticalPath)
}
