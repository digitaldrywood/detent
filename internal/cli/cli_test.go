package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/cli"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestRootCommandHelpListsAdminCommands(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	if !strings.HasPrefix(output, "Examples:\n") {
		t.Fatalf("help output does not lead with examples:\n%s", output)
	}
	for _, want := range []string{"detent", "agent orchestrator", "doctor", "dev-runtime", "init", "add-project", "pause", "unpause", "promote", "remove-project"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestCommandHelpLeadsWithExample(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"add-project", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	if !strings.HasPrefix(output, "Examples:\ndetent add-project --id api") {
		t.Fatalf("add-project help does not lead with its runnable example:\n%s", output)
	}
	if strings.Count(output, "Examples:") != 1 {
		t.Fatalf("add-project help examples count = %d, want 1:\n%s", strings.Count(output, "Examples:"), output)
	}
}

func TestRootCommandSuggestsMistypedCommandsAndFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		want       []string
		dontWant   []string
		wantCount  map[string]int
		wantNoBoot bool
	}{
		{
			name:       "command typo",
			args:       []string{"paues"},
			want:       []string{`unknown command "paues"`, "Did you mean this?", "\tpause"},
			wantNoBoot: true,
		},
		{
			name:     "no close command match",
			args:     []string{"definitely-not-close"},
			want:     []string{`unknown command "definitely-not-close"`},
			dontWant: []string{"Did you mean this?"},
		},
		{
			name:       "semantic command suggestion",
			args:       []string{"rm", "detent"},
			want:       []string{`unknown command "rm"`, "Did you mean this?", "\tremove-project"},
			wantNoBoot: true,
		},
		{
			name:       "semantic command suggestion before intended local flags",
			args:       []string{"add", "--id", "detent", "--workflow", "WORKFLOW.md", "--workdir", "."},
			want:       []string{`unknown command "add"`, "Did you mean this?", "\tadd-project"},
			dontWant:   []string{"unknown flag"},
			wantCount:  map[string]int{"\tadd-project": 1},
			wantNoBoot: true,
		},
		{
			name:       "persistent flag typo",
			args:       []string{"--hedless"},
			want:       []string{"unknown flag: --hedless", "Did you mean this?", "\t--headless"},
			wantNoBoot: true,
		},
		{
			name:       "local flag typo",
			args:       []string{"add-project", "--wokdir", "x"},
			want:       []string{"unknown flag: --wokdir", "Did you mean this?", "\t--workdir"},
			wantNoBoot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			booted := false
			cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(context.Context, cli.BootConfig) error {
				booted = true
				return nil
			}))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("Execute() error = nil, want error")
			}
			output := err.Error()
			for _, want := range tt.want {
				if !strings.Contains(output, want) {
					t.Fatalf("Execute() error missing %q:\n%s", want, output)
				}
			}
			for _, unwanted := range tt.dontWant {
				if strings.Contains(output, unwanted) {
					t.Fatalf("Execute() error contains %q:\n%s", unwanted, output)
				}
			}
			for value, count := range tt.wantCount {
				if got := strings.Count(output, value); got != count {
					t.Fatalf("Execute() error contains %q %d times, want %d:\n%s", value, got, count, output)
				}
			}
			if tt.wantNoBoot && booted {
				t.Fatal("boot executed for invalid command")
			}
		})
	}
}

func TestRootCommandBootsFromGlobalConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)

	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
		booted <- cfg
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := <-booted
	if got.Mode != cli.BootModeRunning {
		t.Fatalf("boot mode = %q, want %q", got.Mode, cli.BootModeRunning)
	}
	if got.Global.Path != path {
		t.Fatalf("booted config path = %q, want %q", got.Global.Path, path)
	}
	if got.ConfigPathRule != globalconfig.PathRuleFlag {
		t.Fatalf("config path rule = %q, want %q", got.ConfigPathRule, globalconfig.PathRuleFlag)
	}
}

