package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/testenv"
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

func (f landingFixture) absorb(t *testing.T) {
	t.Helper()
	runGit(t, f.source, "merge", "--squash", f.head)
	runGit(t, f.source, "commit", "-m", "absorb source")
	runGit(t, f.source, "push", "origin", "main")
	runGit(t, f.info.Path, "reset", "--hard", f.remoteMain(t))
}

func TestLocalGitCreateReviewedLanding(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
			receiver := filepath.Join(testenv.TempDir(t), "receiver")
			runGit(t, fixture.source, "clone", "--no-local", "--single-branch", "--branch", "main", fixture.remote, receiver)
			if _, err := runGitAt(t.Context(), receiver, "cat-file", "-e", fixture.head+"^{commit}"); err == nil {
				t.Fatal("receiver already has the external commit")
			}
			repository := "https://github.com/example/repo"
			runGit(t, receiver, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, receiver, "remote", "set-url", "origin", repository+".git")
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(testenv.TempDir(t), "workspaces"), SourceRoot: receiver, AutoBranch: !test.detached})
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
			result, err := f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: test.method, Message: "Land the feature", PushAttemptBranch: true, ValidationCommand: "test -f main.txt && test -f feature.txt"})
			if err != nil {
				t.Fatalf("LandChange() error = %v", err)
			}
			after := f.remoteMain(t)
			if result.MergeSHA != after || result.BaseRef != "main" || result.BaseBefore != before || result.Method != test.method || !result.AttemptBranchPushed {
				t.Fatalf("result = %#v, remote main = %s (was %s)", result, after, before)
			}
			runGit(t, f.source, "fetch", "origin")
			landedTree := strings.TrimSpace(runGit(t, f.source, "rev-parse", after+"^{tree}"))
			if result.Gate.HeadSHA != after || result.Gate.TreeSHA != landedTree || result.Gate.ExitCode != 0 || result.Gate.DurationNS <= 0 {
				t.Fatalf("gate did not validate the landed tree: %#v, landed tree=%s", result.Gate, landedTree)
			}
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

func TestLocalGitLandingCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Parallel()
	for _, outcome := range []string{"landed", "refused", "conflict", "error", "cancelled", "unreported"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newLandingFixture(t)
			opts := LandOptions{HeadSHA: f.head, Method: "squash"}
			issue := f.issue
			preparedOptions := opts
			issue.Landing = &preparedOptions
			info, err := f.backend.infoForIssue(issue)
			if err != nil {
				t.Fatal(err)
			}
			runGit(t, f.source, "worktree", "add", "-b", info.Branch, info.Path, f.head)
			if err := f.backend.recordCleanupOwnership(t.Context(), info, issue, true); err != nil {
				t.Fatal(err)
			}
			if outcome == "refused" || outcome == "error" || outcome == "cancelled" {
				runGit(t, f.source, "worktree", "add", "--detach", filepath.Join(f.backend.root, "landing-"+info.Key), f.head)
			}
			switch outcome {
			case "refused":
				opts.HeadSHA = strings.Repeat("d", 40)
			case "conflict":
				f.advanceMain(t, "feature.txt", "conflicting\n")
			case "error":
				opts.Remote = "missing"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if outcome == "cancelled" {
				cancel()
			}
			result, err := f.backend.LandChange(ctx, info, issue, opts)
			if (outcome == "landed" || outcome == "unreported") != (err == nil) {
				t.Fatalf("landing outcome %s: %v", outcome, err)
			}
			if outcome == "unreported" {
				if err := RecordLanding(t.Context(), info, f.head, result); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.backend.CleanupLanding(context.WithoutCancel(ctx), info, issue); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{info.Path, filepath.Join(f.backend.root, "landing-"+info.Key)} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("landing directory remains: %s: %v", path, err)
				}
				if got := runGit(t, f.source, "worktree", "list", "--porcelain"); strings.Contains(got, filepath.ToSlash(path)) {
					t.Fatalf("landing registration remains: %s\n%s", path, got)
				}
			}
			if outcome == "unreported" {
				runGit(t, f.source, "worktree", "add", "--detach", info.Path, f.head)
				if kept, ok := keptLanding(t.Context(), info.Path, f.head, "refs/remotes/origin/main"); !ok || kept.MergeSHA != result.MergeSHA {
					t.Fatalf("landing receipt lost after cleanup: %+v, %v", kept, ok)
				}
			}
		})
	}
}

func TestLandingRemovalFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	for _, mode := range []string{"git removal fails", "locked"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "git removal fails" && runtime.GOOS == "windows" {
				t.Skip("git fault injection uses a Unix shell wrapper")
			}
			backend := retentionBackend(t)
			head := strings.TrimSpace(runGit(t, backend.sourceRoot, "rev-parse", "HEAD"))
			path := filepath.Join(backend.root, ".detent", "landing", head, "DD-LAND")
			runGit(t, backend.sourceRoot, "worktree", "add", "--detach", path, head)
			var logs bytes.Buffer
			backend.logger = slog.New(slog.NewTextHandler(&logs, nil))
			if mode == "locked" {
				runGit(t, backend.sourceRoot, "worktree", "lock", path)
			} else {
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				wrapper := t.TempDir()
				script := "#!/bin/sh\nif [ \"$3\" = worktree ] && [ \"$4\" = remove ]; then exit 23; fi\nexec " + shellQuote(realGit) + " \"$@\"\n"
				if err := os.WriteFile(filepath.Join(wrapper, "git"), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			err := backend.removeLandingWorktree(t.Context(), backend.sourceRoot, path)
			if mode == "locked" {
				if err == nil {
					t.Fatal("locked worktree removed")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("landing directory remains: %v", err)
			}
			if listed := runGit(t, backend.sourceRoot, "worktree", "list", "--porcelain"); strings.Contains(listed, filepath.ToSlash(path)) {
				t.Fatalf("landing registration remains:\n%s", listed)
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "landing worktree not removed by git") {
				t.Fatalf("removal failure warning missing: %s", &logs)
			}
		})
	}
}

func TestLocalGitLandChangeRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, test := range []struct {
		integration bool
		name        string
		arrange     func(*testing.T, landingFixture) LandOptions
		wantKind    string
		gateFailure bool
	}{
		{name: "absorbed source verifies without another merge", integration: true, arrange: func(t *testing.T, f landingFixture) LandOptions {
			f.absorb(t)
			f.advanceMain(t, "unrelated.txt", "later work\n")
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}},
		{name: "discarded source does not prove integration", integration: true, arrange: func(t *testing.T, f landingFixture) LandOptions {
			f.advanceMain(t, "unrelated.txt", "different work\n")
			runGit(t, f.info.Path, "reset", "--hard", f.remoteMain(t))
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}, wantKind: LandRefusalNothing},
		{name: "reverted absorbed source does not prove integration", integration: true, arrange: func(t *testing.T, f landingFixture) LandOptions {
			f.absorb(t)
			runGit(t, f.source, "rm", "feature.txt")
			runGit(t, f.source, "commit", "-m", "discard feature")
			runGit(t, f.source, "push", "origin", "main")
			runGit(t, f.info.Path, "reset", "--hard", f.remoteMain(t))
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}, wantKind: LandRefusalNothing},
		{name: "revert after the absorbed checkpoint refuses the current base", integration: true, arrange: func(t *testing.T, f landingFixture) LandOptions {
			f.absorb(t)
			runGit(t, f.source, "rm", "feature.txt")
			runGit(t, f.source, "commit", "-m", "revert after checkpoint")
			runGit(t, f.source, "push", "origin", "main")
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}, wantKind: LandRefusalNothing},
		{name: "unpublished checkpoint cannot identify the actual base", integration: true, arrange: func(t *testing.T, f landingFixture) LandOptions {
			return LandOptions{HeadSHA: f.head, Method: "squash"}
		}, wantKind: LandRefusalHeadMoved},
		{name: "empty published source is not a deliverable", integration: true, arrange: func(t *testing.T, f landingFixture) LandOptions {
			f.absorb(t)
			return LandOptions{HeadSHA: strings.TrimSpace(runGit(t, f.source, "rev-parse", "HEAD^")), Method: "squash"}
		}, wantKind: LandRefusalNothing},
		{name: "gate failure preserves reviewed source and remote base", gateFailure: true, arrange: func(t *testing.T, f landingFixture) LandOptions {
			return LandOptions{HeadSHA: f.head, Method: "squash", ValidationCommand: "git cat-file -e lint-failure-sentinel"}
		}},
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
		{name: "gate cannot rewrite the validated source", arrange: func(t *testing.T, f landingFixture) LandOptions {
			t.Helper()
			return LandOptions{HeadSHA: f.head, Method: "squash", ValidationCommand: "printf changed > feature.txt"}
		}, wantKind: LandRefusalHeadMoved},
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
			sourceBase := f.remoteMain(t)
			opts := test.arrange(t, f)
			before := f.remoteMain(t)
			var err error
			var result LandResult
			if test.integration {
				const repository = "https://github.com/example/integration"
				runGit(t, f.source, "remote", "set-url", "origin", repository)
				runGit(t, f.source, "config", "url."+f.remote+".insteadOf", repository)
				opts.Repository = repository
				base := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
				result, verifyErr := f.backend.VerifyIntegratedChange(t.Context(), f.info, f.issue, opts, sourceBase, base)
				err = verifyErr
				if test.wantKind == "" {
					if err != nil || result.MergeSHA != before || result.BaseRef != "main" || result.Method != "squash" || f.remoteMain(t) != before || strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD")) != base {
						t.Fatalf("integration proof = %+v, error = %v", result, err)
					}
					return
				}
			} else {
				result, err = f.backend.LandChange(context.Background(), f.info, f.issue, opts)
			}
			if test.gateFailure {
				if result.Gate.Command != opts.ValidationCommand || result.Gate.ExitCode == 0 || result.Gate.DurationNS <= 0 || !validLandingHead(result.Gate.TreeSHA) {
					t.Fatalf("failed landing gate lost receipt: %#v", result)
				}
				var validation *ValidationError
				if !errors.As(err, &validation) || !strings.Contains(validation.Output, "lint-failure-sentinel") || f.remoteMain(t) != before || strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD")) != f.head {
					t.Fatalf("failed gate changed source or base: %v", err)
				}
				return
			}
			var refusal *LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != test.wantKind {
				t.Fatalf("LandChange() error = %v, want a %s refusal", err, test.wantKind)
			}
			if test.wantKind == LandRefusalConflict && (!result.Rebased || !strings.Contains(refusal.Reason, "feature.txt")) {
				t.Fatalf("conflict retry lost marker or files: result=%#v refusal=%v", result, refusal)
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	f := newLandingFixture(t)
	first, err := f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash", Message: "Land the feature", ValidationCommand: "true"})
	if err != nil {
		t.Fatalf("LandChange() error = %v", err)
	}
	// The report to the hub failed after the push: the landing is kept.
	if err := RecordLanding(context.Background(), f.info, f.head, first); err != nil {
		t.Fatal(err)
	}
	again, err := f.backend.LandChange(context.Background(), f.info, f.issue, LandOptions{HeadSHA: f.head, Method: "squash", Message: "Land the feature", ValidationCommand: "true"})
	if err != nil {
		t.Fatalf("a kept landing was not reported: %v", err)
	}
	if !reflect.DeepEqual(again.Gate, first.Gate) || again.Gate.Command != "true" || again.MergeSHA != first.MergeSHA || again.BaseRef != first.BaseRef || f.remoteMain(t) != first.MergeSHA {
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

func TestLocalGitNativeLanding(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name                    string
		count                   int
		conflict, invalid, skip int
		moved                   bool
		receiptMismatch         string
	}{
		{name: "clean validated head skips gate", count: 1, conflict: -1, invalid: -1, skip: -1},
		{name: "ten heads share one push", count: 10, conflict: -1, invalid: -1, skip: -1},
		{name: "conflict does not hold later members", count: 4, conflict: 1, invalid: -1, skip: -1},
		{name: "preparation failure releases batch", count: 4, conflict: -1, invalid: -1, skip: 1},
		{name: "missing validation refuses", count: 1, conflict: -1, invalid: 0, skip: -1},
		{name: "wrong head receipt refuses", count: 1, conflict: -1, invalid: 0, skip: -1, receiptMismatch: "head"},
		{name: "wrong tree receipt refuses", count: 1, conflict: -1, invalid: 0, skip: -1, receiptMismatch: "tree"},
		{name: "moved base resquashes without a gate", count: 2, conflict: -1, invalid: -1, skip: -1, moved: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLandingFixture(t)
			requests := make([]LandRequest, test.count)
			for i := range requests {
				issue := Issue{Identifier: fmt.Sprintf("native-%d", i)}
				info, err := f.backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				file := fmt.Sprintf("change-%d.txt", i)
				if err := os.WriteFile(filepath.Join(info.Path, file), []byte("feature\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, info.Path, "add", file)
				runGit(t, info.Path, "commit", "-m", file)
				head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				tree := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD^{tree}"))
				options := LandOptions{Native: true, HeadSHA: head, Method: "squash", TargetBranch: "main", Message: file, ValidationCommand: "exit 73", Validation: &gate.CommandResult{Command: "exit 73", HeadSHA: head, TreeSHA: tree, DurationNS: int64(time.Second)}}
				if i == test.invalid {
					switch test.receiptMismatch {
					case "head":
						options.Validation.HeadSHA = strings.Repeat("a", 40)
					case "tree":
						options.Validation.TreeSHA = strings.Repeat("a", 40)
					default:
						options.Validation = nil
					}
				}
				requests[i] = LandRequest{Info: info, Issue: issue, Options: options}
				if i == test.conflict {
					f.advanceMain(t, file, "base conflicts\n")
				}
			}
			pushes := filepath.Join(t.TempDir(), "pushes")
			if err := os.WriteFile(filepath.Join(f.remote, "hooks", "pre-receive"), []byte("#!/bin/sh\necho push >> '"+pushes+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			base := f.remoteMain(t)
			if test.moved {
				before := base
				f.advanceMain(t, "advanced.txt", "concurrent push\n")
				base = f.remoteMain(t)
				runGit(t, f.remote, "update-ref", "refs/heads/main", before)
				if err := os.Remove(pushes); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(t.TempDir(), "advanced")
				hook := "#!/bin/sh\nif [ ! -f '" + marker + "' ]; then\n git --git-dir='" + f.remote + "' update-ref refs/heads/main " + base + " " + before + " || exit 1\n touch '" + marker + "'\nfi\n"
				if err := os.WriteFile(filepath.Join(f.source, ".git", "hooks", "pre-push"), []byte(hook), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			batch := NewLandingBatch(t.Context())
			tickets := make([]*LandingBatchTicket, len(requests))
			for i := range tickets {
				tickets[i] = batch.Add()
			}
			type completion struct {
				i       int
				outcome LandOutcome
			}
			done := make(chan completion, len(requests))
			started := time.Now()
			for i := len(requests) - 1; i >= 0; i-- {
				go func() {
					if i == test.skip {
						tickets[i].Finish()
						done <- completion{i, LandOutcome{Err: context.Canceled}}
						return
					}
					result, err := tickets[i].Land(f.backend, requests[i])
					done <- completion{i, LandOutcome{Result: result, Err: err}}
				}()
			}
			batch.Seal()
			outcomes := make([]LandOutcome, len(requests))
			for range requests {
				result := <-done
				outcomes[result.i] = result.outcome
			}
			if elapsed := time.Since(started); elapsed >= 30*time.Second {
				t.Fatalf("clean batch took %s", elapsed)
			}
			landed := 0
			for i, outcome := range outcomes {
				if i == test.conflict || i == test.invalid || i == test.skip {
					if outcome.Err == nil || outcome.Result.MergeSHA != "" {
						t.Fatalf("member %d was not refused: %#v", i, outcome)
					}
					continue
				}
				if outcome.Err != nil || outcome.Result.Gate.Command != "" || outcome.Result.MergeSHA == "" || outcome.Result.BaseBefore != base {
					t.Fatalf("member %d = %#v, base %s", i, outcome, base)
				}
				wantPath := "clean_push"
				if test.count > 1 {
					wantPath = "batch_member"
				}
				if outcome.Result.Path != wantPath {
					t.Fatalf("path = %q", outcome.Result.Path)
				}
				landed++
				base = outcome.Result.MergeSHA
			}
			if f.remoteMain(t) != base {
				t.Fatal("remote did not advance to final batch head")
			}
			data, err := os.ReadFile(pushes)
			wantPushes := 1
			if test.moved {
				wantPushes = 2
			}
			if landed > 0 && (err != nil || strings.Count(string(data), "push\n") != wantPushes) {
				t.Fatalf("pushes = %q, %v", data, err)
			}
			if landed == 0 && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused work pushed: %q, %v", data, err)
			}
		})
	}
}

func TestLocalGitRebasedNativeLanding(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX lint fixture")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := newLandingFixture(t)
	for name, content := range map[string]string{"go.mod": "module native-landing.test\n\ngo 1.26\n", "calc.go": "package calc\nfunc Value() int { return 1 }\n"} {
		if err := os.WriteFile(filepath.Join(f.info.Path, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, f.info.Path, "add", "go.mod", "calc.go")
	runGit(t, f.info.Path, "commit", "-m", "add calculator")
	first := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(f.info.Path, "calc.go"), []byte("package calc\nfunc Value() int { return 2 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.info.Path, "calc_test.go"), []byte("package calc\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value()!=2 { t.Fatal(Value()) } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.info.Path, "add", "calc.go", "calc_test.go")
	runGit(t, f.info.Path, "commit", "-m", "change calculator")
	head := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
	runGit(t, f.source, "cherry-pick", first)
	runGit(t, f.source, "push", "origin", "main")
	lint := t.TempDir()
	lintLog := filepath.Join(lint, "arguments")
	if err := os.WriteFile(filepath.Join(lint, "golangci-lint"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > '"+lintLog+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", lint+string(os.PathListSeparator)+os.Getenv("PATH"))
	tree := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD^{tree}"))
	result, err := f.backend.LandChange(t.Context(), f.info, f.issue, LandOptions{Native: true, HeadSHA: head, Method: "squash", TargetBranch: "main", Message: "rebased", ValidationCommand: "exit 73", Validation: &gate.CommandResult{Command: "exit 73", HeadSHA: head, TreeSHA: tree, DurationNS: int64(time.Second)}})
	if err != nil || !result.Rebased || result.Path != "rebase_short_validation" || !reflect.DeepEqual(result.Packages, []string{"."}) || result.Gate.HeadSHA != result.MergeSHA || !strings.Contains(result.Gate.Command, "go test -p ${TEST_PROCS:-4} -short -timeout=60s '.'") || !strings.Contains(result.Gate.Output, "native-landing.test") || f.remoteMain(t) != result.MergeSHA {
		t.Fatalf("rebase outcome = %#v, %v", result, err)
	}
	data, err := os.ReadFile(lintLog)
	if err != nil || !strings.Contains(string(data), "--new-from-rev="+result.BaseBefore+" --whole-files .") {
		t.Fatalf("lint scope = %q, %v", data, err)
	}
}
