package gitexec

import (
	"context"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = IsolatedEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestFetchPublicationBranchIncrementalNegotiation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	work := filepath.Join(root, "work")

	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	packageSize := 0
	for i := range 110 {
		buf := make([]byte, 5*1024)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(source, "blob-"+string(rune('a'+i/26))+string(rune('a'+i%26))+".bin")
		if err := os.WriteFile(name, buf, 0o600); err != nil {
			t.Fatal(err)
		}
		packageSize += len(buf)
	}
	runTestGit(t, source, "add", ".")
	runTestGit(t, source, "commit", "-m", "initial pack exceeding unpackLimit")
	runTestGit(t, source, "remote", "add", "origin", remote)
	runTestGit(t, source, "push", "-u", "origin", "main")
	runTestGit(t, root, "clone", remote, work)

	if err := os.WriteFile(filepath.Join(source, "small.txt"), []byte("tiny update\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "small.txt")
	runTestGit(t, source, "commit", "-m", "small update")
	runTestGit(t, source, "push", "origin", "main")
	wantHead := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))

	objectsDir := filepath.Join(work, ".git", "objects")
	beforeSize := dirTotalBytes(t, objectsDir)

	got, err := FetchPublicationBranch(ctx, work, "origin", remote, "main")
	if err != nil {
		t.Fatalf("FetchPublicationBranch() error = %v", err)
	}
	if got != wantHead {
		t.Fatalf("FetchPublicationBranch() = %q, want %q", got, wantHead)
	}
	delta := dirTotalBytes(t, objectsDir) - beforeSize
	if delta >= int64(packageSize/2) {
		t.Fatalf("incremental fetch grew object store by %d bytes (initial pack %d bytes); expected negotiation to skip existing history", delta, packageSize)
	}
}

func dirTotalBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	if err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

