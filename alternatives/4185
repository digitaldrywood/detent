package instancelock

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestAcquireExcludesAnotherProcessAndReleases(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "detent.db.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	command := exec.CommandContext(t.Context(), os.Args[0])
	command.Env = append(os.Environ(), "DETENT_INSTANCE_LOCK_HELPER="+path, "GOCOVERDIR="+t.TempDir())
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		input.Close()
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		input.Close()
		if !finished {
			command.Process.Kill()
			command.Wait()
		}
	})
	marker, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		waitErr := command.Wait()
		finished = true
		t.Fatalf("helper readiness: %v; exit = %v, stderr = %s", err, waitErr, &stderr)
	}
	if !strings.HasPrefix(marker, "held\n") {
		t.Fatalf("helper output = %q, want held marker", marker)
	}
	if _, err := io.WriteString(input, "exit\n"); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	finished = true
	if err != nil {
		t.Fatalf("helper process error = %v, stderr = %s", err, &stderr)
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

func TestMain(m *testing.M) {
	if path := os.Getenv("DETENT_INSTANCE_LOCK_HELPER"); path != "" {
		if err := acquireHelperProcess(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func acquireHelperProcess(path string) error {
	lock, err := Acquire(path)
	if errors.Is(err, ErrHeld) {
		if _, err := os.Stdout.WriteString("held\n"); err != nil {
			return fmt.Errorf("write held marker: %w", err)
		}
		if _, err := io.ReadFull(os.Stdin, make([]byte, len("exit\n"))); err != nil {
			return fmt.Errorf("wait for exit: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	if err := lock.Close(); err != nil {
		return fmt.Errorf("close acquired lock: %w", err)
	}
	return errors.New("Acquire() succeeded while parent held lock")
}

func TestDashboardAddressMetadata(t *testing.T) {
	for _, tt := range []struct {
		address string
		invalid bool
	}{
		{"100.111.222.33:4303", false}, {"[::1]:4303", false}, {"127.0.0.1:0", true}, {"host:65536", true}, {"host:123\npid=1", true}, {"missing-port", true},
	} {
		t.Run(tt.address, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "detent.db.lock")
			lock, err := Acquire(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lock.Close(); err != nil {
					t.Error(err)
				}
			})
			err = lock.SetDashboardAddress(tt.address)
			if (err != nil) != tt.invalid {
				t.Fatalf("SetDashboardAddress = %v", err)
			}
			inspection, err := Inspect(path)
			if err != nil || inspection.MetadataError != nil {
				t.Fatalf("Inspect = %+v, %v", inspection, err)
			}
			want := ""
			if !tt.invalid {
				want = tt.address
			}
			if inspection.Owner.DashboardAddress != want {
				t.Fatalf("address=%q want %q", inspection.Owner.DashboardAddress, want)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			inspection, err = Inspect(path)
			if err != nil || inspection.Status != StatusClear || inspection.Owner.DashboardAddress != "" {
				t.Fatalf("released: %+v %v", inspection, err)
			}
		})
	}
}
