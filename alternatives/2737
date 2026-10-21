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
			for _, key := range []string{"GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_TERMINAL_PROMPT", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
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
	saved, savedCaptured := hostGitHubCLI.values, hostGitHubCLI.captured
	t.Cleanup(func() { hostGitHubCLI.values, hostGitHubCLI.captured = saved, savedCaptured })
	hostDir := "/host/g h's/config"
	for _, tc := range []struct {
		name, token, hostConfigDir, wantHelper string
	}{
		{name: "handed-off token reads the private config", token: "test-worker-github-token", hostConfigDir: hostDir, wantHelper: "!gh auth git-credential"},
		{name: "no token uses the host default config", token: "", hostConfigDir: "", wantHelper: "!env -u GH_CONFIG_DIR gh auth git-credential"},
		{name: "no token keeps the host GH_CONFIG_DIR", token: " ", hostConfigDir: hostDir, wantHelper: "!env GH_CONFIG_DIR='/host/g h'\\''s/config' gh auth git-credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostGitHubCLI.captured = true
			hostGitHubCLI.values = map[string]*string{"GH_CONFIG_DIR": nil}
			if tc.hostConfigDir != "" {
				dir := tc.hostConfigDir
				hostGitHubCLI.values["GH_CONFIG_DIR"] = &dir
			}
			got := gitCredentialHelperEnvironment(tc.token)
			if got["GIT_CONFIG_VALUE_1"] != tc.wantHelper {
				t.Fatalf("credential helper = %q, want %q", got["GIT_CONFIG_VALUE_1"], tc.wantHelper)
			}
			if got["GIT_TERMINAL_PROMPT"] != "0" || got["GIT_CONFIG_COUNT"] != "2" || got["GIT_CONFIG_KEY_0"] != "credential.helper" || got["GIT_CONFIG_VALUE_0"] != "" || got["GIT_CONFIG_KEY_1"] != "credential.helper" {
				t.Fatalf("git environment = %v, want the prompt disabled and the helper list reset before the gh helper", got)
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
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_TERMINAL_PROMPT", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
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
