package instancelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireExcludesAnotherProcessAndReleases(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestAcquireHelperProcess$")
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
