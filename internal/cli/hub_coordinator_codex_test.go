package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestPrepareCoordinatorCodexIsolatesEnvironment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		goos        string
		command     string
		parent      map[string]string
		wantPresent []string
		wantAbsent  []string
		wantCommand string
		credential  bool
	}{
		{
			name:    "hub secrets and state never reach the child",
			goos:    "linux",
			command: "codex app-server",
			parent: map[string]string{
				"PATH": "/usr/bin", "HOME": "/home/hub", "OPENAI_API_KEY": "sk-provider",
				"WORKOS_API_KEY": "sk_workos", "STRIPE_API_KEY": "sk_stripe", "STRIPE_WEBHOOK_SECRET": "whsec",
				"DETENT_HUB_ADMIN_TOKEN": "admin-token", "DETENT_HUB_DATABASE": "/var/lib/hub.db", "GH_TOKEN": "ghp", "AWS_SECRET_ACCESS_KEY": "aws",
			},
			wantPresent: []string{"PATH=/usr/bin", "OPENAI_API_KEY=sk-provider"},
			wantAbsent:  []string{"WORKOS_API_KEY", "STRIPE_API_KEY", "STRIPE_WEBHOOK_SECRET", "DETENT_HUB_ADMIN_TOKEN", "DETENT_HUB_DATABASE", "GH_TOKEN", "AWS_SECRET_ACCESS_KEY", "HOME=/home/hub"},
			wantCommand: "codex app-server",
			credential:  true,
		},
		{
			name:        "a CODEX_HOME assignment is redirected to the dedicated home",
			goos:        "linux",
			command:     "CODEX_HOME=SOURCE codex app-server",
			parent:      map[string]string{"PATH": "/usr/bin", "CODEX_HOME": "/elsewhere"},
			wantPresent: []string{"PATH=/usr/bin"},
			wantAbsent:  []string{"CODEX_HOME=/elsewhere"},
			wantCommand: "CODEX_HOME='DEDICATED' codex app-server",
			credential:  true,
		},
		{
			name:        "windows keeps what a process needs to start",
			goos:        "windows",
			command:     "codex app-server",
			parent:      map[string]string{"PATH": `C:\bin`, "SystemRoot": `C:\Windows`, "WORKOS_API_KEY": "sk_workos"},
			wantPresent: []string{`PATH=C:\bin`, `SystemRoot=C:\Windows`},
			wantAbsent:  []string{"WORKOS_API_KEY"},
			wantCommand: "codex app-server",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workspace := t.TempDir()
			source := t.TempDir()
			if test.credential {
				if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := strings.ReplaceAll(test.command, "SOURCE", source)
			parent := map[string]string{}
			for key, value := range test.parent {
				parent[key] = value
			}
			if !strings.Contains(command, "CODEX_HOME=") {
				parent["CODEX_HOME"] = source
			}
			lookup := func(name string) (string, bool) {
				value, ok := parent[name]
				return value, ok
			}
			launch, err := prepareCoordinatorCodex(command, workspace, lookup, func() (string, error) { return "/home/hub", nil }, test.goos)
			if err != nil {
				t.Fatalf("prepareCoordinatorCodex() error = %v", err)
			}
			state := filepath.Join(workspace, coordinatorCodexStateDir)
			codexHome := filepath.Join(state, "codex")
			if want := strings.ReplaceAll(test.wantCommand, "DEDICATED", codexHome); launch.Command != want {
				t.Fatalf("command = %q, want %q", launch.Command, want)
			}
			cmd := coordinatorCodexCommand(t.Context(), launch, "")
			if cmd.Dir != workspace {
				t.Fatalf("cwd = %q, want %q", cmd.Dir, workspace)
			}
			env := cmd.Env
			want := append([]string{"HOME=" + filepath.Join(state, "home"), "CODEX_HOME=" + codexHome, "TMPDIR=" + filepath.Join(state, "tmp")}, test.wantPresent...)
			for _, entry := range want {
				if !slices.Contains(env, entry) {
					t.Fatalf("env = %v, want %q", env, entry)
				}
			}
			for _, absent := range test.wantAbsent {
				for _, entry := range env {
					if strings.HasPrefix(entry, absent) || strings.Contains(entry, "sk_") || strings.Contains(entry, "admin-token") {
						t.Fatalf("env carries %q: %v", entry, env)
					}
				}
			}
			for _, entry := range env {
				key, _, _ := strings.Cut(entry, "=")
				allowed := slices.Contains(coordinatorPassthroughEnvironment, key) || slices.Contains(coordinatorWindowsEnvironment, key) ||
					slices.Contains([]string{"HOME", "CODEX_HOME", "TMPDIR", "TMP", "TEMP", "USERPROFILE"}, key)
				if !allowed {
					t.Fatalf("env carries non-allowlisted %q", key)
				}
			}
			link, err := os.Readlink(filepath.Join(codexHome, "auth.json"))
			switch {
			case test.credential && (err != nil || link != filepath.Join(source, "auth.json")):
				t.Fatalf("credential link = %q, %v, want the source credential", link, err)
			case !test.credential && err == nil:
				t.Fatalf("credential link = %q, want none without a credential", link)
			}
			entries, err := os.ReadDir(codexHome)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "auth.json" {
					t.Fatalf("dedicated Codex home carries %q, want only the credential", entry.Name())
				}
			}
		})
	}
}

