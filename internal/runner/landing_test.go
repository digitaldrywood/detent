package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type landingBackend struct {
	workspace.Backend
	result             workspace.LandResult
	err                error
	received           workspace.LandOptions
	githubCalled       bool
	githubRequest      bool
	preparationRequest bool
}

func (b *landingBackend) Create(ctx context.Context, issue workspace.Issue) (workspace.Info, error) {
	info, err := b.Backend.Create(ctx, issue)
	if err == nil && b.preparationRequest {
		err = issue.Landing.GitHubClient.REST(ctx, "GET", "repos/example/repo/pulls/7", nil, nil)
	}
	return info, err
}

func (b *landingBackend) LandChange(_ context.Context, _ workspace.Info, _ workspace.Issue, opts workspace.LandOptions) (workspace.LandResult, error) {
	b.received = opts
	return b.result, b.err
}

func (b *landingBackend) LandChangeViaGitHub(ctx context.Context, _ workspace.Info, _ workspace.Issue, opts workspace.LandOptions) (workspace.LandResult, error) {
	b.githubCalled = true
	b.received = opts
	if b.githubRequest {
		return workspace.LandResult{}, opts.GitHubClient.REST(ctx, "GET", "/repos/example/repo/pulls", nil, nil)
	}
	return b.result, b.err
}

type landingStub struct {
	target    NativeLandingTarget
	targetErr error
	recordErr error
	recorded  []NativeLanding
}

func (s *landingStub) LandingTarget(context.Context) (NativeLandingTarget, error) {
	return s.target, s.targetErr
}

func (s *landingStub) RecordLanding(_ context.Context, landing NativeLanding) error {
	s.recorded = append(s.recorded, landing)
	return s.recordErr
}

