package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestNativeExecutionLandsReviewedVersion drives one item from a finished
// work run through review to landing against a real hub: the approval moves
// it to the landing lane, the runner claims it there, the execution names the
// reviewed version and its merge method, and recording the landing finishes
// the item and the Change Request.
func TestNativeExecutionLandsReviewedVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		github bool
		ssh    bool
	}{
		{"plain git by default", false, false},
		{"approved GitHub PR policy", true, false},
		{"SSH native landing", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testNativeExecutionLandsReviewedVersion(t, false, test.github, test.ssh)
		})
	}
}

func TestLinkedNativeIssueLandsWithoutGitHub(t *testing.T) {
	t.Parallel()
	testNativeExecutionLandsReviewedVersion(t, true, false, false)
}

func testNativeExecutionLandsReviewedVersion(t *testing.T, linked, github, ssh bool) {
	t.Helper()
	h := newNativeChangeHubWithStates(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}, intakeRepositoryBackend{})
	if github {
		next := h.descriptor
		next.Gates.GitHubPullRequest = true
		next = next.WithID()
		if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
			t.Fatal(err)
		}
		h.descriptor = next
	}
	var issue connector.Issue
	if !linked {
		issue = h.createInProgress(t, "Land me")
	}
	sourceCalls := 0
	if linked {
		// Reuse the exact landing journey, with a historical GitHub source.
		if err := h.admin.client.request(t.Context(), http.MethodPost, h.admin.base()+"/onboarding/repository", map[string]any{"idempotency_key": "attach", "expected_revision": "1", "repository": "acme/orders"}, nil); err != nil {
			t.Fatal(err)
		}
		created, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), GitHubIssueURL: "https://github.com/acme/orders/issues/12", State: "In Progress"})
		if err != nil {
			t.Fatal(err)
		}
		issue = issueFromNative(created)
		h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
			sourceCalls++
			if sourceCalls > 1 {
				return tracker.GitHubIssueSnapshot{}, errors.New("GitHub source access removed after intake")
			}
			snapshot := intakeSnapshot()
			snapshot.Title = "Land me"
			return snapshot, nil
		}
	}
	item := tracker.NativeWorkItemID(issue.ID)
	head := strings.Repeat("c", 40)
	h.claim(t, issue.ID)
	work := h.scheduler.RunExecution(issue.ID)
	if work == nil {
		t.Fatal("claimed issue has no native execution")
	}
	guarded, stop, err := work.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	work.(runner.DiffExecution).SetDiffSource(nativeChangeDiff(head, "README.md"))
	work.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
	if err := work.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := work.Checkpoint(guarded, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", ExternalEffect: "none", EffectState: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := work.Finish(guarded, "succeeded"); err != nil {
		t.Fatal(err)
	}
	stop()
	change := work.(runner.ChangeExecution).NativeChange()
	if change == nil || change.ChangeID == "" || change.VersionID == "" {
		t.Fatalf("native change = %#v", change)
	}
	h.complete(t, issue.ID, change)
	if state := h.state(t, issue.ID); state != "Human Review" {
		t.Fatalf("after the run the item is in %s", state)
	}

	// Nothing lands before the review.
	if candidates := h.candidatesIn(t, "Merging"); len(candidates) != 0 {
		t.Fatalf("an unreviewed item was offered for landing: %#v", candidates)
	}
	if _, err := h.admin.ReviewChange(t.Context(), item, change.ChangeID, change.VersionID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "approved", Body: "Ship it."}); err != nil {
		t.Fatal(err)
	}
	if state := h.state(t, issue.ID); state != "Merging" {
		t.Fatalf("after approval the item is in %s, want Merging", state)
	}

	candidates := h.candidatesIn(t, "Merging")
	if len(candidates) != 1 || candidates[0].ID != issue.ID || candidates[0].State != "Merging" {
		t.Fatalf("landing candidates = %#v", candidates)
	}
	if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	landing := h.scheduler.RunExecution(issue.ID)
	if landing == nil {
		t.Fatal("claimed issue has no landing execution")
	}
	guarded, stop, err = landing.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if ssh {
		landing, _ = nativeSSHExecution(t, guarded, landing, t.TempDir())
	}
	execution, ok := landing.(runner.LandingExecution)
	if !ok {
		t.Fatalf("execution %T cannot land", landing)
	}
	target, err := execution.LandingTarget(guarded)
	if err != nil {
		t.Fatal(err)
	}
	if target.ChangeID != change.ChangeID || target.VersionID != change.VersionID || target.HeadSHA != head || target.Method != "squash" || target.Number != 1 || target.Title != "Land me" || target.Repository != nativeChangeRepository || target.GitHubPullRequest != github {
		t.Fatalf("landing target = %#v", target)
	}
	if err := execution.RecordLanding(guarded, runner.NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, RefusalKind: "conflict"}); err == nil {
		t.Fatal("a refusal was recorded as a landing")
	}
	landed := runner.NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Landed: true, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: target.Method}
	if err := execution.RecordLanding(guarded, landed); err != nil {
		t.Fatal(err)
	}
	if err := execution.RecordLanding(guarded, landed); err != nil {
		t.Fatalf("recording the landing again: %v", err)
	}
	if state := h.state(t, issue.ID); state != "Done" {
		t.Fatalf("after landing the item is in %s, want Done", state)
	}
	detail, err := h.admin.Change(t.Context(), item, change.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Change.Landed == nil || detail.Change.Landed.MergeSHA != landed.MergeSHA || detail.Summary.Status != "landed" {
		t.Fatalf("landed change = %#v, summary %#v", detail.Change.Landed, detail.Summary)
	}
	for _, fetch := range []struct {
		name string
		run  func() ([]connector.Issue, error)
	}{
		{name: "by ID", run: func() ([]connector.Issue, error) {
			return h.connector.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
		}},
		{name: "by state", run: func() ([]connector.Issue, error) {
			return h.connector.FetchIssuesByStates(t.Context(), []string{"Done"})
		}},
	} {
		t.Run(fetch.name, func(t *testing.T) {
			issues, err := fetch.run()
			if err != nil || len(issues) != 1 || !issues[0].Closed || issues[0].Metadata["hub_landed_head_sha"] != head || issues[0].Metadata["hub_landed_merge_sha"] != landed.MergeSHA {
				t.Fatalf("cleanup issue = %#v, error = %v", issues, err)
			}
		})
	}
	if _, err := execution.LandingTarget(guarded); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("landing target after landing = %v, want %v", err, runner.ErrLandingNotReviewed)
	}
	if linked && sourceCalls != 1 {
		t.Fatalf("source calls across implementation and native landing = %d, want 1", sourceCalls)
	}
}

