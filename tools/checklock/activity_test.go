package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

func TestValidationOwnerActivity(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, output, phase string }{
		{"build", "go build -o tmp/detent ./cmd/detent\n", "build"},
		{"tests", "bash scripts/test-race-cover.sh\n", "tests"},
		{"generic", "some output\n", "running"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "gate.lock")
			holder := acquireTestLock(t, path)
			var output, diagnostics bytes.Buffer
			recorder := newValidationActivityRecorder(path, &diagnostics)
			writer := validationActivityWriter{output: &output, recorder: recorder}
			if n, err := io.WriteString(writer, tt.output); err != nil || n != len(tt.output) || output.String() != tt.output {
				t.Fatalf("output forwarding = %d, %v, %q", n, err, &output)
			}
			owner, err := instancelock.Inspect(path)
			if err != nil {
				t.Fatal(err)
			}
			activity, ok := readValidationActivity(path, owner)
			if !ok || activity.Phase != tt.phase || activity.Bytes != int64(len(tt.output)) || diagnostics.Len() != 0 {
				t.Fatalf("activity = %+v, known=%t, diagnostics=%s", activity, ok, &diagnostics)
			}
			if err := holder.Close(); err != nil {
				t.Fatal(err)
			}
			acquireTestLock(t, path)
			owner, err = instancelock.Inspect(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := readValidationActivity(path, owner); ok {
				t.Fatal("previous owner's activity accepted")
			}
			if err := os.WriteFile(path+".activity", []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok := readValidationActivity(path, owner); ok {
				t.Fatal("invalid activity accepted")
			}
		})
	}
}

func TestValidationLongHeldOwner(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		output, release bool
		wantWait        time.Duration
	}{
		{"active owner retains ticket", true, true, 35*time.Minute + 700*time.Millisecond},
		{"silent owner expires", false, false, 15*time.Minute + 300*time.Millisecond},
		{"active owner respects total bound", true, false, 40*time.Minute + 800*time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
				defer cancel()
				path := filepath.Join(t.TempDir(), "gate.lock")
				holder := acquireTestLock(t, path)
				recorder := newValidationActivityRecorder(path, io.Discard)
				var older []validationWaiter
				for range 5 {
					w, err := registerValidationWaiter(t.Context(), path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { w.lock.Close() })
					older = append(older, w)
				}
				var diagnostics bytes.Buffer
				started := time.Now()
				calls := 0
				var ticket string
				lock, _, err := acquireValidationLockWithTimeouts(ctx, path, &diagnostics, 15*time.Minute, 40*time.Minute, func(ctx context.Context, path, name string) (int, int, error) {
					if ticket != "" && ticket != name {
						t.Fatal("waiter lost its ticket")
					}
					ticket = name
					if calls > 0 {
						time.Sleep(5 * time.Minute)
						if tt.output {
							recorder.observe([]byte("test output\n"))
						}
						if tt.output && calls <= 3 {
							if err := older[calls-1].lock.Close(); err != nil {
								t.Fatal(err)
							}
						}
						if tt.release && calls == 7 {
							for _, w := range older {
								if err := w.lock.Close(); err != nil {
									t.Fatal(err)
								}
							}
							if err := holder.Close(); err != nil {
								t.Fatal(err)
							}
						}
					}
					calls++
					return validationPosition(ctx, path, name)
				})
				if elapsed := time.Since(started); elapsed != tt.wantWait {
					t.Fatalf("waited %s, want %s", elapsed, tt.wantWait)
				}
				if tt.release {
					if err != nil {
						t.Fatalf("active owner lost progress: %v\n%s", err, &diagnostics)
					}
					defer lock.Close()
					if time.Since(started) < 35*time.Minute {
						t.Fatal("gate acquired before owner release")
					}
				} else {
					if lock != nil || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("timeout = %v, %v", lock, err)
					}
					if tt.output && !strings.Contains(err.Error(), "total queue wait limit") {
						t.Fatal(err)
					}
					inspection, inspectErr := instancelock.Inspect(path)
					if inspectErr != nil || inspection.Status != instancelock.StatusHeld {
						t.Fatal("timeout released owner")
					}
					newer, registerErr := registerValidationWaiter(t.Context(), path)
					if registerErr != nil {
						t.Fatal(registerErr)
					}
					defer newer.lock.Close()
					retry, registerErr := registerValidationWaiter(t.Context(), path)
					if registerErr != nil {
						t.Fatal(registerErr)
					}
					defer retry.lock.Close()
					position, size, positionErr := validationPosition(t.Context(), path, retry.name)
					if positionErr != nil || position != size || retry.name <= newer.name {
						t.Fatalf("retry bypassed FIFO: %d/%d %v", position, size, positionErr)
					}
				}
				if !strings.Contains(diagnostics.String(), "owner_phase_hint=") || (tt.output && !strings.Contains(diagnostics.String(), "reason=owner_output")) {
					t.Fatalf("missing owner evidence: %s", &diagnostics)
				}
			})
		})
	}
}

func TestValidationActivityUnavailable(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing owner", "missing metadata", "unwritable metadata", "future activity"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "gate.lock")
			if mode != "missing owner" {
				acquireTestLock(t, path)
			}
			if mode == "unwritable metadata" {
				if err := os.Mkdir(path+".activity.tmp", 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var diagnostics bytes.Buffer
			if mode != "missing metadata" {
				recorder := newValidationActivityRecorder(path, &diagnostics)
				recorder.observe(nil)
				if mode == "future activity" {
					recorder.activity.At = time.Now().Add(time.Hour)
					recorder.publish()
				}
			}
			owner, err := instancelock.Inspect(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := readValidationActivity(path, owner); ok {
				t.Fatal("unavailable activity accepted")
			}
			if (mode == "missing owner" || mode == "unwritable metadata") && !strings.Contains(diagnostics.String(), "activity unavailable") {
				t.Fatalf("missing diagnostic: %s", &diagnostics)
			}
		})
	}
}
