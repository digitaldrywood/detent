package scenariotest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RunNativeExecutionLandingTargetRefusesUnreviewed(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	t.Parallel()
	h := newPlannerHub(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Merging", "Done"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Todo"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}, factories)
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "No change", Body: "Nothing to land." + issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	confirmIssueContract(t, h.admin, issue.ID)
	if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Merging"); err != nil {
		t.Fatal(err)
	}
	// Missing reviewed evidence is refused at admission, before any lease.
	if candidates := h.candidatesIn(t, "Merging"); len(candidates) != 0 || len(h.scheduler.ActiveExecutions()) != 0 {
		t.Fatalf("unreviewed landing dispatched: candidates=%v claims=%d", candidates, len(h.scheduler.ActiveExecutions()))
	}
	// Adopt an eligible coding claim, then move the item while that claim is
	// held. The landing reader must independently refuse the missing evidence.
	if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Todo"); err != nil {
		t.Fatal(err)
	}
	h.claim(t, issue.ID)
	if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Merging"); err != nil {
		t.Fatal(err)
	}
	execution := h.scheduler.RunExecution(issue.ID).(runner.LandingExecution)
	if _, err := execution.LandingTarget(t.Context()); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("landing target = %v, want %v", err, runner.ErrLandingNotReviewed)
	}
}

func RunNativeExecutionOperatorLandingTarget(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	t.Parallel()
	h := newPlannerHub(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Human Review", "Merging"}},
		{Name: "Human Review", Transitions: []string{"Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review", "Rework"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Merging"}},
		{Name: "Done", Terminal: true},
	}, factories)
	next := h.descriptor
	next.Gates.GitHubPullRequest = true
	next = next.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = next
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: api.NativeMutationKey(), Policy: tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}}); err != nil {
		t.Fatal(err)
	}
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Operator source", Body: issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	item := tracker.NativeWorkItemID(issue.ID)
	change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: api.NativeMutationKey(), Title: "Operator source"})
	if err != nil {
		t.Fatal(err)
	}
	head := nativeChangeFixtureHead("c")
	external := &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: nativeChangeRepository + "/pull/7"}
	capture := nativeChangeSourceFixture(t.Context(), t, nativeChangeFixtureHead("a"), head)
	version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
		Mutation: api.NativeMutationKey(), SourceBundle: capture.Bundle,
		ChangeVersionInput: tracker.ChangeVersionInput{Source: &capture.Source, BaseSHA: nativeChangeFixtureHead("a"), HeadSHA: head, MergeBaseSHA: nativeChangeFixtureHead("a"), Repository: nativeChangeRepository,
			Code: tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: policy.Digest([]byte(head)), Availability: "unverified"}, PolicyID: h.descriptor.ID, External: external},
	})
	if err != nil {
		t.Fatal(err)
	}
	if version.RunID != "" || version.AttemptID != "" || version.Actor.Kind != "human" {
		t.Fatalf("operator version = %#v", version)
	}
	if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Human Review"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: api.NativeMutationKey(), Decision: "approved"}); err != nil {
		t.Fatal(err)
	}
	candidates := h.candidatesIn(t, "Merging")
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v", candidates)
	}
	if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	execution := h.scheduler.RunExecution(issue.ID).(runner.LandingExecution)
	h.configureProjectCheckout(t, "")
	if _, err := execution.LandingTarget(t.Context()); err == nil || !strings.Contains(err.Error(), "landing requires a configured project repository") {
		t.Fatalf("missing project repository: %v", err)
	}
	h.configureProjectCheckout(t, "example/other")
	if _, err := execution.LandingTarget(t.Context()); err == nil || !strings.Contains(err.Error(), "reviewed GitHub repository differs") {
		t.Fatalf("different project repository: %v", err)
	}
	h.configureProjectCheckout(t, nativeChangeRepository)
	checkReviewedRepositoryAdmission(t, h, candidates[0], change.ID, execution)
	target, err := execution.LandingTarget(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	var transported struct {
		External *tracker.ChangeExternalReference
	}
	if err := json.Unmarshal(raw, &transported); err != nil {
		t.Fatal(err)
	}
	if transported.External == nil || *transported.External != *external {
		t.Fatalf("landing dropped operator external PR: %s", raw)
	}
	remote, closeRemote := nativeSSHExecution(t, t.Context(), h.scheduler.RunExecution(issue.ID), t.TempDir())
	remoteTarget, err := remote.(runner.LandingExecution).LandingTarget(t.Context())
	closeRemote()
	if err != nil || remoteTarget.External == nil || *remoteTarget.External != *external || remoteTarget.HeadSHA != head {
		t.Fatalf("remote landing target = %#v, error = %v", remoteTarget, err)
	}
	secondInput := version.ChangeVersionInput
	secondInput.HeadSHA = nativeChangeFixtureHead("d")
	secondInput.Code.URI = nativeChangeRepository + "/commit/" + secondInput.HeadSHA
	secondInput.Code.SHA256 = policy.Digest([]byte(secondInput.HeadSHA))
	secondCapture := nativeChangeSourceFixture(t.Context(), t, secondInput.BaseSHA, secondInput.HeadSHA)
	secondInput.Source = &secondCapture.Source
	second, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{Mutation: api.NativeMutationKey(), ExpectedVersionID: version.ID, ChangeVersionInput: secondInput, SourceBundle: secondCapture.Bundle})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.LandingTarget(t.Context()); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("unreviewed replacement target error = %v", err)
	}
	if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, second.ID, tracker.ReviewChange{Mutation: api.NativeMutationKey(), Decision: "approved"}); err != nil {
		t.Fatal(err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "released"); err != nil {
		t.Fatal(err)
	}
	next = h.descriptor
	next.Gates.Validator = true
	next = next.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = next
	if _, err := execution.LandingTarget(t.Context()); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("stale policy target error = %v", err)
	}
}