func TestDevRuntimeCommandBuildsIsolatedBootConfig(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
		booted <- cfg
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--host", "127.0.0.2",
		"--port", "0",
		"dev-runtime",
		"--home", home,
		"--db", "fixture.db",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := <-booted
	if got.Isolated == nil {
		t.Fatal("Isolated = nil, want isolated runtime metadata")
	}
	if got.Runner == nil {
		t.Fatal("Runner = nil, want fake runner override")
	}
	if got.ConnectorFactory == nil {
		t.Fatal("ConnectorFactory = nil, want stateful memory connector factory")
	}
	if got.Host != "127.0.0.2" {
		t.Fatalf("Host = %q, want 127.0.0.2", got.Host)
	}
	if got.Port == nil || *got.Port != 0 {
		t.Fatalf("Port = %v, want 0", got.Port)
	}
	if got.RuntimeDBPath != filepath.Join(home, "fixture.db") {
		t.Fatalf("RuntimeDBPath = %q, want fixture DB under home", got.RuntimeDBPath)
	}
	if got.Isolated.DBMode != "file" || got.Isolated.TrackerMode != "memory" {
		t.Fatalf("isolated metadata = %#v, want file DB and memory tracker", got.Isolated)
	}
}

func TestConfigPathCommandPrintsResolvedPathAndRule(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "--format", "pretty", "config", "path"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	for _, want := range []string{path, string(globalconfig.PathRuleFlag)} {
		if !strings.Contains(output, want) {
			t.Fatalf("config path output missing %q:\n%s", want, output)
		}
	}
}

func TestOutputFormatPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		stdoutTTY bool
		args      []string
		wantJSON  bool
	}{
		{
			name:     "non tty defaults to json",
			wantJSON: true,
		},
		{
			name:      "tty defaults to pretty",
			stdoutTTY: true,
		},
		{
			name: "env pretty overrides non tty",
			env:  "pretty",
		},
		{
			name:      "env json overrides tty",
			env:       "json",
			stdoutTTY: true,
			wantJSON:  true,
		},
		{
			name:     "flag json overrides env pretty",
			env:      "pretty",
			args:     []string{"--format", "json"},
			wantJSON: true,
		},
		{
			name:      "flag pretty overrides env json",
			env:       "json",
			stdoutTTY: false,
			args:      []string{"--format", "pretty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "global.yaml")
			cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return tt.stdoutTTY }))
			if tt.env != "" {
				t.Setenv("DETENT_FORMAT", tt.env)
			}
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			args := append([]string{}, tt.args...)
			args = append(args, "--config", path, "config", "path")
			cmd.SetArgs(args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if tt.wantJSON {
				var got struct {
					Path string `json:"path"`
					Rule string `json:"rule"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatalf("Unmarshal() error = %v\n%s", err, stdout.String())
				}
				if got.Path != path || got.Rule != string(globalconfig.PathRuleFlag) {
					t.Fatalf("json output = %#v, want path %q rule %q", got, path, globalconfig.PathRuleFlag)
				}
				return
			}
			for _, want := range []string{"path: " + path, "rule: " + string(globalconfig.PathRuleFlag)} {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("pretty output missing %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestResolveOutputFormatFromArgsPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      []string
		env       string
		stdoutTTY bool
		want      cli.OutputFormat
	}{
		{
			name:      "flag before unknown command beats tty",
			args:      []string{"--format", "json", "paues"},
			stdoutTTY: true,
			want:      cli.OutputFormatJSON,
		},
		{
			name:      "flag after unknown command beats tty",
			args:      []string{"paues", "--format", "json"},
			stdoutTTY: true,
			want:      cli.OutputFormatJSON,
		},
		{
			name:      "equals form beats tty",
			args:      []string{"paues", "--format=json"},
			stdoutTTY: true,
			want:      cli.OutputFormatJSON,
		},
		{
			name: "flag beats env and non tty",
			args: []string{"paues", "--format", "pretty"},
			env:  "json",
			want: cli.OutputFormatPretty,
		},
		{
			name:      "env beats tty",
			args:      []string{"paues"},
			env:       "json",
			stdoutTTY: true,
			want:      cli.OutputFormatJSON,
		},
		{
			name:      "tty defaults pretty",
			args:      []string{"paues"},
			stdoutTTY: true,
			want:      cli.OutputFormatPretty,
		},
		{
			name:      "terminator stops flag scan",
			args:      []string{"paues", "--", "--format", "json"},
			stdoutTTY: true,
			want:      cli.OutputFormatPretty,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := cli.ResolveOutputFormatFromArgs(tt.args, func(string) string {
				return tt.env
			}, tt.stdoutTTY)
			if err != nil {
				t.Fatalf("ResolveOutputFormatFromArgs() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveOutputFormatFromArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRootCommandPassesVersionToBoot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)

	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(context.Background(),
		cli.WithVersion("v7.6.5"),
		cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
			booted <- cfg
			return nil
		}),
	)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := <-booted; got.Version != "v7.6.5" {
		t.Fatalf("boot version = %q, want v7.6.5", got.Version)
	}
}

func TestRootCommandCapturesHeadlessFlagAndTerminalState(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)

	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(
		context.Background(),
		cli.WithStdoutTTY(func() bool { return true }),
		cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
			booted <- cfg
			return nil
		}),
	)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "--headless"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := <-booted
	if !got.Headless {
		t.Fatal("Headless = false, want true")
	}
	if !got.StdoutTTY {
		t.Fatal("StdoutTTY = false, want true")
	}
}

func TestRootCommandPrintsBootBanner(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONFIG_HOME", filepath.Join(root, ".detent"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cmd := cli.NewRootCommand(ctx, cli.WithVersion("v1.2.3"))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", filepath.Join(root, ".detent", "global.yaml"), "--host", "127.0.0.1", "--port", "0"})

	if err := cmd.Execute(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want %v", err, context.Canceled)
	}

	output := stdout.String()
	for _, want := range []string{
		"Detent v1.2.3",
		"Project: https://github.com/digitaldrywood/detent",
		"Dashboard: http://localhost:",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("boot banner missing %q:\n%s", want, output)
		}
	}
	for _, unwanted := range []string{"http://0.0.0.0", "http://127.0.0.1", "http://localhost:0"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("boot banner contains non-localhost URL %q:\n%s", unwanted, output)
		}
	}
}

func TestRootCommandBootsFromDefaultWorkflowWhenGlobalConfigIsMissing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONFIG_HOME", filepath.Join(root, ".detent"))
	writeWorkflow(t, filepath.Join(root, "WORKFLOW.md"), validWorkflowContent())
	t.Chdir(root)

	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
		booted <- cfg
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	got := <-booted
	if got.Mode != cli.BootModeRunning {
		t.Fatalf("boot mode = %q, want %q", got.Mode, cli.BootModeRunning)
	}
	if got.WorkflowPath != filepath.Join(root, "WORKFLOW.md") {
		t.Fatalf("workflow path = %q, want default WORKFLOW.md", got.WorkflowPath)
	}
	if len(got.Global.Projects) != 1 {
		t.Fatalf("projects length = %d, want 1", len(got.Global.Projects))
	}
	if got.Global.Projects[0].Workflow != got.WorkflowPath {
		t.Fatalf("project workflow = %q, want %q", got.Global.Projects[0].Workflow, got.WorkflowPath)
	}
}

func TestRootCommandUsesDefaultWorkflowServerAddress(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONFIG_HOME", filepath.Join(root, ".detent"))
	writeWorkflow(t, filepath.Join(root, "WORKFLOW.md"), workflowContentWithServer("0.0.0.0", 4101))
	t.Chdir(root)

	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
		booted <- cfg
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	assertBootServer(t, <-booted, "0.0.0.0", 4101)
}

func TestRootCommandUsesConfiguredProjectWorkflowServerAddress(t *testing.T) {
	t.Parallel()

	paths := createProjectFilesWithWorkflow(t, workflowContentWithServer("127.0.0.2", 4102))
	configPath := filepath.Join(paths.root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "detent", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
	})

	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
		booted <- cfg
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", configPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	assertBootServer(t, <-booted, "127.0.0.2", 4102)
}

func TestRootCommandCLIAddressOverridesWorkflowServerAddress(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONFIG_HOME", filepath.Join(root, ".detent"))
	writeWorkflow(t, filepath.Join(root, "WORKFLOW.md"), workflowContentWithServer("0.0.0.0", 4103))
	t.Chdir(root)

	booted := make(chan cli.BootConfig, 1)
	cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
		booted <- cfg
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--host", "127.0.0.3", "--port", "0"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	assertBootServer(t, <-booted, "127.0.0.3", 0)
}

func TestRootCommandUsesOnboardingModeWithoutValidWorkflow(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "missing workflow"},
		{name: "invalid workflow", content: "not frontmatter\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CONFIG_HOME", filepath.Join(root, ".detent"))
			if tt.content != "" {
				writeWorkflow(t, filepath.Join(root, "WORKFLOW.md"), tt.content)
			}
			t.Chdir(root)

			booted := make(chan cli.BootConfig, 1)
			cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(_ context.Context, cfg cli.BootConfig) error {
				booted <- cfg
				return nil
			}))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			got := <-booted
			if got.Mode != cli.BootModeOnboarding {
				t.Fatalf("boot mode = %q, want %q", got.Mode, cli.BootModeOnboarding)
			}
			if len(got.Global.Projects) != 0 {
				t.Fatalf("projects = %#v, want none in onboarding mode", got.Global.Projects)
			}
		})
	}
}

func TestInitWritesDefaultGlobalConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".detent", "global.yaml")
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "init"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	cfg, err := globalconfig.Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if cfg.APIVersion != globalconfig.APIVersion || cfg.Kind != globalconfig.Kind {
		t.Fatalf("config identity = %s/%s", cfg.APIVersion, cfg.Kind)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
	}
}

func TestProjectCommandsWriteJSONResults(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "init",
			args: []string{"init"},
			want: fmt.Sprintf(`{"status":"ok","path":%q,"rule":"--config"}`, configPath),
		},
		{
			name: "add-project",
			args: []string{
				"add-project",
				"--id", "detent",
				"--workflow", paths.workflowPath,
				"--workdir", paths.workdirPath,
				"--weight", "5",
				"--priority", "50",
				"--paused",
				"--credential-ref", "github-default",
			},
			want: fmt.Sprintf(`{"id":"detent","workflow":%q,"workdir":%q,"weight":5,"priority":50,"paused":true,"credential_ref":"github-default"}`, paths.workflowPath, paths.workdirPath),
		},
		{
			name: "pause",
			args: []string{"pause", "detent"},
			want: `{"status":"ok","project":"detent","paused":true}`,
		},
		{
			name: "unpause",
			args: []string{"unpause", "detent"},
			want: `{"status":"ok","project":"detent","paused":false}`,
		},
		{
			name: "promote",
			args: []string{"promote", "detent", "--priority", "1"},
			want: `{"status":"ok","project":"detent","priority":1}`,
		},
		{
			name: "remove-project",
			args: []string{"remove-project", "detent"},
			want: `{"status":"ok","project":"detent","removed":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := cli.NewRootCommand(context.Background())
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			args := append([]string{"--config", configPath}, tt.args...)
			cmd.SetArgs(args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			assertJSONEqual(t, stdout.Bytes(), tt.want)
		})
	}
}

func TestInitRefusesExistingConfigWithoutForce(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)

	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "init"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Execute() error = %v, want already exists", err)
	}
}

