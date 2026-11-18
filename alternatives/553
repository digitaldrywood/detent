//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestWorkspaceHookGitHubTokenInheritance(t *testing.T) {

	source := initRunnerSourceRepo(t)
	// Cleanup preserves unpublished commits; publish the fixture to a local remote.
	remote := filepath.Join(t.TempDir(), "remote.git")
	runRunnerGit(t, source, "init", "--bare", "-b", "main", remote)
	runRunnerGit(t, source, "remote", "add", "origin", remote)
	runRunnerGit(t, source, "push", "-u", "origin", "main")
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(key, "operator-secret")
	}
	tests := []struct {
		name   string
		kind   string
		prefix string
		want   string
	}{
		{name: "native", kind: workflowconfig.TrackerHubNative, want: "|||"},
		{name: "native explicit hook credential", kind: workflowconfig.TrackerHubNative, prefix: "GH_TOKEN=hook-opt-in; ", want: "hook-opt-in|||"},
		{name: "GitHub", kind: workflowconfig.TrackerGitHub, want: "operator-secret|operator-secret|operator-secret|operator-secret"},
	}
	for _, tt := range tests {
		for _, backendKind := range []string{workspace.KindLocalGit, workspace.KindFilesystem} {
			t.Run(tt.name+"/"+backendKind, func(t *testing.T) {
				cfg := workflowconfig.Default()
				cfg.Tracker.Kind = tt.kind
				cfg.Workspace.Kind = backendKind
				cfg.Workspace.Root = filepath.Join(t.TempDir(), "workspaces")
				trace := filepath.Join(t.TempDir(), "tokens")
				hook := tt.prefix + "printf '%s|%s|%s|%s\\n' \"$GH_TOKEN\" \"$GITHUB_TOKEN\" \"$GH_ENTERPRISE_TOKEN\" \"$GITHUB_ENTERPRISE_TOKEN\" >> " + runnerShellQuote(trace)
				cfg.Hooks.AfterCreate = hook
				cfg.Hooks.BeforeRun = hook
				cfg.Hooks.AfterRun = hook
				cfg.Hooks.BeforeRemove = hook

				backend, err := buildWorkspaceBackend(cfg, source, nil)
				if err != nil {
					t.Fatal(err)
				}
				issue := workspace.Issue{Identifier: "DD-3187"}
				info, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				if err := backend.BeforeRun(t.Context(), info, issue); err != nil {
					t.Fatal(err)
				}
				backend.AfterRun(t.Context(), info, issue)
				if err := backend.Cleanup(t.Context(), info.Key); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(trace)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := string(data), strings.Repeat(tt.want+"\n", 4); got != want {
					t.Fatalf("hook tokens = %q, want %q", got, want)
				}
			})
		}
	}
}
