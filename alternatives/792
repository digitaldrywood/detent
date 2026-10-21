package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestProbeBackendTiers(t *testing.T) {
	for _, test := range []struct {
		name, tier                    string
		sprite, native, sandbox, hang bool
		want                          []string
		wantReasons                   map[string]string
	}{
		{name: "machine both", tier: isolation.NativeTrusted, native: true, sandbox: true, want: []string{isolation.NativeTrusted, isolation.Sandbox}},
		{name: "machine native only", native: true, want: []string{isolation.NativeTrusted}, wantReasons: map[string]string{isolation.Sandbox: "backend sandbox tooling is unavailable"}},
		{name: "machine unavailable", want: []string{}, wantReasons: map[string]string{isolation.NativeTrusted: "native version or login failed", isolation.Sandbox: "backend sandbox tooling is unavailable"}},
		{name: "full access Sprite skips hanging optional sandbox", tier: isolation.NativeTrusted, sprite: true, native: true, hang: true, want: []string{isolation.NativeTrusted}},
		{name: "full access Sprite native failure", tier: isolation.NativeTrusted, sprite: true, hang: true, want: []string{}, wantReasons: map[string]string{isolation.NativeTrusted: "native version or login failed"}},
		{name: "sandbox Sprite still probes", tier: isolation.Sandbox, sprite: true, native: true, sandbox: true, want: []string{isolation.NativeTrusted, isolation.Sandbox}},
		{name: "sandbox Sprite failure", tier: isolation.Sandbox, sprite: true, native: true, want: []string{isolation.NativeTrusted}, wantReasons: map[string]string{isolation.Sandbox: "backend sandbox tooling is unavailable"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				got, reasons := probeBackendTiers(ctx, workflowconfig.AgentBackend{}, isolation.Policy{Tier: test.tier}, test.sprite, func(ctx context.Context, _ workflowconfig.AgentBackend, policy isolation.Policy) error {
					if policy.Tier == isolation.NativeTrusted {
						if test.native {
							return nil
						}
						return errors.New("native version or login failed")
					}
					if test.hang {
						<-ctx.Done()
						return ctx.Err()
					}
					if test.sandbox {
						return nil
					}
					return errors.New("backend sandbox tooling is unavailable")
				})
				if ctx.Err() != nil {
					t.Fatalf("optional probe invalidated completed native checks: %v", ctx.Err())
				}
				if len(reasons) != len(test.wantReasons) {
					t.Fatalf("reasons = %#v, want %#v", reasons, test.wantReasons)
				}
				for tier, reason := range test.wantReasons {
					if reasons[tier] != reason {
						t.Fatalf("%s reason = %q, want %q", tier, reasons[tier], reason)
					}
				}
				if !reflect.DeepEqual(got, test.want) {
					t.Fatalf("tiers = %v, want %v", got, test.want)
				}
			})
		})
	}
}

func TestProbeRunnerIsolationNativeFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, test := range []struct {
		name, failingCommand, output string
	}{
		{"version", "--version", "version probe failed"},
		{"login", "login status", "Not logged in"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("CODEX_HOME", root)
			t.Setenv("DETENT_SERVICE_MANAGER", "test")
			script := filepath.Join(root, "codex-probe")
			content := "#!/bin/sh\nif [ \"$*\" = " + strconv.Quote(test.failingCommand) + " ]; then\necho " + strconv.Quote(test.output) + " >&2\nexit 1\nfi\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.160.0'; exit 0; fi\necho 'unexpected sandbox probe' >&2\nexit 1\n"
			if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
				t.Fatal(err)
			}
			workflow := filepath.Join(root, "WORKFLOW.md")
			content = "---\ntracker:\n  kind: hub_native\ncodex:\n  command: " + strconv.Quote(strconv.Quote(script)+" app-server") + "\nworkspace:\n  root: " + filepath.Join(root, "workspaces") + "\n---\nTest prompt\n"
			if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			report, problems := probeRunnerIsolation(t.Context(), globalconfig.Config{Projects: []globalconfig.Project{{ID: "example-project", Workdir: root, Workflow: workflow}}})
			if len(report) != 1 || report.Supports(isolation.NativeTrusted) || report.Supports(isolation.Sandbox) {
				t.Fatalf("failed native checks advertised capability: %#v", report)
			}
			if len(problems) != 1 {
				t.Fatalf("problems = %+v", problems)
			}
			problem := problems[0]
			if problem.ProjectID != "example-project" || !strings.HasSuffix(problem.Check, test.failingCommand) || !strings.Contains(problem.ErrorOutput, test.output) {
				t.Fatalf("native failure lost actionable diagnostic: %+v", problem)
			}
		})
	}
}

