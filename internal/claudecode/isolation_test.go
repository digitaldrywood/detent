package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/shell"
)

func TestIsolationSettings(t *testing.T) {
	for _, test := range []struct {
		tier         string
		valid        bool
		localBinding bool
	}{{"sandbox", true, false}, {"sandbox", true, true}, {"native-trusted", true, false}, {"container", false, false}} {
		t.Run(test.tier, func(t *testing.T) {
			p := isolation.Policy{ExtraNetworkDomains: []string{"fonts.googleapis.com", "fonts.gstatic.com"}, AllowLocalBinding: test.localBinding, Tier: test.tier, WritableRoots: []string{"/worktree", "/runtime"}, HostServices: []string{"unix:/var/run/example.sock"}}
			settings, err := IsolationSettings(p)
			if test.tier == isolation.Sandbox && !isolation.SandboxAvailable() {
				if !errors.Is(err, isolation.ErrSandboxUnavailable) {
					t.Fatalf("error = %v, want sandbox unavailable", err)
				}
				return
			}
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
				domains := n["allowedDomains"].([]string)
				if !slices.Contains(domains, "fonts.googleapis.com") || !slices.Contains(domains, "fonts.gstatic.com") || slices.Contains(domains, "example.com") {
					t.Fatalf("project domain grants = %#v", domains)
				}
				if !reflect.DeepEqual(n["allowUnixSockets"], []string{"/var/run/example.sock"}) || n["strictAllowlist"] != true || n["allowLocalBinding"] != test.localBinding {
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
			if tier == isolation.Sandbox && !isolation.SandboxAvailable() {
				if !errors.Is(err, isolation.ErrSandboxUnavailable) {
					t.Fatalf("error = %v, want sandbox unavailable", err)
				}
				return
			}
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
			backend, err := NewAgentBackend(Options{ExtraArgs: test.extra, CommandFactory: func(ctx context.Context) *exec.Cmd { called = true; return exec.CommandContext(ctx, "claude") }, IsolationPolicy: func() (isolation.Policy, error) { return test.policy, test.err }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.command(t.Context(), runner.AgentTurnRequest{Workspace: "/worktree"}); err == nil || called {
				t.Fatalf("error = %v, factory called = %v", err, called)
			}
		})
	}
}

func TestEffectiveIsolationRejectsWeakenedSettings(t *testing.T) {
	expected := sandboxSettings(t)
	for _, change := range []string{"none", "disabled", "unsandboxed", "domains", "sockets", "excluded commands"} {
		t.Run(change, func(t *testing.T) {
			encoded, _ := json.Marshal(expected)
			var effective map[string]any
			if err := json.Unmarshal(encoded, &effective); err != nil {
				t.Fatal(err)
			}
			sandbox := effective["sandbox"].(map[string]any)
			switch change {
			case "disabled":
				sandbox["enabled"] = false
			case "unsandboxed":
				sandbox["allowUnsandboxedCommands"] = true
			case "domains":
				sandbox["network"].(map[string]any)["allowedDomains"] = []any{"example.com"}
			case "sockets":
				sandbox["network"].(map[string]any)["allowAllUnixSockets"] = true
			case "excluded commands":
				sandbox["excludedCommands"] = []any{"curl"}
			}
			if err := VerifyEffectiveIsolation(expected, effective); (err == nil) != (change == "none") {
				t.Fatalf("verification = %v", err)
			}
		})
	}
}

func TestSandboxHandshakeVerifiesBeforePrompt(t *testing.T) {
	settings := sandboxSettings(t)
	for _, valid := range []bool{true, false} {
		expected := settings
		var effective any = settings
		if !valid {
			effective = map[string]any{"sandbox": map[string]any{"enabled": false}}
		}
		init := `{"type":"control_response","response":{"subtype":"success","request_id":"initialize","response":{}}}`
		reply, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": "get_settings", "response": map[string]any{"effective": effective}}})
		var input bytes.Buffer
		_, err := verifySandboxProcess(t.Context(), &input, strings.NewReader(init+"\n"+string(reply)+"\n"), expected)
		if (err == nil) != valid {
			t.Fatalf("handshake error = %v", err)
		}
		if strings.Contains(input.String(), `"type":"user"`) || !strings.Contains(input.String(), "get_settings") {
			t.Fatalf("unexpected handshake input: %s", input.String())
		}
	}
}

func TestSandboxProductionShellFactory(t *testing.T) {
	for _, weakened := range []bool{false, true} {
		t.Run(map[bool]string{false: "enforced", true: "managed override"}[weakened], func(t *testing.T) {
			observed := filepath.Join(t.TempDir(), "prompt.json")
			t.Setenv("DETENT_CLAUDE_SHELL_HELPER", "1")
			t.Setenv("DETENT_CLAUDE_OBSERVED", observed)
			if weakened {
				t.Setenv("DETENT_CLAUDE_WEAKEN", "1")
			} else {
				t.Setenv("DETENT_CLAUDE_WEAKEN", "")
			}
			backend, err := NewAgentBackend(Options{IsolationPolicy: func() (isolation.Policy, error) { return isolation.Policy{Tier: isolation.Sandbox}, nil }, CommandFactoryWithArgs: func(ctx context.Context, args []string) *exec.Cmd {
				return shell.CommandWithArgs(ctx, "exec", "sh", append([]string{os.Args[0], "-test.run=^TestSandboxShellHelper$", "--"}, args...))
			}})
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			_, err = backend.RunTurn(t.Context(), runner.AgentTurnRequest{Workspace: root, TempDir: root, Prompt: "sandbox test prompt"}, nil)
			if !isolation.SandboxAvailable() {
				if !errors.Is(err, isolation.ErrSandboxUnavailable) {
					t.Fatalf("error = %v, want sandbox unavailable", err)
				}
				return
			}
			if (err != nil) != weakened {
				t.Fatalf("turn error = %v", err)
			}
			data, readErr := os.ReadFile(observed)
			if weakened {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("prompt sent before verification: %s %v", data, readErr)
				}
			} else if readErr != nil || !strings.Contains(string(data), "sandbox test prompt") {
				t.Fatalf("prompt = %s %v", data, readErr)
			}
		})
	}
}

func TestSandboxShellHelper(t *testing.T) {
	if os.Getenv("DETENT_CLAUDE_SHELL_HELPER") != "1" {
		return
	}
	var settings map[string]any
	for i, arg := range os.Args {
		if arg == "--settings" && i+1 < len(os.Args) {
			if err := json.Unmarshal([]byte(os.Args[i+1]), &settings); err != nil {
				os.Exit(2)
			}
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var message map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			os.Exit(3)
		}
		switch message["type"] {
		case "control_request":
			result := map[string]any{}
			if message["request_id"] == "get_settings" {
				if os.Getenv("DETENT_CLAUDE_WEAKEN") == "1" {
					settings["sandbox"].(map[string]any)["enabled"] = false
				}
				result["effective"] = settings
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": message["request_id"], "response": result}})
		case "user":
			if err := os.WriteFile(os.Getenv("DETENT_CLAUDE_OBSERVED"), scanner.Bytes(), 0600); err != nil {
				os.Exit(4)
			}
			_, _ = os.Stdout.WriteString(`{"type":"system","subtype":"init","session_id":"test-session","model":"test-model"}` + "\n" + `{"type":"result","subtype":"success","session_id":"test-session","result":"done"}` + "\n")
		}
	}
	os.Exit(0)
}

func sandboxSettings(t *testing.T) map[string]any {
	t.Helper()
	settings, err := IsolationSettings(isolation.Policy{Tier: isolation.Sandbox, WritableRoots: []string{t.TempDir()}})
	if !isolation.SandboxAvailable() {
		if !errors.Is(err, isolation.ErrSandboxUnavailable) {
			t.Fatalf("error = %v, want sandbox unavailable", err)
		}
		t.Skip("the sandbox tier is unavailable on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	return settings
}
