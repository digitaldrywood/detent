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
	"github.com/digitaldrywood/detent/internal/hubserver"
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name         string
		github       bool
		ssh          bool
		batch        bool
		otherMachine string
		policyChange func(*policy.Descriptor)
		wantRework   bool
	}{
		{name: "plain git by default"},
		{name: "approved GitHub PR policy", github: true},
		{name: "SSH native landing", ssh: true},
		{name: "landing and coding in one refresh", batch: true},
		{name: "local reviewed head stays on its source machine", otherMachine: "unpushed"},
		{name: "published reviewed head remains available across machines", otherMachine: "clean"},
		{name: "non-gate policy approval preserves landing", policyChange: func(p *policy.Descriptor) {
			p.ConfigDigest = policy.Digest([]byte("new non-gate configuration"))
			p.SourceRevision = strings.Repeat("f", 40)
			p.SourceDigest = policy.Digest([]byte("new source"))
			p.Profile = "updated"
		}},
		{name: "merge method change returns to Rework", policyChange: func(p *policy.Descriptor) { p.Gates.MergeMethod = "merge" }, wantRework: true},
		{name: "validator change returns to Rework", policyChange: func(p *policy.Descriptor) { p.Gates.Validator = true }, wantRework: true},
		{name: "required check floor change returns to Rework", policyChange: func(p *policy.Descriptor) { p.Gates.RequiredChecks++ }, wantRework: true},
		{name: "review change returns to Rework", policyChange: func(p *policy.Descriptor) { p.Gates.HumanReview = true }, wantRework: true},
		{name: "GitHub landing gate change returns to Rework", policyChange: func(p *policy.Descriptor) { p.Gates.GitHubPullRequest = true }, wantRework: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testNativeExecutionLandsReviewedVersion(t, false, test.github, test.ssh, test.batch, test.otherMachine, test.policyChange, test.wantRework)
		})
	}
}

func TestLinkedNativeIssueLandsWithoutGitHub(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name   string
		github bool
		ssh    bool
	}{
		{name: "source persists without PR"},
		{name: "source persists with PR", github: true},
		{name: "SSH carries source identity", github: true, ssh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			testNativeExecutionLandsReviewedVersion(t, true, test.github, test.ssh, false, "", nil, false)
		})
	}
}

