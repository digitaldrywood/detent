package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestProviderServicePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("service PATH resolution is for Unix service managers")
	}
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

func TestRunnerServiceProviderHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name      string
		fresh     bool
		providers bool
		other     bool
		delayed   bool
		readError bool
		wantError string
	}{
		{name: "provider heartbeat", fresh: true, providers: true},
		{name: "registration report is not service readiness", providers: true, wantError: "no new runner heartbeat"},
		{name: "heartbeat without provider", fresh: true, wantError: "reported no providers"},
		{name: "another runner", fresh: true, providers: true, other: true, wantError: "no new runner heartbeat"},
		{name: "wait for service heartbeat", fresh: true, providers: true, delayed: true},
		{name: "Hub read fails", readError: true, wantError: "read runner before service start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				baseline := time.Now()
				reads := 0
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				stub := &serviceRunnerStub{startResults: []servicepkg.StartResult{{Action: servicepkg.ActionStarted}}}
				service := runnerHeartbeatService{ServiceRunner: stub, cfg: globalconfig.Config{Path: "/runner/global.yaml"}, id: "runner", read: func(context.Context) (runnerauth.Fleet, error) {
					reads++
					if test.readError {
						return runnerauth.Fleet{}, errors.New("unavailable")
					}
					runner := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: "runner"}, LastHeartbeatAt: baseline}
					if reads > 1 && test.fresh && (!test.delayed || reads > 2) {
						runner.LastHeartbeatAt = baseline.Add(time.Second)
					}
					if test.providers {
						runner.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{Provider: "openai"}}}
					}
					if test.other {
						runner.RunnerID = "another"
					}
					return runnerauth.Fleet{Runners: []runnerauth.Runner{runner}}, nil
				}}
				_, err := service.Start(ctx, servicepkg.StartOptions{Install: true})
				if !test.readError && len(stub.startOptions) != 1 {
					t.Fatalf("service starts = %d", len(stub.startOptions))
				}
				if test.wantError == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("heartbeat check = %v, want %s", err, test.wantError)
				}
			})
		})
	}
}

func TestServiceCommandUsesResolvedProviderPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("service PATH resolution is for Unix service managers")
	}
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
