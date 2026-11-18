//go:build !windows

package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestHostGitHubTokenLookupIgnoresPrivateWorkerConfig(t *testing.T) {

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
