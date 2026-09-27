package cli

import (
	"context"
	"errors"
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
			if want := strings.ReplaceAll(test.wantCommand, "DEDICATED", codexHome) + coordinatorCodexToolFlags(); launch.Command != want {
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
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	state := filepath.Join(base, "state")
	for _, dir := range []string{workspace, state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "hub.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	linkedWorkspace := filepath.Join(base, "linked-workspace")
	if err := os.Symlink(state, linkedWorkspace); err != nil {
		t.Fatal(err)
	}
	linkedState := filepath.Join(base, "linked-state")
	if err := os.Symlink(filepath.Join(workspace, "hidden"), linkedState); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "hidden"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, workspace, path string
		want                  bool
	}{
		{name: "database inside the workspace", workspace: workspace, path: filepath.Join(workspace, "hub.db"), want: true},
		{name: "nested state not created yet", workspace: workspace, path: filepath.Join(workspace, "a", "hosted.yaml"), want: true},
		{name: "the workspace itself", workspace: workspace, path: workspace, want: true},
		{name: "a symlinked workspace pointing at the hub state directory", workspace: linkedWorkspace, path: filepath.Join(state, "hub.db"), want: true},
		{name: "a state path reached through a symlink into the workspace", workspace: workspace, path: filepath.Join(linkedState, "hub.db"), want: true},
		{name: "a sibling", workspace: workspace, path: filepath.Join(state, "hub.db")},
		{name: "a prefix sibling", workspace: workspace, path: workspace + "-state/hub.db"},
		{name: "no path", workspace: workspace, path: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := workspaceHoldsPath(test.workspace, test.path); got != test.want {
				t.Fatalf("workspaceHoldsPath(%q, %q) = %t, want %t", test.workspace, test.path, got, test.want)
			}
		})
	}
}

func TestCoordinatorCodexCommandDisablesBuiltInTools(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	lookup := func(name string) (string, bool) {
		if name == "CODEX_HOME" {
			return t.TempDir(), true
		}
		return "", false
	}
	launch, err := prepareCoordinatorCodex("codex app-server", workspace, lookup, os.UserHomeDir, "linux")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(coordinatorCodexCommand(t.Context(), launch, "").Args, " ")
	for _, flag := range []string{
		"-c features.shell_tool=false", "-c features.unified_exec=false", "-c features.view_image=false",
		"-c features.code_mode_host=false", "-c features.apps=false", "-c features.plugins=false",
		"-c features.browser_use=false", "-c features.computer_use=false", "-c features.multi_agent=false",
		"-c features.image_generation=false", "-c web_search=disabled",
	} {
		if !strings.Contains(script, flag) {
			t.Fatalf("command %q does not carry %q", script, flag)
		}
	}
	if !strings.HasPrefix(launch.Command, "codex app-server -c ") {
		t.Fatalf("command = %q, want the flags after the app-server subcommand", launch.Command)
	}
}

func TestVerifyCoordinatorCodexFeatures(t *testing.T) {
	t.Parallel()
	all := strings.Join(coordinatorRequiredCodexFeatures, " stable true\n") + " stable true\n"
	for _, test := range []struct {
		name       string
		command    string
		output     string
		listErr    error
		wantBinary string
		wantErr    string
	}{
		{name: "every required feature is known", command: "codex app-server", output: all, wantBinary: "codex"},
		{name: "an environment assignment is skipped", command: "CODEX_HOME=/x /opt/bin/codex app-server", output: all, wantBinary: "/opt/bin/codex"},
		{name: "a Codex without shell_tool fails closed", command: "codex app-server", output: strings.ReplaceAll(all, "shell_tool", "renamed_shell"), wantBinary: "codex", wantErr: "cannot disable shell_tool"},
		{name: "a failing feature list fails closed", command: "codex app-server", listErr: errors.New("boom"), wantBinary: "codex", wantErr: "boom"},
		{name: "no executable", command: "  ", wantErr: "names no executable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var gotBinary string
			var gotEnv []string
			list := func(_ context.Context, binary string, env []string) ([]byte, error) {
				gotBinary, gotEnv = binary, env
				return []byte(test.output), test.listErr
			}
			err := verifyCoordinatorCodexFeatures(t.Context(), coordinatorCodexLaunch{Command: test.command, Env: []string{"PATH=/usr/bin"}}, list)
			if test.wantErr == "" && err != nil {
				t.Fatalf("verify() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("verify() error = %v, want %q", err, test.wantErr)
			}
			if gotBinary != test.wantBinary {
				t.Fatalf("binary = %q, want %q", gotBinary, test.wantBinary)
			}
			if test.wantBinary != "" && !slices.Equal(gotEnv, []string{"PATH=/usr/bin"}) {
				t.Fatalf("env = %v, want the coordinator environment", gotEnv)
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
