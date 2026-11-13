package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func independentFailureFixture(t *testing.T) changeFixture {
	t.Helper()
	now := time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC)
	f := newChangeFixture(t, newClockedPostgresNativeService(t, Config{now: func() time.Time { return now }}))
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		record.Core.RequireDependencies = true
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func independentFailureScope(t *testing.T, f nativeFixture) hubscope.NativeScope {
	t.Helper()
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
}

func independentIssue(t *testing.T, f nativeFixture, id tracker.NativeWorkItemID) tracker.NativeIssue {
	t.Helper()
	issue, err := f.service.readWorkItemSourcePage(t.Context(), independentFailureScope(t, f), string(id))
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func independentDetail(t *testing.T, f changeFixture) tracker.ChangeDetail {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.path, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, response, &detail)
	current, err := f.service.selectedCurrentChangeSummary(t.Context(), independentFailureScope(t, f.nativeFixture), f.issue.WorkItemID, detail.Change.ID, f.service.config.now())
	if err != nil || !reflect.DeepEqual(current, detail.Summary) {
		t.Fatalf("current summary = %#v, full summary = %#v, error = %v", current, detail.Summary, err)
	}
	return detail
}

func independentIssueCount(t *testing.T, f nativeFixture) int {
	t.Helper()
	count := 0
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		items, err := r.WorkItems(t.Context(), f.project.ID, 0, 200)
		count = len(items)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func independentMove(t *testing.T, f nativeFixture, id tracker.NativeWorkItemID, state string) {
	t.Helper()
	current := independentIssue(t, f, id)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(id)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-" + string(id) + "-" + state}, ExpectedRevision: current.Revision, State: state, Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
}

func independentCreateOwner(t *testing.T, f nativeFixture, request tracker.CreateIssue) tracker.NativeIssue {
	t.Helper()
	scope := independentFailureScope(t, f)
	plan, err := f.service.loadWorkItemPlan(t.Context(), scope, "", "", request)
	if err == nil {
		plan, err = f.service.planWorkItemRecord(t.Context(), scope, plan, request)
	}
	if err != nil {
		t.Fatal(err)
	}
	plan.current.NativeID = plan.issue.WorkItemID
	record, err := f.service.prepareScheduledAttributionRecord(t.Context(), scope, plan, tracker.CollaborationData{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		return f.service.applyIndependentRecord(t.Context(), w, scope, record)
	}); err != nil {
		t.Fatal(err)
	}
	return plan.issue
}

func independentReview(t *testing.T, f changeFixture, change tracker.ChangeRequest, review *tracker.ChangeReview, persist bool) error {
	t.Helper()
	ctx := f.service.runDetailContext(t.Context())
	credential, _, err := f.service.authenticateTenantAPIHash(ctx, f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		return err
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		plan, err := f.service.prepareIndependentRepairs(ctx, scope, change.WorkItemID, review.IndependentFailures(), change.ID+"/"+review.ID, review.CreatedAt)
		if errors.Is(err, corestore.ErrSerializationFailure) {
			continue
		}
		if err != nil {
			return err
		}
		if plan != nil {
			review.RepairDependencies, review.RepairRefusal = plan.dependencies, plan.refusal
		}
		review.Actor = scope.Actor()
		raw, err := json.Marshal(review)
		if err != nil {
			return err
		}
		var evidence eventstore.Evidence
		if persist {
			evidence, err = eventstore.NewContent(f.service.runDetailStore()).PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: change.ID, VersionID: new(review.VersionID), Kind: "review", CreatedAt: review.CreatedAt}, raw)
			if err != nil {
				return err
			}
		}
		err = f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			refusal, err := f.service.checkIndependentRepairs(ctx, w, scope, plan)
			if err != nil {
				return err
			}
			if plan != nil && refusal != plan.refusal {
				return corestore.ErrSerializationFailure
			}
			if err := f.service.applyIndependentRepairs(ctx, w, scope, plan); err != nil {
				return err
			}
			if persist {
				_, err = w.AppendEvidence(ctx, evidence)
			}
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		return err
	}
}

func independentRepairReview(version tracker.ChangeVersion, now time.Time) tracker.ChangeReview {
	diagnostic := "source.go:4:2: invalid input"
	base := failureattribution.Observation{Command: "check-input", HeadSHA: version.BaseSHA, ExitCode: 1, Output: diagnostic, Complete: true}
	result := &gate.ValidatorResult{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Verdict: "rework", Findings: []gate.Finding{{Severity: "p1", Body: diagnostic, CheckCommand: base.Command, CheckDiagnostic: diagnostic}}, Commands: []gate.CommandResult{{Command: base.Command, HeadSHA: version.HeadSHA, ExitCode: 1, Output: diagnostic, FinishedAt: now, BaseReproduction: &base}}}
	gate.NormalizeAttribution(result)
	return tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: version.ID, Decision: "changes_requested", Body: "Independent source failure", Validator: result, CreatedAt: now}
}

