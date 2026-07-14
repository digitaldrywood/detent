package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRunCITriggerLabelRemovesAndAddsExistingLabel(t *testing.T) {
	t.Parallel()

	coordinationDir := t.TempDir()
	now := time.Date(2026, 7, 14, 22, 0, 0, 0, time.UTC)
	var calls [][]string
	result, err := runCITriggerLabel(context.Background(), ciTriggerLabelInput{
		Repository:      "digitaldrywood/detent",
		PullRequest:     1313,
		Label:           "ci:ready",
		StaggerSeconds:  15,
		CoordinationDir: coordinationDir,
	}, ciTriggerLabelDeps{
		now: func() time.Time { return now },
		runCommand: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(calls) == 1 {
				return "bug\nci:ready\n", nil
			}
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("runCITriggerLabel() error = %v", err)
	}
	if !result.Reapplied || result.Repository != "digitaldrywood/detent" || result.PullRequest != 1313 || result.Label != "ci:ready" {
		t.Fatalf("result = %#v", result)
	}
	want := [][]string{
		{"gh", "api", "--paginate", "repos/digitaldrywood/detent/issues/1313/labels", "--jq", ".[].name"},
		{"gh", "api", "--method", "DELETE", "repos/digitaldrywood/detent/issues/1313/labels/ci:ready", "--silent"},
		{"gh", "api", "--method", "POST", "repos/digitaldrywood/detent/issues/1313/labels", "-f", "labels[]=ci:ready", "--silent"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
	_, timestampPath := ciTriggerLabelCoordinationPaths(coordinationDir, "digitaldrywood/detent")
	raw, err := os.ReadFile(timestampPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", timestampPath, err)
	}
	if string(raw) != now.Format(time.RFC3339Nano)+"\n" {
		t.Fatalf("timestamp = %q, want %s", raw, now.Format(time.RFC3339Nano))
	}
}

func TestRunCITriggerLabelWaitsForRepositoryStagger(t *testing.T) {
	t.Parallel()

	coordinationDir := t.TempDir()
	now := time.Date(2026, 7, 14, 22, 0, 5, 0, time.UTC)
	_, timestampPath := ciTriggerLabelCoordinationPaths(coordinationDir, "digitaldrywood/detent")
	if err := os.MkdirAll(filepath.Dir(timestampPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(timestampPath, []byte(now.Add(-5*time.Second).Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	var waits []time.Duration
	var calls [][]string
	_, err := runCITriggerLabel(context.Background(), ciTriggerLabelInput{
		Repository:      "digitaldrywood/detent",
		PullRequest:     1314,
		Label:           "ci:ready",
		StaggerSeconds:  15,
		CoordinationDir: coordinationDir,
	}, ciTriggerLabelDeps{
		now: func() time.Time { return now },
		wait: func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			now = now.Add(duration)
			return nil
		},
		runCommand: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, append([]string{name}, args...))
			return "bug\n", nil
		},
	})
	if err != nil {
		t.Fatalf("runCITriggerLabel() error = %v", err)
	}
	if !reflect.DeepEqual(waits, []time.Duration{10 * time.Second}) {
		t.Fatalf("waits = %#v, want 10s stagger", waits)
	}
	if len(calls) != 2 || calls[1][3] != "POST" {
		t.Fatalf("calls = %#v, want list then add without delete", calls)
	}
}

func TestRunCITriggerLabelRejectsZeroStagger(t *testing.T) {
	t.Parallel()

	_, err := runCITriggerLabel(context.Background(), ciTriggerLabelInput{
		Repository:     "digitaldrywood/detent",
		PullRequest:    1314,
		Label:          "ci:ready",
		StaggerSeconds: 0,
	}, ciTriggerLabelDeps{})
	if err == nil || !strings.Contains(err.Error(), "--stagger-seconds must be greater than 0") {
		t.Fatalf("runCITriggerLabel() error = %v, want positive stagger validation", err)
	}
}
