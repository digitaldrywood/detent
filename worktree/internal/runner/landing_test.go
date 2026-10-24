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
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/gobudget"
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
	headReceipt        *gate.CommandResult
}

func (b *landingBackend) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	if b.headReceipt == nil {
		return "", nil
	}
	return b.headReceipt.HeadSHA, nil
}

func (b *landingBackend) RunReviewCommand(_ context.Context, _ workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	if issue.PullRequestHeadSHA != b.headReceipt.HeadSHA || command != b.headReceipt.Command {
		return gate.CommandResult{}, errors.New("validated a different head or command")
	}
	return *b.headReceipt, nil
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
	for _, mode := range []string{"native", "pull-request", "external"} {
		t.Run(mode, func(t *testing.T) {
			external := mode == "external"
			native := mode == "native"
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
			target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Repository: repository, Method: "merge", GitHubPullRequest: !native}
			if native {
				target.Validation = &gate.CommandResult{Command: "true", HeadSHA: head, TreeSHA: strings.TrimSpace(runRunnerGit(t, recovery, "rev-parse", "HEAD^{tree}")), DurationNS: int64(time.Second)}
			}
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
			reviewThreadReads := 0
			originalClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
				response := ""
				if native {
					operations = append(operations, req.Method)
					return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded"}`))}, nil
				}
				if req.URL.Path == "/graphql" {
					query, err := io.ReadAll(req.Body)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(query, []byte("NativeLandingReviewThreads")) {
						reviewThreadReads++
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}}`))}, nil
					}
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
			if !native {
				cfg.Gate.Run = `test "$DETENT_GO_BUDGET_SLOTS" = 8 && test -n "$DETENT_GO_BUDGET_DIR"`
			}
			cfg.Worker.GitHubToken = t.Name()
			cfg.Tracker.Kind = config.TrackerGitHub
			cfg.Tracker.Endpoint = "https://native-landing.test/graphql"
			provider := &fakeCodexClient{}
			runner, err := NewRunner(Dependencies{ProjectID: "project", Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: provider, GoBudget: gobudget.New(8, filepath.Join(t.TempDir(), "budget"), "/opt/example/detent")})
			if err != nil {
				t.Fatal(err)
			}
			execution := &landingRunExecution{landingStub: landingStub{target: target}}
			result, err := runner.Run(t.Context(), RunRequest{Mode: RunModeMerge, Execution: execution, Issue: issue})
			if err != nil || result.Output != RunOutputNativeLanded || result.NativeLanding == nil || result.NativeLanding.HeadSHA != head || result.NativeLanding.MergeSHA == "" || (!native && result.NativeLanding.MergeSHA != head) || len(execution.recorded) != 1 || provider.calls != 0 {
				t.Fatalf("landing result = %#v, execution %#v, provider calls %d, error %v", result, execution, provider.calls, err)
			}
			landingPath := filepath.Join(filepath.Dir(code.Path), ".detent", "landing", head, filepath.Base(code.Path))
			if _, err := os.Stat(landingPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prepared landing worktree remains: %v", err)
			}
			if listed := runRunnerGit(t, source, "worktree", "list", "--porcelain"); strings.Contains(listed, "worktree "+filepath.ToSlash(landingPath)) {
				t.Fatalf("prepared landing registration remains:\n%s", listed)
			}
			if external && strings.Join(operations, ",") != "GET,GET,PUT" || !external && publishedBranch == code.Branch {
				t.Fatalf("landing used wrong publication owner: %s, %v", publishedBranch, operations)
			}
			if (reviewThreadReads > 0) != external {
				t.Fatalf("unresolved review conversation reads = %d, want reads only for an external PR", reviewThreadReads)
			}
			if native {
				if len(operations) != 0 || result.GitHubScope != nil || result.NativeLanding.Path != "clean_push" || result.NativeLanding.Gate != nil || strings.TrimSpace(runRunnerGit(t, remote, "rev-parse", "refs/heads/main")) != result.NativeLanding.MergeSHA {
					t.Fatalf("native landing contacted GitHub or did not push cleanly: operations %v, result %#v", operations, result)
				}
			} else {
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
	headGate := gate.CommandResult{Command: "make check", HeadSHA: head, TreeSHA: strings.Repeat("a", 40), DurationNS: 123456}
	failedHeadGate := headGate
	failedHeadGate.ExitCode, failedHeadGate.Output = 1, "lint-error-sentinel"
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
		name                string
		stub                landingStub
		backend             landingBackend
		wantOutput          string
		wantErr             string
		wantRecorded        int
		wantRefusal         string
		wantGitHub          bool
		quota               bool
		prepared            *NativeLandingTarget
		infrastructure      bool
		wantMessage         string
		gateFailure         bool
		wantRefusalContains string
		wantHeadStage       string
	}{
		{name: "lands and records", stub: landingStub{target: target}, backend: landingBackend{result: workspace.LandResult{Gate: gateResult, MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantMessage: wantMessage},
		{name: "GitHub source issues retain their original numbers without duplicate closing lines", stub: landingStub{target: githubTarget}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantGitHub: true, wantMessage: wantMessage + "\n\nCloses acme/orders#12\nCloses digitaldrywood/detent#3410"},
		{name: "opted-in project uses GitHub PR landing", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantGitHub: true, wantMessage: "Land " + head + "\n\nChange Request change_1, round 0, head " + head + "."},
		{name: "release PR retains its recorded base", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example-org/example-project", GitHubPullRequest: true, BaseRef: "release/v1.2", BaseSHA: strings.Repeat("e", 40), SourceMergeSHA: strings.Repeat("a", 40)}}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge", Rebased: true}}, wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantGitHub: true},
		{name: "quota retains reviewed identity and actual metrics", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{githubRequest: true}, wantErr: "github rate limited", quota: true},
		{name: "an unreceipted reviewed head is validated before it lands", stub: landingStub{target: target}, backend: landingBackend{headReceipt: &headGate, result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge", Path: "clean_push"}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1},
		{name: "a failing unreceipted reviewed head returns its output without landing", stub: landingStub{target: target}, backend: landingBackend{headReceipt: &failedHeadGate}, wantOutput: RunOutputNativeLandingRefused, gateFailure: true, wantRefusalContains: "lint-error-sentinel", wantHeadStage: gate.StageLanding},
		{name: "gate command failure retains output and reviewed identity", stub: landingStub{target: target}, backend: landingBackend{result: workspace.LandResult{Gate: failedGate}, err: &workspace.ValidationError{Stage: workspace.ValidationStageLanding, Result: failedGate, Output: "lint-error-sentinel", Err: errors.New("exit status 1")}}, wantOutput: RunOutputNativeLandingRefused, gateFailure: true, wantRefusalContains: "lint-error-sentinel"},
		{name: "CI waiting retains reviewed identity without recording a merge", stub: landingStub{target: githubTarget}, backend: landingBackend{result: workspace.LandResult{CI: &tracker.NativeLandingCIReceipt{HeadSHA: head, PullRequest: 7, State: "pending", TriggerLabel: "run-full-ci", Triggered: true}}}, wantOutput: RunOutputNativeLandingRefused, wantGitHub: true},
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
			r := &Runner{workflow: config.Workflow{Config: config.Config{Gate: gate.Config{RequiredStatusChecks: []string{"Full CI"}, CITriggerLabel: "run-full-ci"}}}}
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
			if result.NativeLanding != nil && result.NativeLanding.GateFailed != test.gateFailure {
				t.Fatalf("gate failure lost evidence: %#v", result.NativeLanding)
			}
			if test.wantRefusalContains != "" && (result.NativeLanding == nil || !strings.Contains(result.NativeLanding.Refusal, test.wantRefusalContains)) {
				t.Fatalf("landing refusal = %+v, want gate output %q", result.NativeLanding, test.wantRefusalContains)
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
			if receipt := backend.headReceipt; receipt != nil {
				wantReceipt := *receipt
				wantReceipt.Stage = test.wantHeadStage
				if result.NativeLanding.Gate == nil || !reflect.DeepEqual(*result.NativeLanding.Gate, wantReceipt) || receipt.ExitCode == 0 && (backend.received.Validation == nil || !reflect.DeepEqual(*backend.received.Validation, *receipt)) || receipt.ExitCode != 0 && backend.received.HeadSHA != "" {
					t.Fatalf("unreceipted head validation = landing %#v, options %#v", result.NativeLanding, backend.received)
				}
			}
			if result.NativeLanding.Rebased != backend.result.Rebased {
				t.Fatalf("landing lost retry evidence: %#v", result.NativeLanding)
			}
			if !reflect.DeepEqual(result.NativeLanding.CI, backend.result.CI) || backend.githubCalled && (!slices.Equal(backend.received.RequiredStatusChecks, []string{"Full CI"}) || backend.received.CITriggerLabel != "run-full-ci" || backend.received.CITriggerLabelStagger != 15*time.Second) {
				t.Fatalf("native CI policy or receipt was lost: options=%+v landing=%+v", backend.received, result.NativeLanding)
			}
			for _, recorded := range stub.recorded {
				if recorded.Rebased != backend.result.Rebased {
					t.Fatalf("recorded landing lost retry evidence: %#v", recorded)
				}
			}
			if backend.received.HeadSHA != "" && stub.target.SourceMergeSHA != "" && !strings.Contains(backend.received.Message, "(cherry picked from commit "+stub.target.SourceMergeSHA+")") {
				t.Fatalf("release squash lost source provenance: %q", backend.received.Message)
			}
			expectedBase := ""
			if stub.target.SourceMergeSHA != "" {
				expectedBase = stub.target.BaseSHA
			}
			if backend.received.ExpectedBaseSHA != "" && backend.received.Authorize == nil {
				t.Fatal("release landing lost active execution authority")
			}
			if backend.received.HeadSHA != "" && backend.received.ExpectedBaseSHA != expectedBase {
				t.Fatalf("landing lost reviewed release base: %+v", backend.received)
			}
			if backend.received.HeadSHA != "" && backend.received.TargetBranch != stub.target.BaseRef {
				t.Fatalf("landing lost target branch: %+v", backend.received)
			}
			if result.ForgeWriteCompleted != (test.wantGitHub && test.wantRecorded == 1) {
				t.Fatalf("forge landing completion evidence = %t", result.ForgeWriteCompleted)
			}
			if result.NativeLanding.RefusalKind != test.wantRefusal || result.NativeLanding.Landed != (test.wantRefusal == "" && !test.gateFailure && backend.result.CI == nil) {
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
				paths, err := filepath.Glob(filepath.Join(source, ".git", "detent-landing-*.json"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("kept landing records = %v, error = %v", paths, err)
				}
				recordPath = paths[0]
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
			target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "squash", Repository: repository, GitHubPullRequest: true}
			stub := landingStub{target: target}
			if test.reportFailure {
				stub.recordErr = errors.New("hub unavailable")
			}
			var logs bytes.Buffer
			r := &Runner{workflow: config.Workflow{Config: config.Config{Gate: gate.Config{Run: "git status --porcelain"}}}, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			result, err := r.landNativeChange(t.Context(), RunRequest{Execution: &testExecution{}}, &stub, backend, info, issue, workerGitHubPolicy{}, nil)
			if cleanupErr := backend.CleanupLanding(t.Context(), info, issue); cleanupErr != nil {
				t.Fatal(cleanupErr)
			}
			if _, statErr := os.Stat(info.Path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("landing workspace remains after cleanup: %v", statErr)
			}
			if listed := runRunnerGit(t, source, "worktree", "list", "--porcelain"); strings.Contains(listed, "worktree "+filepath.ToSlash(info.Path)) {
				t.Fatalf("landing registration remains after cleanup: %s", listed)
			}
			if test.cleanupStatus != 0 && (!strings.Contains(logs.String(), "worker_native_landing_warning") || !strings.Contains(logs.String(), "cleanup unavailable") || !strings.Contains(logs.String(), merge)) {
				t.Fatalf("post-merge warning lost failure evidence: %s", logs.String())
			}
			if test.reportFailure {
				if err == nil || !strings.Contains(err.Error(), "record landing") || len(stub.recorded) != landingReportTries {
					t.Fatalf("Hub failure = %v, report attempts = %d", err, len(stub.recorded))
				}
				readKept()
				stub.recordErr = nil
				info, err = backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				previousRequests := requests
				result, err = r.landNativeChange(t.Context(), RunRequest{Execution: &testExecution{}}, &stub, backend, info, issue, workerGitHubPolicy{}, nil)
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
	startErr            error
	observations        []tracker.NativeRuntimeObservation
	landingObservations []NativeLanding
}

func (e *landingRunExecution) StartLanding(context.Context, int64, uint64) error {
	e.started = true
	return e.startErr
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

func TestNativeReviewConversationsCleared(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	refusal := workspace.ReviewConversationsRefusalText + " Address each finding in Rework, then resolve its conversation with evidence of the fix or dismissal.\n- example-bot on app.go:3: P2 finding"
	refused := func(version string) tracker.NativeRecovery {
		return tracker.NativeRecovery{
			Change:   &tracker.NativeChangeReference{ChangeID: "change_current", VersionID: "version_current", HeadSHA: head},
			Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{ChangeID: "change_current", VersionID: version, HeadSHA: head, RefusalKind: workspace.LandRefusalProtected}}}}},
			History:  []tracker.CollaborationEvent{{Type: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: refusal}}},
		}
	}
	current := &NativeChange{Changed: true, ChangeID: "change_current", VersionID: "version_current", HeadSHA: head}
	for _, test := range []struct {
		name       string
		recovery   tracker.NativeRecovery
		change     *NativeChange
		unresolved bool
		readFails  bool
		want       bool
		wantErr    bool
		wantReads  int
	}{
		{name: "conversations resolved on GitHub clear the refusal", recovery: refused("version_current"), change: current, want: true, wantReads: 1},
		{name: "an unresolved conversation keeps the refusal", recovery: refused("version_current"), change: current, unresolved: true, wantReads: 1},
		{name: "a failed read is an error, not an unresolved answer", recovery: refused("version_current"), change: current, readFails: true, wantErr: true, wantReads: 1},
		{name: "a newly published version does not wait on GitHub", recovery: refused("version_current"), change: &NativeChange{Changed: true, ChangeID: "change_current", VersionID: "version_next", HeadSHA: strings.Repeat("2", 40)}},
		{name: "a refusal of another version is not consulted", recovery: refused("version_old"), change: current},
		{name: "work without a review refusal reads nothing", recovery: tracker.NativeRecovery{}, change: current},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution := &landingRunExecution{testExecution: testExecution{recovery: test.recovery}, landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_current", VersionID: "version_current", Repository: "https://github.com/example/repo", HeadSHA: head, BaseRef: "develop", GitHubPullRequest: true, External: &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/example/repo/pull/7"}}}}
			r := &Runner{workflow: config.Workflow{Config: config.Config{Tracker: config.Tracker{Kind: config.TrackerHubNative}, Worker: config.Worker{GitHubToken: "worker-test-token"}}}}
			reads := 0
			originalClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
				reads++
				if test.readFails {
					return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"bad gateway"}`))}, nil
				}
				nodes := ""
				if test.unresolved {
					nodes = `{"id":"T1","isResolved":false,"comments":{"nodes":[{"body":"P2 finding","author":{"__typename":"Bot","login":"example-bot"}}]}}`
				}
				body := fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"number":7,"headRefOid":%q,"baseRefName":"develop","headRepository":{"nameWithOwner":"example/repo"},"repository":{"nameWithOwner":"example/repo"},"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}}}}`, head, nodes)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = originalClient })
			got, err := r.nativeReviewConversationsCleared(t.Context(), RunRequest{Execution: execution, Mode: RunModeImplement, Issue: connector.Issue{ID: "wi_current", State: "Rework"}}, test.change)
			if got != test.want || (err != nil) != test.wantErr || reads != test.wantReads {
				t.Fatalf("cleared = %t, error = %v after %d GitHub reads; want %t, error %t after %d", got, err, reads, test.want, test.wantErr, test.wantReads)
			}
		})
	}
}

