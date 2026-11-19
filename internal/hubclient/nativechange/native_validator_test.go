package nativechange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativePrepareReviewAcrossAttempts(t *testing.T) {
	for _, outcome := range []string{"audit publication failed", "succeeded"} {
		t.Run(outcome, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			previousPolicy := h.descriptor.ID
			h.descriptor.Gates.AutomatedReview = gate.AutomatedReviewRequired
			h.descriptor.Gates.SecurityAudit = true
			h.descriptor = h.descriptor.WithID()
			if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previousPolicy, Policy: h.descriptor}); err != nil {
				t.Fatal(err)
			}
			issue := h.createInProgress(t, "Review preserved source")
			item := tracker.NativeWorkItemID(issue.ID)
			head := nativeChangeFixtureHead("c")
			start := func() *nativeExecution {
				t.Helper()
				h.claim(t, issue.ID)
				e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				e.SetRepository(nativeChangeRepository)
				e.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return nativeChangeSourceFixture(ctx, t, base, head), nil
				})
				e.SetDiffSource(nativeChangeDiff(head, "README.md"))
				if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				if err := e.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
					t.Fatal(err)
				}
				if err := e.PrepareFinish(t.Context(), "succeeded", "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", nil); err != nil {
					t.Fatal(err)
				}
				return e
			}
			first := start()
			change := first.NativeChange()
			original, err := h.admin.Change(t.Context(), item, change.ChangeID)
			if err != nil || len(original.Versions) != 1 {
				t.Fatalf("first publication: %+v, %v", original, err)
			}
			finish := "succeeded"
			if outcome == "audit publication failed" {
				audit := tracker.SecurityAuditReview{VersionID: change.VersionID, Verdict: "pass", Run: securityaudit.Run{BaseSHA: change.BaseSHA, HeadSHA: change.HeadSHA}}
				if err := first.RecordSecurityAudit(t.Context(), audit); err == nil {
					t.Fatal("audit without execution evidence was accepted")
				}
				finish = "failed"
			}
			if err := first.Finish(t.Context(), finish); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			current, err := h.admin.Issue(t.Context(), item)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != "In Progress" || finish == "succeeded" {
				target := "In Progress"
				if current.State == "Done" || current.State == "In Progress" {
					target = "Todo"
				}
				if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: target, Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				confirmIssueContract(t, h.admin, issue.ID)
			}
			second := start()
			if second.data.AttemptID == first.data.AttemptID {
				t.Fatal("second execution reused the first attempt identity")
			}
			change = second.NativeChange()
			ref := tracker.NativeChangeReference{ChangeID: change.ChangeID, VersionID: change.VersionID, HeadSHA: change.HeadSHA}
			if err := second.PrepareReview(t.Context(), ref); err != nil {
				t.Fatalf("prepare second attempt review: %v", err)
			}
			review, err := second.ReviewVersion(t.Context())
			if err != nil || review.Version == nil || review.Version.ID != ref.VersionID || review.Version.BaseSHA != original.Versions[0].BaseSHA || review.Version.HeadSHA != head || review.Version.PolicyID != h.descriptor.ID || review.Version.Policy.ID != h.descriptor.ID || review.Version.AttemptID != first.data.AttemptID || review.Version.RunID != first.data.RunID {
				t.Fatalf("review lost source or recorded owner binding: %+v, %v", review.Version, err)
			}
			latest, err := h.admin.Change(t.Context(), item, ref.ChangeID)
			if err != nil || !reflect.DeepEqual(original.Versions, latest.Versions) {
				t.Fatalf("prior version provenance changed: %+v, %v", latest.Versions, err)
			}
			for _, test := range []struct {
				name            string
				code            string
				previousVersion string
				mutate          func(*tracker.ChangeDetail)
				prepare         func(*nativeExecution)
			}{
				{name: "missing change", code: "change_missing", prepare: func(e *nativeExecution) { e.change = nil }},
				{name: "landing", code: "change_landed", prepare: func(e *nativeExecution) { e.change.Landing = &runner.NativeLanding{} }},
				{name: "change id", code: "change_id_mismatch", prepare: func(e *nativeExecution) { e.change.ChangeID = "private id" }},
				{name: "version id", code: "version_id_mismatch", prepare: func(e *nativeExecution) { e.change.VersionID = "private version" }},
				{name: "published head", code: "head_mismatch", prepare: func(e *nativeExecution) { e.change.HeadSHA = "private head" }},
				{name: "missing disposition", code: "disposition_missing", prepare: func(e *nativeExecution) { e.preparedDisposition = nil }},
				{name: "incomplete disposition", code: "disposition_incomplete", prepare: func(e *nativeExecution) { e.preparedDisposition.Status = "private status" }},
				{name: "blockers", code: "disposition_blockers", prepare: func(e *nativeExecution) { e.preparedDisposition.Blockers = true }},
				{name: "human action", code: "disposition_human_action", prepare: func(e *nativeExecution) { e.preparedDisposition.HumanAction = true }},
				{name: "reason", code: "disposition_reason", prepare: func(e *nativeExecution) { e.preparedDisposition.ReasonCode = "private reason" }},
				{name: "no change", code: "no_change_source_mismatch", prepare: func(e *nativeExecution) { e.change.NoChange = &tracker.NativeNoChangeCompletion{} }},
				{name: "changed current version", code: "version_not_current", previousVersion: "version_previous"},
				{name: "landed current version", code: "version_not_current", mutate: func(d *tracker.ChangeDetail) { d.Change.Landed = &tracker.ChangeLanding{VersionID: ref.VersionID} }},
				{name: "unavailable version", code: "version_missing", mutate: func(d *tracker.ChangeDetail) { d.Versions = nil }},
				{name: "head", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].HeadSHA = nativeChangeFixtureHead("d") }},
				{name: "base", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].BaseSHA = nativeChangeFixtureHead("b") }},
				{name: "policy", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].PolicyID = "policy_other" }},
				{name: "frozen policy", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].Policy.ID = "policy_other" }},
				{name: "item", code: "", mutate: func(d *tracker.ChangeDetail) { d.Change.WorkItemID = "wi_other" }},
				{name: "project", code: "", mutate: func(d *tracker.ChangeDetail) { d.Change.ProjectID = "prj_other" }},
				{name: "attempt", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].AttemptID = "attempt_other" }},
				{name: "run", code: "source_owner_mismatch", mutate: func(d *tracker.ChangeDetail) { d.Versions[0].RunID = "run_other" }},
				{name: "unrecovered source", code: "source_owner_mismatch", prepare: func(e *nativeExecution) { e.recoveredSource = nil }},
				{name: "unrecorded source owner", code: "source_owner_mismatch", prepare: func(e *nativeExecution) { e.claim.recovery.Attempts = nil }},
			} {
				t.Run(test.name, func(t *testing.T) {
					savedChange, savedDisposition := second.change, second.preparedDisposition
					change, disposition := *savedChange, *savedDisposition
					second.change, second.preparedDisposition = &change, &disposition
					t.Cleanup(func() { second.change, second.preparedDisposition = savedChange, savedDisposition })
					source, recovery := second.recoveredSource, second.claim.recovery
					t.Cleanup(func() { second.recoveredSource, second.claim.recovery = source, recovery })
					if test.prepare != nil {
						test.prepare(second)
					}
					transport := fixtureClient(h.native.Transport()).requests.Load()
					fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response, err := transport.RoundTrip(request)
						if err != nil || request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/"+ref.ChangeID) {
							return response, err
						}
						var page operatortool.ChangeResult
						if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
							response.Body.Close()
							return nil, err
						}
						response.Body.Close()
						if test.mutate != nil && page.Detail != nil && len(page.Detail.Versions) != 0 {
							test.mutate(page.Detail)
						}
						body, err := json.Marshal(page)
						if err != nil {
							return nil, err
						}
						response.Body = io.NopCloser(strings.NewReader(string(body)))
						return response, nil
					}))
					t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
					expected := ref
					if test.previousVersion != "" {
						expected.VersionID = test.previousVersion
						second.change.VersionID = test.previousVersion
					}
					err := second.PrepareReview(t.Context(), expected)
					if test.code == "" {
						var readErr *runner.NativeImplementationReviewError
						if !errors.As(err, &readErr) || !errors.Is(err, ErrUnavailable) {
							t.Fatalf("invalid Hub response = %v, want unavailable review", err)
						}
						return
					}
					var check reviewRefusal
					if !errors.Is(err, runner.ErrNativeRecoveryRequired) || !errors.As(err, &check) || string(check) != test.code || err.Error() != runner.ErrNativeRecoveryRequired.Error()+": "+test.code {
						t.Fatalf("review refusal = %v, want recovery with fixed check %q", err, test.code)
					}
				})
			}
		})
	}
}

