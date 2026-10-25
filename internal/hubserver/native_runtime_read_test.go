package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runtimeReadQuery struct {
	nativeQueryer
	statements []string
	arguments  [][]any
}

func (q *runtimeReadQuery) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	q.statements = append(q.statements, statement)
	q.arguments = append(q.arguments, args)
	return q.nativeQueryer.QueryContext(ctx, statement, args...)
}

func (q *runtimeReadQuery) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	q.statements = append(q.statements, statement)
	q.arguments = append(q.arguments, args)
	return q.nativeQueryer.QueryRowContext(ctx, statement, args...)
}

func TestNativeRuntimeReadHistoryCost(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "current", "")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	db := f.service.database.db
	path := f.path + "/versions/" + version.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-current"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, changeTestResult(version)), http.StatusOK)
	now := f.service.config.now().Add(time.Second)
	f.service.config.now = func() time.Time { return now }
	registered := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	registered.enroll(t)
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	mergeAttempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(registered.binding.MachineID), "succeeded", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now}}, now, -1)
	receipt := tracker.NativeLandingReceipt{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, FromState: "Merging", TargetState: "In Progress", RefusalKind: "base_protected", Refusal: "base_protected", ObservedAt: now, Gate: &gate.CommandResult{ReceiptID: "receipt_example", Command: "make check-landing", HeadSHA: version.HeadSHA, TreeSHA: strings.Repeat("d", 40), FinishedAt: now, Output: strings.Repeat("validation output ", 4096)}}
	if err := persistAttemptLanding(t.Context(), tx, mergeAttempt, receipt); err != nil {
		t.Fatal(err)
	}
	finding := "Unresolved review: integration actor overwrites the runner actor"
	issue := f.issue
	issue.State = "In Progress"
	if _, err := persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Merging", ToState: issue.State, Reason: "worker_progress", ReasonDetail: finding}, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Second)
	repair := seedHealthRateAttempt(t, tx, scope, issue, string(registered.binding.MachineID), "running", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Phase: "rework", HeartbeatAt: now}}, now, -1)
	if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.attempt_id',id) WHERE id=?", repair); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE leases SET released_at=NULL WHERE lease_id=(SELECT lease_id FROM native_attempts WHERE id=?)", repair); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	previous, count, explanationCount := 0, 0, 0
	for _, history := range []int{1, 64, 256} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			for i := previous; i < history; i++ {
				decisionTx, err := db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := appendNativeHistory(t.Context(), decisionTx, scope, string(f.issue.WorkItemID), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Reason: "recorded refusal", At: now}}, now); err != nil {
					decisionTx.Rollback()
					t.Fatal(err)
				}
				if err := decisionTx.Commit(); err != nil {
					t.Fatal(err)
				}
				old := version
				old.ID, old.Number = newNativeID("version"), int64(i+1000)
				raw, err := marshalNative(old)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), "INSERT INTO change_versions (id, change_id, number, record_json) VALUES (?, ?, ?, ?)", old.ID, old.ChangeID, old.Number, raw); err != nil {
					t.Fatal(err)
				}
				for kind, record := range map[string]any{
					"review":     tracker.ChangeReview{ID: newNativeID("review"), VersionID: old.ID, Decision: "approved", Body: strings.Repeat("historical review ", 512), Actor: tracker.Actor{PrincipalID: "old-reviewer"}},
					"check":      tracker.ChangeCheck{VersionID: old.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: newNativeID("check"), Conclusion: "failure"}},
					"discussion": tracker.ChangeDiscussion{ID: newNativeID("comment"), VersionID: old.ID, Body: strings.Repeat("historical discussion ", 512)},
				} {
					raw, err := marshalNative(record)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, ?, ?)", old.ChangeID, old.ID, kind, raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			previous = history
			q := &runtimeReadQuery{nativeQueryer: db}
			evidence, err := readNativeRuntime(t.Context(), q, scope, string(f.issue.WorkItemID), "", f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			full := f.detail(t)
			want := full.Summary
			want.Messages = nil
			if !reflect.DeepEqual(evidence.Change.Summary, want) || evidence.Change.Change.CurrentVersion != version.ID || len(full.Versions) != history+1 || len(full.Discussion) != history || evidence.Change.Versions != nil || evidence.Change.Reviews != nil || evidence.Change.Checks != nil || evidence.Change.Discussion != nil {
				t.Fatalf("runtime lost current summary or loaded history: %#v", evidence.Change)
			}
			if count == 0 {
				count = len(q.statements)
			}
			projected := explain.FromNativeEvidence(evidence)
			if evidence.CurrentVersion == nil || evidence.CurrentVersion.VersionID != version.ID || evidence.CurrentVersion.HeadSHA != version.HeadSHA || evidence.CurrentVersion.Landing == nil || !evidence.CurrentVersion.Landing.Gate.OutputTruncated || len(evidence.CurrentVersion.Landing.Gate.Output) > 4096 || projected.RequiredGate.State != explain.GateFailed || projected.RequiredGate.Reason != finding || projected.RequiredGate.ExternalMerge.ObservedAt == nil || !projected.RequiredGate.ExternalMerge.ObservedAt.Equal(receipt.ObservedAt) || projected.RequiredGate.LocalValidation.State != explain.GatePassed || projected.Attempt.NativeID != repair || evidence.CurrentLease == nil || projected.Eligibility.Current.Outcome != "claimed" {
				t.Fatalf("current refusal or claimed repair lost: gate=%#v merge=%#v version=%#v lease=%#v scheduling=%#v now=%s receipt_at=%s change_at=%s", projected.RequiredGate, projected.RequiredGate.ExternalMerge, evidence.CurrentVersion, evidence.CurrentLease, evidence.Scheduling, now.Format(time.RFC3339Nano), receipt.ObservedAt.Format(time.RFC3339Nano), evidence.Change.Change.UpdatedAt.Format(time.RFC3339Nano))
			}
			if len(q.statements) != count {
				t.Fatalf("runtime query count grew with history: %d -> %d", count, len(q.statements))
			}
			for i, statement := range q.statements {
				if !strings.Contains(statement, "FROM change_evidence") && !strings.Contains(statement, "FROM change_versions") {
					continue
				}
				rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, q.arguments[i]...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var plans []string
				for rows.Next() {
					var id, parent, unused int
					var plan string
					if err := rows.Scan(&id, &parent, &unused, &plan); err != nil {
						t.Fatal(err)
					}
					plans = append(plans, plan)
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					t.Fatal(err)
				}
				index := "change_evidence_current_idx"
				if strings.Contains(statement, "FROM change_versions") {
					index = "sqlite_autoindex_change_versions_1 (id=?)"
				} else if strings.Contains(statement, "SELECT EXISTS") {
					index = "change_evidence_approval_idx"
				}
				if !strings.Contains(strings.Join(plans, " "), index) {
					t.Fatalf("evidence query scans history: %v", plans)
				}
			}
			explanationQuery := &runtimeReadQuery{nativeQueryer: f.service.database.reader}
			ctx := context.WithValue(t.Context(), explanationReadKey{}, true)
			current, err := readNativeRuntime(ctx, explanationQuery, scope, string(f.issue.WorkItemID), "", now)
			if err != nil {
				t.Fatal(err)
			}
			if explanationCount == 0 {
				explanationCount = len(explanationQuery.statements)
			}
			result := explain.FromNativeEvidence(current)
			if len(explanationQuery.statements) != explanationCount || result.RequiredGate.State != projected.RequiredGate.State || result.RequiredGate.Reason != finding || result.Attempt.NativeID != repair || current.LatestDecision == nil || current.NativeCandidateExclusion == nil || current.NativeCandidateExclusion.Data.Decision.Reason != "recorded refusal" {
				t.Fatalf("explanation lost current evidence or grew with history: queries=%d gate=%+v", len(explanationQuery.statements), result.RequiredGate)
			}
			t.Logf("explanation history=%d queries=%d latest_decision=%s", history, len(explanationQuery.statements), current.LatestDecision.ID)
			t.Logf("%d historical versions: %d runtime queries; indexed current evidence", history, len(q.statements))
		})
	}
	now = now.Add(time.Minute)
	newVersion := f.publish(t, "superseding", version.ID)
	evidence, err := readNativeRuntime(t.Context(), db, scope, string(f.issue.WorkItemID), "", now)
	if err != nil {
		t.Fatal(err)
	}
	projected := explain.FromNativeEvidence(evidence)
	if evidence.CurrentVersion.VersionID != newVersion.ID || evidence.CurrentVersion.Landing != nil || projected.RequiredGate.State == explain.GateFailed || projected.RequiredGate.State == explain.GatePassed || len(projected.Reasons) != 0 || projected.RequiredGate.ExternalMerge.State != explain.GateUnknown {
		t.Fatalf("superseding publication retained an old blocker: %#v", projected)
	}
	for _, payload := range []string{"dependencies", "validation", "retained_fields", "failure_signature", "gate_identity"} {
		t.Run(payload, func(t *testing.T) {
			oversized := evidence
			current := *evidence.CurrentVersion
			oversized.CurrentVersion = &current
			var failure *explain.FailureSignature
			switch payload {
			case "dependencies":
				for runner := range 16 {
					admission := tracker.NativeRuntimeAdmission{RunnerID: "runner_" + strconv.Itoa(runner), Outcome: "skipped"}
					for dependency := range 100 {
						admission.UnresolvedDependencies = append(admission.UnresolvedDependencies, tracker.NativeDependency{ID: tracker.NativeWorkItemID("wi_" + strconv.Itoa(dependency)), Title: strings.Repeat("example ", 256)})
					}
					oversized.Admission = append(oversized.Admission, admission)
				}
			case "validation":
				current.Validation = &gate.CommandResult{ReceiptID: "validation_receipt", Command: "make check-landing", HeadSHA: current.HeadSHA, TreeSHA: strings.Repeat("c", 40), ExitCode: 1, Pipeline: &gate.PipelineEvidence{}, Evidence: &gate.CommandEvidence{}}
				for range 1024 {
					current.Validation.Pipeline.Timings = append(current.Validation.Pipeline.Timings, gate.PipelineTiming{ReceiptID: "timing_" + strings.Repeat("a", 32), Stage: "landing_validation", Command: "go test " + strings.Repeat("a", 240), HeadSHA: current.HeadSHA, TreeSHA: strings.Repeat("c", 40), StartedAt: now, FinishedAt: now.Add(time.Second)})
					if len(current.Validation.Evidence.Checks) < 32 {
						current.Validation.Evidence.Checks = append(current.Validation.Evidence.Checks, gate.CheckObservation{Scope: "unit", Command: "go test " + strings.Repeat("a", 240), HeadSHA: current.HeadSHA, TreeSHA: strings.Repeat("c", 40), StartedAt: now, FinishedAt: now.Add(time.Second)})
					}
				}
				if !current.Validation.Pipeline.Valid() || !current.Validation.Evidence.Valid() {
					t.Fatal("invalid oversized validation fixture")
				}
				raw, err := json.Marshal(current.Validation)
				if err != nil || len(raw) <= 256*1024 {
					t.Fatalf("validation fixture does not exceed response budget: %d %v", len(raw), err)
				}

			case "retained_fields":
				oversized.Scheduling.Reason = strings.Repeat("large retained reason ", 32768)
			case "failure_signature":
				failure = &explain.FailureSignature{Signature: strings.Repeat("failure ", 65536), Count: 1}
			case "gate_identity":
				current.Validation = &gate.CommandResult{ReceiptID: strings.Repeat("identity", 65536)}
			}
			bounded, err := boundedNativeExplanation(oversized, failure)
			if payload == "gate_identity" {
				if err == nil {
					t.Fatal("oversized retained gate identity escaped final budget")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(bounded)
			wantGate := explain.FromNativeEvidence(oversized).RequiredGate
			if err != nil || len(encoded) > 256*1024 || bounded.NativeRuntime == nil || bounded.Identity.IssueID != string(f.issue.WorkItemID) || bounded.RequiredGate.State != wantGate.State || bounded.RequiredGate.EvidenceID != wantGate.EvidenceID || !reflect.DeepEqual(bounded.RequiredGate.LocalValidation, wantGate.LocalValidation) {
				t.Fatalf("response bound lost current fields: bytes=%d error=%v", len(encoded), err)
			}
			if payload == "validation" {
				validation := bounded.NativeRuntime.CurrentVersion.Validation
				if validation.Pipeline != nil || validation.Evidence != nil || !slices.Contains(bounded.NativeRuntime.Unavailable, "current_version_validation_evidence_omitted") || current.Validation.Pipeline == nil {
					t.Fatal("validation compaction lost omission marker or mutated source")
				}
			} else if !slices.Contains(bounded.NativeRuntime.Unavailable, "explanation_evidence_truncated") {
				t.Fatal("missing response omission marker")
			}
		})
	}

}

func TestNativeExplanationIncompleteGateHistory(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"approved", "changes_requested", "checks"} {
		t.Run(decision, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			if decision == "checks" {
				check := f.rules.RequiredChecks[0]
				f.rules.RequiredChecks = nil
				for i := range 65 {
					next := check
					next.Name = "test-" + strconv.Itoa(i)
					f.rules.RequiredChecks = append(f.rules.RequiredChecks, next)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "many-checks"}, ExpectedID: f.rules.ID, Policy: f.rules})
				requireNativeStatus(t, response, http.StatusOK)
			}
			version := f.publish(t, "current", "")
			review := decision
			if decision == "checks" {
				review = "approved"
			}
			path := f.path + "/versions/" + version.ID
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "effective-review"}, Decision: review}), http.StatusOK)
			for i, check := range version.Checks {
				result := changeTestResult(version)
				result.IdempotencyKey, result.CheckRunID = "check-"+strconv.Itoa(i), check.CheckRunID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, result), http.StatusOK)
			}
			if decision != "checks" {
				for i := range 65 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "comment-" + strconv.Itoa(i)}, Decision: "commented", Body: "Still discussing"}), http.StatusOK)
				}
			}
			full := f.detail(t)
			if full.Summary.NativeReview != review || full.Summary.Checks != "success" {
				t.Fatalf("invalid complete fixture: %+v", full.Summary)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			ctx := context.WithValue(t.Context(), explanationReadKey{}, true)
			evidence, err := readNativeRuntime(ctx, f.service.database.reader, scope, string(f.issue.WorkItemID), "", f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			if evidence.Change.Summary.Status != "evidence_incomplete" || !slices.Contains(evidence.Unavailable, "change_gate_history_truncated") {
				t.Fatalf("history falsely complete: %+v", evidence.Change.Summary)
			}
			for _, oversized := range []bool{false, true} {
				if oversized {
					evidence.Scheduling.Reason = strings.Repeat("oversized reason ", 32768)
				}
				result, err := boundedNativeExplanation(evidence, nil)
				if err != nil {
					t.Fatal(err)
				}
				if result.RequiredGate.State != explain.GateUnknown || result.RequiredGate.SourceState != explain.SourceUnavailable || result.RequiredGate.EvidenceID != version.ID || !slices.Contains(result.NativeRuntime.Unavailable, "change_gate_history_truncated") {
					t.Fatalf("truncated history became complete gate: %+v", result.RequiredGate)
				}
			}
		})
	}
}

