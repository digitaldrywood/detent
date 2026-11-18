package hooklifetime_test

import (
	"bufio"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestStartupRecoveryRequiresAttemptOwnership(t *testing.T) {
	for _, marked := range []bool{false, true} {
		name := "unmarked child inside scratch"
		if marked {
			name = "marked child outside scratch"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			scratch, err := workspace.PrepareWorkerScratch(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := workspace.CleanupWorkerScratch(root, scratch); err != nil {
					t.Error(err)
				}
			})
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "echo ready; read value")
			cmd.Dir = scratch
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			if marked {
				cmd.Dir = root
				procgroup.SetTempDir(cmd, scratch)
			}
			procgroup.Configure(t.Context(), cmd)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := procgroup.Start(cmd); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var childErr error
			go func() { childErr = procgroup.Wait(cmd); close(done) }()
			t.Cleanup(func() { _ = input.Close(); <-done })
			if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("shell readiness: %q %v", line, err)
			}
			n, err := workspace.ReapWorkerArtifactPathProcesses(t.Context(), root, scratch, time.Second)
			if marked {
				if err != nil || n != 1 {
					t.Fatalf("verified child recovery: reaped=%d error=%v", n, err)
				}
				select {
				case <-done:
					if childErr == nil {
						t.Fatal("verified child exited without a signal")
					}
				case <-testenv.WaitContext(t, t.Context()).Done():
					t.Fatal("verified child survived recovery")
				}
				return
			}
			if err == nil || n != 0 {
				t.Fatalf("unresolved child recovery: reaped=%d error=%v", n, err)
			}
			if _, err := os.Stat(scratch); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Write([]byte("done\n")); err != nil {
				t.Fatalf("unowned shell was signaled: %v", err)
			}
		})
	}
}