func TestFetchPublicationBranchPreservesSiblingObjectsUnderAutoGC(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	work := filepath.Join(root, "work")

	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "README.md")
	runTestGit(t, source, "commit", "-m", "base")
	runTestGit(t, source, "remote", "add", "origin", remote)
	runTestGit(t, source, "push", "-u", "origin", "main")
	runTestGit(t, root, "clone", remote, work)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, work, cfg...)
	}

	runTestGit(t, work, "checkout", "-b", "sibling")
	if err := os.WriteFile(filepath.Join(work, "sibling.txt"), []byte("sibling only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, work, "add", "sibling.txt")
	runTestGit(t, work, "commit", "-m", "sibling commit")
	siblingSHA := strings.TrimSpace(runTestGit(t, work, "rev-parse", "HEAD"))
	runTestGit(t, work, "checkout", "main")

	runTestGit(t, work, "repack", "-a", "-d")
	packDir := filepath.Join(work, ".git", "objects", "pack")
	for i := range 55 {
		cmd := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
		cmd.Dir = work
		cmd.Env = IsolatedEnvironment()
		cmd.Stdin = strings.NewReader(fmt.Sprintf("pack-blob-%d\n", i))
		oidOut, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hash-object failed: %v\n%s", err, oidOut)
		}
		packCmd := exec.CommandContext(ctx, "git", "pack-objects", "-q", filepath.Join(packDir, "pack"))
		packCmd.Dir = work
		packCmd.Env = IsolatedEnvironment()
		packCmd.Stdin = strings.NewReader(string(oidOut))
		if out, err := packCmd.CombinedOutput(); err != nil {
			t.Fatalf("pack-objects failed: %v\n%s", err, out)
		}
	}
	runTestGit(t, work, "prune-packed")
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	entries, err := os.ReadDir(packDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		p := filepath.Join(packDir, entry.Name())
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	packsBefore := len(entries)

	if err := os.WriteFile(filepath.Join(source, "next.txt"), []byte("next\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "next.txt")
	runTestGit(t, source, "commit", "-m", "advance main")
	runTestGit(t, source, "push", "origin", "main")
	wantHead := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))

	got, err := fetchPublicationBranch(ctx, work, "origin", remote, "main", "-c", "gc.autoDetach=false")
	if err != nil {
		t.Fatalf("FetchPublicationBranch() error = %v", err)
	}
	if got != wantHead {
		t.Fatalf("FetchPublicationBranch() = %q, want %q", got, wantHead)
	}
	if _, err := ReadPublicationSource(ctx, work, "cat-file", "-e", siblingSHA+"^{commit}"); err != nil {
		t.Fatalf("sibling branch commit %s was pruned by scratch fetch: %v", siblingSHA, err)
	}
	entriesAfter, err := os.ReadDir(packDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != packsBefore {
		t.Fatalf("scratch fetch triggered auto-maintenance repack: pack entries changed from %d to %d", packsBefore, len(entriesAfter))
	}
}

func TestFetchPublicationBranchImportsObjectsWithoutMutatingTrackingRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	work := filepath.Join(root, "work")

	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "f.txt")
	runTestGit(t, source, "commit", "-m", "v1")
	initialSHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	runTestGit(t, source, "remote", "add", "origin", remote)
	runTestGit(t, source, "push", "-u", "origin", "main")
	runTestGit(t, root, "clone", remote, work)

	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "f.txt")
	runTestGit(t, source, "commit", "-m", "v2")
	newerSHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	runTestGit(t, source, "push", "origin", "main")

	hookSentinel := filepath.Join(root, "ref-tx-hook-ran")
	hookPath := filepath.Join(work, ".git", "hooks", "reference-transaction")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\ntouch "+hookSentinel+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := FetchPublicationBranch(ctx, work, "origin", remote, "main")
	if err != nil || got != newerSHA {
		t.Fatalf("FetchPublicationBranch() = %q, err = %v, want %q", got, err, newerSHA)
	}
	if _, err := os.Stat(hookSentinel); err == nil {
		t.Fatal("reference-transaction hook executed during publication object fetch")
	}
	if _, err := ReadPublicationSource(ctx, work, "cat-file", "-e", newerSHA+"^{commit}"); err != nil {
		t.Fatalf("fetched commit object missing from repository object store: %v", err)
	}
	if tracked := strings.TrimSpace(runTestGit(t, work, "rev-parse", "refs/remotes/origin/main")); tracked != initialSHA {
		t.Fatalf("tracking ref mutated to %q, want unchanged %q", tracked, initialSHA)
	}
}