func TestRunnerResolvesLandingBeforeWorkspace(t *testing.T) {
	originalClient := http.DefaultClient
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	quotaStatus := http.StatusForbidden
	http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "native-prepare.test" {
			t.Fatalf("unexpected credential request: %s", req.URL)
		}
		if req.URL.Path == "/graphql" {
			return workerGitHubPrincipalResponse(), nil
		}
		if req.URL.Path != "/repos/example/repo/pulls/7" {
			t.Fatalf("unexpected preparation request: %s", req.URL)
		}
		headers := make(http.Header)
		headers.Set("X-RateLimit-Remaining", "0")
		headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		if quotaStatus == http.StatusTooManyRequests {
			headers.Set("X-RateLimit-Remaining", "4990")
			headers.Set("Retry-After", "120")
		}
		return &http.Response{StatusCode: quotaStatus, Header: headers, Body: io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded"}`))}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = originalClient })
	for _, test := range []struct {
		name        string
		interrupted bool
		targetErr   error
		createErr   error
		github      bool
		created     bool
		quota       bool
		status      int
	}{
		{name: "landing reuses published source without replaying interrupted provider recovery", interrupted: true, github: true, created: true, createErr: errors.New("stop at creation")},
		{name: "approved external source", github: true, created: true, createErr: errors.New("stop at creation")},
		{name: "unreviewed source", github: true, targetErr: ErrLandingNotReviewed},
		{name: "current policy disables PR landing"},
		{name: "hydration refusal", github: true, created: true, createErr: &workspace.LandRefusal{Kind: workspace.LandRefusalHeadMoved, Reason: "external PR moved"}},
		{name: "hydration quota retains reviewed identity and metrics", github: true, created: true, quota: true},
		{name: "hydration 429 retains Retry-After and healthy primary reset", github: true, created: true, quota: true, status: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			quotaStatus = http.StatusForbidden
			if test.status != 0 {
				quotaStatus = test.status
			}
			backend := &fakeWorkspaceBackend{createErr: test.createErr}
			external := &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/example/repo/pull/7"}
			execution := &landingRunExecution{landingStub: landingStub{targetErr: test.targetErr, target: NativeLandingTarget{
				ChangeID: "change_1", VersionID: "version_1", HeadSHA: strings.Repeat("c", 40), Repository: "https://github.com/example/repo", GitHubPullRequest: test.github, External: external,
			}}}
			if test.interrupted {
				execution.recovery = tracker.NativeRecovery{Lease: tracker.NativeLease{PolicyID: "current"}, Attempts: []tracker.NativeAttempt{{Status: "interrupted", NativeRunData: tracker.NativeRunData{PolicyID: "previous"}, Checkpoint: &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", WorktreeState: "unpushed", HeadSHA: execution.target.HeadSHA}}}}
			}
			cfg := config.Config{}
			cfg.Gate.Run = "true"
			cfg.Worker.GitHubToken = test.name
			cfg.Tracker.Kind = config.TrackerGitHub
			cfg.Tracker.Endpoint = "https://native-prepare.test/graphql"
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: &landingBackend{Backend: backend, preparationRequest: test.quota}, AgentBackend: &fakeCodexClient{}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(t.Context(), RunRequest{Mode: RunModeMerge, Execution: execution, Issue: connector.Issue{ID: "native", Identifier: "native#98"}})
			if backend.created != test.created || len(execution.recorded) != 0 {
				t.Fatalf("workspace created = %v, receipts = %#v", backend.created, execution.recorded)
			}
			if test.created {
				options := backend.createIssue.Landing
				if options == nil || options.HeadSHA != execution.target.HeadSHA || options.Repository != execution.target.Repository || options.External == nil || *options.External != *external {
					t.Fatalf("workspace lost reviewed target: %#v", options)
				}
			}
			if test.quota {
				var status *github.StatusError
				if !errors.As(err, &status) || !errors.Is(err, github.ErrRateLimited) || !status.ResetAt.Equal(reset) || status.CredentialIdentity == "" || status.ObservedAt.IsZero() || result.NativeLanding == nil || result.NativeLanding.ChangeID != execution.target.ChangeID || result.NativeLanding.VersionID != execution.target.VersionID || result.NativeLanding.HeadSHA != execution.target.HeadSHA || result.NativeLanding.RefusalKind != "" || result.NativeLanding.Landed || result.GitHubRESTUsage == nil || !result.GitHubRESTUsage.RateLimited || result.GitHubRESTUsage.TotalRequests != 1 || execution.finish != "failed" || !execution.stopped {
					t.Fatalf("hydration quota result = %#v, execution %#v, error %v", result, execution, err)
				}
				if test.status == http.StatusTooManyRequests && (status.StatusCode != test.status || status.RetryAfter != 120*time.Second || status.RateLimit.Remaining != 4990) {
					t.Fatalf("secondary response evidence = %#v", status)
				}
				if !execution.started || len(execution.observations) != 1 || execution.observations[0].Phase != "completed" || execution.observations[0].REST == nil || len(execution.observations[0].REST.Windows) != 1 || len(execution.landingObservations) != 0 {
					t.Fatalf("quota runtime evidence=%#v", execution)
				}
				operation := execution.observations[0].REST.Windows[0]
				if operation.Status != quotaStatus || operation.ResetAt != status.ResetAt || operation.ObservedAt.IsZero() || operation.CredentialIdentity != status.CredentialIdentity || !operation.RateLimited || test.status == http.StatusTooManyRequests && operation.RetryAfterSeconds != 120 {
					t.Fatalf("quota operation authority=%#v", operation)
				}
				return
			}
			var refusal *workspace.LandRefusal
			if test.createErr != nil && !errors.As(test.createErr, &refusal) {
				if !errors.Is(err, test.createErr) {
					t.Fatalf("run error = %v", err)
				}
			} else if err != nil || result.Output != RunOutputNativeLandingRefused || result.NativeLanding == nil || result.NativeLanding.Landed {
				t.Fatalf("run result = %#v, error = %v", result, err)
			}
		})
	}
}

func TestRunnerLandingPreservesCodeAndOperatorOwners(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%v", external), func(t *testing.T) {
			source := initRunnerSourceRepo(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			runRunnerGit(t, source, "init", "--bare", "-b", "main", remote)
			repository := "https://github.com/example/repo"
			runRunnerGit(t, source, "config", "url.file://"+remote+".insteadOf", repository+".git")
			runRunnerGit(t, source, "remote", "add", "origin", repository+".git")
			runRunnerGit(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{ID: "native", Identifier: "native#170"}
			code, err := backend.Create(t.Context(), workspaceIssue("project", issue))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(code.Path, "README.md"), []byte("preserved staged source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRunnerGit(t, code.Path, "add", "README.md")
			if err := os.WriteFile(filepath.Join(code.Path, "untracked"), []byte("preserved working source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			recovery := filepath.Join(t.TempDir(), "recovery")
			sourceBranch := "operator/recovery"
			runRunnerGit(t, source, "worktree", "add", "-b", sourceBranch, recovery, "main")
			if err := os.WriteFile(filepath.Join(recovery, "feature.txt"), []byte("genuine operator source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRunnerGit(t, recovery, "add", "feature.txt")
			runRunnerGit(t, recovery, "commit", "-m", "finish preserved source")
			head := strings.TrimSpace(runRunnerGit(t, recovery, "rev-parse", "HEAD"))
			target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Repository: repository, Method: "merge", GitHubPullRequest: true}
			if external {
				target.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: repository + "/pull/7"}
				runRunnerGit(t, source, "push", "origin", head+":refs/heads/"+sourceBranch, head+":refs/pull/7/head")
			}
			files := make(map[string][]byte)
			for _, path := range []string{source, code.Path, recovery} {
				for _, name := range []string{"HEAD", "index"} {
					file := strings.TrimSpace(runRunnerGit(t, path, "rev-parse", "--path-format=absolute", "--git-path", name))
					data, err := os.ReadFile(file)
					if err != nil {
						t.Fatal(err)
					}
					files[file] = data
				}
			}
			for _, file := range []string{filepath.Join(source, "README.md"), filepath.Join(code.Path, "README.md"), filepath.Join(code.Path, "untracked"), filepath.Join(recovery, "feature.txt")} {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				files[file] = data
			}
			refs := make(map[string]string)
			for _, ref := range []string{"main", code.Branch, sourceBranch} {
				refs[ref] = runRunnerGit(t, source, "rev-parse", ref)
			}
			publishedBranch := sourceBranch
			var operations []string
			originalClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
				response := ""
				if req.URL.Path == "/graphql" {
					return workerGitHubPrincipalResponse(), nil
				}
				operations = append(operations, req.Method)
				if req.Method == http.MethodGet && req.URL.Path == "/repos/example/repo/pulls" {
					publishedBranch = strings.TrimPrefix(req.URL.Query().Get("head"), "example:")
				}
				pull := fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/repo"}},"base":{"ref":"main","repo":{"full_name":"example/repo"}}}`, head, publishedBranch)
				switch req.Method {
				case http.MethodGet:
					response = pull
					if req.URL.Path == "/repos/example/repo/pulls" {
						response = "[" + pull + "]"
					}
				case http.MethodPut:
					var body map[string]string
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					published := strings.TrimSpace(runRunnerGit(t, remote, "rev-parse", "refs/heads/"+publishedBranch))
					if body["sha"] != head || body["merge_method"] != target.Method || published != head {
						t.Fatalf("merge lost reviewed identity: %v, published %s", body, published)
					}
					runRunnerGit(t, remote, "update-ref", "refs/heads/main", head)
					response = fmt.Sprintf(`{"merged":true,"sha":%q}`, head)
				default:
					t.Fatalf("unexpected forge operation %s %s", req.Method, req.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = originalClient })
			cfg := config.Config{}
			cfg.Gate.Run = "true"
			cfg.Worker.GitHubToken = t.Name()
			cfg.Tracker.Kind = config.TrackerGitHub
			cfg.Tracker.Endpoint = "https://native-landing.test/graphql"
			provider := &fakeCodexClient{}
			runner, err := NewRunner(Dependencies{ProjectID: "project", Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: provider})
			if err != nil {
				t.Fatal(err)
			}
			execution := &landingRunExecution{landingStub: landingStub{target: target}}
			result, err := runner.Run(t.Context(), RunRequest{Mode: RunModeMerge, Execution: execution, Issue: issue})
			if err != nil || result.Output != RunOutputNativeLanded || result.NativeLanding == nil || result.NativeLanding.HeadSHA != head || result.NativeLanding.MergeSHA != head || len(execution.recorded) != 1 || provider.calls != 0 {
				t.Fatalf("landing result = %#v, execution %#v, provider calls %d, error %v", result, execution, provider.calls, err)
			}
			landingPath := filepath.Join(filepath.Dir(code.Path), ".detent", "landing", head, filepath.Base(code.Path))
			if _, err := os.Stat(landingPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prepared landing worktree remains: %v", err)
			}
			if listed := runRunnerGit(t, source, "worktree", "list", "--porcelain"); strings.Contains(listed, "worktree "+landingPath) {
				t.Fatalf("prepared landing registration remains:\n%s", listed)
			}
			if external && strings.Join(operations, ",") != "GET,GET,PUT" || !external && publishedBranch == code.Branch {
				t.Fatalf("landing used wrong publication owner: %s, %v", publishedBranch, operations)
			}
			if result.GitHubScope == nil || len(execution.observations) == 0 || execution.observations[0].GitHub == nil || result.GitHubScope.WallElapsedNS == nil || result.GitHubScope.SubStepElapsedNS != nil || result.GitHubScope.ObservedAt.Before(result.GitHubScope.StartedAt) {
				t.Fatalf("landing lost recorded GitHub scope: %#v", result.GitHubScope)
			}
			var restHTTP, restToken int64
			for _, timing := range result.GitHubScope.Timings {
				if timing.TimedCount != timing.AttemptCount || timing.ElapsedSumNS == nil || timing.ElapsedMaxNS == nil || timing.FirstObservedAt.IsZero() || timing.LastObservedAt.Before(timing.FirstObservedAt) {
					t.Fatalf("landing lost actual timing: %#v", timing)
				}
				if timing.Protocol == "rest" {
					switch timing.Boundary {
					case "http_transport":
						restHTTP += timing.AttemptCount
					case "token_resolution_inclusive":
						restToken += timing.AttemptCount
					}
				}
			}
			if restHTTP != int64(len(operations)) || restToken != restHTTP {
				t.Fatalf("timing changed upstream request population: %d HTTP, %d token, operations %v", restHTTP, restToken, operations)
			}
			for file, expected := range files {
				actual, err := os.ReadFile(file)
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("landing changed owner file %s: %v", file, err)
				}
			}
			for ref, expected := range refs {
				if actual := runRunnerGit(t, source, "rev-parse", ref); actual != expected {
					t.Fatalf("landing changed source ref %s: %s", ref, actual)
				}
			}
		})
	}
}

func TestLandNativeChange(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()
	head := strings.Repeat("c", 40)
	merge := strings.Repeat("e", 40)
	gateResult := gate.CommandResult{Command: "make check-land", HeadSHA: merge, TreeSHA: strings.Repeat("a", 40), DurationNS: 123456}
	failedGate := gateResult
	failedGate.ExitCode, failedGate.Output = 1, "lint-error-sentinel"
	target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "merge", Title: "Add a sign-in link", Number: 2}
	githubTarget := target
	githubTarget.Repository, githubTarget.GitHubPullRequest = "https://github.com/digitaldrywood/detent", true
	githubTarget.SourceIssues = []tracker.ExternalReference{
		tracker.GitHubIssueSourceReference("I_original", "https://github.com/digitaldrywood/detent/issues/3410"),
		tracker.GitHubIssueSourceReference("https://github.com/acme/orders/issues/12", "https://github.com/acme/orders/issues/12"),
		tracker.GitHubIssueSourceReference("I_original", "https://github.com/digitaldrywood/detent/issues/3410"),
	}
	wantMessage := "Add a sign-in link\n\nChange Request change_1, round 2, head " + head + "."
	for _, test := range []struct {
		name           string
		stub           landingStub
		backend        landingBackend
		wantOutput     string
		wantErr        string
		wantRecorded   int
		wantRefusal    string
		wantGitHub     bool
		quota          bool
		prepared       *NativeLandingTarget
		infrastructure bool
		wantMessage    string
		gateFailure    bool
	}{
		{name: "lands and records", stub: landingStub{target: target}, backend: landingBackend{result: workspace.LandResult{Gate: gateResult, MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantMessage: wantMessage},
		{name: "GitHub source issues retain their original numbers without duplicate closing lines", stub: landingStub{target: githubTarget}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantGitHub: true, wantMessage: wantMessage + "\n\nCloses acme/orders#12\nCloses digitaldrywood/detent#3410"},
		{name: "opted-in project uses GitHub PR landing", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantGitHub: true, wantMessage: "Land " + head + "\n\nChange Request change_1, round 0, head " + head + "."},
		{name: "quota retains reviewed identity and actual metrics", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{githubRequest: true}, wantErr: "github rate limited", quota: true},
		{name: "gate command failure retains output and reviewed identity", stub: landingStub{target: target}, backend: landingBackend{result: workspace.LandResult{Gate: failedGate}, err: &workspace.ValidationError{Output: "lint-error-sentinel", Err: errors.New("exit status 1")}}, wantOutput: RunOutputNativeLandingRefused, gateFailure: true},
		{name: "a refusal is reported, not recorded", stub: landingStub{target: target}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalProtected, Reason: "the base branch main refused the push"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalProtected},
		{name: "a GitHub conflict retains reviewed identity without a landing receipt", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, Reason: "Pull Request is not mergeable (HTTP 405)"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalConflict, wantGitHub: true},
		{name: "an atomic GitHub head refusal retains reviewed identity without a receipt", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalHeadMoved, Reason: "Head branch was modified (HTTP 409)"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalHeadMoved, wantGitHub: true},
		{name: "genuine GitHub outage retains identity for infrastructure retry", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{err: forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: "github.update_pull_request repos/example/repo/pulls/7/merge"}, forgeavailability.ClassServer, &github.StatusError{Err: github.ErrUnexpectedStatus, StatusCode: 503, Body: `{"message":"Service Unavailable"}`})},
			wantErr: "forge_unavailable", wantGitHub: true, infrastructure: true},
		{name: "genuine refresh outage takes precedence over projection continuation", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{err: errors.Join(&workspace.LandRefusal{Kind: workspace.LandRefusalBaseMoved, Reason: "GitHub returned mergeability HTTP 405"}, forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: "git fetch"}, forgeavailability.ClassTimeout, errors.New("operation timed out")))},
			wantErr: "operation timed out", wantGitHub: true, infrastructure: true},
		{name: "quota evidence takes precedence over repository refusal", stub: landingStub{target: target}, backend: landingBackend{err: errors.Join(fmt.Errorf("%w: remaining=0 reserve=100", ErrWorkerGitHubRESTReserved), &workspace.LandRefusal{Kind: workspace.LandRefusalProtected, Reason: "HTTP 403"})},
			wantErr: "remaining=0 reserve=100"},
		{name: "typed quota evidence takes precedence over repository refusal", stub: landingStub{target: target}, backend: landingBackend{err: errors.Join(&github.StatusError{Err: github.ErrRateLimited, StatusCode: http.StatusForbidden, RateLimitKind: "primary_exhausted", CredentialIdentity: "landing-token", ObservedAt: time.Now()}, &workspace.LandRefusal{Kind: workspace.LandRefusalProtected, Reason: "HTTP 403"})},
			wantErr: "github rate limited"},
		{name: "an unreviewed change is a refusal", stub: landingStub{target: target, targetErr: errors.New("hub says: " + ErrLandingNotReviewed.Error())},
			wantErr: "resolve landing target"},
		{name: "an unreviewed change from the hub is a refusal", stub: landingStub{target: target, targetErr: ErrLandingNotReviewed},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalNothing},
		{name: "a replaced version cannot land the prepared workspace", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: "new_version", HeadSHA: head}}, prepared: &target,
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalHeadMoved},
		{name: "a git failure fails the run", stub: landingStub{target: target}, backend: landingBackend{err: errors.New("git fetch origin: network down")},
			wantErr: "network down"},
		{name: "an unrecorded landing fails the run after retrying the report", stub: landingStub{target: target, recordErr: errors.New("hub unavailable")}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}},
			wantErr: "record landing", wantRecorded: landingReportTries},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := &Runner{}
			stub, backend := test.stub, test.backend
			reset := time.Now().Add(time.Hour).Truncate(time.Second)
			policy := workerGitHubPolicy{Token: test.name, HTTPClient: workerGitHubHTTPClientFunc(func(*http.Request) (*http.Response, error) {
				headers := make(http.Header)
				headers.Set("X-RateLimit-Remaining", "0")
				headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
				return &http.Response{StatusCode: 403, Header: headers, Body: io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded"}`))}, nil
			})}
			result, err := r.landNativeChange(t.Context(), RunRequest{}, &stub, &backend, workspace.Info{Path: t.TempDir(), Branch: "detent/land"}, workspace.Issue{Identifier: "DD-1"}, policy, test.prepared)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				if test.quota {
					var status *github.StatusError
					if !errors.As(err, &status) || !status.ResetAt.Equal(reset) || result.NativeLanding == nil || result.NativeLanding.ChangeID != target.ChangeID || result.NativeLanding.VersionID != target.VersionID || result.NativeLanding.HeadSHA != head || result.NativeLanding.RefusalKind != "" || result.GitHubRESTUsage == nil || !result.GitHubRESTUsage.RateLimited || result.GitHubRESTUsage.TotalRequests != 1 {
						t.Fatalf("quota result = %#v error %v", result, err)
					}
				}
				if len(stub.recorded) != test.wantRecorded {
					t.Fatalf("recorded = %#v", stub.recorded)
				}
				if IsCapacityError(backend.err) && !IsCapacityError(err) || errors.Is(backend.err, github.ErrRateLimited) && !errors.Is(err, github.ErrRateLimited) {
					t.Fatalf("quota lost capacity ownership: %v, %#v", err, result)
				}
				if IsCapacityError(backend.err) || errors.Is(backend.err, github.ErrRateLimited) || test.infrastructure {
					if result.NativeLanding == nil || result.NativeLanding.ChangeID != target.ChangeID || result.NativeLanding.VersionID != target.VersionID || result.NativeLanding.HeadSHA != head || result.NativeLanding.Landed || result.NativeLanding.RefusalKind != "" || result.Output != "" || result.FinalState != "" {
						t.Fatalf("capacity wait lost reviewed identity or became a completed refusal: %#v", result)
					}
					if test.infrastructure && !errors.Is(err, backend.err) {
						t.Fatalf("original infrastructure refusal lost: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.NativeLanding != nil && (result.NativeLanding.GateFailed != test.gateFailure || test.gateFailure && !strings.Contains(result.NativeLanding.Refusal, "lint-error-sentinel")) {
				t.Fatalf("gate failure lost evidence: %#v", result.NativeLanding)
			}
			if result.FinalState != FinalStateCompleted || result.Output != test.wantOutput || result.NativeLanding == nil {
				t.Fatalf("result = %#v", result)
			}
			if backend.result.Gate.Command != "" {
				if result.NativeLanding.Gate == nil || *result.NativeLanding.Gate != backend.result.Gate {
					t.Fatalf("landing lost gate command evidence: %#v", result.NativeLanding)
				}
				for _, recorded := range stub.recorded {
					if recorded.Gate == nil || *recorded.Gate != backend.result.Gate {
						t.Fatalf("published landing lost gate: %#v", recorded)
					}
				}
			}
			if result.NativeLanding.Rebased != backend.result.Rebased {
				t.Fatalf("landing lost retry evidence: %#v", result.NativeLanding)
			}
			for _, recorded := range stub.recorded {
				if recorded.Rebased != backend.result.Rebased {
					t.Fatalf("recorded landing lost retry evidence: %#v", recorded)
				}
			}
			if result.ForgeWriteCompleted != (test.wantGitHub && test.wantRecorded == 1) {
				t.Fatalf("forge landing completion evidence = %t", result.ForgeWriteCompleted)
			}
			if result.NativeLanding.RefusalKind != test.wantRefusal || result.NativeLanding.Landed != (test.wantRefusal == "" && !test.gateFailure) {
				t.Fatalf("landing = %#v", result.NativeLanding)
			}
			if result.NativeLanding.ChangeID != target.ChangeID || result.NativeLanding.VersionID != target.VersionID || result.NativeLanding.HeadSHA != head || test.wantRefusal != "" && result.NativeLanding.MergeSHA != "" {
				t.Fatalf("landing lost exact reviewed identity or invented a merge: %#v", result.NativeLanding)
			}
			if len(stub.recorded) != test.wantRecorded {
				t.Fatalf("recorded = %#v, want %d", stub.recorded, test.wantRecorded)
			}
			if backend.githubCalled != test.wantGitHub {
				t.Fatalf("GitHub landing called = %t, want %t", backend.githubCalled, test.wantGitHub)
			}
			if test.wantMessage != "" && backend.received.Message != test.wantMessage {
				t.Fatalf("PR message = %q, want %q", backend.received.Message, test.wantMessage)
			}
			if test.wantRecorded == 1 {
				if backend.received.ValidationCommand != "make check" || backend.received.HeadSHA != head || backend.received.Method != "merge" || !backend.received.PushAttemptBranch || (!test.wantGitHub && (!strings.Contains(backend.received.Message, "Add a sign-in link") || !strings.Contains(backend.received.Message, "round 2"))) {
					t.Fatalf("land options = %#v", backend.received)
				}
				if stub.recorded[0].MergeSHA != merge || stub.recorded[0].BaseRef != "main" || stub.recorded[0].ChangeID != "change_1" {
					t.Fatalf("recorded landing = %#v", stub.recorded[0])
				}
			}
		})
	}
}

