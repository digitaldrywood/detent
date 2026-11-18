//go:build !windows

package cli

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleasedHubExecutionRequiresStoppedProcessIdentity(t *testing.T) {

	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	procgroup.Configure(t.Context(), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		t.Fatal(err)
	}
	attempt := tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{
		WorkerLaunch: &tracker.NativeWorkerLaunch{PID: identity.PID, GroupID: identity.GroupID, ProcessStart: identity.StartedAt},
	}}}
	stopped, err := remoteReleasedStopped(context.Background(), "wi_example", attempt)
	if err != nil || stopped {
		t.Fatalf("live worker authorized cleanup: stopped=%t error=%v", stopped, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed worker exited successfully")
	}
	stopped, err = remoteReleasedStopped(context.Background(), "wi_example", attempt)
	if err != nil || !stopped {
		t.Fatalf("stopped worker could not be reconciled: stopped=%t error=%v", stopped, err)
	}
	attempt.Runtime.WorkerLaunch.ProcessStart = time.Time{}
	stopped, err = remoteReleasedStopped(context.Background(), "wi_example", attempt)
	if err != nil || stopped {
		t.Fatalf("missing process identity authorized cleanup: stopped=%t error=%v", stopped, err)
	}
}