func TestRecordValidatorRecoversCommittedLegacyReview(t *testing.T) {
	h := newNativeChangeHub(t, true)
	previousPolicy := h.descriptor.ID
	h.descriptor.Gates.Validator = true
	h.descriptor = h.descriptor.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previousPolicy, Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	issue := h.createInProgress(t, "Recover committed validator review")
	item := tracker.NativeWorkItemID(issue.ID)
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	head := nativeChangeFixtureHead("c")
	execution.SetRepository(nativeChangeRepository)
	execution.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
		return nativeChangeSourceFixture(ctx, t, base, head), nil
	})
	execution.SetDiffSource(nativeChangeDiff(head, "README.md"))
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "example"}); err != nil {
		t.Fatal(err)
	}
	sessions := api.NewRunnerSessions(h.native)
	ctx := sessions.BindExecution(t.Context(), execution)
	finishSession := func(role string) int64 {
		t.Helper()
		now := h.clock()
		id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: issue.ID, StartedAt: now, AgentRole: role, Model: "example"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sessions.FinishSession(ctx, id, store.SessionFinish{CompletedAt: now, FinalState: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.RecordUsageEvent(ctx, store.UsageEvent{IssueID: issue.ID, SessionID: id, StartedAt: now, FinishedAt: now, Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{IssueID: issue.ID, SessionID: id, StartedAt: now, FinishedAt: now, PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := execution.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: finishSession(runner.RoleCode)}}); err != nil {
		t.Fatal(err)
	}
	if err := execution.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, ExternalEffect: "none", EffectState: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := execution.PrepareFinish(ctx, "succeeded", "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", nil); err != nil {
		t.Fatal(err)
	}
	change := execution.NativeChange()
	input, err := execution.ValidatorVersion(ctx)
	if err != nil || input.Version == nil {
		t.Fatalf("validator input: %v", err)
	}
	version := input.Version
	result := gate.ValidatorResult{
		VersionID: version.ID, SessionID: finishSession("validator"), Submitted: true, Verdict: "pass", Score: 1,
		Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, DiffDigest: version.Source.DiffSHA256,
		CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: "test", Reference: "go test ./...", Behavior: "The test checks the completed work"}},
	}
	command := tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: fmt.Sprintf("%s:validator:%d", execution.data.AttemptID, result.SessionID), LeaseID: execution.claim.lease.ID, FencingToken: execution.claim.lease.FencingToken},
		ExpectedVersionID: version.ID, Decision: "approved", Validator: &result,
	}
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	result.CriteriaEvidence[0].Behavior += strings.Repeat("x", maxResponseBytes-len(raw))
	for i := range 201 {
		if _, err := h.admin.DiscussChange(t.Context(), item, change.ChangeID, tracker.DiscussChange{Mutation: nativeMutationKey(), Body: fmt.Sprintf("Retained discussion %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.admin.Change(t.Context(), item, change.ChangeID); err == nil {
		t.Fatal("legacy detail read accepted more than 200 records")
	} else {
		var refusal *APIError
		if !errors.As(err, &refusal) || refusal.Code != "change_detail_too_large" {
			t.Fatal(err)
		}
	}
	cfg := fixtureClient(h.native.Transport()).config
	transport := cfg.HTTPClient.Transport
	posts := 0
	cfg.HTTPClient = &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("view") == "validator_receipt" {
			r = r.Clone(r.Context())
			r.URL.RawQuery = ""
		}
		response, err := transport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(r.URL.Path, "/capabilities") {
			response.Body.Close()
			response.Body = io.NopCloser(strings.NewReader(`{"version":"0.190.2","features":["native_runtime_evidence"]}`))
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews") && response.StatusCode == http.StatusOK {
			posts++
			ack, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				return nil, err
			}
			t.Logf("committed inline review request=%d bytes acknowledgement=%d bytes", r.ContentLength, len(ack))
			if len(ack) <= maxResponseBytes {
				t.Errorf("committed review acknowledgement has %d bytes", len(ack))
			}
			response.Body = io.NopCloser(bytes.NewReader(ack))
		}
		return response, nil
	})}
	client, err := newTestClient(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	execution.claim.source = fixtureConnector(legacy)
	if err := execution.RecordValidator(ctx, result); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || execution.stepReviews == nil || len(execution.stepReviews.Reviews) != 1 {
		t.Fatal("runner did not recover the single committed review")
	}
	review := execution.stepReviews.Reviews[0]
	if review.Validator == nil || review.Validator.SessionID != result.SessionID || review.SessionMachineID != execution.claim.lease.MachineID || !reflect.DeepEqual(review.Validator.CriteriaEvidence, result.CriteriaEvidence) {
		t.Fatal("recovered validator review differs from the committed evidence")
	}
}