func checkReviewedRepositoryAdmission(t *testing.T, h *nativeChangeHub, issue connector.Issue, changeID string, execution runner.LandingExecution) {
	t.Helper()
	agent, err := runner.NewRunner(runner.Dependencies{
		ProcessSource: emptyProcessSource,
		Store:         api.NewRunnerSessions(h.native), ProjectID: "local", Workspace: &conversationStubWorkspace{}, AgentBackend: NewReviewAgent(),
		Workflow: config.Workflow{Config: config.Config{Policy: h.descriptor, Tracker: config.Tracker{Kind: config.TrackerHubNative}}, Prompt: "Complete the issue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, repository string }{
		{"percent escaped", "https://github.com/%65xample/native-change"},
		{"trailing slash", nativeChangeRepository + "/"},
		{"git suffix", nativeChangeRepository + ".git"},
		{"Unicode case fold", nativeChangeRepository + "ſ"},
		{"host case", "https://GitHub.com/example/native-change"},
		{"explicit HTTPS port", "https://github.com:443/example/native-change"},
		{"HTTPS userinfo", "https://example@github.com/example/native-change"},
		{"dot segment", "https://github.com/example/./native-change"},
		{"different repository", "https://github.com/example/other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "Unicode case fold" {
				h.configureProjectCheckout(t, "example/native-changes")
				defer h.configureProjectCheckout(t, nativeChangeRepository)
			}
			requests := fixtureClient(h.native.Transport()).requests
			original := requests.Load()
			requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				response, err := original.RoundTrip(request)
				if err != nil || request.URL.Query().Get("view") != "bounded" || !strings.HasSuffix(request.URL.Path, "/changes/"+changeID) || response.StatusCode != http.StatusOK {
					return response, err
				}
				defer response.Body.Close()
				var result operatortool.ChangeResult
				if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
					return nil, err
				}
				for i := range result.Detail.Versions {
					result.Detail.Versions[i].Repository = test.repository
				}
				body, err := json.Marshal(result)
				if err != nil {
					return nil, err
				}
				response.Body = io.NopCloser(bytes.NewReader(body))
				response.ContentLength = int64(len(body))
				return response, nil
			}))
			defer requests.Store(original)
			probe := &landingAdmissionExecution{Execution: h.scheduler.RunExecution(issue.ID), LandingExecution: execution}
			_, err := agent.Run(t.Context(), runner.RunRequest{Execution: probe, ProjectID: "local", Issue: issue, Mode: runner.RunModeMerge})
			if probe.starts != 0 || err == nil || !strings.Contains(err.Error(), "reviewed GitHub repository differs") {
				t.Fatalf("landing admission for %q returned %v after %d starts", test.repository, err, probe.starts)
			}
		})
	}
}