func TestLandNativeChangeKeepsAnUnreportedLanding(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name          string
		cleanupStatus int
		reportFailure bool
	}{
		{name: "keeps an unreported landing", reportFailure: true},
		{name: "reports a merged landing despite cleanup 502", cleanupStatus: http.StatusBadGateway},
		{name: "reports the kept landing after cleanup 502 and a Hub outage", cleanupStatus: http.StatusBadGateway, reportFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initRunnerSourceRepo(t)
			remote := filepath.Join(t.TempDir(), "origin.git")
			runRunnerGit(t, source, "init", "--bare", "-b", "main", remote)
			repository := "https://github.com/example/repo"
			runRunnerGit(t, source, "config", "url.file://"+remote+".insteadOf", repository+".git")
			runRunnerGit(t, source, "remote", "add", "origin", repository+".git")
			runRunnerGit(t, source, "push", "-u", "origin", "main")
			base := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("reviewed source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRunnerGit(t, source, "add", "feature.txt")
			runRunnerGit(t, source, "commit", "-m", "reviewed source")
			head := strings.TrimSpace(runRunnerGit(t, source, "rev-parse", "HEAD"))
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			var info workspace.Info
			var recordPath, merge string
			var requests int
			readKept := func() workspace.LandResult {
				t.Helper()
				raw, err := os.ReadFile(recordPath)
				if err != nil {
					t.Fatalf("merged landing was not kept: %v", err)
				}
				var kept struct {
					HeadSHA string               `json:"head_sha"`
					Result  workspace.LandResult `json:"result"`
				}
				if err := json.Unmarshal(raw, &kept); err != nil {
					t.Fatal(err)
				}
				if kept.HeadSHA != head || kept.Result.MergeSHA != merge || kept.Result.BaseBefore != base || kept.Result.Gate.Command != "git status --porcelain" || kept.Result.Gate.TreeSHA == "" {
					t.Fatalf("kept landing lost reviewed identity or evidence: %#v", kept)
				}
				return kept.Result
			}
			client, err := github.NewClient(github.ClientConfig{
				TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true,
				HTTPClient: workerGitHubHTTPClientFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					status, body := http.StatusOK, `[]`
					switch {
					case req.Method == http.MethodGet && req.URL.Path == "/repos/example/repo/pulls":
						if req.URL.Query().Get("state") == "open" {
							readKept()
							if test.cleanupStatus != 0 {
								status, body = test.cleanupStatus, `{"message":"cleanup unavailable"}`
							}
						}
					case req.Method == http.MethodPost && req.URL.Path == "/repos/example/repo/pulls":
						body = `{"number":7,"state":"open"}`
					case req.Method == http.MethodPut && req.URL.Path == "/repos/example/repo/pulls/7/merge":
						var payload map[string]string
						if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						if payload["sha"] != head || payload["merge_method"] != "squash" {
							t.Fatalf("merge request lost reviewed identity: %#v", payload)
						}
						merge = strings.TrimSpace(runRunnerGit(t, info.Path, "commit-tree", head+"^{tree}", "-p", base, "-m", "squash landing"))
						runRunnerGit(t, info.Path, "push", "origin", merge+":refs/heads/main")
						body = fmt.Sprintf(`{"merged":true,"sha":%q}`, merge)
					default:
						t.Fatalf("unexpected landing request: %s %s", req.Method, req.URL)
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			issue := workspace.Issue{Identifier: "DD-1", Landing: &workspace.LandOptions{HeadSHA: head, Repository: repository, GitHubClient: client}}
			info, err = backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			recordPath = strings.TrimSpace(runRunnerGit(t, info.Path, "rev-parse", "--git-path", "detent-landing.json"))
			if !filepath.IsAbs(recordPath) {
				recordPath = filepath.Join(info.Path, recordPath)
			}
			target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "squash", Repository: repository, GitHubPullRequest: true}
			stub := landingStub{target: target}
			if test.reportFailure {
				stub.recordErr = errors.New("hub unavailable")
			}
			var logs bytes.Buffer
			r := &Runner{workflow: config.Workflow{Config: config.Config{Gate: gate.Config{Run: "git status --porcelain"}}}, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			result, err := r.landNativeChange(t.Context(), RunRequest{}, &stub, backend, info, issue, workerGitHubPolicy{}, nil)
			if test.cleanupStatus != 0 && (!strings.Contains(logs.String(), "worker_native_landing_warning") || !strings.Contains(logs.String(), "cleanup unavailable") || !strings.Contains(logs.String(), merge)) {
				t.Fatalf("post-merge warning lost failure evidence: %s", logs.String())
			}
			if test.reportFailure {
				if err == nil || !strings.Contains(err.Error(), "record landing") || len(stub.recorded) != landingReportTries {
					t.Fatalf("Hub failure = %v, report attempts = %d", err, len(stub.recorded))
				}
				readKept()
				stub.recordErr = nil
				previousRequests := requests
				result, err = r.landNativeChange(t.Context(), RunRequest{}, &stub, backend, info, issue, workerGitHubPolicy{}, nil)
				if requests != previousRequests {
					t.Fatalf("second landing called the forge: requests=%d want=%d", requests, previousRequests)
				}
			}
			if err != nil || result.Output != RunOutputNativeLanded || result.FinalState != FinalStateCompleted || result.NativeLanding == nil || !result.NativeLanding.Landed || result.NativeLanding.RefusalKind != "" || result.NativeLanding.MergeSHA != merge || !result.ForgeWriteCompleted {
				t.Fatalf("merged change was not reported landed: %#v, error=%v", result, err)
			}
			for _, recorded := range stub.recorded {
				if !recorded.Landed || recorded.MergeSHA != merge || recorded.HeadSHA != head || recorded.RefusalKind != "" {
					t.Fatalf("landing report lost the merge: %#v", recorded)
				}
			}
			if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reported landing was kept: %v", err)
			}
		})
	}
}

func gitCommand(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type landingRunExecution struct {
	testExecution
	landingStub
	stopped             bool
	started             bool
	observations        []tracker.NativeRuntimeObservation
	landingObservations []NativeLanding
}

func (e *landingRunExecution) StartLanding(context.Context, int64, uint64) error {
	e.started = true
	return nil
}

func (e *landingRunExecution) ObserveRuntime(_ context.Context, observation tracker.NativeRuntimeObservation) error {
	e.observations = append(e.observations, observation)
	return nil
}

func (e *landingRunExecution) ObserveLanding(_ context.Context, landing NativeLanding) error {
	e.landingObservations = append(e.landingObservations, landing)
	return nil
}

func (e *landingRunExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() { e.stopped = true }, nil
}

func TestNativeLandingQuotaFinishesRun(t *testing.T) {
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "native-finish.test" || req.URL.Path != "/graphql" {
			t.Errorf("unexpected credential request: %s", req.URL)
			return nil, errors.New("unexpected credential request")
		}
		return workerGitHubPrincipalResponse(), nil
	})}
	t.Cleanup(func() { http.DefaultClient = originalClient })
	head := strings.Repeat("c", 40)
	now := time.Now().UTC()
	execution := &landingRunExecution{landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "squash", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}}
	quota := &github.StatusError{StatusCode: 403, Err: github.ErrRateLimited, CredentialIdentity: "runner", RateLimitKind: "primary_exhausted", ObservedAt: now, ResetAt: now.Add(time.Hour)}
	backend := &landingBackend{Backend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, err: quota}
	cfg := config.Config{}
	cfg.Gate.Run = "true"
	cfg.Worker.GitHubToken = "runner-token"
	cfg.Tracker.Kind = config.TrackerGitHub
	cfg.Tracker.Endpoint = "https://native-finish.test/graphql"
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: &fakeCodexClient{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "native", State: "Merging"}, Mode: RunModeMerge, Execution: execution})
	if result.RuntimeIdentity.Role != RoleMerge || result.RuntimeIdentity.BackendKind != "git" || result.RuntimeIdentity.ResolvedModel.Value != "none" {
		t.Fatalf("native landing result identity = %#v", result.RuntimeIdentity)
	}
	if !errors.Is(err, github.ErrRateLimited) || execution.finish != "failed" || !execution.stopped || len(execution.recorded) != 0 || result.NativeLanding == nil || result.NativeLanding.HeadSHA != head || result.NativeLanding.Landed {
		t.Fatalf("quota run = %#v, execution %#v, error %v", result, execution, err)
	}
}