func TestNativeReviewThreadTools(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	for _, test := range []struct {
		name          string
		mode          string
		tracker       string
		tool          string
		arguments     string
		unreviewed    bool
		humanRoot     bool
		unreviewable  bool
		lostClaim     bool
		movedAt       int
		wantTools     bool
		wantSuccess   bool
		wantMutations int
		wantContent   string
	}{
		{name: "reviewed source supports explicit bot disposition", wantTools: true, wantSuccess: true, wantMutations: 2},
		{name: "read exposes the actual published source even before review", tool: "detent_read_review_threads", arguments: `{}`, unreviewed: true, wantTools: true, wantSuccess: true, wantContent: `"reviewed":false`},
		{name: "unreviewed source cannot resolve feedback", unreviewed: true, wantTools: true, wantContent: `"reason_code":"host_publication_pending"`},
		{name: "unpublished repair cannot resolve unreviewed feedback", unreviewed: true, arguments: `{"head_sha":"local-repair","thread_id":"T1","disposition":"fixed","evidence":"source.go:17 correction"}`, wantTools: true, wantContent: "current published head"},
		{name: "human-rooted conversation remains human owned", humanRoot: true, wantTools: true, wantContent: "human"},
		{name: "wrong item authority cannot be supplied", arguments: `{"work_item_id":"wi_other"}`, wantTools: true, wantContent: "invalid review disposition arguments"},
		{name: "non-review landing refusal is not a host review wait", unreviewable: true, wantTools: true, wantContent: "not reviewed"},
		{name: "lost claim cannot write to GitHub", lostClaim: true, wantTools: true, wantContent: "claim lost"},
		{name: "changed native version cannot resolve feedback", movedAt: 3, wantTools: true, wantMutations: 1, wantContent: "source changed"},
		{name: "a stale supplied head cannot resolve feedback", arguments: `{"head_sha":"old","thread_id":"T1","disposition":"fixed","evidence":"source.go:17 correction"}`, wantTools: true, wantContent: "current published head"},
		{name: "missing disposition evidence cannot use host metadata as proof", arguments: `{"head_sha":"` + head + `","thread_id":"T1","disposition":"fixed","evidence":""}`, wantTools: true, wantContent: "explicit source or test evidence"},
		{name: "unknown authority arguments are refused", arguments: `{"project_id":"another-project"}`, wantTools: true, wantContent: "invalid review disposition arguments"},
		{name: "planning does not acquire forge mutation authority", mode: RunModePlan},
		{name: "non-native workflows keep their configured ownership", tracker: config.TrackerGitHub},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution := &landingRunExecution{landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_current", VersionID: "version_current", Repository: "https://github.com/example/repo", HeadSHA: head, BaseRef: "develop", GitHubPullRequest: true, External: &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/example/repo/pull/7"}}}}
			if test.unreviewed {
				execution.targetErr = ErrLandingNotReviewed
				execution.target.ReviewPending = true
			}
			if test.unreviewable {
				execution.targetErr = ErrLandingNotReviewed
			}
			if test.lostClaim {
				execution.validateErr = errors.New("claim lost")
			}
			validations := 0
			execution.onValidate = func() {
				validations++
				if validations == test.movedAt {
					execution.target.VersionID = "another-version"
				}
			}
			kind := test.tracker
			if kind == "" {
				kind = config.TrackerHubNative
			}
			r := &Runner{workflow: config.Workflow{Config: config.Config{Tracker: config.Tracker{Kind: kind}, Worker: config.Worker{GitHubToken: "worker-test-token"}}}}
			mode := test.mode
			if mode == "" {
				mode = RunModeImplement
			}
			tools, handle := r.nativeReviewThreadTools(RunRequest{Execution: execution, Mode: mode, Issue: connector.Issue{ID: "wi_current", State: "Rework"}})
			if (len(tools) != 0) != test.wantTools {
				t.Fatalf("tools=%v, available=%t", tools, test.wantTools)
			}
			if !test.wantTools {
				return
			}
			mutations := 0
			resolved := false
			originalClient := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: nativeExecutionTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "api.github.com" || req.URL.Path != "/graphql" || req.Header.Get("Authorization") != "Bearer worker-test-token" {
					t.Fatalf("unexpected host GitHub request %s", req.URL)
				}
				var input struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				body := ""
				switch {
				case strings.Contains(input.Query, "query NativeLandingReviewThreads"):
					if input.Variables["owner"] != "example" || input.Variables["name"] != "repo" || input.Variables["number"] != float64(7) {
						t.Fatalf("unscoped thread query: %#v", input.Variables)
					}
					authorKind := "Bot"
					if test.humanRoot {
						authorKind = "User"
					}
					body = fmt.Sprintf(`{"repository":{"pullRequest":{"number":7,"headRefOid":%q,"baseRefName":"develop","headRepository":{"nameWithOwner":"example/repo"},"repository":{"nameWithOwner":"example/repo"},"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"id":"T1","isResolved":%t,"comments":{"nodes":[{"body":"P2 finding","author":{"__typename":%q,"login":"example-bot"}}]}}]}}}}`, head, resolved, authorKind)
				case strings.Contains(input.Query, "addPullRequestReviewThreadReply"):
					mutations++
					evidence, _ := input.Variables["body"].(string)
					for _, value := range []string{head, "fixed", "source.go:17", "change_current", "version_current"} {
						if !strings.Contains(evidence, value) {
							t.Fatalf("disposition missing %q: %s", value, evidence)
						}
					}
					body = `{"addPullRequestReviewThreadReply":{"comment":{"id":"reply"}}}`
				case strings.Contains(input.Query, "resolveReviewThread"):
					mutations++
					resolved = true
					if input.Variables["thread"] != "T1" {
						t.Fatal("resolved an unrelated conversation")
					}
					body = `{"resolveReviewThread":{"thread":{"id":"T1","isResolved":true}}}`
				default:
					t.Fatalf("unexpected query: %s", input.Query)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":` + body + `}`))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = originalClient })
			name := test.tool
			if name == "" {
				name = "detent_resolve_review_thread"
			}
			arguments := test.arguments
			if arguments == "" {
				arguments = `{"head_sha":"` + head + `","thread_id":"T1","disposition":"fixed","evidence":"Published source.go:17 preserves the immutable head; exact-source regression passed."}`
			}
			result, err := handle(t.Context(), AgentToolCall{Name: name, Arguments: json.RawMessage(arguments)})
			if err != nil && !test.lostClaim {
				t.Fatal(err)
			}
			content := result.Content
			if err != nil {
				content = err.Error()
			}
			if result.Success != test.wantSuccess || mutations != test.wantMutations || !strings.Contains(content, test.wantContent) {
				t.Fatalf("result=%+v err=%v mutations=%d; want success=%t mutations=%d content=%q", result, err, mutations, test.wantSuccess, test.wantMutations, test.wantContent)
			}
		})
	}
}

