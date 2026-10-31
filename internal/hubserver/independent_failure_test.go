package hubserver

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func independentFailureFixture(t *testing.T) changeFixture {
	t.Helper()
	now := time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	return newChangeFixture(t, service)
}

func independentRepairReview(version tracker.ChangeVersion, now time.Time) tracker.ChangeReview {
	diagnostic := "source.go:4:2: invalid input"
	base := failureattribution.Observation{Command: "check-input", HeadSHA: version.BaseSHA, ExitCode: 1, Output: diagnostic, Complete: true}
	result := &gate.ValidatorResult{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Verdict: "rework", Findings: []gate.Finding{{Severity: "p1", Body: diagnostic, CheckCommand: base.Command, CheckDiagnostic: diagnostic}}, Commands: []gate.CommandResult{{Command: base.Command, HeadSHA: version.HeadSHA, ExitCode: 1, Output: diagnostic, FinishedAt: now, BaseReproduction: &base}}}
	gate.NormalizeAttribution(result)
	return tracker.ChangeReview{ID: newNativeID("review"), VersionID: version.ID, Decision: "changes_requested", Body: "Independent source failure", Validator: result, CreatedAt: now}
}

func TestIndependentFailureRepairDependency(t *testing.T) {
	f := independentFailureFixture(t)
	ctx := f.service.runDetailContext(t.Context())
	version := f.publish(t, "version", "")
	f.change = f.detail(t).Change
	credential, _, err := f.service.authenticateAPIToken(ctx, f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	review := independentRepairReview(version, f.service.config.now())
	if err := f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
		return returnChangeForRework(ctx, tx, scope, f.change, review, now)
	}); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	if len(issue.Dependencies) != 1 || issue.DependencyReady || issue.State == "Blocked" {
		t.Fatalf("independent failure did not wait on one repair: %+v", issue)
	}
}

func TestIndependentFailureRepairRouting(t *testing.T) {
	for _, test := range []struct {
		name, attribution, role          string
		existing, cycle, self, ambiguous bool
		wantRepair                       bool
	}{
		{name: "validator creates owner", attribution: "independent", role: "validator", wantRepair: true},
		{name: "review reuses owner", attribution: "independent", role: "review", existing: true, wantRepair: true},
		{name: "audit canonical duplicate", attribution: "independent", role: "audit", existing: true, ambiguous: true, wantRepair: true},
		{name: "change goes to rework", attribution: "change", role: "validator"},
		{name: "unknown goes to rework", attribution: "unknown", role: "validator"},
		{name: "mixed goes to rework", attribution: "mixed", role: "validator"},
		{name: "self refused", attribution: "independent", role: "validator", self: true},
		{name: "cycle refused", attribution: "independent", role: "validator", existing: true, cycle: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := independentFailureFixture(t)
			ctx := f.service.runDetailContext(t.Context())
			version := f.publish(t, "version", "")
			change := f.detail(t).Change
			credential, _, err := f.service.authenticateAPIToken(ctx, f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
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
			var owner tracker.NativeIssue
			err = f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
				issue, _, err := readNativeIssue(ctx, tx, scope, string(f.issue.WorkItemID))
				if err != nil {
					return err
				}
				issue.State = "In Progress"
				if issue, err = persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress", Reason: "user_requested"}, now); err != nil {
					return err
				}
				if test.existing || test.self {
					request := independentRepairRequest(review.IndependentFailures()[0], "existing")
					if test.self {
						issue.Body = request.Body
						_, err = persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: []string{"body"}}, now)
						return err
					}
					owner, err = createNativeIssue(ctx, tx, scope, request, now, true)
					if err != nil {
						return err
					}
					if test.ambiguous {
						owner, err = createNativeIssue(ctx, tx, scope, request, now, true)
						if err != nil {
							return err
						}
					}
					if test.cycle {
						_, err = changeNativeDependencyTx(ctx, tx, scope, string(owner.WorkItemID), tracker.DependencyMutation{ExpectedRevision: owner.Revision, RelatedWorkItemID: f.issue.WorkItemID, Operation: "add"}, now)
					}
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			err = f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
				if err := routeIndependentReview(ctx, tx, scope, change, &review, now); err != nil {
					return err
				}
				if err := storeChangeReview(ctx, tx, string(scope.organization), change.ID, review); err != nil {
					return err
				}
				return returnChangeForRework(ctx, tx, scope, change, review, now)
			})
			if err != nil {
				t.Fatal(err)
			}
			issue, _, err := readNativeIssue(ctx, f.service.database.reader, scope, string(f.issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
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
			detail := f.detail(t)
			if len(detail.Reviews) != 1 || detail.Reviews[0].Body != review.Body || len(detail.Reviews[0].RepairDependencies) != len(review.RepairDependencies) {
				t.Fatal("outcome lost repair evidence")
			}
			if test.wantRepair {
				repair, _, err := readNativeIssue(ctx, f.service.database.reader, scope, string(issue.Dependencies[0]))
				if err != nil {
					t.Fatal(err)
				}
				origin, stamped := issueorigin.Parse(repair.Body)
				if !stamped || origin.Fingerprint == "" || repair.State != "Todo" {
					t.Fatalf("repair provenance or lane lost: %+v", repair)
				}
				var itemID tracker.WorkItemID
				if err := f.service.database.reader.QueryRowContext(ctx, "SELECT id FROM issues WHERE native_id=?", issue.WorkItemID).Scan(&itemID); err != nil {
					t.Fatal(err)
				}
				query := claimCandidateQuery{NativeScope: &scope, WorkItemID: itemID, AvailableAt: now}
				candidates, err := nativeCandidateIDs(ctx, f.service.database.reader, query, nil, nil, nil, nil, nil, nil, nil)
				if err != nil || len(candidates) != 0 {
					t.Fatalf("repair wait dispatch=%v err=%v", candidates, err)
				}
				if _, err := f.service.database.db.ExecContext(ctx, "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Done') WHERE native_id=?", scope.project, repair.WorkItemID); err != nil {
					t.Fatal(err)
				}
				candidates, err = nativeCandidateIDs(ctx, f.service.database.reader, query, nil, nil, nil, nil, nil, nil, nil)
				if err != nil || len(candidates) != 1 {
					t.Fatalf("completed repair did not resume=%v err=%v", candidates, err)
				}
			}
		})
	}
}

