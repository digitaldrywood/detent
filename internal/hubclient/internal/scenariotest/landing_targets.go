package scenariotest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
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
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}}); err != nil {
		t.Fatal(err)
	}
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Operator source", Body: issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	item := tracker.NativeWorkItemID(issue.ID)
	change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Operator source"})
	if err != nil {
		t.Fatal(err)
	}
	head := nativeChangeFixtureHead("c")
	external := &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: nativeChangeRepository + "/pull/7"}
	capture := nativeChangeSourceFixture(t.Context(), t, nativeChangeFixtureHead("a"), head)
	version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
		Mutation: nativeMutationKey(), SourceBundle: capture.Bundle,
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
	if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "approved"}); err != nil {
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
	second, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{Mutation: nativeMutationKey(), ExpectedVersionID: version.ID, ChangeVersionInput: secondInput, SourceBundle: secondCapture.Bundle})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.LandingTarget(t.Context()); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("unreviewed replacement target error = %v", err)
	}
	if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, second.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "approved"}); err != nil {
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

func RunNativeLandingBatchClaimsOneCapacityUnit(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	t.Parallel()
	h := newPlannerHub(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Human Review", "Merging", "In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Human Review"}},
		{Name: "Human Review", Transitions: []string{"Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review"}},
		{Name: "Done", Terminal: true},
	}, factories)
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		issue := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Batch source", Body: issueContractTestSections, State: "Todo"})
		item := issue.WorkItemID
		change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Batch source"})
		if err != nil {
			t.Fatal(err)
		}
		head := nativeChangeFixtureHead("c")
		capture := nativeChangeSourceFixture(t.Context(), t, nativeChangeFixtureHead("a"), head)
		version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{Mutation: nativeMutationKey(), SourceBundle: capture.Bundle, ChangeVersionInput: tracker.ChangeVersionInput{Source: &capture.Source, BaseSHA: nativeChangeFixtureHead("a"), HeadSHA: head, MergeBaseSHA: nativeChangeFixtureHead("a"), Repository: nativeChangeRepository, Code: tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: policy.Digest([]byte(head)), Availability: "unverified"}, PolicyID: h.descriptor.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.connector.UpdateIssueState(t.Context(), string(issue.WorkItemID), "Human Review"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "approved"}); err != nil {
			t.Fatal(err)
		}
	}
	request := orchestrator.SchedulingRequest{ProjectID: "local", WorkflowStates: []string{"Merging"}, Policy: h.descriptor, AdmissionLimit: 1, CandidateLimit: 10, NativeLandingBatch: true, CandidateReady: func(context.Context, connector.Issue) bool { return true }}
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 10 {
		t.Fatalf("batch claims = %d, %v", len(candidates), err)
	}
	for _, candidate := range candidates {
		if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
			t.Fatal(err)
		}
		target, err := h.scheduler.RunExecution(candidate.ID).(runner.LandingExecution).LandingTarget(t.Context())
		if err != nil || target.HeadSHA != nativeChangeFixtureHead("c") {
			t.Fatalf("target = %#v, %v", target, err)
		}
	}
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Ordinary Code remains bounded", Body: issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	request.NativeLandingBatch = false
	request.WorkflowStates = []string{"Todo"}
	candidates, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("Code %s claimed beside full landing slot: %v, %v", issue.ID, candidates, err)
	}
}