func TestIndependentFailureRepairDependency(t *testing.T) {
	f := independentFailureFixture(t)
	version := f.publish(t, "version", "")
	change := f.detail(t).Change
	review := independentRepairReview(version, f.service.config.now())
	if _, err := nativeAttemptTestDatabase(t, f.nativeFixture).ExecContext(t.Context(), "UPDATE issues SET doctor_identity_revision=NULL WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", f.project.OrganizationID, f.project.ID, f.issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	scope := independentFailureScope(t, f.nativeFixture)
	if plan, err := f.service.prepareIndependentRepairs(t.Context(), scope, f.issue.WorkItemID, review.IndependentFailures(), change.ID, review.CreatedAt); err != nil || plan == nil || plan.refusal != "Canonical repair identity coverage is pending" || len(plan.dependencies) != 0 {
		t.Fatalf("uninitialized canonical ownership plan=%+v error=%v", plan, err)
	}
	if more, err := f.service.refreshNativeIdentity(t.Context()); err != nil || !more {
		t.Fatalf("existing issue origin initialization more=%v error=%v", more, err)
	}
	if err := independentReview(t, f, change, &review, false); err != nil {
		t.Fatal(err)
	}
	issue := independentIssue(t, f.nativeFixture, f.issue.WorkItemID)
	if len(issue.Dependencies) != 1 || issue.DependencyReady || issue.State == "Blocked" {
		t.Fatalf("independent failure did not wait on one repair: %+v", issue)
	}
}

func TestIndependentFailureRepairRouting(t *testing.T) {
	for _, test := range []struct {
		name, attribution, role                 string
		existing, cycle, self, ambiguous, batch bool
		wantRepair                              bool
	}{
		{name: "validator creates owner", attribution: "independent", role: "validator", wantRepair: true},
		{name: "review reuses owner", attribution: "independent", role: "review", existing: true, wantRepair: true},
		{name: "audit canonical duplicate", attribution: "independent", role: "audit", existing: true, ambiguous: true, wantRepair: true},
		{name: "change goes to rework", attribution: "change", role: "validator"},
		{name: "unknown goes to rework", attribution: "unknown", role: "validator"},
		{name: "mixed goes to rework", attribution: "mixed", role: "validator"},
		{name: "self refused", attribution: "independent", role: "validator", self: true},
		{name: "cycle refused", attribution: "independent", role: "validator", existing: true, cycle: true},
		{name: "batch cycle refuses every repair", attribution: "independent", role: "validator", existing: true, cycle: true, batch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := independentFailureFixture(t)
			ctx := f.service.runDetailContext(t.Context())
			version := f.publish(t, "version", "")
			change := f.detail(t).Change
			scope := independentFailureScope(t, f.nativeFixture)
			now := f.service.config.now()
			review := independentRepairReview(version, now)
			if test.attribution == "change" {
				review.Validator.Commands[0].BaseReproduction.ExitCode = 0
				review.Validator.Commands[0].BaseReproduction.Output = ""
			}
			if test.attribution == "unknown" {
				review.Validator.Commands[0].BaseReproduction.Complete = false
			}
			if test.attribution == "mixed" {
				review.Validator.Findings = append(review.Validator.Findings, gate.Finding{Severity: "p1", Body: "Unproven finding"})
			}
			if test.batch {
				other := independentRepairReview(version, now).Validator
				command := other.Commands[0]
				base := *command.BaseReproduction
				command.Command, command.Output = "check-other", "other.go:9:2: invalid other input"
				command.Findings, command.Attribution = nil, ""
				base.Command, base.Output = command.Command, command.Output
				command.BaseReproduction = &base
				review.Validator.Commands = append([]gate.CommandResult{command}, review.Validator.Commands...)
				review.Validator.Findings = append([]gate.Finding{{Severity: "p1", Body: command.Output, CheckCommand: command.Command, CheckDiagnostic: command.Output}}, review.Validator.Findings...)
			}
			gate.NormalizeAttribution(review.Validator)
			if test.role == "review" {
				review.ImplementationReview, review.Validator = review.Validator, nil
			}
			if test.role == "audit" {
				result := review.Validator
				review.SecurityAudit = &tracker.SecurityAuditReview{Verdict: "rework", Run: securityaudit.Run{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Findings: []securityaudit.Finding{{ID: "input", Severity: "p1", Body: result.Findings[0].Body, CheckCommand: result.Findings[0].CheckCommand, CheckDiagnostic: result.Findings[0].CheckDiagnostic}}, Checks: []securityaudit.CheckComparison{{Head: gate.CommandObservation(result.Commands[0]), Base: *result.Commands[0].BaseReproduction}}}}
				review.SecurityAudit.Run.Findings = append(review.SecurityAudit.Run.Findings, securityaudit.Finding{ID: "resolved", Status: "resolved", Severity: "p1", Body: "Resolved historical finding"})
				securityaudit.NormalizeAttribution(&review.SecurityAudit.Run)
				review.Validator = nil
			}
			independentMove(t, f.nativeFixture, f.issue.WorkItemID, "In Progress")
			var owner tracker.NativeIssue
			if test.existing || test.self {
				failures := review.IndependentFailures()
				if len(failures) == 0 {
					t.Fatalf("review carries no independent failure: %+v", review)
				}
				request := independentRepairRequest(failures[len(failures)-1], "existing")
				if test.self {
					current := independentIssue(t, f.nativeFixture, f.issue.WorkItemID)
					response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(current.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "self-origin"}, ExpectedRevision: current.Revision, Body: &request.Body})
					requireNativeStatus(t, response, http.StatusOK)
				} else {
					owner = independentCreateOwner(t, f.nativeFixture, request)
					if test.ambiguous {
						owner = independentCreateOwner(t, f.nativeFixture, request)
					}
					if test.cycle {
						response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(owner.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "owner-cycle"}, ExpectedRevision: owner.Revision, RelatedWorkItemID: f.issue.WorkItemID, Operation: "add"})
						requireNativeStatus(t, response, http.StatusOK)
					}
				}
			}
			for {
				more, err := f.service.refreshNativeIdentity(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if !more {
					break
				}
			}
			beforeCount := independentIssueCount(t, f.nativeFixture)
			if err := independentReview(t, f, change, &review, true); err != nil {
				t.Fatal(err)
			}
			issue := independentIssue(t, f.nativeFixture, f.issue.WorkItemID)
			if issue.State == "Blocked" || issue.State == "In Review" {
				t.Fatalf("failure left dispatchable lane: %s", issue.State)
			}
			if (len(issue.Dependencies) == 1) != test.wantRepair || issue.DependencyReady == test.wantRepair {
				t.Fatalf("dependencies=%v ready=%v", issue.Dependencies, issue.DependencyReady)
			}
			if test.wantRepair && test.existing && issue.Dependencies[0] != owner.WorkItemID {
				t.Fatalf("canonical owner=%s want=%s", issue.Dependencies[0], owner.WorkItemID)
			}
			if (review.RepairRefusal != "") != (test.self || test.cycle) {
				t.Fatalf("refusal=%q", review.RepairRefusal)
			}
			if (test.self || test.cycle) && independentIssueCount(t, f.nativeFixture) != beforeCount {
				t.Fatal("refused batch created a repair owner")
			}
			if test.cycle {
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					comments, err := r.Comments(t.Context(), scope.Project, owner.WorkItemID, 0, 10)
					if err == nil && len(comments) != 0 {
						t.Fatalf("refused batch left repair occurrence=%+v", comments)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			detail := independentDetail(t, f)
			if len(detail.Reviews) != 1 || detail.Reviews[0].Body != review.Body || len(detail.Reviews[0].RepairDependencies) != len(review.RepairDependencies) {
				t.Fatal("outcome lost repair evidence")
			}
			if test.wantRepair {
				repair := independentIssue(t, f.nativeFixture, issue.Dependencies[0])
				origin, stamped := issueorigin.Parse(repair.Body)
				if !stamped || origin.Fingerprint == "" || repair.State != "Todo" {
					t.Fatalf("repair provenance or lane lost: %+v", repair)
				}
				candidates := func() []claimstore.Candidate {
					t.Helper()
					var page claimstore.CandidatePage
					if err := f.service.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						page, err = r.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: scope.Project, WorkItemID: issue.WorkItemID, AvailableAt: now, Limit: 10})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return page.Items
				}
				if got := candidates(); len(got) != 0 {
					t.Fatalf("repair wait dispatch=%v", got)
				}
				independentMove(t, f.nativeFixture, repair.WorkItemID, "Done")
				if got := candidates(); len(got) != 1 {
					t.Fatalf("completed repair did not resume=%v", got)
				}
			}
		})
	}
}