func TestCLIValidationErrorsCarryHints(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, configPath, nil)

	tests := []struct {
		name         string
		args         []string
		wantMessage  string
		wantHint     string
		wantCommands []string
	}{
		{
			name:         "root port",
			args:         []string{"--config", configPath, "--port", "-1"},
			wantMessage:  "--port must be greater than or equal to 0",
			wantHint:     "e.g. detent --port 0",
			wantCommands: []string{"detent --port 0"},
		},
		{
			name:         "missing project id",
			args:         []string{"--config", configPath, "add-project", "--workflow", "./WORKFLOW.md", "--workdir", "~/code/api"},
			wantMessage:  "--id is required",
			wantHint:     "e.g. detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api",
			wantCommands: []string{"detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api"},
		},
		{
			name:         "missing project workflow",
			args:         []string{"--config", configPath, "add-project", "--id", "api", "--workdir", "~/code/api"},
			wantMessage:  "--workflow is required",
			wantHint:     "e.g. detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api",
			wantCommands: []string{"detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api"},
		},
		{
			name:         "missing project workdir",
			args:         []string{"--config", configPath, "add-project", "--id", "api", "--workflow", "./WORKFLOW.md"},
			wantMessage:  "--workdir is required",
			wantHint:     "e.g. detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api",
			wantCommands: []string{"detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api"},
		},
		{
			name:         "invalid project weight",
			args:         []string{"--config", configPath, "add-project", "--id", "api", "--workflow", "./WORKFLOW.md", "--workdir", "~/code/api", "--weight", "0"},
			wantMessage:  "--weight must be positive",
			wantHint:     "e.g. detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api --weight 1",
			wantCommands: []string{"detent add-project --id api --workflow ./WORKFLOW.md --workdir ~/code/api --weight 1"},
		},
		{
			name:         "invalid promote priority",
			args:         []string{"--config", configPath, "promote", "api", "--priority", "0"},
			wantMessage:  "--priority must be positive",
			wantHint:     "e.g. detent promote api --priority 10",
			wantCommands: []string{"detent promote api --priority 10"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer
			cmd := cli.NewRootCommand(context.Background(), cli.WithBootFunc(func(context.Context, cli.BootConfig) error {
				t.Fatal("boot should not run")
				return nil
			}))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&stderr)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("Execute() error = nil, want error")
			}
			if !errors.Is(err, cli.ErrValidation) {
				t.Fatalf("Execute() error = %v, want %v", err, cli.ErrValidation)
			}
			assertHintedError(t, err, nil, tt.wantMessage, tt.wantHint, tt.wantCommands)
			if !strings.Contains(stderr.String(), "Hint: "+tt.wantHint) {
				t.Fatalf("stderr missing hint %q:\n%s", tt.wantHint, stderr.String())
			}
		})
	}
}