func TestBackendVersionAtLeast(t *testing.T) {
	for _, test := range []struct {
		output string
		want   bool
	}{
		{"codex-cli 0.159.2", true}, {"codex-cli 0.160.0", true},
		{"codex-cli 0.159.1", false}, {"unknown", false}, {"codex-cli 1.0.0", true},
	} {
		t.Run(test.output, func(t *testing.T) {
			if got := backendVersionAtLeast([]byte(test.output), [3]int{0, 159, 2}); got != test.want {
				t.Fatalf("version supported = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProbeBackendRejectsUnsupportedCommand(t *testing.T) {
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: "codex app-server --dangerously-bypass-approvals-and-sandbox"}
	if err := probeBackendIsolation(t.Context(), backend, isolation.Policy{Tier: isolation.Sandbox}); err == nil {
		t.Fatal("unsafe command advertised sandbox")
	}
}

func TestBackendProbeReapsDescendantPipes(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess cancellation integration")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	cmd := backendProbeCommand(ctx, "sh -c 'sleep 30 & wait'", "sh", nil)
	_, err := runBackendProbe(ctx, cmd)
	if err == nil {
		t.Fatal("probe ignored cancellation")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("probe pipe cleanup took %v", elapsed)
	}
}

func TestBackendProbeReapsChildrenAfterParentExits(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	if runtime.GOOS == "windows" {
		t.Skip("requires Unix process groups")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := backendProbeCommand(ctx, "sh -c 'sleep 30 & echo $!'", "sh", nil)
	output, err := runBackendProbe(ctx, cmd)
	if err == nil {
		t.Fatal("probe did not report inherited output pipes")
	}
	pid := strings.TrimSpace(string(output))
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("child PID = %q: %v", pid, err)
	}
	status, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", pid).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("inspect child %s: %v", pid, err)
		}
		return
	}
	if state := strings.TrimSpace(string(status)); state != "" && !strings.HasPrefix(state, "Z") {
		t.Fatalf("probe left child %s running with state %s", pid, state)
	}
}

func TestProbeBackendCodexEnforcesSandbox(t *testing.T) {
	if os.Getenv("DETENT_TEST_CODEX_SANDBOX") != "1" {
		t.Skip("requires an installed Codex sandbox backend")
	}
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: "codex app-server"}
	if err := probeBackendIsolation(t.Context(), backend, isolation.Policy{Tier: isolation.Sandbox}); err != nil {
		t.Fatalf("real Codex filesystem and network enforcement probe: %v", err)
	}
}

func TestBackendReadinessProblem(t *testing.T) {
	for _, test := range []struct {
		name, kind, tier, goos, command, output, fix string
		signedOut                                    bool
	}{
		{"codex signed out", workflowconfig.AgentBackendCodex, "", "linux", "codex login status", "Not logged in", "codex login --device-auth", true},
		{"claude signed out", workflowconfig.AgentBackendClaudeCode, "", "linux", "claude auth status --json", `{"loggedIn":false}`, "claude auth login", true},
		{"codex killed", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "darwin", "codex login status", "signal: killed", "", false},
		{"claude killed", workflowconfig.AgentBackendClaudeCode, isolation.NativeTrusted, "linux", "claude auth status --json", "signal: killed", "", false},
		{"codex missing", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "linux", "codex --version", "codex: command not found", "npm install -g @openai/codex", false},
		{"codex missing default shell", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "linux", "codex --version", "sh: 1: codex: not found", "npm install -g @openai/codex", false},
		{"claude missing default shell", workflowconfig.AgentBackendClaudeCode, isolation.NativeTrusted, "linux", "claude --version", "sh: 1: claude: not found", "npm install -g @anthropic-ai/claude-code", false},
		{"sandbox", workflowconfig.AgentBackendCodex, isolation.Sandbox, "linux", "codex sandbox", "operation not permitted", "", false},
		{"sandbox tooling missing", workflowconfig.AgentBackendCodex, isolation.Sandbox, "linux", "codex sandbox", "bwrap: command not found", "sudo apt-get install bubblewrap socat", false},
		{"sandbox tooling missing default shell", workflowconfig.AgentBackendCodex, isolation.Sandbox, "linux", "codex sandbox", "sh: 1: bwrap: not found", "sudo apt-get install bubblewrap socat", false},
		{"sandbox socket tooling missing", workflowconfig.AgentBackendClaudeCode, isolation.Sandbox, "linux", "claude --print", "socat: no such file or directory", "sudo apt-get install bubblewrap socat", false},
		{"host repair", workflowconfig.AgentBackendCodex, isolation.Sandbox, "darwin", "codex sandbox", "bwrap: command not found", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := backendReadinessProblem("codex", test.kind, test.tier, test.goos, &backendCheckError{Check: test.command, Err: errors.New(test.output), SignedOut: test.signedOut})
			if p.Check != test.command || p.ErrorOutput != test.output || p.FixCommand != test.fix || !strings.HasPrefix(p.Subject, "codex") {
				t.Fatalf("problem=%+v", p)
			}
			if test.output == "signal: killed" && p.Code != "backend_probe_failed" {
				t.Fatalf("killed probe reported missing backend: %+v", p)
			}
			if test.signedOut && p.Message != "not signed in" {
				t.Fatalf("message=%q", p.Message)
			}
			if err := runnerauth.ValidateReportedProblems([]runnerauth.Problem{p}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunnerCodexSignInRefresh(t *testing.T) {
	if address := os.Getenv("DETENT_TEST_LOGIN_PROBE_ADDRESS"); address != "" {
		connection, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		var data [1]byte
		if _, err := connection.Read(data[:]); err != nil {
			t.Fatal(err)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	home := t.TempDir()
	t.Setenv("DETENT_SERVICE_MANAGER", "test")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, "codex-probe")
	marker := filepath.Join(home, "signed-in")
	observed := filepath.Join(home, "observed-home")
	content := "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > " + strconv.Quote(observed) + "\nif test -f " + strconv.Quote(marker) + "; then\nif test -n \"$DETENT_TEST_LOGIN_PROBE_ADDRESS\"; then exec " + strconv.Quote(executable) + " -test.run=^TestRunnerCodexSignInRefresh$ -- \"$@\"; fi\nexit 0\nfi\necho 'Not logged in' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: strconv.Quote(script) + " app-server"}
	err = probeBackendSignIn(t.Context(), backend)
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("auth error=%v", err)
	}
	profile, err := os.ReadFile(observed)
	if err != nil || string(profile) != filepath.Join(home, workerCodexProfileDir) {
		t.Fatalf("profile=%q error=%v", profile, err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeBackendSignIn(t.Context(), backend); err != nil {
		t.Fatalf("sign-in did not clear: %v", err)
	}
	for _, test := range []struct {
		name string
		want error
	}{
		{"parent cancelled", context.Canceled},
		{"parent deadline", context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			t.Setenv("DETENT_TEST_LOGIN_PROBE_ADDRESS", listener.Addr().String())
			ctx := &controlledRuntimeCommandContext{
				deadline: time.Now().Add(3 * time.Second),
				done:     make(chan struct{}),
				value:    t.Context().Value,
			}
			defer ctx.cancel(context.Canceled)
			done := make(chan error, 1)
			go func() {
				defer listener.Close()
				done <- probeBackendSignIn(ctx, backend)
			}()
			connection, err := listener.Accept()
			if err != nil {
				t.Fatalf("probe exited before login readiness: accept=%v probe=%v", err, <-done)
			}
			defer connection.Close()
			ctx.cancel(test.want)
			err = <-done
			if !errors.Is(err, test.want) {
				t.Fatalf("probe lost parent error: %v", err)
			}
			problem := backendReadinessProblem("example-project/codex", backend.Kind, isolation.NativeTrusted, runtime.GOOS, err)
			if problem.Code != "backend_probe_failed" || problem.FixCommand != "" || strings.Contains(problem.FixHint, "Sign in") || !strings.Contains(problem.Check, "login status") {
				t.Fatalf("cancelled probe recommended backend repair: %+v", problem)
			}
		})
	}
}
