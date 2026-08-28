package instancelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireExcludesAnotherProcessAndReleases(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAcquireHelperProcess$")
	command.Env = append(os.Environ(), "DETENT_INSTANCE_LOCK_HELPER="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process error = %v, output = %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); !strings.HasPrefix(got, "held\n") {
		t.Fatalf("helper output = %q, want held marker", got)
	}

	if err := lock.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reacquired, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() after Close error = %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("reacquired Close() error = %v", err)
	}
}

func TestAcquireReportsLiveOwner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	_, err = Acquire(path)
	var heldErr *HeldError
	if !errors.As(err, &heldErr) {
		t.Fatalf("Acquire() error = %v, want HeldError", err)
	}
	if heldErr.Owner.PID != os.Getpid() {
		t.Fatalf("holder PID = %d, want %d", heldErr.Owner.PID, os.Getpid())
	}
	if heldErr.Owner.Hostname == "" || heldErr.Owner.StartedAt.IsZero() {
		t.Fatalf("holder = %+v, want hostname and start time", heldErr.Owner)
	}
}

func TestAcquireRecoversStaleOwnerAndClearsOnClose(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	startedAt := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	contents := "pid=987654\nhostname=old-host\nstarted_at=" + startedAt.Format(time.RFC3339Nano) + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	inspection, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if inspection.Status != StatusStale || inspection.Owner.PID != 987654 {
		t.Fatalf("inspection = %+v, want stale owner", inspection)
	}

	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	recovery, ok := lock.Recovery()
	if !ok || recovery.Owner.PID != 987654 || !recovery.Owner.StartedAt.Equal(startedAt) {
		t.Fatalf("Recovery() = %+v, %v, want stale owner", recovery, ok)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	inspection, err = Inspect(path)
	if err != nil {
		t.Fatalf("Inspect() after Close error = %v", err)
	}
	if inspection.Status != StatusClear {
		t.Fatalf("inspection after Close = %+v, want clear", inspection)
	}
}

func TestAcquireRecoversLegacyPIDOnlyLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	if err := os.WriteFile(path, []byte("pid=987654\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	recovery, ok := lock.Recovery()
	if !ok || recovery.Owner.PID != 987654 || recovery.MetadataError == nil {
		t.Fatalf("Recovery() = %+v, %v, want legacy stale owner", recovery, ok)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestAcquireHelperProcess(t *testing.T) {
	path := os.Getenv("DETENT_INSTANCE_LOCK_HELPER")
	if path == "" {
		t.Skip("helper process")
	}

	lock, err := Acquire(path)
	if errors.Is(err, ErrHeld) {
		if _, err := os.Stdout.WriteString("held\n"); err != nil {
			t.Fatalf("WriteString() error = %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	t.Fatal("Acquire() succeeded while parent held lock")
}
