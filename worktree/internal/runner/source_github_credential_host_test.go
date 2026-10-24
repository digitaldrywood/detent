package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestSourceGitCredentialsUseGitHubHostForNativeTracker(t *testing.T) {
	for _, tc := range []struct {
		name, kind, endpoint, wantHost string
	}{
		{"native inherited tracker endpoint", config.TrackerHubNative, config.Default().Tracker.Endpoint, "github.com"},
		{"native custom tracker endpoint", config.TrackerHubNative, "https://tracker.example.test/graphql", "github.com"},
		{"github cloud", config.TrackerGitHub, "https://api.github.com/graphql", "github.com"},
		{"github enterprise", config.TrackerGitHub, "https://code.example.test/api/graphql", "code.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
				t.Setenv(key, os.Getenv(key))
			}
			cfg := config.Default()
			cfg.Tracker.Kind, cfg.Tracker.Endpoint = tc.kind, tc.endpoint
			cfg.Worker.GitHubToken = "test-worker-github-token"
			cfg.Workspace.Kind = workspace.KindLocalGit
			cfg.Workspace.SourceRoot = t.TempDir()
			if err := os.Mkdir(filepath.Join(cfg.Workspace.SourceRoot, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			if err := PrepareSSHSource(t.Context(), cfg, cfg.Workspace.SourceRoot, "", scratch); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(scratch, "github-cli", "hosts.yml"))
			if err != nil {
				t.Fatal(err)
			}
			var hosts map[string]map[string]string
			if err := json.Unmarshal(raw, &hosts); err != nil {
				t.Fatal(err)
			}
			selected, ok := hosts[tc.wantHost]
			if !ok || len(hosts) != 1 {
				var names []string
				for name := range hosts {
					names = append(names, name)
				}
				t.Fatalf("source Git credential hosts = %v, want only %q for tracker %q", names, tc.wantHost, tc.kind)
			}
			if selected["oauth_token"] != cfg.Worker.GitHubToken {
				t.Fatal("source Git credential handoff lost the selected token")
			}
		})
	}
}

func TestWorkerGitCredentialHelperFollowsTokenHandoff(t *testing.T) {
	for _, test := range []struct {
		name, token, hostDir, want string
		hostConfigured             bool
	}{
		{name: "worker token", token: "worker-token", want: "!gh auth git-credential"},
		{name: "blank token", token: " \t", want: "!env -u GH_CONFIG_DIR gh auth git-credential"},
		{name: "host default", want: "!env -u GH_CONFIG_DIR gh auth git-credential"},
		{name: "host custom directory", hostConfigured: true, hostDir: "/configured/host gh", want: "!env GH_CONFIG_DIR='/configured/host gh' gh auth git-credential"},
		{name: "quoted host directory", hostConfigured: true, hostDir: "/configured/host's gh", want: "!env GH_CONFIG_DIR='/configured/host'\\''s gh' gh auth git-credential"},
		{name: "empty host directory", hostConfigured: true, want: "!env GH_CONFIG_DIR='' gh auth git-credential"},
		{name: "worker token with host directory", token: "worker-token", hostConfigured: true, hostDir: "/configured/host gh", want: "!gh auth git-credential"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hostGitHubCLI.mu.Lock()
			captured, values := hostGitHubCLI.captured, hostGitHubCLI.values
			hostGitHubCLI.captured = true
			hostGitHubCLI.values = map[string]*string{"GH_CONFIG_DIR": nil}
			if test.hostConfigured {
				hostGitHubCLI.values["GH_CONFIG_DIR"] = &test.hostDir
			}
			hostGitHubCLI.mu.Unlock()
			t.Cleanup(func() {
				hostGitHubCLI.mu.Lock()
				hostGitHubCLI.captured, hostGitHubCLI.values = captured, values
				hostGitHubCLI.mu.Unlock()
			})
			got := gitCredentialHelperEnvironment(test.token)
			if got["GIT_CONFIG_VALUE_1"] != test.want || got["GIT_CONFIG_COUNT"] != "2" || got["GIT_CONFIG_KEY_0"] != "credential.helper" || got["GIT_CONFIG_KEY_1"] != "credential.helper" || got["GIT_CONFIG_VALUE_0"] != "" {
				t.Fatalf("credential helper environment = %#v", got)
			}
			if askpass, ok := got["GIT_ASKPASS"]; !ok || askpass != "" || got["GIT_TERMINAL_PROMPT"] != "0" {
				t.Fatalf("interactive authentication enabled: %#v", got)
			}
		})
	}
}

func TestHostGitHubTokenLookupIgnoresPrivateWorkerConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	hostConfig := t.TempDir()
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$GH_CONFIG_DIR\" = \"" + hostConfig + "\" ] || { echo \"no oauth token found for github.com\" >&2; exit 1; }\necho host-token\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		t.Setenv(key, os.Getenv(key))
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CONFIG_DIR", hostConfig)
	saved := hostGitHubCLI.values
	savedCaptured := hostGitHubCLI.captured
	hostGitHubCLI.values, hostGitHubCLI.captured = nil, false
	t.Cleanup(func() { hostGitHubCLI.values, hostGitHubCLI.captured = saved, savedCaptured })

	cfg := config.Default()
	cfg.Tracker.Kind = config.TrackerHubNative
	cfg.Workspace.Kind = workspace.KindFilesystem
	scratch := t.TempDir()
	if err := PrepareSSHSource(t.Context(), cfg, t.TempDir(), "", scratch); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("GH_CONFIG_DIR"); got != filepath.Join(scratch, "github-cli") {
		t.Fatalf("worker GH_CONFIG_DIR = %q, want private worker config", got)
	}
	token, err := defaultWorkerGitHubToken(t.Context())
	if err != nil || token != "host-token" {
		t.Fatalf("host token lookup = %q, %v; want the host gh login", token, err)
	}
}