func TestConfigAndProjectConflictErrorsCarryHints(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "detent", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
	})

	tests := []struct {
		name         string
		args         []string
		wantErr      error
		wantMessage  string
		wantHint     string
		wantCommands []string
	}{
		{
			name:         "config exists",
			args:         []string{"--config", configPath, "init"},
			wantErr:      cli.ErrConfigExists,
			wantMessage:  "global config already exists: " + configPath,
			wantHint:     "run detent init --force to overwrite it, or edit the file reported by detent config path",
			wantCommands: []string{"detent init --force", "detent config path"},
		},
		{
			name: "project exists",
			args: []string{
				"--config", configPath,
				"add-project",
				"--id", "detent",
				"--workflow", paths.workflowPath,
				"--workdir", paths.workdirPath,
			},
			wantErr:      cli.ErrProjectExists,
			wantMessage:  `project "detent" already exists`,
			wantHint:     `project id "detent" is already taken; run detent config path to inspect current projects before choosing a new --id`,
			wantCommands: []string{"detent config path"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer
			cmd := cli.NewRootCommand(context.Background())
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&stderr)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("Execute() error = nil, want error")
			}
			assertHintedError(t, err, tt.wantErr, tt.wantMessage, tt.wantHint, tt.wantCommands)
			if !strings.Contains(stderr.String(), "Hint: "+tt.wantHint) {
				t.Fatalf("stderr missing hint %q:\n%s", tt.wantHint, stderr.String())
			}
		})
	}
}

