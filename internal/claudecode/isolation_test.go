package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestIsolationSettings(t *testing.T) {
	for _, test := range []struct {
		tier  string
		valid bool
	}{{"sandbox", true}, {"native-trusted", true}, {"container", false}} {
		t.Run(test.tier, func(t *testing.T) {
			p := isolation.Policy{Tier: test.tier, WritableRoots: []string{"/worktree", "/runtime"}, HostServices: []string{"unix:/var/run/example.sock"}}
			settings, err := IsolationSettings(p)
			if (err == nil) != test.valid {
				t.Fatalf("error = %v", err)
			}
			if !test.valid {
				return
			}
			s := settings["sandbox"].(map[string]any)
			if s["enabled"] != (test.tier == "sandbox") {
				t.Fatalf("sandbox = %#v", s)
			}
			if test.tier == "sandbox" {
				if s["allowUnsandboxedCommands"] != false || s["failIfUnavailable"] != true {
					t.Fatalf("sandbox = %#v", s)
				}
				f := s["filesystem"].(map[string]any)
				if !reflect.DeepEqual(f["allowWrite"], p.WritableRoots) || len(f["denyRead"].([]string)) < 4 {
					t.Fatalf("filesystem = %#v", f)
				}
				n := s["network"].(map[string]any)
				if !reflect.DeepEqual(n["allowUnixSockets"], []string{"/var/run/example.sock"}) || n["strictAllowlist"] != true {
					t.Fatalf("network = %#v", n)
				}
			}
		})
	}
}

func TestCommandAppliesRunnerIsolation(t *testing.T) {
	for _, tier := range []string{isolation.Sandbox, isolation.NativeTrusted} {
		t.Run(tier, func(t *testing.T) {
			backend, err := NewAgentBackend(Options{CommandFactoryWithArgs: func(ctx context.Context, args []string) *exec.Cmd { return exec.CommandContext(ctx, "claude", args...) }, IsolationPolicy: func() (isolation.Policy, error) {
				return isolation.Policy{Tier: tier, HostServices: []string{"unix:/var/run/example.sock"}}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			cmd, err := backend.command(t.Context(), runner.AgentTurnRequest{Workspace: "/worktree", TempDir: "/runtime"})
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal([]byte(cmd.Args[len(cmd.Args)-1]), &settings); err != nil {
				t.Fatal(err)
			}
			sandbox := settings["sandbox"].(map[string]any)
			if sandbox["enabled"] != (tier == isolation.Sandbox) {
				t.Fatalf("sandbox = %#v", sandbox)
			}
			if tier == isolation.Sandbox && (!slices.Contains(cmd.Args, "Bash,Read,Glob,Grep") || !slices.Contains(cmd.Args, "--strict-mcp-config")) {
				t.Fatalf("unbounded tools: %v", cmd.Args)
			}
			if tier == isolation.Sandbox && sandbox["allowUnsandboxedCommands"] != false {
				t.Fatalf("sandbox = %#v", sandbox)
			}
		})
	}
}

func TestCommandRejectsIsolationFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		extra  []string
		policy isolation.Policy
		err    error
	}{
		{"missing policy", nil, isolation.Policy{}, errors.New("routing unavailable")},
		{"unsupported tier", nil, isolation.Policy{Tier: "container"}, nil},
		{"extra settings", []string{"--settings", "{}"}, isolation.Policy{Tier: isolation.Sandbox}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			backend, err := NewAgentBackend(Options{ExtraArgs: test.extra, CommandFactory: func(context.Context) *exec.Cmd { called = true; return exec.Command("claude") }, IsolationPolicy: func() (isolation.Policy, error) { return test.policy, test.err }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.command(t.Context(), runner.AgentTurnRequest{Workspace: "/worktree"}); err == nil || called {
				t.Fatalf("error = %v, factory called = %v", err, called)
			}
		})
	}
}