func TestIndependentFailureConcurrentRepairOwner(t *testing.T) {
	f := independentFailureFixture(t)
	version := f.publish(t, "version", "")
	first := f.detail(t).Change
	second := f.createIndependent(t, "another change")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(second.WorkItemID)+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "second"}, Title: "Another change"})
	requireNativeStatus(t, response, http.StatusOK)
	var other tracker.ChangeRequest
	decodeHubResponse(t, response, &other)
	other.CurrentVersion = version.ID
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, change := range []tracker.ChangeRequest{first, other} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			review := independentRepairReview(version, f.service.config.now())
			results <- independentReview(t, f, change, &review, false)
		}()
	}
	close(start)
	firstErr, secondErr := <-results, <-results
	workers.Wait()
	if firstErr != nil || secondErr != nil {
		t.Fatalf("concurrent repair errors=%v %v", firstErr, secondErr)
	}
	one := independentIssue(t, f.nativeFixture, f.issue.WorkItemID)
	two := independentIssue(t, f.nativeFixture, second.WorkItemID)
	if len(one.Dependencies) != 1 || len(two.Dependencies) != 1 || one.Dependencies[0] != two.Dependencies[0] {
		t.Fatalf("concurrent repairs differ: %v %v", one.Dependencies, two.Dependencies)
	}
	if count := independentIssueCount(t, f.nativeFixture); count != 3 {
		t.Fatalf("duplicate repair filed: issues=%d", count)
	}
}

