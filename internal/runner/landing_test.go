package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		name      string
		targetErr error
		createErr error
		github    bool
		created   bool
		quota     bool
		status    int
	}{
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
			cfg := config.Config{}
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

func TestLandNativeChange(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("c", 40)
	merge := strings.Repeat("e", 40)
	target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "merge", Title: "Add a sign-in link", Number: 2}
	for _, test := range []struct {
		name         string
		stub         landingStub
		backend      landingBackend
		wantOutput   string
		wantErr      string
		wantRecorded int
		wantRefusal  string
		wantGitHub   bool
		quota        bool
		prepared     *NativeLandingTarget
	}{
		{name: "lands and records", stub: landingStub{target: target}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge"}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1},
		{name: "opted-in project uses GitHub PR landing", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge"}},
			wantOutput: RunOutputNativeLanded, wantRecorded: 1, wantGitHub: true},
		{name: "quota retains reviewed identity and actual metrics", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{githubRequest: true}, wantErr: "github rate limited", quota: true},
		{name: "a refusal is reported, not recorded", stub: landingStub{target: target}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalProtected, Reason: "the base branch main refused the push"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalProtected},
		{name: "a GitHub conflict retains reviewed identity without a landing receipt", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, Reason: "Pull Request is not mergeable (HTTP 405)"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalConflict, wantGitHub: true},
		{name: "an atomic GitHub head refusal retains reviewed identity without a receipt", stub: landingStub{target: NativeLandingTarget{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Method: "merge", Repository: "https://github.com/example/repo", GitHubPullRequest: true}}, backend: landingBackend{err: &workspace.LandRefusal{Kind: workspace.LandRefusalHeadMoved, Reason: "Head branch was modified (HTTP 409)"}},
			wantOutput: RunOutputNativeLandingRefused, wantRefusal: workspace.LandRefusalHeadMoved, wantGitHub: true},
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
		{name: "an unrecorded landing fails the run after retrying the report", stub: landingStub{target: target, recordErr: errors.New("hub unavailable")}, backend: landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "merge"}},
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
				if IsCapacityError(backend.err) || errors.Is(backend.err, github.ErrRateLimited) {
					if result.NativeLanding == nil || result.NativeLanding.ChangeID != target.ChangeID || result.NativeLanding.VersionID != target.VersionID || result.NativeLanding.HeadSHA != head || result.NativeLanding.Landed || result.NativeLanding.RefusalKind != "" || result.Output != "" || result.FinalState != "" {
						t.Fatalf("capacity wait lost reviewed identity or became a completed refusal: %#v", result)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.FinalState != FinalStateCompleted || result.Output != test.wantOutput || result.NativeLanding == nil {
				t.Fatalf("result = %#v", result)
			}
			if result.NativeLanding.RefusalKind != test.wantRefusal || result.NativeLanding.Landed != (test.wantRefusal == "") {
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
			if test.wantRecorded == 1 {
				if backend.received.HeadSHA != head || backend.received.Method != "merge" || !backend.received.PushAttemptBranch || (!test.wantGitHub && (!strings.Contains(backend.received.Message, "Add a sign-in link") || !strings.Contains(backend.received.Message, "round 2"))) {
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
	t.Parallel()
	head := strings.Repeat("c", 40)
	merge := strings.Repeat("e", 40)
	target := NativeLandingTarget{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Method: "squash"}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.test"}, {"config", "user.name", "t"}, {"config", "commit.gpgsign", "false"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := gitCommand(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	info := workspace.Info{Path: dir, Branch: "detent/land"}
	stub := landingStub{target: target, recordErr: errors.New("hub unavailable")}
	backend := landingBackend{result: workspace.LandResult{MergeSHA: merge, BaseRef: "main", Method: "squash"}}
	r := &Runner{}
	_, err := r.landNativeChange(t.Context(), RunRequest{}, &stub, &backend, info, workspace.Issue{Identifier: "DD-1"}, workerGitHubPolicy{Token: "landing-test-token"}, nil)
	if err == nil || !strings.Contains(err.Error(), "record landing") {
		t.Fatalf("error = %v, want the report failure", err)
	}
	if len(stub.recorded) != landingReportTries {
		t.Fatalf("report attempts = %d, want %d", len(stub.recorded), landingReportTries)
	}
	kept, err := os.ReadFile(filepath.Join(dir, ".git", "detent-landing.json"))
	if err != nil {
		t.Fatalf("the pushed landing was not kept: %v", err)
	}
	if !strings.Contains(string(kept), merge) || !strings.Contains(string(kept), head) {
		t.Fatalf("kept landing = %s", kept)
	}
	// The next run reports it and forgets it.
	stub.recordErr = nil
	if _, err := r.landNativeChange(t.Context(), RunRequest{}, &stub, &backend, info, workspace.Issue{Identifier: "DD-1"}, workerGitHubPolicy{Token: "landing-test-token"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "detent-landing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reported landing was kept: %v", err)
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
	stopped bool
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
	cfg.Worker.GitHubToken = "runner-token"
	cfg.Tracker.Kind = config.TrackerGitHub
	cfg.Tracker.Endpoint = "https://native-finish.test/graphql"
	r, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: backend, AgentBackend: &fakeCodexClient{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "native", State: "Merging"}, Mode: RunModeMerge, Execution: execution})
	if !errors.Is(err, github.ErrRateLimited) || execution.finish != "failed" || !execution.stopped || len(execution.recorded) != 0 || result.NativeLanding == nil || result.NativeLanding.HeadSHA != head || result.NativeLanding.Landed {
		t.Fatalf("quota run = %#v, execution %#v, error %v", result, execution, err)
	}
}
