package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// landingFixture is a source checkout with a bare origin, a workspace backend
// over it, and one attempt worktree with a reviewed commit on its branch.
type landingFixture struct {
	source  string
	remote  string
	backend *LocalGit
	issue   Issue
	info    Info
	head    string
}

func newLandingFixture(t *testing.T) landingFixture {
	t.Helper()
	source := initSourceRepo(t)
	remote := initBareRemote(t)
	runGit(t, source, "remote", "add", "origin", remote)
	runGit(t, source, "push", "-u", "origin", "main")
	backend, err := NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatalf("NewBackend() error = %v", err)
	}
	local, ok := backend.(*LocalGit)
	if !ok {
		t.Fatalf("backend is %T, want *LocalGit", backend)
	}
	issue := Issue{Identifier: "DD-LAND"}
	info, err := backend.Create(context.Background(), issue)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, info.Path, "add", "feature.txt")
	runGit(t, info.Path, "commit", "-m", "feature one")
	if err := os.WriteFile(filepath.Join(info.Path, "feature.txt"), []byte("feature\nmore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, info.Path, "commit", "-am", "feature two")
	head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
	return landingFixture{source: source, remote: remote, backend: local, issue: issue, info: info, head: head}
}

func (f landingFixture) remoteMain(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(runGit(t, f.remote, "rev-parse", "refs/heads/main"))
}

func (f landingFixture) advanceMain(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.source, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.source, "add", name)
	runGit(t, f.source, "commit", "-m", "main: "+name)
	runGit(t, f.source, "push", "origin", "main")
}

func TestLocalGitLandChangeMethods(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		method      string
		wantParents int
		wantCommits int
	}{
		{name: "squash", method: "squash", wantParents: 1, wantCommits: 1},
		{name: "merge", method: "merge", wantParents: 2, wantCommits: 1},
		{name: "rebase", method: "rebase", wantParents: 1, wantCommits: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newLandingFixture(t)
			f.advanceMain(t, "main.txt", "main\n")
			before := f.remoteMain(t)
			result, err := f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: test.method, Message: "Land the feature", PushAttemptBranch: true})
			if err != nil {
				t.Fatalf("LandChange() error = %v", err)
			}
			after := f.remoteMain(t)
			if result.MergeSHA != after || result.BaseRef != "main" || result.BaseBefore != before || result.Method != test.method || !result.AttemptBranchPushed {
				t.Fatalf("result = %#v, remote main = %s (was %s)", result, after, before)
			}
			runGit(t, f.source, "fetch", "origin")
			parents := strings.Fields(strings.TrimSpace(runGit(t, f.source, "rev-list", "--parents", "-n", "1", after)))
			if len(parents)-1 != test.wantParents {
				t.Fatalf("landed commit has %d parents, want %d", len(parents)-1, test.wantParents)
			}
			commits := strings.Split(strings.TrimSpace(runGit(t, f.source, "rev-list", before+".."+after)), "\n")
			if test.method != "merge" && len(commits) != test.wantCommits {
				t.Fatalf("base advanced by %d commits, want %d", len(commits), test.wantCommits)
			}
			tree := runGit(t, f.source, "show", after+":feature.txt")
			if !strings.Contains(tree, "more") {
				t.Fatalf("landed feature.txt = %q", tree)
			}
			if got := strings.TrimSpace(runGit(t, f.remote, "rev-parse", "refs/heads/"+f.info.Branch)); got != f.head {
				t.Fatalf("attempt branch on the remote = %s, want %s", got, f.head)
			}
			if entries, err := os.ReadDir(filepath.Dir(f.info.Path)); err == nil {
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), "landing-") {
						t.Fatalf("landing worktree %s was left behind", entry.Name())
					}
				}
			}
		})
	}
}