func TestFetchCheckpointIncrementalNegotiation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	work := filepath.Join(root, "work")

	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	packageSize := 0
	for i := range 110 {
		buf := make([]byte, 5*1024)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(source, "blob-"+string(rune('a'+i/26))+string(rune('a'+i%26))+".bin")
		if err := os.WriteFile(name, buf, 0o600); err != nil {
			t.Fatal(err)
		}
		packageSize += len(buf)
	}
	runTestGit(t, source, "add", ".")
	runTestGit(t, source, "commit", "-m", "initial pack")
	runTestGit(t, source, "remote", "add", "origin", remote)
	runTestGit(t, source, "push", "-u", "origin", "main")
	runTestGit(t, root, "clone", remote, work)

	if err := os.WriteFile(filepath.Join(source, "cp.txt"), []byte("checkpoint update\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "cp.txt")
	runTestGit(t, source, "commit", "-m", "checkpoint commit")
	cpSHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	cpRef := "refs/detent/checkpoints/test-inc"
	runTestGit(t, source, "push", "origin", cpSHA+":"+cpRef)

	objectsDir := filepath.Join(work, ".git", "objects")
	beforeSize := dirTotalBytes(t, objectsDir)

	if err := FetchCheckpoint(ctx, work, "origin", cpSHA, cpRef); err != nil {
		t.Fatalf("FetchCheckpoint() error = %v", err)
	}
	if _, err := ReadPublicationSource(ctx, work, "cat-file", "-e", cpSHA+"^{commit}"); err != nil {
		t.Fatalf("checkpoint commit %s missing after FetchCheckpoint: %v", cpSHA, err)
	}
	delta := dirTotalBytes(t, objectsDir) - beforeSize
	if delta >= int64(packageSize/2) {
		t.Fatalf("checkpoint fetch grew object store by %d bytes (initial pack %d bytes); expected incremental negotiation", delta, packageSize)
	}

	runTestGit(t, work, "remote", "add", "credremote", "https://user:supersecret@127.0.0.1:1/owner/repo.git")
	credErr := FetchCheckpoint(ctx, work, "credremote", strings.Repeat("a", len(cpSHA)), cpRef)
	if credErr == nil {
		t.Fatal("expected FetchCheckpoint() to fail for unreachable credentialed remote")
	}
	if strings.Contains(credErr.Error(), "supersecret") {
		t.Fatalf("FetchCheckpoint() error leaked remote credentials: %v", credErr)
	}

	runTestGit(t, work, "remote", "add", "badportremote", "https://user:malformedsecret@127.0.0.1:badport/owner/repo.git")
	badPortErr := FetchCheckpoint(ctx, work, "badportremote", strings.Repeat("a", len(cpSHA)), cpRef)
	if badPortErr == nil {
		t.Fatal("expected FetchCheckpoint() to fail for malformed-port credentialed remote")
	}
	if strings.Contains(badPortErr.Error(), "malformedsecret") {
		t.Fatalf("FetchCheckpoint() error leaked malformed-URL remote credentials: %v", badPortErr)
	}

	runTestGit(t, work, "remote", "add", "queryremote", "https://user:pw@127.0.0.1:1/owner/repo.git?token=querysecret")
	queryErr := FetchCheckpoint(ctx, work, "queryremote", strings.Repeat("a", len(cpSHA)), cpRef)
	if queryErr == nil {
		t.Fatal("expected FetchCheckpoint() to fail for query-token remote")
	}
	if strings.Contains(queryErr.Error(), "querysecret") {
		t.Fatalf("FetchCheckpoint() error leaked query-string credentials: %v", queryErr)
	}
	var cmdErr *CommandError
	if errors.As(queryErr, &cmdErr) && cmdErr.Output != "" {
		t.Fatalf("FetchCheckpoint() CommandError.Output was not cleared: %q", cmdErr.Output)
	}
}

func TestFetchCheckpointRespectsGlobalInsteadOf(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	work := filepath.Join(root, "work")

	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "a.txt")
	runTestGit(t, source, "commit", "-m", "v1")
	runTestGit(t, source, "remote", "add", "origin", remote)
	runTestGit(t, source, "push", "-u", "origin", "main")
	runTestGit(t, root, "clone", remote, work)

	if err := os.WriteFile(filepath.Join(source, "cp.txt"), []byte("cp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "cp.txt")
	runTestGit(t, source, "commit", "-m", "cp")
	cpSHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	cpRef := "refs/detent/checkpoints/test-insteadof"
	runTestGit(t, source, "push", "origin", cpSHA+":"+cpRef)

	runTestGit(t, work, "remote", "set-url", "origin", "https://alias.example/owner/repo.git")
	globalCfg := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[url \""+remote+"\"]\n\tinsteadOf = https://alias.example/owner/repo.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	decoyCheckout := filepath.Join(root, "decoy-checkout")
	runTestGit(t, root, "init", "-b", "main", decoyCheckout)
	runTestGit(t, decoyCheckout, "remote", "add", "origin", "https://127.0.0.1:1/decoy.git")
	t.Setenv("GIT_DIR", filepath.Join(decoyCheckout, ".git"))

	if err := FetchCheckpoint(ctx, work, "origin", cpSHA, cpRef); err != nil {
		t.Fatalf("FetchCheckpoint() with global insteadOf and inherited GIT_DIR error = %v", err)
	}
	if _, err := ReadPublicationSource(ctx, work, "cat-file", "-e", cpSHA+"^{commit}"); err != nil {
		t.Fatalf("checkpoint commit %s missing after FetchCheckpoint: %v", cpSHA, err)
	}

	other := filepath.Join(root, "other.git")
	runTestGit(t, root, "init", "--bare", other)
	overlapCfg := filepath.Join(root, "overlap-gitconfig")
	overlapContent := fmt.Sprintf("[url %q]\n\tinsteadOf = https://alias.example/owner/repo.git\n[url %q]\n\tinsteadOf = %s\n", remote, other, remote)
	if err := os.WriteFile(overlapCfg, []byte(overlapContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", overlapCfg)
	if err := FetchCheckpoint(ctx, work, "origin", strings.Repeat("b", len(cpSHA)), cpRef); err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Fatalf("FetchCheckpoint() with overlapping insteadOf err = %v, want redirected error", err)
	}

	spacedRemote := remote + " "
	runTestGit(t, root, "init", "--bare", spacedRemote)
	runTestGit(t, source, "push", spacedRemote, cpSHA+":"+cpRef)
	spaceCfg := filepath.Join(root, "space-gitconfig")
	spaceContent := fmt.Sprintf("[url %q]\n\tinsteadOf = https://alias.example/owner/repo.git\n[url %q]\n\tinsteadOf = %s\n", remote, spacedRemote, remote)
	if err := os.WriteFile(spaceCfg, []byte(spaceContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", spaceCfg)
	if err := FetchCheckpoint(ctx, work, "origin", cpSHA, cpRef); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("FetchCheckpoint() with trailing-space redirect err = %v, want whitespace rejection", err)
	}
}

func TestFetchRemoteBranchNativeLocalRemoteSemantics(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	work := filepath.Join(root, "work")

	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "f.txt")
	runTestGit(t, source, "commit", "-m", "v1")
	v1SHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))

	bareGit := filepath.Join(root, "upstream.git")
	runTestGit(t, root, "init", "--bare", "-b", "main", bareGit)
	runTestGit(t, source, "push", bareGit, "main")
	runTestGit(t, root, "clone", bareGit, work)

	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "f.txt")
	runTestGit(t, source, "commit", "-m", "v2")
	v2SHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	runTestGit(t, source, "push", bareGit, "main")

	if err := os.MkdirAll(filepath.Join(root, "upstream"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "upstream", "HEAD"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"../upstream", "../upstream/"} {
		branch, err := IsolatedRemoteDefaultBranch(ctx, work, rel)
		if err != nil || branch != "main" {
			t.Fatalf("IsolatedRemoteDefaultBranch(%q) = %q, err = %v, want main", rel, branch, err)
		}
		got, err := FetchRemoteBranch(ctx, work, "origin", rel, "main")
		if err != nil || got != v2SHA {
			t.Fatalf("FetchRemoteBranch(%q) = %q, err = %v, want %q", rel, got, err, v2SHA)
		}
	}

	gitfileTarget := filepath.Join(root, "gitfile-target.git")
	runTestGit(t, root, "init", "--bare", "-b", "main", gitfileTarget)
	runTestGit(t, source, "push", gitfileTarget, v2SHA+":refs/heads/main")
	decoySibling := filepath.Join(root, "alias.git")
	runTestGit(t, root, "init", "--bare", "-b", "main", decoySibling)
	runTestGit(t, source, "push", decoySibling, v1SHA+":refs/heads/main")
	if err := os.WriteFile(filepath.Join(root, "alias"), []byte("gitdir: "+gitfileTarget+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotGitfile, err := FetchRemoteBranch(ctx, work, "origin", "../alias", "main")
	if err != nil || gotGitfile != v2SHA {
		t.Fatalf("FetchRemoteBranch(../alias gitfile) = %q, err = %v, want %q", gotGitfile, err, v2SHA)
	}

	homeDir := filepath.Join(root, "home")
	homeRemote := filepath.Join(homeDir, "home-remote.git")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "init", "--bare", "-b", "main", homeRemote)
	runTestGit(t, source, "push", homeRemote, v2SHA+":refs/heads/main")
	t.Setenv("HOME", homeDir)
	gotHome, err := FetchRemoteBranch(ctx, work, "origin", "~/home-remote.git", "main")
	if err != nil || gotHome != v2SHA {
		t.Fatalf("FetchRemoteBranch(~/home-remote.git) = %q, err = %v, want %q", gotHome, err, v2SHA)
	}
}

func TestFetchRemoteBranchShallowClone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	shallowWork := filepath.Join(root, "shallow-work")

	runTestGit(t, root, "init", "--bare", "-b", "main", remote)
	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "f.txt")
	runTestGit(t, source, "commit", "-m", "v1")
	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "f.txt")
	runTestGit(t, source, "commit", "-m", "v2")
	runTestGit(t, source, "remote", "add", "origin", remote)
	runTestGit(t, source, "push", "-u", "origin", "main")

	runTestGit(t, root, "clone", "--depth=1", "-b", "main", "file://"+remote, shallowWork)
	if got := strings.TrimSpace(runTestGit(t, shallowWork, "rev-parse", "--is-shallow-repository")); got != "true" {
		t.Fatalf("expected shallow clone, got is-shallow-repository=%q", got)
	}

	if err := os.WriteFile(filepath.Join(source, "f.txt"), []byte("v3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "f.txt")
	runTestGit(t, source, "commit", "-m", "v3")
	v3SHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	runTestGit(t, source, "push", "origin", "main")

	got, err := FetchRemoteBranch(ctx, shallowWork, "origin", remote, "main")
	if err != nil || got != v3SHA {
		t.Fatalf("FetchRemoteBranch() on shallow clone = %q, err = %v, want %q", got, err, v3SHA)
	}
	if _, err := ReadPublicationSource(ctx, shallowWork, "cat-file", "-e", v3SHA+"^{commit}"); err != nil {
		t.Fatalf("shallow fetched commit missing from object store: %v", err)
	}

	runTestGit(t, source, "checkout", "-b", "sibling", "HEAD~2")
	if err := os.WriteFile(filepath.Join(source, "sibling.txt"), []byte("sibling\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "sibling.txt")
	runTestGit(t, source, "commit", "-m", "sibling checkpoint")
	siblingSHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	siblingRef := "refs/detent/checkpoints/test-shallow-sibling"
	runTestGit(t, source, "push", "origin", siblingSHA+":"+siblingRef)

	if err := FetchCheckpoint(ctx, shallowWork, "origin", siblingSHA, siblingRef); err != nil {
		t.Fatalf("FetchCheckpoint() on shallow clone sibling branch error = %v", err)
	}
	if _, err := ReadPublicationSource(ctx, shallowWork, "cat-file", "-e", siblingSHA+"^{commit}"); err != nil {
		t.Fatalf("sibling checkpoint commit %s missing after shallow FetchCheckpoint: %v", siblingSHA, err)
	}

	isAnc, ancErr := VerifyPublicationAncestryComplete(ctx, shallowWork, siblingSHA, v3SHA)
	if ancErr == nil || isAnc {
		t.Fatalf("VerifyPublicationAncestryComplete() on shallow repo = (%v, %v), want (false, error)", isAnc, ancErr)
	}
	isAncFull, ancFullErr := VerifyPublicationAncestryComplete(ctx, source, siblingSHA, v3SHA)
	if ancFullErr != nil || isAncFull {
		t.Fatalf("VerifyPublicationAncestryComplete() non-ancestor on full repo = (%v, %v), want (false, nil)", isAncFull, ancFullErr)
	}
	v2SHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", v3SHA+"^"))
	isAncTrue, ancTrueErr := VerifyPublicationAncestryComplete(ctx, source, v2SHA, v3SHA)
	if ancTrueErr != nil || !isAncTrue {
		t.Fatalf("VerifyPublicationAncestryComplete() ancestor on full repo = (%v, %v), want (true, nil)", isAncTrue, ancTrueErr)
	}
}

func TestPublicationRepositorySkipsFIFO(t *testing.T) {
	t.Parallel()
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(realRoot, "upstream")
	if out, err := exec.CommandContext(t.Context(), mkfifo, fifoPath).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo failed: %v (%s)", err, out)
	}
	bareGit := filepath.Join(realRoot, "upstream.git")
	runTestGit(t, realRoot, "init", "--bare", "-b", "main", bareGit)

	if got := PublicationRepository("", fifoPath); got != bareGit {
		t.Fatalf("PublicationRepository(fifo) = %q, want %q", got, bareGit)
	}
}

func TestCopyShallowBoundaryRejectsFIFOAndSymlink(t *testing.T) {
	if commonDir := os.Getenv("DETENT_TEST_SHALLOW_FIFO_COMMON"); commonDir != "" {
		if err := copyShallowBoundary(commonDir, os.Getenv("DETENT_TEST_SHALLOW_FIFO_SCRATCH")); err == nil {
			t.Fatal("copyShallowBoundary(fifo) succeeded, want error")
		}
		return
	}
	t.Parallel()
	t.Run("fifo", func(t *testing.T) {
		t.Parallel()
		mkfifo, err := exec.LookPath("mkfifo")
		if err != nil {
			t.Skip("mkfifo unavailable")
		}
		commonDir := t.TempDir()
		scratchDir := t.TempDir()
		fifoPath := filepath.Join(commonDir, "shallow")
		if out, err := exec.CommandContext(t.Context(), mkfifo, fifoPath).CombinedOutput(); err != nil {
			t.Fatalf("mkfifo failed: %v (%s)", err, out)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCopyShallowBoundaryRejectsFIFOAndSymlink$")
		cmd.WaitDelay = time.Second
		cmd.Env = append(os.Environ(),
			"DETENT_TEST_SHALLOW_FIFO_COMMON="+commonDir,
			"DETENT_TEST_SHALLOW_FIFO_SCRATCH="+scratchDir,
			"GOCOVERDIR="+t.TempDir(),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("copyShallowBoundary(fifo) subprocess failed: %v\n%s", err, out)
		}
	})
	for _, tc := range []struct {
		name     string
		relative bool
	}{
		{name: "absolute symlink", relative: false},
		{name: "relative symlink", relative: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			commonDir := t.TempDir()
			scratchDir := t.TempDir()
			target := filepath.Join(commonDir, "real-shallow")
			if err := os.WriteFile(target, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			linkTarget := target
			if tc.relative {
				linkTarget = "real-shallow"
			}
			if err := os.Symlink(linkTarget, filepath.Join(commonDir, "shallow")); err != nil {
				t.Fatal(err)
			}
			if err := copyShallowBoundary(commonDir, scratchDir); err == nil {
				t.Fatalf("copyShallowBoundary(%s) succeeded, want error", tc.name)
			}
		})
	}
}

func TestFetchPreservesGlobalCredentialHelper(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	source := filepath.Join(root, "source")
	work := filepath.Join(root, "work")

	runTestGit(t, root, "init", "--bare", remote)
	runTestGit(t, root, "init", "-b", "main", source)
	for _, cfg := range [][]string{
		{"config", "user.name", "Test"},
		{"config", "user.email", "test@example.com"},
		{"config", "commit.gpgsign", "false"},
	} {
		runTestGit(t, source, cfg...)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "a.txt")
	runTestGit(t, source, "commit", "-m", "v1")
	runTestGit(t, source, "remote", "add", "origin", remote)
	runTestGit(t, source, "push", "-u", "origin", "main")
	runTestGit(t, root, "clone", remote, work)

	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "a.txt")
	runTestGit(t, source, "commit", "-m", "v2")
	branchSHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	runTestGit(t, source, "push", "origin", "main")

	if err := os.WriteFile(filepath.Join(source, "cp.txt"), []byte("cp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "cp.txt")
	runTestGit(t, source, "commit", "-m", "cp")
	cpSHA := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	cpRef := "refs/detent/checkpoints/test-auth"
	runTestGit(t, source, "push", "origin", cpSHA+":"+cpRef)

	const wantUser = "runner-user"
	const wantPass = "runner-secret"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != wantUser || pass != wantPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/repo.git/info/refs" && r.URL.Query().Get("service") == "git-upload-pack" {
			cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", "--advertise-refs", remote)
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			header := "# service=git-upload-pack\n"
			_, _ = fmt.Fprintf(w, "%04x%s0000", len(header)+4, header)
			_, _ = w.Write(out)
			return
		}
		if r.URL.Path == "/repo.git/git-upload-pack" {
			cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", remote)
			cmd.Stdin = r.Body
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = w.Write(out)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	caPath := filepath.Join(root, "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(root, "cred-helper.sh")
	helperScript := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"get\" ]; then\n  printf 'username=%s\\npassword=%s\\n'\nfi\n", wantUser, wantPass)
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	globalCfg := filepath.Join(root, "gitconfig")
	cfgContent := fmt.Sprintf("[http]\n\tsslCAInfo = %s\n[credential]\n\thelper = %s\n", caPath, helperPath)
	if err := os.WriteFile(globalCfg, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	httpsRepo := srv.URL + "/repo.git"
	runTestGit(t, work, "remote", "set-url", "origin", httpsRepo)

	got, err := FetchPublicationBranch(ctx, work, "origin", httpsRepo, "main")
	if err != nil || got != branchSHA {
		t.Fatalf("FetchPublicationBranch() with global credential helper = %q, err = %v, want %q", got, err, branchSHA)
	}
	if err := FetchCheckpoint(ctx, work, "origin", cpSHA, cpRef); err != nil {
		t.Fatalf("FetchCheckpoint() with global credential helper error = %v", err)
	}
	if _, err := ReadPublicationSource(ctx, work, "cat-file", "-e", cpSHA+"^{commit}"); err != nil {
		t.Fatalf("checkpoint commit %s missing after authenticated FetchCheckpoint: %v", cpSHA, err)
	}

	envCACfg := filepath.Join(root, "env-ca-gitconfig")
	if err := os.WriteFile(envCACfg, []byte(fmt.Sprintf("[credential]\n\thelper = %s\n", helperPath)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", envCACfg)
	t.Setenv("GIT_SSL_CAINFO", caPath)
	badSysCfg := filepath.Join(root, "bad-sys-gitconfig")
	if err := os.WriteFile(badSysCfg, []byte(fmt.Sprintf("[url \"https://127.0.0.1:1/broken.git\"]\n\tinsteadOf = %s\n", httpsRepo)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", badSysCfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gotEnvCA, err := FetchPublicationBranch(ctx, work, "origin", httpsRepo, "main")
	if err != nil || gotEnvCA != branchSHA {
		t.Fatalf("FetchPublicationBranch() with GIT_SSL_CAINFO and GIT_CONFIG_NOSYSTEM = %q, err = %v, want %q", gotEnvCA, err, branchSHA)
	}

	mirror := filepath.Join(root, "mirror.git")
	runTestGit(t, root, "init", "--bare", mirror)
	rewriteCfg := filepath.Join(root, "rewrite-gitconfig")
	if err := os.WriteFile(rewriteCfg, []byte("[url \""+mirror+"\"]\n\tinsteadOf = "+remote+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", rewriteCfg)

	if err := PushPublication(ctx, source, remote, cpSHA+":refs/heads/pub-match"); err != nil {
		t.Fatalf("PushPublication() error = %v", err)
	}
	head, ok, err := PublicationBranchHead(ctx, remote, "pub-match")
	if err != nil || !ok || head != cpSHA {
		t.Fatalf("PublicationBranchHead() = (%q, %v, %v), want (%q, true, nil)", head, ok, err, cpSHA)
	}
}