func TestIndependentFailureConcurrentRepairOwner(t *testing.T) {
	f := independentFailureFixture(t)
	ctx := f.service.runDetailContext(t.Context())
	version := f.publish(t, "version", "")
	first := f.detail(t).Change
	second := f.createIndependent(t, "another change")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(second.WorkItemID)+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "second"}, Title: "Another change"})
	requireNativeStatus(t, response, http.StatusOK)
	var other tracker.ChangeRequest
	decodeHubResponse(t, response, &other)
	other.CurrentVersion = version.ID
	credential, _, err := f.service.authenticateAPIToken(ctx, f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, change := range []tracker.ChangeRequest{first, other} {
		go func() {
			ctx := f.service.freshRunDetailContext(ctx)
			<-start
			review := independentRepairReview(version, f.service.config.now())
			results <- f.service.hubTransact(ctx, func(tx *sql.Tx, now time.Time) error {
				issue, _, err := readNativeIssue(ctx, tx, scope, string(change.WorkItemID))
				if err != nil {
					return err
				}
				_, _, err = attachIndependentRepairs(ctx, tx, scope, issue, review.IndependentFailures(), change.ID, now)
				return err
			})
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	one, _, err := readNativeIssue(ctx, f.service.database.reader, scope, string(f.issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := readNativeIssue(ctx, f.service.database.reader, scope, string(second.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Dependencies) != 1 || len(two.Dependencies) != 1 || one.Dependencies[0] != two.Dependencies[0] {
		t.Fatalf("concurrent repairs differ: %v %v", one.Dependencies, two.Dependencies)
	}
	var count int
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM issues WHERE project_id=?", scope.project).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("duplicate repair filed: issues=%d", count)
	}
}

func TestIndependentFailureGateAttemptReplay(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
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
			baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
			result := independentRepairReview(tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: baseSHA, HeadSHA: headSHA}}, now).Validator.Commands[0]
			result.Stage, result.TreeSHA, result.DurationNS, result.StartedAt = gate.StageSourceFinalization, strings.Repeat("c", 40), int64(time.Second), now.Add(-time.Second)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "source-checkpoint", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.HeadSHA = headSHA
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "failed-gate", 3, outcome
			event.Data.Handoff = nil
			event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now, Validation: &result}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			event.IdempotencyKey = "same-event-replayed"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			credential, _, err := f.service.authenticateAPIToken(ctx, f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
			attempt, err := readNativeAttempt(ctx, f.service.database.reader, scope, string(issue.WorkItemID), event.Data.AttemptID, now)
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "cancelled" {
				if attempt.Runtime == nil || len(attempt.Runtime.RepairDependencies) != 0 {
					t.Fatal("cancelled execution filed a repair")
				}
				return
			}
			if attempt.Runtime == nil || len(attempt.Runtime.RepairDependencies) != 0 {
				t.Fatalf("worker follow-up became a required repair: %+v", attempt.Runtime)
			}
			current, _, err := readNativeIssue(ctx, f.service.database.reader, scope, string(issue.WorkItemID))
			if err != nil || len(current.Dependencies) != 0 || !current.DependencyReady {
				t.Fatalf("worker follow-up stranded originating item: %+v error=%v", current, err)
			}
			var count int
			if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM issues WHERE project_id=?", f.project.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("replay created duplicate repair: %d", count)
			}
			event.Data.Sequence, event.IdempotencyKey = 4, "usage-correction"
			event.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "test-model", Input: 10, Output: 1, Currency: "USD", Revision: 1, From: now.Add(-time.Second), To: now, ReportedAt: now}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			corrected, err := readNativeAttempt(ctx, f.service.database.reader, scope, string(issue.WorkItemID), event.Data.AttemptID, now)
			if err != nil {
				t.Fatal(err)
			}
			if corrected.Runtime == nil || len(corrected.Runtime.RepairDependencies) != 0 {
				t.Fatal("usage correction added a repair dependency")
			}
		})
	}
}
