//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestReapWorkerProcessesAtStartup(t *testing.T) {
	t.Parallel()

	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("scratch reservation survives owner loss/stale=%t", stale), func(t *testing.T) {

			root := t.TempDir()
			scratch, err := workspace.PrepareWorkerScratch(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			backend := &memorySessions{}
			sessionID, err := backend.StartSession(t.Context(), store.SessionStart{IssueID: "host-gate", StartedAt: time.Now().UTC(), AgentRole: store.WorkerProcessRoleHostValidation})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.UpdateSessionWorkerProcess(t.Context(), sessionID, store.WorkerProcessRegistration{CleanupRoot: root, CleanupPath: scratch}); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "printf 'ready\n'; read value")
			cmd.Dir = scratch
			procgroup.SetTempDir(cmd, scratch)
			procgroup.Configure(t.Context(), cmd)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stdin.Close() })
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			wait := make(chan error, 1)
			go func() { wait <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("child startup: %q %v", line, err)
			}
			if stale {
				if err := backend.UpdateSessionWorkerProcess(t.Context(), sessionID, store.WorkerProcessRegistration{
					WorkerProcessIdentity: store.WorkerProcessIdentity{PID: os.Getpid(), GroupID: os.Getpid(), StartedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)},
					CleanupRoot:           root, CleanupPath: scratch,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := reapWorkerProcesses(workspace.WithProcessSource(t.Context(), func(context.Context, string) ([]int, error) {
				identity, err := procgroup.Inspect(cmd)
				if errors.Is(err, procgroup.ErrProcessNotRunning) {
					return nil, nil
				}
				if err != nil {
					return nil, err
				}
				alive, err := procgroup.Alive(identity)
				if !alive {
					return nil, err
				}
				return []int{identity.PID}, err
			}), backend, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "startup", time.Second, time.Now, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-wait:
			case <-time.After(5 * time.Second):
				t.Fatal("scratch child survived startup recovery")
			}
			active, err := backend.ListActiveWorkerProcesses(t.Context())
			if err != nil || len(active) != 0 {
				t.Fatalf("reservation not released after child termination: %+v %v", active, err)
			}
			_, statErr := os.Stat(scratch)
			if stale {
				if statErr != nil {
					t.Fatalf("stale identity artifacts removed: %v", statErr)
				}
				if err := workspace.CleanupWorkerScratch(root, scratch); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("scratch cleanup: %v", statErr)
			}
		})
	}

	startedAt := time.Date(2026, 7, 12, 16, 0, 0, 0, time.UTC)
	reapedAt := startedAt.Add(time.Minute)
	processStore := &shutdownWorkerProcessStore{processes: []store.WorkerProcess{
		{
			SessionID:  1214,
			Identifier: "digitaldrywood/detent#1214",
			WorkerProcessIdentity: store.WorkerProcessIdentity{
				PID:       4242,
				GroupID:   4242,
				StartedAt: startedAt,
			},
		},
		{
			SessionID:  1215,
			Identifier: "digitaldrywood/detent#1215",
			WorkerProcessIdentity: store.WorkerProcessIdentity{
				PID:       4343,
				GroupID:   4343,
				StartedAt: startedAt,
			},
		},
	}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	err := reapWorkerProcesses(
		context.Background(),
		processStore,
		logger,
		"startup",
		time.Millisecond,
		func() time.Time { return reapedAt },
		func(_ context.Context, identity procgroup.Identity, _ time.Duration) (procgroup.TerminationOutcome, error) {
			if identity.PID == 4242 {
				return procgroup.TerminationOutcomeTerminated, nil
			}
			return procgroup.TerminationOutcomeAlreadyExited, nil
		},
	)
	if err != nil {
		t.Fatalf("reapWorkerProcesses() error = %v", err)
	}
	if len(processStore.reaped) != 2 {
		t.Fatalf("reaped processes = %#v", processStore.reaped)
	}
	if processStore.reaped[0].reap.Outcome != store.WorkerProcessOutcomeTerminated || processStore.reaped[1].reap.Outcome != store.WorkerProcessOutcomeAlreadyExited {
		t.Fatalf("reap outcomes = %#v", processStore.reaped)
	}
	for _, reaped := range processStore.reaped {
		if !reaped.reap.ReapedAt.Equal(reapedAt) {
			t.Fatalf("reaped at = %s, want %s", reaped.reap.ReapedAt, reapedAt)
		}
		if reaped.reap.Reason != "startup" {
			t.Fatalf("reap reason = %q, want startup", reaped.reap.Reason)
		}
	}
	for _, want := range []string{
		"reason=startup",
		"decision=terminated",
		"decision=already_exited",
		"issue_identifier=digitaldrywood/detent#1214",
		"issue_identifier=digitaldrywood/detent#1215",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs missing %q:\n%s", want, logs.String())
		}
	}
}
