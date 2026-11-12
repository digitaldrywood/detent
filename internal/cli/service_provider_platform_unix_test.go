//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestProviderServicePath(t *testing.T) {

	for _, test := range []struct {
		name      string
		bins      []string
		commands  []string
		wantDirs  []string
		wantError string
	}{
		{name: "system path unchanged", bins: []string{"system/codex"}, commands: []string{"codex app-server"}},
		{name: "user provider directories", bins: []string{".npm-global/bin/codex", ".local/bin/claude"}, commands: []string{"codex app-server", "claude"}, wantDirs: []string{".npm-global/bin", ".local/bin"}},
		{name: "shared directory once", bins: []string{".local/bin/codex", ".local/bin/claude"}, commands: []string{"codex app-server", "claude"}, wantDirs: []string{".local/bin"}},
		{name: "absolute command", bins: []string{"custom tools/codex"}, commands: []string{`"HOME/custom tools/codex" app-server`}, wantDirs: []string{"custom tools"}},
		{name: "missing CLI", commands: []string{"codex app-server"}, wantError: `provider CLI "codex"`},
		{name: "not executable", bins: []string{".local/bin/codex"}, commands: []string{"codex app-server"}, wantError: `provider CLI "codex"`},
		{name: "disabled provider", bins: []string{"system/codex"}, commands: []string{"missing-claude", "codex app-server"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			base := filepath.Join(home, "system")
			for _, bin := range test.bins {
				path := filepath.Join(home, bin)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0o700)
				if test.name == "not executable" {
					mode = 0o600
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
					t.Fatal(err)
				}
			}
			cfg := globalconfig.Config{}
			for index, command := range test.commands {
				id := workflowconfig.DefaultAgentBackendID
				if index > 0 {
					id = strconv.Itoa(index)
				}
				cfg.Global.Agents.Backends = append(cfg.Global.Agents.Backends, workflowconfig.AgentBackend{ID: id, Kind: workflowconfig.AgentBackendClaudeCode, Command: strings.ReplaceAll(command, "HOME", home), Disabled: test.name == "disabled provider" && index == 0})
			}
			got, err := providerServicePath(cfg, base, home)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || !strings.Contains(err.Error(), "export PATH") {
					t.Fatalf("path error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantDirectories := []string{base}
			for _, directory := range test.wantDirs {
				wantDirectories = append(wantDirectories, filepath.Join(home, directory))
			}
			want := strings.Join(wantDirectories, string(os.PathListSeparator))
			if got != want {
				t.Fatalf("PATH = %q, want %q", got, want)
			}
			for _, goos := range []string{"linux", "darwin"} {
				platform := servicepkg.Config{GOOS: goos, HomeDir: home, Path: got, Name: "detent.runner", BinaryPath: "/usr/bin/detent", ConfigPath: filepath.Join(home, "global.yaml"), UserSystemdPath: filepath.Join(home, "runner.service"), SystemUnitPaths: []string{}, UserUnitPaths: []string{}, InspectManual: func(string) (servicepkg.Inspection, error) { return servicepkg.Inspection{}, nil }, RunCommand: func(context.Context, string, ...string) (string, error) {
					return "Could not find service", errors.New("exit status 113")
				}}
				controller, err := servicepkg.New(platform)
				if err != nil {
					t.Fatal(err)
				}
				result, err := controller.Start(t.Context(), servicepkg.StartOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if result.Definition == nil || !strings.Contains(result.Definition.Content, want) {
					t.Fatalf("%s definition = %+v", goos, result.Definition)
				}
			}
		})
	}
}

func TestServiceCommandUsesResolvedProviderPath(t *testing.T) {

	t.Parallel()
	root := t.TempDir()
	bin := filepath.Join(root, "provider", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "global.yaml")
	cfg := globalconfig.Config{Path: path}
	port := 4100
	cfg.Port = &port
	cfg.Global.Agents.Backends = []workflowconfig.AgentBackend{{ID: workflowconfig.DefaultAgentBackendID, Kind: workflowconfig.AgentBackendCodex, Command: shellQuote(bin) + " app-server"}}
	base := filepath.Join(root, "system")
	for _, command := range []string{"start", "register"} {
		t.Run(command, func(t *testing.T) {
			opts := defaultOptions()
			opts.lookupEnv = func(key string) string {
				if key == "PATH" {
					return base
				}
				return ""
			}
			opts.readOrDefault = func(string) (globalconfig.Config, error) { return cfg, nil }
			var captured servicepkg.Config
			opts.service = func(cfg servicepkg.Config) (ServiceRunner, error) { captured = cfg; return &serviceRunnerStub{}, nil }
			host, runtimePort := "", -1
			if _, err := serviceRunnerForCommand(&cobra.Command{Use: command}, &path, &host, &runtimePort, opts); err != nil {
				t.Fatal(err)
			}
			want := base + string(os.PathListSeparator) + filepath.Dir(bin)
			if captured.Path != want {
				t.Fatalf("service PATH = %q, want %q", captured.Path, want)
			}
		})
	}
}
