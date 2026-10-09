package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestProbeBackendTiers(t *testing.T) {
	for _, test := range []struct {
		name    string
		native  bool
		sandbox bool
		want    []string
	}{
		{"both", true, true, []string{"native-trusted", "sandbox"}},
		{"native only", true, false, []string{"native-trusted"}},
		{"unavailable", false, false, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reasons := probeBackendTiers(t.Context(), workflowconfig.AgentBackend{}, isolation.Policy{}, func(_ context.Context, _ workflowconfig.AgentBackend, policy isolation.Policy) error {
				if policy.Tier == isolation.NativeTrusted && test.native || policy.Tier == isolation.Sandbox && test.sandbox {
					return nil
				}
				return errors.New("backend sandbox tooling is unavailable")
			})
			if !test.sandbox && reasons[isolation.Sandbox] != "backend sandbox tooling is unavailable" {
				t.Fatalf("sandbox reason lost: %#v", reasons)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("tiers = %v, want %v", got, test.want)
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
	_, err := runBackendProbe(cmd)
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
	output, err := runBackendProbe(cmd)
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
		{"codex missing", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "linux", "codex --version", "codex: command not found", "npm install -g @openai/codex", false},
		{"sandbox", workflowconfig.AgentBackendCodex, isolation.Sandbox, "linux", "codex sandbox", "operation not permitted", "", false},
		{"sandbox tooling missing", workflowconfig.AgentBackendCodex, isolation.Sandbox, "linux", "codex sandbox", "bwrap: command not found", "sudo apt-get install bubblewrap socat", false},
		{"sandbox socket tooling missing", workflowconfig.AgentBackendClaudeCode, isolation.Sandbox, "linux", "claude --print", "socat: no such file or directory", "sudo apt-get install bubblewrap socat", false},
		{"host repair", workflowconfig.AgentBackendCodex, isolation.Sandbox, "darwin", "codex sandbox", "bwrap: command not found", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := backendReadinessProblem("codex", test.kind, test.tier, test.goos, &backendCheckError{Check: test.command, Err: errors.New(test.output), SignedOut: test.signedOut})
			if p.Check != test.command || p.ErrorOutput != test.output || p.FixCommand != test.fix || !strings.HasPrefix(p.Subject, "codex") {
				t.Fatalf("problem=%+v", p)
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
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	home := t.TempDir()
	t.Setenv("DETENT_SERVICE_MANAGER", "test")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	script := filepath.Join(home, "codex-probe")
	marker := filepath.Join(home, "signed-in")
	observed := filepath.Join(home, "observed-home")
	content := "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > " + strconv.Quote(observed) + "\nif test -f " + strconv.Quote(marker) + "; then exit 0; fi\necho 'Not logged in' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: strconv.Quote(script) + " app-server"}
	err := probeBackendSignIn(t.Context(), backend)
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
}
