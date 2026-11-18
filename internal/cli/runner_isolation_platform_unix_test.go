//go:build !windows

package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/isolation"
)

func TestProbeRunnerIsolationNativeFailures(t *testing.T) {

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
			report, problems := probeRunnerIsolation(t.Context(), globalconfig.Config{Projects: []globalconfig.Project{{ID: "example-project", Workdir: root, Workflow: workflow}}}, isolation.Policy{Tier: isolation.Sandbox})
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

func TestProbeRunnerIsolationRoutedHostServices(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Setenv("DETENT_SERVICE_MANAGER", "test")
	script := filepath.Join(root, "codex-probe")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.160.0'; fi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(root, "WORKFLOW.md")
	content := "---\ntracker:\n  kind: hub_native\ncodex:\n  command: " + strconv.Quote(strconv.Quote(script)+" app-server") + "\nworkspace:\n  root: " + filepath.Join(root, "workspaces") + "\n---\nTest prompt\n"
	if err := os.WriteFile(workflow, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{ID: "example-project", Workdir: root, Workflow: workflow}}}
	for _, test := range []struct {
		name, service string
		sandbox       bool
	}{
		{"exact socket", "unix:" + filepath.Join(root, "service.sock"), true},
		{"unsupported TCP grant", "tcp:127.0.0.1:8080", false},
		{"socket path now a directory", "unix:" + root, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, problems := probeRunnerIsolation(t.Context(), cfg, isolation.Policy{Tier: isolation.Sandbox, HostServices: []string{test.service}})
			wantSandbox := test.sandbox && isolation.SandboxAvailable()
			if !report.Supports(isolation.NativeTrusted) || report.Supports(isolation.Sandbox) != wantSandbox || (len(problems) == 0) != wantSandbox {
				t.Fatalf("capability report = %v, problems = %+v, want sandbox = %t", report, problems, wantSandbox)
			}
		})
	}
}

func TestBackendProbeReapsChildrenAfterParentExits(t *testing.T) {

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