func testNativeExecutionLandsReviewedVersion(t *testing.T, linked, github, ssh, batch bool, otherMachine string, policyChange func(*policy.Descriptor), wantRework bool) {
	t.Helper()
	intakeCalls := 0
	backend := intakeRepositoryBackend{snapshot: func(request hubserver.GitHubImportRequest) tracker.GitHubIssueSnapshot {
		if request.Stage == "issue" {
			intakeCalls++
		}
		snapshot := intakeSnapshot()
		snapshot.Title = "Land me"
		return snapshot
	}}
	h := newNativeChangeHubTransport(t, "Human Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review", "Rework"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Merging"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}, true, backend)
	if batch {
		h.scheduler.machine.Capacity = 6
	}
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
		created, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Land me", GitHubIssueURL: "https://github.com/acme/orders/issues/12", State: "In Progress"})
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"Todo", "In Progress"} {
			if err := h.connector.UpdateIssueState(t.Context(), string(created.WorkItemID), state); err != nil {
				t.Fatal(err)
			}
		}
		created, err = h.admin.Issue(t.Context(), created.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		issue = issueFromNative(created)
		h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
			sourceCalls++
			return tracker.GitHubIssueSnapshot{}, errors.New("GitHub source access removed after Hub intake")
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
	worktreeState := "unpushed"
	if otherMachine != "" {
		worktreeState = otherMachine
	}
	if err := work.Checkpoint(guarded, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: worktreeState, HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
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
	publishedPolicyID := h.descriptor.ID
	if policyChange != nil {
		next := h.descriptor
		policyChange(&next)
		next = next.WithID()
		if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
			t.Fatal(err)
		}
		h.descriptor = next
		if wantRework {
			if candidates := h.candidatesIn(t, "Merging"); len(candidates) != 0 || len(h.scheduler.nativeClaims) != 0 {
				t.Fatalf("stale gates were claimed for landing: %+v", candidates)
			}
			if state := h.state(t, issue.ID); state != "Rework" {
				t.Fatalf("after claim evaluation the item is in %s, want Rework", state)
			}
			detail, err := h.admin.Change(t.Context(), item, change.ChangeID)
			if err != nil || detail.Change.CurrentVersion != change.VersionID || detail.Change.Landed != nil || len(detail.Versions) != 1 || detail.Versions[0].HeadSHA != head || detail.Summary.Status != "stale_policy" {
				t.Fatalf("Rework changed immutable source or landing evidence: %+v, %v", detail, err)
			}
			return
		}
	}
	if otherMachine != "" {
		other, err := NewScheduler(h.scheduler.client, SchedulerConfig{
			OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project},
			Machine: Machine{ID: "other-machine", Hostname: "other", Capacity: 1, Version: "test"}, LeaseTTL: 90 * time.Second, HeartbeatInterval: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := other.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Merging"}})
		want := 0
		if otherMachine == "clean" {
			want = 1
		}
		if err != nil || len(candidates) != want {
			t.Fatalf("landing candidates for head availability %s: candidates=%v want=%d err=%v", otherMachine, candidates, want, err)
		}
		if otherMachine == "clean" {
			if err := other.ReleaseClaim(t.Context(), issue.ID, "released"); err != nil {
				t.Fatal(err)
			}
		}
		if otherMachine == "unpushed" {
			if _, err := h.native.Claim(t.Context(), tracker.NativeClaim{PolicyID: h.descriptor.ID, WorkItemID: item, MachineID: "other-machine", SessionID: "wrong-host", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}); err == nil {
				t.Fatal("direct claim bypassed reviewed head ownership")
			}
		}
	}

	var candidates []connector.Issue
	want := 1
	if batch {
		want = 6
		for range 5 {
			if _, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Parallel coding", State: "Todo"}); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		candidates, err = h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Merging", "Todo"}, DispatchPriorityByState: []string{"Merging", "Todo"}, AdmissionLimit: 6, CandidateLimit: 14, CandidateReady: func(context.Context, connector.Issue) bool { return true }})
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != want {
			t.Fatalf("landing batch = %d, want %d", len(candidates), want)
		}
		for _, candidate := range candidates[1:] {
			if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
				t.Fatal(err)
			}
			coding := h.scheduler.RunExecution(candidate.ID)
			if coding == nil {
				t.Fatal("claimed issue has no coding execution")
			}
			if err := coding.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(candidate.ID))
			if err != nil {
				t.Fatal(err)
			}
			if len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "running" || recovery.Attempts[0].Identity.Role != runner.RoleCode {
				t.Fatalf("parallel coding event missing: %+v", recovery.Attempts)
			}
		}
	} else {
		candidates = h.candidatesIn(t, "Merging")
	}
	if len(candidates) != want || candidates[0].ID != issue.ID || candidates[0].State != "Merging" {
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
	nativeLanding := landing
	if ssh {
		landing, _ = nativeSSHExecution(t, guarded, landing, t.TempDir())
	}
	execution, ok := landing.(runner.LandingExecution)
	if !ok {
		t.Fatalf("execution %T cannot land", landing)
	}
	if err := landing.(runner.LandingRuntimeExecution).StartLanding(guarded, 168, 27); err != nil {
		t.Fatal(err)
	}
	if batch {
		recovery, err := h.admin.Recovery(t.Context(), item)
		if err != nil {
			t.Fatal(err)
		}
		if len(recovery.Attempts) != 2 {
			t.Fatalf("parallel merge event missing: %+v", recovery.Attempts)
		}
		started := false
		for _, attempt := range recovery.Attempts {
			started = started || attempt.Status == "running" && attempt.Identity.Role == runner.RoleMerge
		}
		if !started {
			t.Fatal("batch did not start the merge phase")
		}
	}
	evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
	if err != nil || evidence.Attempt == nil || evidence.Attempt.PolicyID != h.descriptor.ID || evidence.Attempt.Runtime == nil || evidence.Attempt.Runtime.LocalAttemptID != 168 || evidence.Attempt.Runtime.Generation != 27 || evidence.Attempt.Runtime.Phase != "merging" || evidence.Attempt.Runtime.Identity.BackendKind != "git" || !evidence.Attempt.Current {
		t.Fatalf("landing attempt=%#v err=%v", evidence.Attempt, err)
	}
	target, err := execution.LandingTarget(guarded)
	if err != nil {
		t.Fatal(err)
	}
	if target.ChangeID != change.ChangeID || target.VersionID != change.VersionID || target.HeadSHA != head || target.Method != "squash" || target.Number != 1 || target.Title != "Land me" || target.Repository != nativeChangeRepository || target.GitHubPullRequest != github {
		t.Fatalf("landing target = %#v", target)
	}
	if linked {
		if len(target.SourceIssues) != 1 || target.SourceIssues[0].Repository != "acme/orders" || target.SourceIssues[0].Number != 12 || intakeCalls != 1 || sourceCalls != 0 {
			t.Fatalf("landing lost durable source identity or polled GitHub: sources=%+v intake=%d calls=%d", target.SourceIssues, intakeCalls, sourceCalls)
		}
	} else if len(target.SourceIssues) != 0 {
		t.Fatalf("native item acquired source issues: %+v", target.SourceIssues)
	}
	if err := execution.RecordLanding(guarded, runner.NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, RefusalKind: "conflict"}); err == nil {
		t.Fatal("a refusal was recorded as a landing")
	}
	refused := runner.NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, RefusalKind: "conflict", Refusal: "private command and credentials"}
	if err := landing.(runner.LandingRuntimeExecution).ObserveLanding(guarded, refused); err != nil {
		t.Fatal(err)
	}
	evidence, err = h.admin.RuntimeEvidence(t.Context(), item, "")
	if err != nil || evidence.Attempt.Runtime.Landing == nil || evidence.Attempt.Runtime.Landing.Landed || evidence.Attempt.Runtime.Landing.RefusalKind != "conflict" || evidence.Change.Change.Landed != nil {
		t.Fatalf("refused receipt=%#v err=%v", evidence, err)
	}
	landed := runner.NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: head, Landed: true, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: target.Method, Rebased: true}
	if err := execution.RecordLanding(guarded, landed); err != nil {
		t.Fatal(err)
	}
	if err := execution.RecordLanding(guarded, landed); err != nil {
		t.Fatalf("recording the landing again: %v", err)
	}
	if github {
		at := time.Now().UTC()
		elapsed := int64(time.Millisecond)
		key := tracker.NativeGitHubKey{Stage: "merging", Step: "land", EndpointFamily: "pull requests", Outcome: "200"}
		scope := &tracker.NativeGitHubScope{Scope: "native_landing", StartedAt: at, ObservedAt: at, WallElapsedNS: &elapsed,
			RESTCounts: []tracker.NativeGitHubCount{{NativeGitHubKey: key, Count: 1}},
			Timings: []tracker.NativeGitHubTiming{
				{NativeGitHubKey: key, Protocol: "rest", Boundary: "http_transport", AttemptCount: 1, TimedCount: 1, ElapsedSumNS: &elapsed, ElapsedMaxNS: &elapsed, FirstObservedAt: at, LastObservedAt: at},
				{NativeGitHubKey: key, Protocol: "rest", Boundary: "token_resolution_inclusive", AttemptCount: 1, TimedCount: 1, ElapsedSumNS: &elapsed, ElapsedMaxNS: &elapsed, FirstObservedAt: at, LastObservedAt: at},
			}}
		if err := landing.(runner.RuntimeExecution).ObserveRuntime(guarded, tracker.NativeRuntimeObservation{HeartbeatAt: at, GitHub: scope}); err != nil {
			t.Fatalf("observing completed REST landing timings: %v", err)
		}
	}
	if err := landing.(runner.LandingRuntimeExecution).ObserveLanding(guarded, landed); err != nil {
		t.Fatal(err)
	}
	if err := nativeLanding.Finish(guarded, "succeeded"); err != nil {
		t.Fatal(err)
	}
	evidence, err = h.admin.RuntimeEvidence(t.Context(), item, "")
	if err != nil || evidence.Attempt.Status != "succeeded" || !evidence.Attempt.Runtime.Landing.Rebased || evidence.Attempt.Runtime.Landing.MergeSHA != landed.MergeSHA || evidence.Change.Change.CurrentVersion != change.VersionID || evidence.Change.Change.Landed == nil || evidence.LatestTransition.Actor.Kind != "runner" || evidence.LatestTransition.Data.Reason != "worker_progress" || evidence.LatestDecision == nil || evidence.LatestDecision.Data.Decision.Source != "native_claim" || evidence.LatestDecision.Data.Decision.Outcome != "claimed" {
		t.Fatalf("landed receipt=%#v err=%v", evidence, err)
	}
	if github && (evidence.Attempt.Runtime.GitHub == nil || len(evidence.Attempt.Runtime.GitHub.Timings) != 2 || evidence.Attempt.Runtime.GitHub.Timings[0].QueryPurpose != "") {
		t.Fatalf("completed REST timing receipt = %#v", evidence.Attempt.Runtime.GitHub)
	}
	if state := h.state(t, issue.ID); state != "Done" {
		t.Fatalf("after landing the item is in %s, want Done", state)
	}
	detail, err := h.admin.Change(t.Context(), item, change.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Versions) != 1 || detail.Versions[0].PolicyID != publishedPolicyID || detail.Change.Landed == nil || !detail.Change.Landed.Rebased || detail.Change.Landed.MergeSHA != landed.MergeSHA || detail.Summary.Status != "landed" {
		t.Fatalf("landed change = %#v, summary %#v", detail.Change.Landed, detail.Summary)
	}
	if otherMachine != "" {
		attempts, err := h.admin.Attempts(t.Context(), item, "")
		if err != nil {
			t.Fatal(err)
		}
		merges := 0
		for _, attempt := range attempts.Items {
			if attempt.Identity.Role == runner.RoleMerge {
				merges++
				if attempt.MachineID != h.scheduler.machine.ID {
					t.Fatalf("landing ran away from reviewed head: %+v", attempt)
				}
			}
		}
		if merges != 1 {
			t.Fatalf("landing started %d times, want once", merges)
		}
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
	if linked && (intakeCalls != 1 || sourceCalls != 0) {
		t.Fatalf("source calls across implementation and native landing: intake=%d, runner=%d; want 1 and 0", intakeCalls, sourceCalls)
	}
}

// TestNativeExecutionLandingTargetRefusesUnreviewed checks the landing
// target of an item that reached the landing lane without a reviewed
// version: it is a refusal the run reports, not a landing.
func TestNativeExecutionLandingTargetRefusesUnreviewed(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	// Missing reviewed evidence is refused at admission, before any lease.
	if candidates := h.candidatesIn(t, "Merging"); len(candidates) != 0 || len(h.scheduler.nativeClaims) != 0 {
		t.Fatalf("unreviewed landing dispatched: candidates=%v claims=%d", candidates, len(h.scheduler.nativeClaims))
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

func (h *nativeChangeHub) candidatesIn(t *testing.T, states ...string) []connector.Issue {
	t.Helper()
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: states})
	if err != nil {
		t.Fatal(err)
	}
	return candidates
}

func TestNativeExecutionOperatorLandingTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	next = h.descriptor
	next.Gates.MergeMethod = "merge"
	next = next.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = next
	if _, err := execution.LandingTarget(t.Context()); !errors.Is(err, runner.ErrLandingNotReviewed) {
		t.Fatalf("stale policy target error = %v", err)
	}
}