type completionLandingExecution struct {
	landingRunExecution
	failure *tracker.NativeTerminalFailure
}

func (e *completionLandingExecution) PrepareFinish(_ context.Context, _, _ string, failure *tracker.NativeTerminalFailure) error {
	e.failure = failure
	return nil
}

func TestNativeLandingAdmissionAndQuotaOutcomes(t *testing.T) {
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
	quota := &github.StatusError{StatusCode: 403, Err: github.ErrRateLimited, CredentialIdentity: "runner", RateLimitKind: "primary_exhausted", ObservedAt: now, ResetAt: now.Add(time.Hour)}
	cfg := config.Config{}
	cfg.Gate.Run = "true"
	cfg.Worker.GitHubToken = "runner-token"
	cfg.Tracker.Kind = config.TrackerGitHub
	cfg.Tracker.Endpoint = "https://native-finish.test/graphql"
	for _, test := range []struct {
		name        string
		admission   error
		waiting     bool
		pushRefusal string
		deferred    bool
	}{
		{name: "GitHub quota remains a capacity error"},
		{name: "project capacity becomes successful landing wait", admission: &workspace.LandRefusal{Reason: "Project landing capacity is full"}, waiting: true},
		{name: "unreviewed source remains a nonwaiting refusal", admission: &workspace.LandRefusal{Kind: workspace.LandRefusalNothing, Reason: "Current Change version is not ready for native landing"}},
		{name: "permission denied retains push evidence", pushRefusal: workspace.LandRefusalProtected},
		{name: "deferred permission denied retains push evidence", pushRefusal: workspace.LandRefusalProtected, deferred: true},
		{name: "moved base retains push evidence", pushRefusal: workspace.LandRefusalBaseMoved},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution := &completionLandingExecution{landingRunExecution: landingRunExecution{startErr: test.admission, landingStub: landingStub{target: NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "squash", Repository: "https://github.com/example/repo", GitHubPullRequest: test.pushRefusal == ""}}}}
			backend := &validationLandingBackend{landingBackend{Backend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}, recoveryStates: []workspace.RecoveryState{{HeadSHA: head}}}, err: quota}}
			if test.pushRefusal != "" {
				token := "gh" + "p_" + strings.Repeat("x", 36)
				backend.err = fmt.Errorf("%w: %w", &workspace.LandRefusal{Kind: test.pushRefusal, Reason: "the base branch refused the push"}, &workspace.CommandError{Command: "git", Args: []string{"push", "origin", "HEAD:refs/heads/develop"}, Err: errors.New("exit status 128"), Output: "Permission denied token=" + token})
			}
			r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: &fakeCodexClient{}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "native", State: "Merging"}, Mode: RunModeMerge, Execution: execution, DeferExecutionFinish: test.deferred})
			if result.RuntimeIdentity.Role != RoleMerge || result.RuntimeIdentity.BackendKind != "git" || result.RuntimeIdentity.ResolvedModel.Value != "none" {
				t.Fatalf("native landing result identity = %#v", result.RuntimeIdentity)
			}
			if test.pushRefusal != "" {
				if err != nil || result.FinalState != FinalStateCompleted || result.Output != RunOutputNativeLandingRefused || result.NativeLanding == nil || result.NativeLanding.RefusalKind != test.pushRefusal || result.NativeLanding.Landed {
					t.Fatalf("push refusal changed: result=%#v err=%v", result, err)
				}
				if !execution.stopped || !test.deferred && execution.finish != "succeeded" || test.deferred && execution.finish != "" {
					t.Fatalf("push refusal completion changed: stopped=%t finish=%q", execution.stopped, execution.finish)
				}
				if execution.failure == nil || execution.failure.ErrorClass != "git_push" || execution.failure.Summary != "Permission denied [redacted]" || strings.Contains(execution.failure.Error, strings.Repeat("x", 36)) {
					t.Fatalf("completion lost sanitized push diagnostics: %#v", execution.failure)
				}
			} else if test.admission != nil {
				if err != nil || execution.finish != "succeeded" || !execution.stopped || result.NativeLanding == nil || result.NativeLanding.HeadSHA != head || result.NativeLanding.Landed || result.NativeLanding.Waiting != test.waiting || backend.githubCalled {
					t.Fatalf("admission did not preserve healthy wait/refusal: result%#v, execution%#v, error%v", result, execution, err)
				}
			} else if !errors.Is(err, github.ErrRateLimited) || execution.finish != "failed" || !execution.stopped || len(execution.recorded) != 0 || result.NativeLanding == nil || result.NativeLanding.HeadSHA != head || result.NativeLanding.Landed {
				t.Fatalf("quota run = %#v, execution %#v, error %v", result, execution, err)
			}
		})
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
