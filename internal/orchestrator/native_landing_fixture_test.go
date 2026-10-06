package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeLandingJourney struct {
	runner     *runpkg.Runner
	execution  *nativeLandingJourneyExecution
	provider   *mergeFastPathAgentBackend
	issue      connector.Issue
	target     runpkg.NativeLandingTarget
	base       string
	remote     string
	merge      string
	mergeCalls *atomic.Int64
	info       workspace.Info
}

func nativeLandingGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newNativeLandingJourney(t *testing.T, issue connector.Issue, mergeMessage string, mergeStatus int, sourceConflict, currentBase bool) *nativeLandingJourney {
	t.Helper()
	if testing.Short() {
		t.Skip("real git and native landing integration")
	}
	if mergeStatus == 0 {
		mergeStatus = http.StatusMethodNotAllowed
	}
	source := testenv.TempDir(t)
	remote := filepath.Join(testenv.TempDir(t), "origin.git")
	nativeLandingGit(t, t.Context(), source, "init", "-b", "main")
	nativeLandingGit(t, t.Context(), source, "config", "user.name", "Landing Test")
	nativeLandingGit(t, t.Context(), source, "config", "user.email", "landing@example.test")
	nativeLandingGit(t, t.Context(), source, "config", "commit.gpgsign", "false")
	nativeLandingGit(t, t.Context(), source, "config", "core.hooksPath", os.DevNull)
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflictFiles := []string{"internal/web/templates/work_templ.go", "internal/store/sqlc/db.go"}
	if sourceConflict {
		for _, name := range conflictFiles {
			path := filepath.Join(source, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("initial generated source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	nativeLandingGit(t, t.Context(), source, "add", ".")
	nativeLandingGit(t, t.Context(), source, "commit", "-m", "initial")
	nativeLandingGit(t, t.Context(), source, "init", "--bare", "-b", "main", remote)
	repository := "https://github.com/example/repo"
	nativeLandingGit(t, t.Context(), source, "config", "url.file://"+remote+".insteadOf", repository+".git")
	nativeLandingGit(t, t.Context(), source, "remote", "add", "origin", repository+".git")
	nativeLandingGit(t, t.Context(), source, "push", "-u", "origin", "main")
	base := nativeLandingGit(t, t.Context(), remote, "rev-parse", "refs/heads/main")
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(testenv.TempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "default", ID: issue.ID, Identifier: issue.Identifier})
	if err != nil {
		t.Fatal(err)
	}
	issue.BranchName = info.Branch
	if err := os.WriteFile(filepath.Join(info.Path, "feature.txt"), []byte("reviewed feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sourceConflict {
		for _, name := range conflictFiles {
			if err := os.WriteFile(filepath.Join(info.Path, name), []byte("reviewed generated source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, name), []byte("parallel generated source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	nativeLandingGit(t, t.Context(), info.Path, "add", ".")
	nativeLandingGit(t, t.Context(), info.Path, "commit", "-m", "reviewed feature")
	head := nativeLandingGit(t, t.Context(), info.Path, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "parallel.txt"), []byte("parallel landing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeLandingGit(t, t.Context(), source, "add", ".")
	nativeLandingGit(t, t.Context(), source, "commit", "-m", "parallel landing")
	freshBase := nativeLandingGit(t, t.Context(), source, "rev-parse", "HEAD")
	if currentBase {
		freshBase = base
	}
	var merge string
	if sourceConflict {
		cmd := exec.CommandContext(t.Context(), "git", "-C", source, "merge-tree", "--write-tree", "--name-only", freshBase, head)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("fixture did not reproduce a source conflict: %s, %v", output, err)
		}
		for _, name := range conflictFiles {
			if !strings.Contains(string(output), name) {
				t.Fatalf("fixture conflict missing %s: %s", name, output)
			}
		}
	} else {
		tree := nativeLandingGit(t, t.Context(), source, "merge-tree", "--write-tree", freshBase, head)
		merge = nativeLandingGit(t, t.Context(), source, "commit-tree", tree, "-p", freshBase, "-m", "land reviewed feature")
	}
	var puts atomic.Int64
	publishedBranch := info.Branch
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/graphql":
			fmt.Fprint(w, `{"data":{"viewer":{"databaseId":42,"login":"detent-worker[bot]","__typename":"Bot"}}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/example/repo/pulls":
			publishedBranch = strings.TrimPrefix(request.URL.Query().Get("head"), "example:")
			fmt.Fprintf(w, `[{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/repo"}},"base":{"sha":%q,"ref":"main","repo":{"full_name":"example/repo"}}}]`, head, publishedBranch, nativeLandingGit(t, request.Context(), remote, "rev-parse", "refs/heads/main"))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/example/repo/pulls/7":
			fmt.Fprintf(w, `{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/repo"}},"base":{"sha":%q,"ref":"main","repo":{"full_name":"example/repo"}}}`, head, publishedBranch, base)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/example/repo/pulls/7/merge":
			body, err := io.ReadAll(request.Body)
			published := nativeLandingGit(t, request.Context(), remote, "rev-parse", "refs/heads/"+publishedBranch)
			if err != nil || !strings.Contains(string(body), published) || !strings.Contains(string(body), "squash") {
				t.Errorf("atomic merge lost reviewed head or policy: %s, %v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if mergeStatus == http.StatusOK {
				nativeLandingGit(t, request.Context(), source, "push", "origin", merge+":refs/heads/main")
				fmt.Fprintf(w, `{"merged":true,"sha":%q}`, merge)
				return
			}
			if puts.Add(1) == 1 || sourceConflict || mergeStatus == http.StatusMethodNotAllowed && !strings.HasPrefix(mergeMessage, "Base branch was modified") && mergeMessage != "Pull Request has merge conflicts" {
				nativeLandingGit(t, request.Context(), source, "push", "origin", freshBase+":refs/heads/main")
				w.WriteHeader(mergeStatus)
				fmt.Fprintf(w, `{"message":%q}`, mergeMessage)
				return
			}
			if fetched := nativeLandingGit(t, request.Context(), info.Path, "rev-parse", "refs/remotes/origin/main"); fetched != freshBase {
				t.Errorf("retry used stale base %s, want %s", fetched, freshBase)
			}
			nativeLandingGit(t, request.Context(), source, "push", "origin", merge+":refs/heads/main")
			fmt.Fprintf(w, `{"merged":true,"sha":%q}`, merge)
		default:
			t.Errorf("unexpected forge operation %s %s", request.Method, request.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: nativeLandingTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "native-landing.test" {
			return nil, fmt.Errorf("unexpected landing host %s", request.URL.Host)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Result(), nil
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	cfg := config.Config{}
	cfg.Gate.Run = "true"
	cfg.Worker.GitHubToken = "native-landing-test-token"
	cfg.Tracker.Kind = config.TrackerGitHub
	cfg.Tracker.Endpoint = "https://native-landing.test/graphql"
	provider := &mergeFastPathAgentBackend{}
	agent, err := runpkg.NewRunner(runpkg.Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: provider, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	target := runpkg.NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Repository: repository, Method: "squash", GitHubPullRequest: true}
	execution := &nativeLandingJourneyExecution{target: target}
	return &nativeLandingJourney{runner: agent, execution: execution, provider: provider, issue: issue, target: target, base: base, remote: remote, merge: merge, info: info, mergeCalls: &puts}
}

type nativeLandingTransport func(*http.Request) (*http.Response, error)

func (f nativeLandingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (j *nativeLandingJourney) run(t *testing.T) (runpkg.RunResult, error) {
	t.Helper()
	result, err := j.runner.Run(t.Context(), runpkg.RunRequest{Mode: runpkg.RunModeMerge, Issue: j.issue, Execution: j.execution})
	if result.NativeLanding == nil || result.NativeLanding.ChangeID != j.target.ChangeID || result.NativeLanding.VersionID != j.target.VersionID || result.NativeLanding.HeadSHA != j.target.HeadSHA || nativeLandingGit(t, t.Context(), j.info.Path, "rev-parse", "HEAD") != j.target.HeadSHA {
		t.Fatalf("landing lost immutable identity: result %#v, error %v", result, err)
	}
	if result.NativeLanding.Landed && (result.NativeLanding.MergeSHA != j.merge || nativeLandingGit(t, t.Context(), j.remote, "rev-parse", "refs/heads/main") != j.merge) {
		t.Fatalf("landing receipt differs from actual base: %#v", result.NativeLanding)
	}
	return result, err
}

type nativeLandingJourneyExecution struct {
	target   runpkg.NativeLandingTarget
	started  int
	recorded []runpkg.NativeLanding
}

func (*nativeLandingJourneyExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (*nativeLandingJourneyExecution) Validate(context.Context) error { return nil }
func (*nativeLandingJourneyExecution) Start(context.Context, tracker.NativeExecutionIdentity) error {
	return errors.New("landing must not start coding or request review")
}
func (*nativeLandingJourneyExecution) Checkpoint(context.Context, tracker.NativeCheckpoint) error {
	return nil
}
func (*nativeLandingJourneyExecution) Finish(context.Context, string) error { return nil }
func (*nativeLandingJourneyExecution) Recovery() tracker.NativeRecovery {
	return tracker.NativeRecovery{}
}
func (e *nativeLandingJourneyExecution) StartLanding(context.Context, int64, uint64) error {
	e.started++
	return nil
}
func (*nativeLandingJourneyExecution) ObserveLanding(context.Context, runpkg.NativeLanding) error {
	return nil
}
func (e *nativeLandingJourneyExecution) LandingTarget(context.Context) (runpkg.NativeLandingTarget, error) {
	return e.target, nil
}
func (e *nativeLandingJourneyExecution) RecordLanding(_ context.Context, landing runpkg.NativeLanding) error {
	e.recorded = append(e.recorded, landing)
	return nil
}
