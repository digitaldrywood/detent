package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestValidationEvents(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		args    []string
		want    string
		missing bool
	}{
		{"passed", []string{"-test.run=^$"}, "passed", false},
		{"failed", []string{"-invalid-test-flag"}, "failed", false},
		{"lookup failed", nil, "failed", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "events.jsonl")
			command := os.Args[0]
			if tt.missing {
				command = filepath.Join(dir, "missing-command")
			}
			args := append([]string{"-lock", filepath.Join(dir, "gate.lock"), "-events", path, "--", command}, tt.args...)
			var stderr bytes.Buffer
			code := run(t.Context(), args, nil, io.Discard, &stderr)
			if (code == 0) != (tt.want == "passed") {
				t.Fatalf("run = %d: %s", code, &stderr)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 4 {
				t.Fatalf("events = %s", data)
			}
			var runID string
			for i, phase := range []string{"waiting", "queued", "running", tt.want} {
				var event validationEvent
				if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					runID = event.RunID
				}
				if event.RunID != runID {
					t.Fatal("run identity changed")
				}
				if i == 3 && (event.HoldSeconds <= 0 || event.HoldSeconds < event.RunSeconds) {
					t.Fatalf("hold does not include execution: %+v", event)
				}
				if event.RunID == "" || event.Host == "" || event.Schema != 1 || event.Phase != phase || event.PID != os.Getpid() || len(event.CommandHash) != 64 || event.At.Before(event.StartedAt) || event.WaitSeconds < 0 || event.RunSeconds < 0 {
					t.Fatalf("event = %+v, want phase %s", event, phase)
				}
			}
			if !strings.Contains(stderr.String(), "validation gate finished: result="+tt.want) {
				t.Fatalf("missing terminal progress: %s", &stderr)
			}
		})
	}
}

func TestValidationEventWriteFailure(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	validationEvent{}.write(failingEventWriter{}, &stderr, "waiting")
	if !strings.Contains(stderr.String(), "write validation event") {
		t.Fatalf("missing event error: %s", &stderr)
	}
}

func TestValidationEventsWaitFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"canceled", "deadline", "parent deadline"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				lockPath := filepath.Join(dir, "gate.lock")
				acquireTestLock(t, lockPath)
				ctx, cancel := context.WithCancel(t.Context())
				if mode == "parent deadline" {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
				}
				defer cancel()
				if mode == "canceled" {
					cancel()
				}
				path := filepath.Join(dir, "events.jsonl")
				var stderr bytes.Buffer
				args := []string{"-lock", lockPath, "-wait-timeout", time.Second.String(), "-events", path, "--", os.Args[0], "-test.run=^$"}
				if code := run(ctx, args, nil, io.Discard, &stderr); code != 1 {
					t.Fatalf("run = %d: %s", code, &stderr)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				wantLines := 2
				if mode != "canceled" {
					wantLines = 3
				}
				if len(lines) != wantLines {
					t.Fatalf("wait failure events = %s", data)
				}
				var event validationEvent
				if err := json.Unmarshal([]byte(lines[len(lines)-1]), &event); err != nil {
					t.Fatal(err)
				}
				if event.Phase != "wait_failed" || event.RunSeconds != 0 || (mode == "deadline" && event.WaitSeconds != 1) || (mode == "parent deadline" && event.WaitSeconds != 0.5) {
					t.Fatalf("wait attributed as run: %+v", event)
				}
			})
		})
	}
}

type failingEventWriter struct{}

func (failingEventWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestValidationUnchangedQueueHistory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		lockPath := filepath.Join(dir, "gate.lock")
		acquireTestLock(t, lockPath)
		path := filepath.Join(dir, "events.jsonl")
		args := []string{"-lock", lockPath, "-wait-timeout", "65s", "-events", path, "--", os.Args[0], "-test.run=^$"}
		if code := run(t.Context(), args, nil, io.Discard, io.Discard); code != 1 {
			t.Fatalf("run = %d", code)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		wants := []struct {
			phase  string
			waited float64
		}{
			{"waiting", 0}, {"queued", 0}, {"queued", 30}, {"queued", 60}, {"wait_failed", 65},
		}
		if len(lines) != len(wants) {
			t.Fatalf("events = %s", data)
		}
		for i, want := range wants {
			var event validationEvent
			if err := json.Unmarshal([]byte(lines[i]), &event); err != nil {
				t.Fatal(err)
			}
			if event.Phase != want.phase || event.WaitSeconds != want.waited || event.HoldSeconds != 0 {
				t.Fatalf("event %d = %+v, want %+v", i, event, want)
			}
		}
	})
}