func TestLocalGitLandChangeRefusals(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		arrange  func(*testing.T, landingFixture) LandOptions
		wantKind string
	}{
		{name: "worktree moved past the reviewed head", arrange: func(t *testing.T, f landingFixture) LandOptions {
			t.Helper()
			if err := os.WriteFile(filepath.Join(f.info.Path, "late.txt"), []byte("late\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, f.info.Path, "add", "late.txt")
			runGit(t, f.info.Path, "commit", "-m", "after review")
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}, wantKind: LandRefusalHeadMoved},
		{name: "reviewed head unknown to the checkout", arrange: func(t *testing.T, f landingFixture) LandOptions {
			return LandOptions{HeadSHA: strings.Repeat("d", 40), Method: "squash"}
		}, wantKind: LandRefusalMissingHead},
		{name: "conflict with the base", arrange: func(t *testing.T, f landingFixture) LandOptions {
			t.Helper()
			f.advanceMain(t, "feature.txt", "conflicting\n")
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}, wantKind: LandRefusalConflict},
		{name: "nothing left to land", arrange: func(t *testing.T, f landingFixture) LandOptions {
			t.Helper()
			runGit(t, f.info.Path, "push", "origin", f.head+":refs/heads/main")
			return LandOptions{HeadSHA: f.head, Method: "merge"}
		}, wantKind: LandRefusalNothing},
		{name: "base branch requires pull requests", arrange: func(t *testing.T, f landingFixture) LandOptions {
			t.Helper()
			hook := filepath.Join(f.remote, "hooks", "pre-receive")
			if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'GH006: Protected branch update failed for refs/heads/main.' >&2\necho 'Changes must be made through a pull request.' >&2\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}, wantKind: LandRefusalProtected},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newLandingFixture(t)
			opts := test.arrange(t, f)
			before := f.remoteMain(t)
			_, err := f.backend.LandChange(context.Background(), f.info, f.issue, opts)
			var refusal *LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != test.wantKind {
				t.Fatalf("LandChange() error = %v, want a %s refusal", err, test.wantKind)
			}
			if test.wantKind == LandRefusalProtected && !strings.Contains(refusal.Reason, "enable GitHub pull request mode") {
				t.Fatalf("protected refusal reason = %q", refusal.Reason)
			}
			if after := f.remoteMain(t); after != before {
				t.Fatalf("a refused landing moved the base from %s to %s", before, after)
			}
			if status := strings.TrimSpace(runGit(t, f.info.Path, "status", "--porcelain")); status != "" {
				t.Fatalf("a refused landing dirtied the attempt worktree: %q", status)
			}
		})
	}
}

func TestClassifyLandingPush(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		out  string
		want string
	}{
		{name: "github protected branch", out: "remote: error: GH006: Protected branch update failed for refs/heads/develop.", want: LandRefusalProtected},
		{name: "hook declined", out: "! [remote rejected] main -> main (pre-receive hook declined)", want: LandRefusalProtected},
		{name: "base moved", out: "! [rejected] main -> main (fetch first)", want: LandRefusalBaseMoved},
		{name: "stale lease", out: "! [rejected] main -> main (stale info)", want: LandRefusalBaseMoved},
		{name: "unknown", out: "fatal: unable to access 'https://example.test/': Could not resolve host", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := classifyLandingPush(&CommandError{Command: "git", Args: []string{"push"}, ExitCode: 1, Output: test.out}, "main")
			var refusal *LandRefusal
			if errors.As(err, &refusal) != (test.want != "") || refusal != nil && refusal.Kind != test.want {
				t.Fatalf("classifyLandingPush(%q) = %v, want kind %q", test.out, err, test.want)
			}
		})
	}
}

func TestLocalGitLandChangeReportsAKeptLanding(t *testing.T) {
	t.Parallel()
	f := newLandingFixture(t)
	first, err := f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash", Message: "Land the feature"})
	if err != nil {
		t.Fatalf("LandChange() error = %v", err)
	}
	// The report to the hub failed after the push: the landing is kept.
	if err := RecordLanding(context.Background(), f.info, f.head, first); err != nil {
		t.Fatal(err)
	}
	again, err := f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash", Message: "Land the feature"})
	if err != nil {
		t.Fatalf("a kept landing was not reported: %v", err)
	}
	if again.MergeSHA != first.MergeSHA || again.BaseRef != first.BaseRef || f.remoteMain(t) != first.MergeSHA {
		t.Fatalf("kept landing = %#v, first = %#v, remote = %s", again, first, f.remoteMain(t))
	}
	if err := ForgetLanding(context.Background(), f.info); err != nil {
		t.Fatal(err)
	}
	if _, err := f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash"}); err == nil {
		t.Fatal("after the hub has the landing, landing the same head again was not refused")
	}
	// A kept landing whose commit is no longer on the base is forgotten.
	if err := RecordLanding(context.Background(), f.info, f.head, LandResult{MergeSHA: strings.Repeat("d", 40), BaseRef: "main", Method: "squash"}); err != nil {
		t.Fatal(err)
	}
	var refusal *LandRefusal
	_, err = f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash"})
	if !errors.As(err, &refusal) || refusal.Kind != LandRefusalNothing {
		t.Fatalf("a stale kept landing was reported: %v", err)
	}
}
