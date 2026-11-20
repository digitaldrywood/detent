//go:build unix || windows

package procgroup

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/sessiontimeout"
)

func TestStartAuthorizedWaitsForAcknowledgement(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Setenv("PATH", t.TempDir())
	}
	for _, deny := range []bool{false, true} {
		name := "acknowledged"
		if deny {
			name = "denied"
		}
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "effect")
			rejected := errors.New("registration refused")
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStartAuthorizedWorker$")
			cmd.Env = append(os.Environ(), "DETENT_START_AUTHORIZED_TEST="+marker, "GOCOVERDIR="+t.TempDir())
			cmd.Stdin = strings.NewReader("provider input")
			Configure(t.Context(), cmd)
			t.Cleanup(func() {
				if cmd.Process != nil && cmd.ProcessState == nil {
					_ = TerminateTree(cmd, GroupID(cmd))
					_ = Wait(cmd)
				}
			})
			acknowledged := false
			ctx := WithStartAcknowledgement(t.Context(), func(identity Identity) error {
				acknowledged = identity.PID > 0 && identity.GroupID > 0 && !identity.StartedAt.IsZero()
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					return errors.New("command ran before acknowledgement")
				}
				if deny {
					return rejected
				}
				return nil
			})
			err := StartAuthorized(ctx, cmd)
			if !acknowledged {
				t.Errorf("start callback received no process identity: %v", err)
			}
			if deny {
				if !errors.Is(err, rejected) {
					t.Fatalf("refused launch returned %v", err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused launch created its marker: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := Wait(cmd); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "provider input" {
				t.Fatalf("acknowledged worker did not receive its input: %q %v", data, err)
			}
		})
	}
}

func TestOwnedCommandSuspendsProviderIdleUntilItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifetime := sessiontimeout.Start(t.Context(), time.Minute, 5*time.Minute, nil)
		defer lifetime.Close()
		commandContext, cancel := context.WithTimeout(lifetime.Context(), 2*time.Minute)
		defer cancel()
		stdin, release, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer release.Close()
		defer stdin.Close()
		cmd := exec.CommandContext(commandContext, os.Args[0], "-test.run=^TestStartAuthorizedWorker$")
		cmd.Env = append(os.Environ(), "DETENT_START_AUTHORIZED_TEST="+filepath.Join(t.TempDir(), "effect"), "GOCOVERDIR="+t.TempDir())
		cmd.Stdin = stdin
		var identity Identity
		wait, err := startOwnedCommand(commandContext, cmd, func(_ context.Context, _ *exec.Cmd, id Identity) (func(error, error) error, error) {
			identity = id
			return func(err, cleanupErr error) error { return errors.Join(err, cleanupErr) }, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				cancel()
				_ = wait()
			}
		}()
		<-lifetime.Context().Done()
		var timeout *sessiontimeout.Error
		if err := lifetime.Result(nil); !errors.As(err, &timeout) || timeout.Kind != sessiontimeout.CommandDeadline || timeout.Limit != 2*time.Minute {
			t.Fatalf("timeout = %v, want the two-minute command deadline", err)
		}
		if err := wait(); err == nil {
			t.Fatal("deadline did not terminate the blocked process")
		}
		if alive, err := Alive(identity); alive || err != nil {
			t.Fatalf("command was not retired: alive=%t error=%v", alive, err)
		}
	})
}

func TestStartAuthorizedWorker(t *testing.T) {
	marker := os.Getenv("DETENT_START_AUTHORIZED_TEST")
	if marker == "" {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, input, 0o600); err != nil {
		t.Fatal(err)
	}
}