func TestRecordedValidatorRequiresExactPublication(t *testing.T) {
	for _, test := range []struct {
		name    string
		change  func(*tracker.ChangeDetail, *gate.ValidatorResult)
		matches bool
	}{
		{name: "matching publication", matches: true},
		{name: "empty stored attribution", matches: true, change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Validator.Attribution = "" }},
		{name: "current version changed", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Change.CurrentVersion = "version_new" }},
		{name: "different machine", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) {
			d.Reviews[0].SessionMachineID = "machine_other"
		}},
		{name: "human comment", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Actor.Kind = "human" }},
		{name: "different decision", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Decision = "changes_requested" }},
		{name: "different body", change: func(d *tracker.ChangeDetail, _ *gate.ValidatorResult) { d.Reviews[0].Body = "Other publication" }},
		{name: "different session", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) { r.SessionID++ }},
		{name: "different attribution", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) { r.Attribution = "independent" }},
		{name: "different source", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) { r.HeadSHA = strings.Repeat("d", 40) }},
		{name: "different evidence", change: func(_ *tracker.ChangeDetail, r *gate.ValidatorResult) {
			r.Commands = []gate.CommandResult{{Command: "other check"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := gate.ValidatorResult{VersionID: "version_current", SessionID: 42, Submitted: true, Verdict: "pass", HeadSHA: strings.Repeat("c", 40)}
			stored := result
			gate.NormalizeAttribution(&stored)
			detail := tracker.ChangeDetail{
				Change:  tracker.ChangeRequest{CurrentVersion: result.VersionID},
				Reviews: []tracker.ChangeReview{{ID: "review_current", VersionID: result.VersionID, SessionMachineID: "machine_current", Actor: tracker.Actor{Kind: "runner"}, Decision: "approved", Body: "Validated", Validator: &stored}},
			}
			if test.change != nil {
				test.change(&detail, &stored)
			}
			if got := recordedValidatorMatches(detail, result, "approved", "Validated", "machine_current"); got != test.matches {
				t.Fatalf("publication matched = %t, want %t", got, test.matches)
			}
		})
	}
}