// TestNativeExecutionLandingTargetRefusesUnreviewed checks the landing
// target of an item that reached the landing lane without a reviewed
// version: it is a refusal the run reports, not a landing.
func TestNativeExecutionLandingTargetRefusesUnreviewed(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHubWithStates(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Merging", "Done"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Todo"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	})
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "No change", Body: "Nothing to land."})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Merging"); err != nil {
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
	if _, err := execution.LandingTarget(t.Context()); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("landing target = %v, want %v", err, runner.ErrLandingNotReviewed)
	}
}

func (h *nativeChangeHub) candidatesIn(t *testing.T, states ...string) []connector.Issue {
	t.Helper()
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: states})
	if err != nil {
		t.Fatal(err)
	}
	return candidates
}

func TestNativeExecutionOperatorLandingTarget(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHubTransport(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Human Review", "Merging"}},
		{Name: "Human Review", Transitions: []string{"Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review"}},
		{Name: "Done", Terminal: true},
	}, true)
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
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Operator source"})
	if err != nil {
		t.Fatal(err)
	}
	item := tracker.NativeWorkItemID(issue.ID)
	change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Operator source"})
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("c", 40)
	external := &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: nativeChangeRepository + "/pull/7"}
	version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
		Mutation: nativeMutationKey(),
		ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: head, MergeBaseSHA: strings.Repeat("a", 40), Repository: nativeChangeRepository,
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
	secondInput.HeadSHA = strings.Repeat("d", 40)
	secondInput.Code.URI = nativeChangeRepository + "/commit/" + secondInput.HeadSHA
	secondInput.Code.SHA256 = policy.Digest([]byte(secondInput.HeadSHA))
	second, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{Mutation: nativeMutationKey(), ExpectedVersionID: version.ID, ChangeVersionInput: secondInput})
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
	h.repolicy(t)
	if _, err := execution.LandingTarget(t.Context()); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("stale policy target error = %v", err)
	}
}
