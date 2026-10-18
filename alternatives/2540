//go:build unix

package piagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

// Catch orphaned children after settled completion, a dead parent with inherited
// pipes, protocol failure, timeout, and cancellation. Readiness is a pipe ack,
// and each assertion observes actual process identity rather than a sleep.
func TestRunTurnReapsProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	for _, mode := range []string{"complete", "dead_parent", "protocol", "cancel", "timeout", "stall"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			backend, err := NewAgentBackend(Options{StallTimeout: func() time.Duration {
				if mode == "stall" {
					return 500 * time.Millisecond
				}
				return 0
			}(), CommandFactory: func(ctx context.Context, args []string) *exec.Cmd {
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPiLifecycleProcess$")
				cmd.Env = append(os.Environ(), "DETENT_PI_LIFECYCLE="+mode, "GOCOVERDIR="+t.TempDir())
				return cmd
			}})
			if err != nil {
				t.Fatal(err)
			}
			var identity procgroup.Identity
			childPID := 0
			req := runner.AgentTurnRequest{Workspace: dir, TempDir: dir, Prompt: "fixture"}
			if mode == "timeout" {
				req.TurnTimeout = time.Second
			}
			_, err = backend.RunTurn(ctx, req, func(u runner.AgentUpdate) error {
				if u.Type == runner.AgentUpdateProcessStarted {
					identity = u.WorkerProcess
				}
				if u.Type == runner.AgentUpdateToolStarted {
					var parseErr error
					childPID, parseErr = strconv.Atoi(u.Command)
					if parseErr != nil {
						return parseErr
					}
					if mode == "cancel" {
						cancel()
					}
				}
				return nil
			})
			// Emergency cleanup applies only after production has returned and the
			// assertions have run, so it cannot make a broken backend pass.
			defer func() {
				if childPID > 0 {
					_, _ = procgroup.Terminate(context.Background(), identity, procgroup.DefaultTerminationGrace)
				}
			}()
			if mode == "complete" && err != nil {
				t.Fatal(err)
			}
			if mode != "complete" && err == nil {
				t.Fatal("expected terminal failure")
			}
			if childPID == 0 {
				t.Fatalf("fixture never reported a ready child: %v", err)
			}
			observations, observeErr := procgroup.Observe([]procgroup.Identity{identity})
			if observeErr != nil || len(observations) != 1 || observations[0].Alive {
				t.Fatalf("group survived: %v, %v; turn %v", observations, observeErr, err)
			}
		})
	}
}

func TestPiLifecycleProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	mode := os.Getenv("DETENT_PI_LIFECYCLE")
	if mode == "" {
		return
	}
	if mode == "child" {
		ready := os.NewFile(3, "ready")
		if _, err := ready.Write([]byte{1}); err != nil {
			os.Exit(2)
		}
		if err := ready.Close(); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		os.Exit(2)
	}
	send := func(record any) {
		if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
			os.Exit(3)
		}
	}
	send(map[string]any{"type": "response", "id": "state", "command": "get_state", "success": true, "data": map[string]any{"sessionId": "group-session", "model": map[string]string{"id": "fixture"}}})
	if !scanner.Scan() {
		os.Exit(4)
	}
	readiness, writer, err := os.Pipe()
	if err != nil {
		os.Exit(5)
	}
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPiLifecycleProcess$")
	scratch := os.Getenv("TMPDIR")
	if scratch == "" {
		os.Exit(6)
	}
	coverageDir, err := os.MkdirTemp(scratch, "child-coverage-")
	if err != nil {
		os.Exit(6)
	}
	child.Env = append(os.Environ(), "DETENT_PI_LIFECYCLE=child", "GOCOVERDIR="+coverageDir)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.ExtraFiles = []*os.File{writer}
	if err := child.Start(); err != nil {
		os.Exit(6)
	}
	if err := writer.Close(); err != nil {
		os.Exit(6)
	}
	var ready [1]byte
	if _, err := io.ReadFull(readiness, ready[:]); err != nil {
		os.Exit(7)
	}
	if err := readiness.Close(); err != nil {
		os.Exit(7)
	}
	send(map[string]any{"type": "response", "id": "prompt", "command": "prompt", "success": true, "data": map[string]string{"disposition": "started"}})
	send(map[string]any{"type": "tool_execution_start", "toolCallId": "child", "toolName": "bash", "args": map[string]string{"command": strconv.Itoa(child.Process.Pid)}})
	switch mode {
	case "dead_parent":
		os.Exit(0)
	case "protocol":
		fmt.Fprintln(os.Stdout, "invalid protocol")
	case "complete":
		send(map[string]string{"type": "agent_settled"})
	}
	for {
		time.Sleep(time.Hour)
	}
}

// A startup error must retain stderr without interpreting diagnostics as RPC.
func TestRunTurnCapturesStderr(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	backend, err := NewAgentBackend(Options{CommandFactory: func(ctx context.Context, args []string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo fixture-startup-error >&2; exit 1")
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.RunTurn(t.Context(), runner.AgentTurnRequest{Workspace: t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "fixture-startup-error") {
		t.Fatalf("stderr missing: %v", err)
	}
}
