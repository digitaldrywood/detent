//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestLocalWorkerReleaseAndPolicyCoexistence(t *testing.T) {
	for _, scenario := range []string{"coexistence", "failures", "authority"} {
		t.Run(scenario, func(t *testing.T) { testLocalWorkerScenario(t, scenario) })
	}
}

func TestFencedLocalWorkerAcknowledgement(t *testing.T) {
	for _, scenario := range []string{"before_registration", "after_commit_before_reply", "after_ack_before_release", "lost_reply", "unreachable", "second_launch", "overlapping", "preparation_unreachable", "preparation_acknowledged", "preparation_readmitted", "recovered_live", "recovered_exited"} {
		t.Run(scenario, func(t *testing.T) { testFencedLaunchAcknowledgement(t, scenario) })
	}
}

func TestLocalWorkerBootstrapExitsOnParentEOF(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := testenv.WaitContext(t, t.Context())
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestLocalWorkerProcess$", "--", SSHWorkerArgument)
	cmd.Env = append(os.Environ(), "DETENT_EXECUTION_FIXTURE_VERSION=1.0.0", "DETENT_EXECUTION_REAL_WORKER=true", "GOCOVERDIR="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closeErr := stdin.Close()
	if err := errors.Join(closeErr, cmd.Wait()); err != nil {
		t.Fatalf("worker did not exit on parent EOF before release: %v", err)
	}
}