func TestAddProjectWritesConfigAndSignalsManager(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	signals := make(chan cli.Signal, 1)

	cmd := cli.NewRootCommand(
		context.Background(),
		cli.WithStdoutTTY(func() bool { return true }),
		cli.WithSignalFunc(func(_ context.Context, signal cli.Signal) error {
			signals <- signal
			return nil
		}),
	)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"add-project",
		"--id", " detent ",
		"--workflow", paths.workflowPath,
		"--workdir", paths.workdirPath,
		"--weight", "5",
		"--priority", "50",
		"--paused",
		"--credential-ref", " github-default ",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects length = %d, want 1", len(cfg.Projects))
	}
	got := cfg.Projects[0]
	want := globalconfig.Project{
		ID:            "detent",
		Workflow:      paths.workflowPath,
		Workdir:       paths.workdirPath,
		Weight:        5,
		Priority:      50,
		Paused:        true,
		CredentialRef: "github-default",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("project = %#v, want %#v", got, want)
	}

	signal := <-signals
	if signal.Operation != cli.OperationAddProject || !reflect.DeepEqual(signal.Project, want) {
		t.Fatalf("signal = %#v, want add project %#v", signal, want)
	}
}

func TestAddProjectCommandEmitsJSONResult(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"add-project",
		"--id", "detent",
		"--workflow", paths.workflowPath,
		"--workdir", paths.workdirPath,
		"--weight", "5",
		"--paused",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		ID       string `json:"id"`
		Workflow string `json:"workflow"`
		Workdir  string `json:"workdir"`
		Weight   int    `json:"weight"`
		Paused   bool   `json:"paused"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("add-project output is not JSON: %v\n%s", err, stdout.String())
	}
	if got.ID != "detent" || got.Workflow != paths.workflowPath || got.Workdir != paths.workdirPath {
		t.Fatalf("project = %#v, want detent project", got)
	}
	if got.Weight != 5 || !got.Paused {
		t.Fatalf("project weight/paused = %d/%v, want 5/true", got.Weight, got.Paused)
	}
}

func TestRootCommandClassifiesMistypedCommandSuggestion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "near miss", args: []string{"paues"}, want: "pause"},
		{name: "semantic alias", args: []string{"rm", "api"}, want: "remove-project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := cli.NewRootCommand(context.Background())
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if err == nil {
				t.Fatal("Execute() error = nil, want unknown command")
			}
			problem := cli.ProblemForCommandError(cmd, err)
			if len(problem.DidYouMean) != 1 || problem.DidYouMean[0] != tt.want {
				t.Fatalf("did_you_mean = %#v, want %s", problem.DidYouMean, tt.want)
			}
			if !strings.Contains(problem.SuggestedFix, "detent "+tt.want) {
				t.Fatalf("suggested_fix = %q, want detent %s", problem.SuggestedFix, tt.want)
			}
		})
	}
}

func TestProblemForErrorClassifiesSemanticFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "validation", err: cli.NewValidationError("--id is required", "Run detent add-project.", nil), want: cli.ExitValidation},
		{name: "not found", err: fmt.Errorf("%w: api", cli.ErrProjectNotFound), want: cli.ExitNotFoundOrConfig},
		{name: "auth", err: errors.New(`GITHUB_TOKEN is not set and github_token is not configured. Run gh auth login --scopes "repo,read:org,project" and set github_token: gh in global.yaml.`), want: cli.ExitAuth},
		{name: "general", err: errors.New("disk is full"), want: cli.ExitGeneral},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := cli.ProblemForError(tt.err).ExitCode; got != tt.want {
				t.Fatalf("ProblemForError().ExitCode = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRootCommandSuggestsMistypedFlag(t *testing.T) {
	t.Parallel()

	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--hedless"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want unknown flag")
	}
	for _, want := range []string{"unknown flag: --hedless", "Did you mean this?", "\t--headless"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Execute() error missing %q:\n%v", want, err)
		}
	}
}

func TestProjectAdminCommandsEditConfigAndSignalManager(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{
		{
			ID:       "detent",
			Workflow: paths.workflowPath,
			Workdir:  paths.workdirPath,
			Weight:   2,
			Priority: 4,
		},
	})

	signals := make(chan cli.Signal, 4)
	runCommand := func(args ...string) {
		t.Helper()

		allArgs := append([]string{"--config", configPath}, args...)
		cmd := cli.NewRootCommand(
			context.Background(),
			cli.WithStdoutTTY(func() bool { return true }),
			cli.WithSignalFunc(func(_ context.Context, signal cli.Signal) error {
				signals <- signal
				return nil
			}),
		)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(allArgs)

		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v) error = %v", args, err)
		}
	}

	runCommand("pause", "detent")
	assertProject(t, configPath, "detent", func(project globalconfig.Project) {
		if !project.Paused {
			t.Fatal("Paused = false, want true")
		}
	})
	assertSignal(t, signals, cli.OperationPauseProject, "detent")

	runCommand("unpause", "detent")
	assertProject(t, configPath, "detent", func(project globalconfig.Project) {
		if project.Paused {
			t.Fatal("Paused = true, want false")
		}
	})
	assertSignal(t, signals, cli.OperationUnpauseProject, "detent")

	runCommand("promote", "detent", "--priority", "1")
	assertProject(t, configPath, "detent", func(project globalconfig.Project) {
		if project.Priority != 1 {
			t.Fatalf("Priority = %d, want 1", project.Priority)
		}
	})
	assertSignal(t, signals, cli.OperationPromoteProject, "detent")

	runCommand("remove-project", "detent")
	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want empty", cfg.Projects)
	}
	assertSignal(t, signals, cli.OperationRemoveProject, "detent")
}

func TestProjectAdminCommandsPreserveProjectPathLiterals(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "global.yaml")
	if err := os.WriteFile(configPath, []byte(`apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: cli.go
    workdir: .
    weight: 1
    priority: 0
  - id: docs
    workflow: cli.go
    workdir: .
    weight: 2
    priority: 1
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", configPath, "pause", "detent"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var written struct {
		Projects []struct {
			ID       string `yaml:"id"`
			Workflow string `yaml:"workflow"`
			Workdir  string `yaml:"workdir"`
			Paused   bool   `yaml:"paused"`
		} `yaml:"projects"`
	}
	if err := yaml.Unmarshal(raw, &written); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(written.Projects) != 2 {
		t.Fatalf("Projects length = %d, want 2", len(written.Projects))
	}
	for _, project := range written.Projects {
		if project.Workflow != "cli.go" {
			t.Fatalf("project %s workflow = %q, want cli.go", project.ID, project.Workflow)
		}
		if project.Workdir != "." {
			t.Fatalf("project %s workdir = %q, want .", project.ID, project.Workdir)
		}
	}
	if !written.Projects[0].Paused {
		t.Fatal("detent Paused = false, want true")
	}
	if written.Projects[1].Paused {
		t.Fatal("docs Paused = true, want false")
	}
}

