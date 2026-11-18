package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func TestBackendReadinessProblem(t *testing.T) {
	for _, test := range []struct {
		name, kind, tier, goos, command, output, fix string
		signedOut                                    bool
	}{
		{"codex signed out", workflowconfig.AgentBackendCodex, "", "linux", "codex login status", "Not logged in", "codex login --device-auth", true},
		{"claude signed out", workflowconfig.AgentBackendClaudeCode, "", "linux", "claude auth status --json", `{"loggedIn":false}`, "claude auth login", true},
		{"antigravity signed out", workflowconfig.AgentBackendAntigravity, "", "linux", "agy models", "Error: Please sign in to view available models.", "agy", true},
		{"antigravity missing", workflowconfig.AgentBackendAntigravity, isolation.NativeTrusted, "linux", "agy --version", "sh: 1: agy: not found", "", false},
		{"codex killed", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "darwin", "codex login status", "signal: killed", "", false},
		{"claude killed", workflowconfig.AgentBackendClaudeCode, isolation.NativeTrusted, "linux", "claude auth status --json", "signal: killed", "", false},
		{"codex missing", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "linux", "codex --version", "codex: command not found", "npm install -g @openai/codex", false},
		{"codex missing default shell", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "linux", "codex --version", "sh: 1: codex: not found", "npm install -g @openai/codex", false},
		{"claude missing default shell", workflowconfig.AgentBackendClaudeCode, isolation.NativeTrusted, "linux", "claude --version", "sh: 1: claude: not found", "npm install -g @anthropic-ai/claude-code", false},
		{"sandbox", workflowconfig.AgentBackendCodex, isolation.Sandbox, "linux", "codex sandbox", "operation not permitted", "", false},
		{"app-server tooling missing", workflowconfig.AgentBackendCodex, isolation.NativeTrusted, "linux", "codex app-server", "Codex could not find bubblewrap on PATH", "sudo apt-get install bubblewrap socat", false},
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
			if test.goos == "linux" && (strings.Contains(test.output, "bwrap") || strings.Contains(test.output, "bubblewrap") || strings.Contains(test.output, "socat")) {
				if p.Code != "backend_missing" || !strings.Contains(p.Message, "missing from PATH") {
					t.Fatalf("missing prerequisite was not identified: %+v", p)
				}
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

func TestProbeRunnerIsolationWorkflowFailures(t *testing.T) {
	for _, test := range []struct {
		name, content, ref            string
		cancel, deadline, wantProblem bool
	}{
		{name: "canceled git load", ref: "origin/HEAD", cancel: true},
		{name: "expired git load", ref: "origin/HEAD", deadline: true},
		{name: "missing workflow", wantProblem: true},
		{name: "parse error", content: "---\ntracker: [\n---\n", wantProblem: true},
		{name: "parse error despite cancellation", content: "---\ntracker: [\n---\n", cancel: true, wantProblem: true},
		{name: "missing workflow despite deadline", deadline: true, wantProblem: true},
		{name: "git failure", ref: "HEAD^{invalid}", wantProblem: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			workflow := filepath.Join(root, "WORKFLOW.md")
			if test.content != "" {
				if err := os.WriteFile(workflow, []byte(test.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				cancel()
			}
			if test.deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			}
			report, problems := probeRunnerIsolation(ctx, globalconfig.Config{Projects: []globalconfig.Project{{ID: "example-project", Workdir: root, Workflow: workflow, WorkflowRef: test.ref}}}, isolation.Policy{Tier: isolation.Sandbox})
			if len(report) != 0 || (len(problems) != 0) != test.wantProblem {
				t.Fatalf("report=%v problems=%+v", report, problems)
			}
			if test.wantProblem && (len(problems) != 1 || problems[0].Code != "settings_invalid" || problems[0].Check != "load workflow") {
				t.Fatalf("workflow failure was lost: %+v", problems)
			}
		})
	}
}