func TestPrepareCoordinatorCodexIsIdempotent(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) {
		if name == "CODEX_HOME" {
			return source, true
		}
		return "", false
	}
	for range 2 {
		if _, err := prepareCoordinatorCodex("codex app-server", workspace, lookup, os.UserHomeDir, "linux"); err != nil {
			t.Fatalf("prepareCoordinatorCodex() error = %v", err)
		}
	}
}

func TestCoordinatorCodexOptionsForceReadOnly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		cfg  workflowconfig.CodexOptions
	}{
		{name: "defaults"},
		{name: "permissive options are overridden", cfg: workflowconfig.CodexOptions{ApprovalPolicy: workflowconfig.StringOrMap{IsString: true, String: "on-request"}, ThreadSandbox: "danger-full-access", TurnSandboxPolicy: map[string]any{"type": "dangerFullAccess"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := coordinatorCodexOptions(test.cfg)
			if options.ApprovalPolicy != "never" || options.ThreadSandbox != "read-only" || options.TurnSandboxPolicy != nil || options.DeliverableElicitationAllowlist != nil {
				t.Fatalf("options = %#v, want read-only and never", options)
			}
		})
	}
}

func TestWorkspaceHoldsPath(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	for _, test := range []struct {
		name, path string
		want       bool
	}{
		{name: "database inside the workspace", path: filepath.Join(workspace, "hub.db"), want: true},
		{name: "nested state", path: filepath.Join(workspace, "a", "hosted.yaml"), want: true},
		{name: "the workspace itself", path: workspace, want: true},
		{name: "a sibling", path: filepath.Join(filepath.Dir(workspace), "other", "hub.db")},
		{name: "a prefix sibling", path: workspace + "-state/hub.db"},
		{name: "no path", path: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := workspaceHoldsPath(workspace, test.path); got != test.want {
				t.Fatalf("workspaceHoldsPath(%q) = %t, want %t", test.path, got, test.want)
			}
		})
	}
}

func TestCoordinatorCodexCommandReplacesEnvironment(t *testing.T) {
	t.Setenv("DETENT_COORDINATOR_TEST_SECRET", "hub-secret")
	cmd := coordinatorCodexCommand(context.Background(), coordinatorCodexLaunch{Command: "codex app-server", Workspace: t.TempDir(), Env: []string{"PATH=/usr/bin"}}, "")
	for _, entry := range cmd.Environ() {
		if strings.Contains(entry, "hub-secret") {
			t.Fatalf("child environment inherited %q", entry)
		}
	}
}