func TestProjectAdminCommandsRejectMissingProject(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "api", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
		{ID: "web", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
		{ID: "infra", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
	})

	var stderr bytes.Buffer
	cmd := cli.NewRootCommand(context.Background(), cli.WithSignalFunc(func(context.Context, cli.Signal) error {
		t.Fatal("signal should not be emitted")
		return nil
	}))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--config", configPath, "pause", "ap"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if !errors.Is(err, cli.ErrProjectNotFound) {
		t.Fatalf("Execute() error = %v, want %v", err, cli.ErrProjectNotFound)
	}
	assertHintedError(t, err, cli.ErrProjectNotFound, `project "ap" not found`, "available: api, web, infra\n"+
		"did you mean \"api\"? see `detent config path`, then retry", []string{"detent config path"})
	for _, want := range []string{"Hint: available: api, web, infra", "did you mean \"api\"? see `detent config path`, then retry"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestAddProjectRejectsDuplicateID(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	writeGlobalConfig(t, configPath, []globalconfig.Project{
		{ID: "detent", Workflow: paths.workflowPath, Workdir: paths.workdirPath, Weight: 1},
	})

	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"add-project",
		"--id", "detent",
		"--workflow", paths.workflowPath,
		"--workdir", paths.workdirPath,
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if !errors.Is(err, cli.ErrProjectExists) {
		t.Fatalf("Execute() error = %v, want %v", err, cli.ErrProjectExists)
	}
}

func TestWithProjectManagerSignalsLiveManager(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	configPath := filepath.Join(paths.root, "global.yaml")
	manager := &projectManagerProbe{}
	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return true }), cli.WithProjectManager(manager))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"add-project",
		"--id", "detent",
		"--workflow", paths.workflowPath,
		"--workdir", paths.workdirPath,
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if manager.added.ID != "detent" {
		t.Fatalf("added project = %#v, want detent", manager.added)
	}
}

