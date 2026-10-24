package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalGitCreateReadsPublishedBranchWithRepositoryCredential is #1408. The
// runner checkout clones with the GitHub CLI credential helper; the published
// branch lookup and fetches during workspace preparation must carry the same
// credential, or a runner service without a global git credential fails every
// attempt with ls-remote exit status 128 before the provider starts.
func TestLocalGitCreateReadsPublishedBranchWithRepositoryCredential(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	skipWindows(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")

	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "git.log")
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend, err := NewLocalGit(LocalGitOptions{
		Root:       filepath.Join(t.TempDir(), "workspaces"),
		SourceRoot: source,
		AutoBranch: true,
	})
	if err != nil {
		t.Fatalf("NewLocalGit() error = %v", err)
	}
	if _, err := backend.Create(context.Background(), Issue{Identifier: "DD-REMOTE-CREDENTIAL"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	const helper = "-c credential.https://github.com.helper=!gh auth git-credential"
	var lookups, fetches int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(line, " ls-remote ") && !strings.Contains(line, " fetch ") {
			continue
		}
		if !strings.Contains(line, helper) {
			t.Errorf("remote git command without repository credential: %s", line)
		}
		if strings.Contains(line, " ls-remote origin refs/heads/") {
			lookups++
		} else if strings.Contains(line, " fetch ") {
			fetches++
		}
	}
	if lookups == 0 || fetches == 0 {
		t.Fatalf("published branch lookups = %d, fetches = %d, want both; git log:\n%s", lookups, fetches, raw)
	}
}