type validationLandingBackend struct {
	landingBackend
}

func (b *validationLandingBackend) RecoveryState(ctx context.Context, info workspace.Info, issue workspace.Issue) (workspace.RecoveryState, error) {
	return b.Backend.(workspace.RecoveryStateProvider).RecoveryState(ctx, info, issue)
}

type validationLandingExecution struct {
	landingRunExecution
	validationCalls int
}

func (e *validationLandingExecution) PublishValidationEvidence(context.Context, []ValidationEvidence) error {
	e.validationCalls++
	return errors.New("historical evidence must not be republished")
}

func TestNativeLandingDoesNotRepublishValidationEvidence(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	validation := filepath.Join(directory, ".detent", "validation")
	if err := os.MkdirAll(validation, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 11 {
		if err := os.WriteFile(filepath.Join(validation, strconv.Itoa(i)+".png"), []byte("historical screenshot"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	head, merge := strings.Repeat("c", 40), strings.Repeat("d", 40)
	execution := &validationLandingExecution{landingRunExecution: landingRunExecution{landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "squash"}}}}
	workspaceBackend := &fakeWorkspaceBackend{info: workspace.Info{Path: directory}, recoveryStates: []workspace.RecoveryState{{HeadSHA: head}}}
	backend := &validationLandingBackend{landingBackend{Backend: workspaceBackend, result: workspace.LandResult{MergeSHA: merge, Method: "squash"}}}
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: config.Config{}}, Workspace: backend, AgentBackend: &fakeCodexClient{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "native", State: "Merging"}, Mode: RunModeMerge, Execution: execution})
	if err != nil || result.NativeLanding == nil || !result.NativeLanding.Landed || result.NativeLanding.MergeSHA != merge || result.NativeLanding.HeadSHA != head || execution.finish != "succeeded" {
		t.Fatalf("genuine landing was not completed: result=%#v finish=%q error=%v", result, execution.finish, err)
	}
	if len(execution.recorded) != 1 || execution.validationCalls != 0 || execution.checkpoint == nil || !workspaceBackend.afterRun {
		t.Fatalf("landing epilogue: receipts=%d screenshot calls=%d checkpoint=%#v cleanup=%t", len(execution.recorded), execution.validationCalls, execution.checkpoint, workspaceBackend.afterRun)
	}
}