type landingAdmissionExecution struct {
	runner.Execution
	runner.LandingExecution
	starts int
}

func (e *landingAdmissionExecution) StartLanding(context.Context, int64, uint64) error {
	e.starts++
	return errors.New("unexpected landing start")
}

func (*landingAdmissionExecution) ObserveLanding(context.Context, runner.NativeLanding) error {
	return nil
}

func (*landingAdmissionExecution) Finish(context.Context, string) error { return nil }

func RunReviewedItemsUseOneLeaseSlot(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	t.Parallel()
	h := newPlannerHub(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Human Review", "Merging", "In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Human Review"}},
		{Name: "Human Review", Transitions: []string{"Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review"}},
		{Name: "Done", Terminal: true},
	}, factories)
	h.configureProjectCheckout(t, nativeChangeRepository)
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: api.NativeMutationKey(), Policy: tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}}); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		issue := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: "Reviewed source", Body: issueContractTestSections, State: "Todo"})
		item := issue.WorkItemID
		change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: api.NativeMutationKey(), Title: "Reviewed source"})
		if err != nil {
			t.Fatal(err)
		}
		head := nativeChangeFixtureHead("c")
		capture := nativeChangeSourceFixture(t.Context(), t, nativeChangeFixtureHead("a"), head)
		version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{Mutation: api.NativeMutationKey(), SourceBundle: capture.Bundle, ChangeVersionInput: tracker.ChangeVersionInput{Source: &capture.Source, BaseSHA: nativeChangeFixtureHead("a"), HeadSHA: head, MergeBaseSHA: nativeChangeFixtureHead("a"), Repository: nativeChangeRepository, Code: tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: policy.Digest([]byte(head)), Availability: "unverified"}, PolicyID: h.descriptor.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.connector.UpdateIssueState(t.Context(), string(issue.WorkItemID), "Human Review"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: api.NativeMutationKey(), Decision: "approved"}); err != nil {
			t.Fatal(err)
		}
	}
	request := orchestrator.SchedulingRequest{ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}, Policy: h.descriptor, AdmissionLimit: 1, CandidateLimit: 10, CandidateReady: func(context.Context, connector.Issue) bool { return true }}
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("reviewed claims = %d, %v", len(candidates), err)
	}
	candidate := candidates[0]
	if candidate.State != "Merging" || candidate.NativeLandingHeadSHA != nativeChangeFixtureHead("c") {
		t.Fatalf("claim omitted reviewed landing evidence: %+v", candidate)
	}
	if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
		t.Fatal(err)
	}
	target, err := h.scheduler.RunExecution(candidate.ID).(runner.LandingExecution).LandingTarget(t.Context())
	if err != nil || target.HeadSHA != candidate.NativeLandingHeadSHA {
		t.Fatalf("target = %#v, %v", target, err)
	}
	candidates, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("claimed beside the active lease: %v, %v", candidates, err)
	}
}

func (h *nativeChangeHub) configureProjectCheckout(t *testing.T, repository string) {
	t.Helper()
	h.seed(func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), h.project)
		if err != nil {
			return err
		}
		project.Settings.CheckoutRepository = strings.TrimPrefix(repository, "https://github.com/")
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	})
}
