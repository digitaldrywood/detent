package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	runGit(t, source, "config", "commit.gpgsign", "false")
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

// Git for Windows bounds worktree metadata paths. t.TempDir includes the full
// subtest name, which can consume that budget before Git appends its metadata.
// Keep the receiver and landing roots short within the test process's scratch.
func landingTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(filepath.Dir(sourceRepoSeedDir), "landing-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
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

func TestLocalGitCreateReviewedLanding(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name, change, refusal string
		external, detached    bool
		unsafe                bool
		wantError             string
	}{
		{name: "hydrate external reviewed head", external: true},
		{name: "hydrate operator head without external PR"},
		{name: "reuse worker published head", change: "worker"},
		{name: "preserve dirty Code workspace", external: true, change: "dirty"},
		{name: "preserve operator Code workspace and recovery branch", change: "dirty"},
		{name: "preserve dirty detached Code and original branch holder", change: "dirty-held"},
		{name: "preserve Code workspace with detached landing", change: "dirty", detached: true},
		{name: "refuse dirty landing workspace", external: true, change: "landing-dirty", refusal: LandRefusalHeadMoved},
		{name: "refuse moved landing workspace", change: "landing-head", refusal: LandRefusalHeadMoved},
		{name: "refuse foreign landing workspace", change: "landing-repository", refusal: LandRefusalProtected},
		{name: "refuse mutable head", change: "mutable", refusal: LandRefusalMissingHead},
		{name: "unavailable reviewed commit retains hydration failure", change: "missing", wantError: "hydrate reviewed landing head"},
		{name: "refuse redirected landing parent", change: "path", unsafe: true},
		{name: "another reviewed head preserves earlier landing checkout", change: "version"},
		{name: "source lock cancellation preserves owners", change: "locked"},
		{name: "preserve moved source branch", external: true, change: "branch"},
		{name: "preserve another active source worktree", external: true, change: "held"},
		{name: "refuse moved fetched PR head", external: true, change: "remote", refusal: LandRefusalHeadMoved},
		{name: "refuse cross-project source", external: true, change: "repository", refusal: LandRefusalProtected},
		{name: "refuse cross-org external reference", external: true, change: "external", refusal: LandRefusalProtected},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLandingFixture(t)
			branch := "detent/external-source"
			runGit(t, fixture.source, "push", "origin", fixture.head+":refs/heads/"+branch, fixture.head+":refs/pull/7/head")
			receiver := filepath.Join(landingTempDir(t), "receiver")
			runGit(t, fixture.source, "clone", "--no-local", "--single-branch", "--branch", "main", fixture.remote, receiver)
			if _, err := runGitAt(t.Context(), receiver, "cat-file", "-e", fixture.head+"^{commit}"); err == nil {
				t.Fatal("receiver already has the external commit")
			}
			repository := "https://github.com/example/repo"
			runGit(t, receiver, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, receiver, "remote", "set-url", "origin", repository+".git")
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(landingTempDir(t), "workspaces"), SourceRoot: receiver, AutoBranch: !test.detached})
			if err != nil {
				t.Fatal(err)
			}
			var requests []string
			client, err := github.NewClient(github.ClientConfig{
				TokenSource: github.StaticTokenSource(t.Name()),
				HTTPClient: landingHTTPClient(func(req *http.Request) (*http.Response, error) {
					requests = append(requests, req.Method)
					body := fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":"%s","ref":"%s","repo":{"full_name":"example/repo"}},"base":{"ref":"main","repo":{"full_name":"example/repo"}}}`, fixture.head, branch)
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			options := &LandOptions{Repository: repository, HeadSHA: fixture.head, GitHubClient: client}
			if test.external {
				options.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: repository + "/pull/7"}
			}
			issue := Issue{ProjectID: "project", Identifier: "native-98", Landing: options}
			var before Info
			var unchanged []func()
			switch test.change {
			case "mutable":
				options.HeadSHA = "main"
			case "missing":
				options.HeadSHA = strings.Repeat("d", 40)
			case "path":
				skipWindows(t)
				other := initSourceRepo(t)
				if err := os.MkdirAll(filepath.Join(backend.root, ".detent"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, filepath.Join(backend.root, ".detent", "landing")); err != nil {
					t.Fatal(err)
				}
				unchanged = append(unchanged, preserveLandingOwner(t, other))
			case "repository":
				options.Repository = "https://github.com/another/repo"
			case "external":
				options.External.URL = "https://github.com/another/repo/pull/7"
			case "remote":
				runGit(t, fixture.remote, "update-ref", "refs/pull/7/head", fixture.remoteMain(t))
			case "branch", "held":
				runGit(t, receiver, "branch", branch, "main")
				if test.change == "held" {
					active := filepath.Join(t.TempDir(), "active")
					runGit(t, receiver, "worktree", "add", active, branch)
					unchanged = append(unchanged, preserveLandingOwner(t, active))
				}
			case "dirty", "dirty-held", "worker":
				runGit(t, receiver, "fetch", "origin", fixture.head)
				prepared := issue
				prepared.Landing = nil
				if test.external {
					prepared.BranchName = branch
				}
				before, err = backend.Create(t.Context(), prepared)
				if err != nil {
					t.Fatal(err)
				}
				if test.change == "worker" {
					runGit(t, before.Path, "reset", "--hard", fixture.head)
				}
				if test.change == "dirty" || test.change == "dirty-held" {
					recovery := filepath.Join(t.TempDir(), "recovery")
					runGit(t, receiver, "worktree", "add", "-b", "operator/recovery", recovery, fixture.head)
					unchanged = append(unchanged, preserveLandingOwner(t, recovery))
					if err := os.WriteFile(filepath.Join(before.Path, "README.md"), []byte("staged source"), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, before.Path, "add", "README.md")
					if err := os.WriteFile(filepath.Join(before.Path, "uncommitted"), []byte("keep me"), 0o600); err != nil {
						t.Fatal(err)
					}
					if test.change == "dirty-held" {
						runGit(t, before.Path, "checkout", "--detach")
						holder := filepath.Join(t.TempDir(), "original-owner")
						runGit(t, receiver, "worktree", "add", holder, before.Branch)
						unchanged = append(unchanged, preserveLandingOwner(t, holder))
					}
				}
			case "landing-dirty", "landing-head", "version":
				before, err = backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				switch test.change {
				case "landing-dirty":
					if err := os.WriteFile(filepath.Join(before.Path, "uncommitted"), []byte("keep me"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "version":
					options.HeadSHA = fixture.remoteMain(t)
				default:
					runGit(t, before.Path, "reset", "--hard", "main")
				}
			case "landing-repository":
				before, err = backend.infoForIssue(issue)
				if err != nil {
					t.Fatal(err)
				}
				initSourceRepoAt(t, before.Path)
			}
			if before.Path != "" {
				unchanged = append(unchanged, preserveLandingOwner(t, before.Path))
			}
			unchanged = append(unchanged, preserveLandingOwner(t, receiver), preserveLandingOwner(t, fixture.source), preserveLandingOwner(t, fixture.info.Path))
			bundle := filepath.Join(t.TempDir(), "source.bundle")
			runGit(t, fixture.info.Path, "bundle", "create", bundle, "HEAD")
			ctx := t.Context()
			if test.change == "locked" {
				release, err := backend.acquireSourceOperation(ctx)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			info, err := backend.Create(ctx, issue)
			if test.change == "locked" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Create() error = %v, want cancellation while source lock is held", err)
				}
			} else if test.unsafe {
				if !errors.Is(err, ErrUnsafePath) {
					t.Fatalf("Create() error = %v, want unsafe path", err)
				}
			} else if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Create() error = %v, want %s", err, test.wantError)
				}
			} else if test.refusal != "" {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != test.refusal {
					t.Fatalf("Create() error = %v, want %s", err, test.refusal)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				head, err := backend.Head(t.Context(), info, issue)
				if err != nil || strings.TrimSpace(head) != options.HeadSHA || test.external && (info.ReviewBranch != branch || info.Branch == branch) {
					t.Fatalf("landing workspace = %#v, head %s, error %v", info, head, err)
				}
				if before.Path != "" && info.Path == before.Path {
					t.Fatal("landing reused Code workspace")
				}
				if err := backend.VerifyReviewTree(t.Context(), info, issue); err != nil {
					t.Fatal(err)
				}
			}
			if test.change == "dirty" {
				if contents, err := os.ReadFile(filepath.Join(before.Path, "uncommitted")); err != nil || string(contents) != "keep me" {
					t.Fatalf("dirty workspace was altered: %s, %v", contents, err)
				}
			}
			if _, err := os.Stat(bundle); err != nil {
				t.Fatal("source bundle was removed", err)
			}
			if head := strings.TrimSpace(runGit(t, fixture.info.Path, "rev-parse", "HEAD")); head != fixture.head {
				t.Fatal("source worktree was changed", head)
			}
			for _, check := range unchanged {
				check()
			}
			for _, method := range requests {
				if method != http.MethodGet {
					t.Fatalf("workspace hydration performed external writes: %v", requests)
				}
			}
		})
	}
}

func preserveLandingOwner(t *testing.T, path string) func() {
	t.Helper()
	files := make(map[string][]byte)
	if err := filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(name)
		files[name] = data
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HEAD", "index"} {
		gitPath := strings.TrimSpace(runGit(t, path, "rev-parse", "--git-path", name))
		if !filepath.IsAbs(gitPath) {
			gitPath = filepath.Join(path, gitPath)
		}
		data, err := os.ReadFile(gitPath)
		if err != nil {
			t.Fatal(err)
		}
		files[gitPath] = data
	}
	head := runGit(t, path, "rev-parse", "HEAD")
	return func() {
		t.Helper()
		for name, expected := range files {
			actual, err := os.ReadFile(name)
			if err != nil || !bytes.Equal(actual, expected) {
				t.Fatalf("landing changed owner file %s: %v", name, err)
			}
		}
		if actual := runGit(t, path, "rev-parse", "HEAD"); actual != head {
			t.Fatalf("landing changed owner head at %s: %s", path, actual)
		}
	}
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
