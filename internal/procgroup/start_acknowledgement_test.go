//go:build unix || windows

package procgroup

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
