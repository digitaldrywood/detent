package gitexec

import (
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestIsLocalGitCommandAndApplyLocalIsolation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantLocal bool
	}{
		{name: "cat-file blob", args: []string{"git", "-C", "/repo", "cat-file", "blob", "abc"}, wantLocal: true},
		{name: "cat-file filters is fenced", args: []string{"git", "-C", "/repo", "cat-file", "--filters", "HEAD:file.go"}, wantLocal: false},
		{name: "symbolic-ref read", args: []string{"git", "-C", "/repo", "symbolic-ref", "--quiet", "--short", "HEAD"}, wantLocal: true},
		{name: "symbolic-ref write is fenced", args: []string{"git", "-C", "/repo", "symbolic-ref", "HEAD", "refs/heads/other"}, wantLocal: false},
		{name: "symbolic-ref abbreviated delete is fenced", args: []string{"git", "-C", "/repo", "symbolic-ref", "--del", "HEAD"}, wantLocal: false},
		{name: "for-each-ref is fenced", args: []string{"git", "-C", "/repo", "for-each-ref", "--format=%(signature:grade)", "refs/heads"}, wantLocal: false},
		{name: "diff-tree raw", args: []string{"git", "-C", "/repo", "diff-tree", "--root", "--no-commit-id", "--raw", "--no-abbrev", "--no-renames", "-r", "-m", "-z", "abc"}, wantLocal: true},
		{name: "diff-tree output file is fenced", args: []string{"git", "-C", "/repo", "diff-tree", "--output=/tmp/out", "abc"}, wantLocal: false},
		{name: "diff-tree ext-diff is fenced", args: []string{"git", "-C", "/repo", "diff-tree", "--ext-diff", "abc"}, wantLocal: false},
		{name: "diff-tree textconv is fenced", args: []string{"git", "-C", "/repo", "diff-tree", "--textconv", "abc"}, wantLocal: false},
		{name: "diff worktree is fenced", args: []string{"git", "-C", "/repo", "diff", "HEAD"}, wantLocal: false},
		{name: "rev-parse", args: []string{"git", "rev-parse", "HEAD"}, wantLocal: true},
		{name: "rev-list range", args: []string{"git", "-C", "/repo", "rev-list", "--count", "base..head"}, wantLocal: true},
		{name: "rev-list signature format is fenced", args: []string{"git", "-C", "/repo", "rev-list", "--format=%G?", "HEAD"}, wantLocal: false},
		{name: "global help is fenced", args: []string{"git", "--help", "rev-parse"}, wantLocal: false},
		{name: "global -p paginate is fenced", args: []string{"git", "-p", "rev-parse", "HEAD"}, wantLocal: false},
		{name: "global --exec-path is fenced", args: []string{"git", "--exec-path=/tmp/evil", "rev-parse", "HEAD"}, wantLocal: false},
		{name: "rev-parse --parseopt is fenced", args: []string{"git", "-C", "/repo", "rev-parse", "--parseopt"}, wantLocal: false},
		{name: "rev-parse --git-path consuming -- still fences trailing flag", args: []string{"git", "-C", "/repo", "rev-parse", "--git-path", "--", "--parseopt"}, wantLocal: false},
		{name: "subcommand help is fenced", args: []string{"git", "-C", "/repo", "rev-parse", "--help"}, wantLocal: false},
		{name: "subcommand short help is fenced", args: []string{"git", "-C", "/repo", "ls-files", "-h"}, wantLocal: false},
		{name: "dash-dash as -C dir still fences subcommand help", args: []string{"git", "-C", "--", "rev-parse", "--help"}, wantLocal: false},
		{name: "ls-files stage is local", args: []string{"git", "-C", "/repo", "ls-files", "--stage", "-z", "--", "file.go"}, wantLocal: true},
		{name: "ls-files modified is fenced", args: []string{"git", "-C", "/repo", "ls-files", "--modified"}, wantLocal: false},
		{name: "checkout is hook-capable and fenced", args: []string{"git", "-C", "/repo", "checkout", "main"}, wantLocal: false},
		{name: "commit is hook-capable and fenced", args: []string{"git", "-C", "/repo", "commit", "-m", "msg"}, wantLocal: false},
		{name: "update-ref is hook-capable and fenced", args: []string{"git", "-C", "/repo", "update-ref", "refs/heads/x", "HEAD"}, wantLocal: false},
		{name: "merge-tree is driver-capable and fenced", args: []string{"git", "-C", "/repo", "merge-tree", "HEAD", "main"}, wantLocal: false},
		{name: "fetch", args: []string{"git", "-C", "/repo", "fetch", "origin"}, wantLocal: false},
		{name: "push", args: []string{"git", "-C", "/repo", "push", "origin", "HEAD"}, wantLocal: false},
		{name: "clone", args: []string{"git", "clone", "https://example.invalid/repo.git"}, wantLocal: false},
		{name: "ls-remote", args: []string{"git", "-C", "/repo", "ls-remote", "origin"}, wantLocal: false},
		{name: "pull", args: []string{"git", "-C", "/repo", "pull"}, wantLocal: false},
		{name: "submodule", args: []string{"git", "-C", "/repo", "submodule", "update"}, wantLocal: false},
		{name: "remote", args: []string{"git", "-C", "/repo", "remote", "update"}, wantLocal: false},
		{name: "unknown subcommand or alias fails closed", args: []string{"git", "-C", "/repo", "custom-helper"}, wantLocal: false},
		{name: "missing subcommand fails closed", args: []string{"git", "-C", "/repo"}, wantLocal: false},
		{name: "non-git command fails closed", args: []string{"sh", "-c", "echo hi"}, wantLocal: false},
	}
	trustedGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("look up git: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := tt.args[0]
			if path == "git" {
				path = trustedGit
			}
			cmd := &exec.Cmd{Path: path, Args: tt.args}
			if got := IsLocalGitCommand(cmd); got != tt.wantLocal {
				t.Fatalf("IsLocalGitCommand(%v) = %v, want %v", tt.args, got, tt.wantLocal)
			}
		})
	}
	spoofed := &exec.Cmd{Path: "/bin/sh", Args: []string{"git", "rev-parse", "HEAD"}}
	if IsLocalGitCommand(spoofed) {
		t.Fatal("IsLocalGitCommand accepted non-git cmd.Path with spoofed Args[0]")
	}
	workspaceGit := &exec.Cmd{Path: "/tmp/workspace/git", Args: []string{"git", "rev-parse", "HEAD"}}
	if IsLocalGitCommand(workspaceGit) {
		t.Fatal("IsLocalGitCommand accepted untrusted workspace git binary")
	}
	traversalGit := &exec.Cmd{Path: filepath.Dir(trustedGit) + "/subdir/../" + filepath.Base(trustedGit), Args: []string{"git", "rev-parse", "HEAD"}}
	if IsLocalGitCommand(traversalGit) {
		t.Fatal("IsLocalGitCommand accepted uncleaned path traversal matching git")
	}
	cmd := exec.Command("git", "cat-file", "blob", "abc")
	cmd.Env = []string{
		"PATH=/usr/bin",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_NO_LAZY_FETCH=0",
		"GIT_OPTIONAL_LOCKS=1",
	}
	ApplyLocalIsolation(cmd)
	for _, forbidden := range []string{"GIT_ALLOW_PROTOCOL=https", "GIT_NO_LAZY_FETCH=0", "GIT_OPTIONAL_LOCKS=1"} {
		if slices.Contains(cmd.Env, forbidden) {
			t.Fatalf("ApplyLocalIsolation retained conflicting env %q in %v", forbidden, cmd.Env)
		}
	}
	for _, want := range []string{"GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0"} {
		if !slices.Contains(cmd.Env, want) {
			t.Fatalf("ApplyLocalIsolation missing %q in %v", want, cmd.Env)
		}
	}
}