func TestIndependentFailureGateAttemptReplay(t *testing.T) {
	for _, test := range []struct {
		name, outcome string
		pending       bool
	}{{name: "failed", outcome: "failed"}, {name: "cancelled", outcome: "cancelled"}, {name: "pending canonical identities", outcome: "failed", pending: true}} {
		t.Run(test.name, func(t *testing.T) {
			outcome := test.outcome
			now := time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
			ctx := f.service.runDetailContext(t.Context())
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "source failure")
			worker := f.worker(t, "worker")
			lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			now = now.Add(time.Second)
			baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
			result := independentRepairReview(tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: baseSHA, HeadSHA: headSHA}}, now).Validator.Commands[0]
			result.Stage, result.TreeSHA, result.DurationNS, result.StartedAt = gate.StageSourceFinalization, strings.Repeat("c", 40), int64(time.Second), now.Add(-time.Second)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "source-checkpoint", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.HeadSHA = headSHA
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			if test.pending {
				unrelated := f.create(t, "uninitialized unrelated issue")
				if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE issues SET doctor_identity_revision=NULL WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", f.project.OrganizationID, f.project.ID, unrelated.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "failed-gate", 3, outcome
			event.Data.Handoff = nil
			event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now, Validation: &result}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			event.IdempotencyKey = "same-event-replayed"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			credential, _, err := f.service.authenticateTenantAPIHash(ctx, f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
			attempt, err := f.service.readNativeAttemptTenant(ctx, scope, string(issue.WorkItemID), event.Data.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "cancelled" {
				if attempt.Runtime == nil || len(attempt.Runtime.RepairDependencies) != 0 {
					t.Fatal("cancelled execution filed a repair")
				}
				return
			}
			if test.pending {
				if attempt.Outcome != "failed" || attempt.Runtime == nil || attempt.Runtime.Validation == nil || attempt.Runtime.Validation.Output != result.Output || len(attempt.Runtime.RepairDependencies) != 0 || independentIssueCount(t, f) != 2 {
					t.Fatalf("pending canonical identities lost failed outcome or filed a repair: %+v", attempt)
				}
			} else if attempt.Runtime == nil || len(attempt.Runtime.RepairDependencies) != 1 {
				t.Fatalf("gate outcome lost repair binding: %+v", attempt.Runtime)
			}
			count := independentIssueCount(t, f)
			if count != 2 {
				t.Fatalf("replay created duplicate repair: %d", count)
			}
			event.Data.Sequence, event.IdempotencyKey = 4, "usage-correction"
			event.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "test-model", Input: 10, Output: 1, Currency: "USD", Revision: 1, From: now.Add(-time.Second), To: now, ReportedAt: now}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			corrected, err := f.service.readNativeAttemptTenant(ctx, scope, string(issue.WorkItemID), event.Data.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if corrected.Outcome != "failed" || corrected.Runtime == nil || corrected.Runtime.Validation == nil || corrected.Runtime.Validation.Output != result.Output || !slices.Equal(corrected.Runtime.RepairDependencies, attempt.Runtime.RepairDependencies) {
				t.Fatal("usage correction erased repair binding")
			}
		})
	}
}