type projectPaths struct {
	root         string
	workflowPath string
	workdirPath  string
}

func createProjectFiles(t *testing.T) projectPaths {
	t.Helper()

	return createProjectFilesWithWorkflow(t, validWorkflowContent())
}

func createProjectFilesWithWorkflow(t *testing.T, content string) projectPaths {
	t.Helper()

	root := t.TempDir()
	workdir := filepath.Join(root, "project")
	workflow := filepath.Join(workdir, "WORKFLOW.md")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeWorkflow(t, workflow, content)

	return projectPaths{
		root:         root,
		workflowPath: workflow,
		workdirPath:  workdir,
	}
}

func writeWorkflow(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func validWorkflowContent() string {
	return `---
tracker:
  kind: memory
---
Test workflow prompt.
`
}

func workflowContentWithServer(host string, port int) string {
	return fmt.Sprintf(`---
tracker:
  kind: memory
server:
  host: %s
  port: %d
---
Test workflow prompt.
`, host, port)
}

func assertBootServer(t *testing.T, cfg cli.BootConfig, host string, port int) {
	t.Helper()

	if cfg.Host != host {
		t.Fatalf("boot host = %q, want %q", cfg.Host, host)
	}
	if cfg.Port == nil {
		t.Fatalf("boot port = nil, want %d", port)
	}
	if *cfg.Port != port {
		t.Fatalf("boot port = %d, want %d", *cfg.Port, port)
	}
}

func writeGlobalConfig(t *testing.T, path string, projects []globalconfig.Project) {
	t.Helper()

	cfg := globalconfig.Config{
		Path:       path,
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 8,
			Scheduling:          globalconfig.SchedulingWeighted,
			FairShare:           map[string]any{"half_life": "1h"},
			Startup:             map[string]any{"jitter_seconds": 0, "max_spawn_per_second": 1},
		},
		Projects: projects,
	}
	if cfg.Projects == nil {
		cfg.Projects = []globalconfig.Project{}
	}
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

func assertProject(t *testing.T, configPath string, id string, assert func(globalconfig.Project)) {
	t.Helper()

	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	for _, project := range cfg.Projects {
		if project.ID == id {
			assert(project)
			return
		}
	}
	t.Fatalf("project %q not found in %#v", id, cfg.Projects)
}

func assertSignal(t *testing.T, signals <-chan cli.Signal, operation cli.Operation, projectID string) {
	t.Helper()

	signal := <-signals
	if signal.Operation != operation || signal.ProjectID != projectID {
		t.Fatalf("signal = %#v, want %s %s", signal, operation, projectID)
	}
}

func assertHintedError(t *testing.T, err error, wantErr error, wantMessage string, wantHint string, wantCommands []string) {
	t.Helper()

	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if err.Error() != wantMessage {
		t.Fatalf("error message = %q, want %q", err.Error(), wantMessage)
	}
	gotHint, gotCommands, ok := cli.HintFor(err)
	if !ok {
		t.Fatalf("HintFor(%v) ok = false, want true", err)
	}
	if gotHint != wantHint {
		t.Fatalf("hint = %q, want %q", gotHint, wantHint)
	}
	if !reflect.DeepEqual(gotCommands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", gotCommands, wantCommands)
	}
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()

	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("Unmarshal(got) error = %v\n%s", err, string(got))
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("Unmarshal(want) error = %v\n%s", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("json output = %#v, want %#v\nraw: %s", gotValue, wantValue, string(got))
	}
}

type projectManagerProbe struct {
	added globalconfig.Project
}

func (p *projectManagerProbe) Add(_ context.Context, cfg globalconfig.Project) error {
	p.added = cfg
	return nil
}

func (p *projectManagerProbe) Remove(context.Context, project.ID) error {
	return nil
}

func (p *projectManagerProbe) Pause(context.Context, project.ID) error {
	return nil
}

func (p *projectManagerProbe) Unpause(context.Context, project.ID) error {
	return nil
}