func TestNativeRuntimeReadSharedCapacityCost(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "runtime-capacity")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "selected runtime")
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	first.enroll(t)
	report := capacityReport(now)
	report.MaxConcurrent = 8
	publishCapacity(t, f, first, report)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", first.redemption.Credential, providerClaim(first, issue, "reserved"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	previous := 1
	for _, total := range []int{1, 8, 100, 101} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			for i := previous; i < total; i++ {
				sharedRunner(t, first)
			}
			previous = total
			raw, err := marshalNative([]any{report})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET provider_reports_json = ? WHERE organization_id = ?", raw, scope.organization); err != nil {
				t.Fatal(err)
			}
			q := &runtimeReadQuery{nativeQueryer: f.service.database.db}
			runners, truncated, err := readRuntimeRunners(t.Context(), q, scope, now)
			if err != nil || len(runners) != min(total, 100) || truncated != (total > 100) || len(q.statements) != 4 {
				t.Fatalf("capacity projection runners=%d truncated=%v queries=%d error=%v", len(runners), truncated, len(q.statements), err)
			}
			for i, r := range runners {
				wantUsed := 0
				if r.RunnerID == first.binding.RunnerID {
					wantUsed = 1
				}
				if r.HostUsed != 1 || r.Used != wantUsed || len(r.ProviderCapacity) != 1 || r.ProviderCapacity[0].Used != 1 || r.ProviderCapacity[0].MaxConcurrent != 8 {
					t.Fatalf("shared occupancy lost: %#v", r)
				}
				if i == 0 || i == len(runners)-1 || total <= 8 {
					full, err := readRunner(t.Context(), f.service.database.db, scope.organization, r.RunnerID, now)
					if err != nil || r.Health != full.Health || !reflect.DeepEqual(r.ProviderCapacity, full.ProviderCapacity) || !reflect.DeepEqual(r.Exclusions(scope.project, policy.Requirements{}, false), full.Exclusions(scope.project, policy.Requirements{}, false)) {
						t.Fatalf("projection differs from routing owner: %#v, %#v, %v", r, full, err)
					}
				}
			}
			evidence, err := readNativeRuntime(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), "", now)
			if err != nil || slices.Contains(evidence.Unavailable, "capacity_projection_truncated") != truncated || evidence.CurrentLease == nil || evidence.CurrentLease.ID != lease.ID {
				t.Fatalf("runtime truncation or fenced lease lost: unavailable=%v lease=%#v error=%v", evidence.Unavailable, evidence.CurrentLease, err)
			}
			if total == 101 {
				currentScope := scope
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM runner_identities WHERE organization_id=? ORDER BY id DESC LIMIT 1", scope.organization).Scan(&currentScope.credential.Runner.RunnerID); err != nil {
					t.Fatal(err)
				}
				currentQuery := &runtimeReadQuery{nativeQueryer: f.service.database.db}
				current, truncated, err := readRuntimeRunners(t.Context(), currentQuery, currentScope, now)
				if err != nil || !truncated || len(current) != 100 || current[0].RunnerID != currentScope.credential.Runner.RunnerID || len(currentQuery.statements) != 4 {
					t.Fatalf("bounded runtime projection dropped current registered runner: runners=%d truncated=%v queries=%d error=%v", len(current), truncated, len(currentQuery.statements), err)
				}
			}
			bounded := &runtimeReadQuery{nativeQueryer: f.service.database.reader}
			explanationRunners, partial, err := readRuntimeRunners(context.WithValue(t.Context(), explanationReadKey{}, true), bounded, scope, now)
			if err != nil || len(explanationRunners) != min(total, 16) || partial != (total > 16) || len(bounded.statements) != 5 {
				t.Fatalf("unbounded explanation capacity: runners=%d partial=%v queries=%d error=%v", len(explanationRunners), partial, len(bounded.statements), err)
			}
			landingQuery := &runtimeReadQuery{nativeQueryer: f.service.database.reader}
			landingEvidence := tracker.NativeRuntimeEvidence{Issue: tracker.NativeIssue{State: "Merging"}}
			landingReady, landingCapacity, err := readAdmissionLandingCapacity(context.WithValue(t.Context(), explanationReadKey{}, true), landingQuery, scope, 0, &landingEvidence, now)
			if err != nil || landingReady != (total <= 64) || total > 64 && landingCapacity != nativeLandingCapacityUnknown {
				t.Fatalf("landing capacity lost bounded current facts: ready=%v capacity=%d error=%v", landingReady, landingCapacity, err)
			}
			if total <= 64 {
				rows, err := f.service.database.reader.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+landingQuery.statements[1], landingQuery.arguments[1]...)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var id, parent, unused int
					var plan string
					if err := rows.Scan(&id, &parent, &unused, &plan); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(plan, "SCAN i") || strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
						t.Fatalf("landing capacity scans project history: %s", plan)
					}
				}
				if err := errors.Join(rows.Err(), rows.Close()); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%d runners sharing a host/provider: %d capacity queries", total, len(q.statements))
		})
	}
	for _, sparse := range []string{"ungranted", "removed"} {
		t.Run(sparse, func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			currentScope := scope
			if err := tx.QueryRowContext(t.Context(), "SELECT id FROM runner_identities WHERE organization_id=? ORDER BY id DESC LIMIT 1", scope.organization).Scan(&currentScope.credential.Runner.RunnerID); err != nil {
				t.Fatal(err)
			}
			if sparse == "ungranted" {
				_, err = tx.ExecContext(t.Context(), "DELETE FROM token_grants WHERE organization_id=? AND token_id IN (SELECT token_id FROM runner_identities WHERE organization_id=? AND id<>?)", scope.organization, scope.organization, currentScope.credential.Runner.RunnerID)
			} else {
				_, err = tx.ExecContext(t.Context(), "UPDATE runner_identities SET removed_at=? WHERE organization_id=? AND id<>?", formatHubTime(now), scope.organization, currentScope.credential.Runner.RunnerID)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), explanationReadKey{}, true)
			candidates, partial, err := readRuntimeRunners(ctx, tx, scope, now)
			if err != nil || !partial || len(candidates) != 0 {
				t.Fatalf("candidate search escaped sparse owner window: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
			candidates, partial, err = readRuntimeRunners(ctx, tx, currentScope, now)
			if err != nil || !partial || len(candidates) != 1 || candidates[0].RunnerID != currentScope.credential.Runner.RunnerID {
				t.Fatalf("selected runner lost priority: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
			if _, err := tx.ExecContext(t.Context(), "DELETE FROM token_grants WHERE organization_id=? AND token_id=(SELECT token_id FROM runner_identities WHERE id=?)", scope.organization, currentScope.credential.Runner.RunnerID); err != nil {
				t.Fatal(err)
			}
			candidates, partial, err = readRuntimeRunners(ctx, tx, currentScope, now)
			if err != nil || !partial || len(candidates) != 0 {
				t.Fatalf("selected runner bypassed grants: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
		})
	}

	foreignOrganization := tracker.OrganizationID(newNativeID("org"))
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)", foreignOrganization, "Foreign", formatHubTime(now)); err != nil {
		t.Fatal(err)
	}
	foreign := newNativeFixture(t, f.service, foreignOrganization, "runtime-foreign")
	runners, _, err := readRuntimeRunners(t.Context(), f.service.database.db, nativeScope{organization: foreign.project.OrganizationID, project: foreign.project.ID}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("cross-organization capacity leaked: %#v %v", runners, err)
	}
	other := newNativeFixture(t, f.service, scope.organization, "runtime-ungranted")
	runners, _, err = readRuntimeRunners(t.Context(), f.service.database.db, nativeScope{organization: scope.organization, project: other.project.ID}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("ungranted project capacity leaked: %#v %v", runners, err)
	}
	q := &runtimeReadQuery{nativeQueryer: f.service.database.db}
	runners, _, err = readRuntimeRunners(t.Context(), q, scope, now.Add(91*time.Second))
	if err != nil || len(q.statements) != 4 {
		t.Fatal(err)
	}
	for _, r := range runners {
		if r.HostUsed != 0 || r.Used != 0 || r.ProviderCapacity[0].Used != 0 {
			t.Fatalf("expired reservation remains occupied: %#v", r)
		}
	}
	evidence, err := readNativeRuntime(t.Context(), f.service.database.db, scope, string(issue.WorkItemID), "", now.Add(91*time.Second))
	if err != nil || evidence.Scheduling.Outcome != "unknown" || !slices.Contains(evidence.Unavailable, "capacity_projection_truncated") {
		t.Fatalf("truncated exclusions became a complete scheduling decision: %#v %v", evidence.Scheduling, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id = (SELECT token_id FROM runner_identities WHERE id = ?) AND project_id = ?", first.binding.RunnerID, scope.project); err != nil {
		t.Fatal(err)
	}
	runners, truncated, err := readRuntimeRunners(t.Context(), f.service.database.db, scope, now)
	if err != nil || truncated || len(runners) != 100 {
		t.Fatalf("project grant removal did not restrict the page: runners=%d truncated=%v error=%v", len(runners), truncated, err)
	}
	for _, r := range runners {
		if r.RunnerID == first.binding.RunnerID || r.HostUsed != 1 || r.Used != 0 || r.ProviderCapacity[0].Used != 1 {
			t.Fatalf("occupancy outside the granted runner page was lost: %#v", r)
		}
	}
}
