package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
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

type runtimeHistoryTraceKey struct{}

type runtimeHistoryTraceCapture struct {
	mu         sync.Mutex
	statements []tracedStatement
}

type runtimeHistoryTrace struct{}

func (runtimeHistoryTrace) record(ctx context.Context, query string, args []any) {
	capture, ok := ctx.Value(runtimeHistoryTraceKey{}).(*runtimeHistoryTraceCapture)
	if !ok {
		return
	}
	values := slices.Clone(args)
	for i, value := range values {
		switch value := value.(type) {
		case []byte:
			values[i] = slices.Clone(value)
		case []string:
			values[i] = slices.Clone(value)
		case []int64:
			values[i] = slices.Clone(value)
		case []tracker.NativeWorkItemID:
			values[i] = slices.Clone(value)
		case []tracker.ProjectID:
			values[i] = slices.Clone(value)
		}
	}
	capture.mu.Lock()
	capture.statements = append(capture.statements, tracedStatement{query: query, args: values})
	capture.mu.Unlock()
}

func (trace runtimeHistoryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace.record(ctx, data.SQL, data.Args)
	return ctx
}

func (runtimeHistoryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (runtimeHistoryTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (trace runtimeHistoryTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	trace.record(ctx, data.SQL, data.Args)
}

func (runtimeHistoryTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func TestNativeRuntimeReadHistoryCost(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	admin := usagePostgresAdmin(t, f.service)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	config, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	config.ConnConfig.Tracer = runtimeHistoryTrace{}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	originalTenant, originalAccess, originalPostgres := f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.database.postgres
	t.Cleanup(func() {
		f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.database.postgres = originalTenant, originalAccess, originalPostgres
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.database.postgres = repository, repository, repository
	version := f.publish(t, "current", "")
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	path := f.path + "/versions/" + version.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-current"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, changeTestResult(version)), http.StatusOK)
	now := f.service.config.now().Add(time.Second).UTC().Truncate(time.Microsecond)
	f.service.config.now = func() time.Time { return now }
	registered := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	registered.enroll(t)
	content := eventstore.NewContent(f.service.runDetailStore())
	marshal := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	seedAttempt := func(status string, observation *tracker.NativeRuntimeObservation) string {
		t.Helper()
		id := newNativeID("attempt")
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(t.Context(), scope.project, f.issue.WorkItemID)
			if err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(registered.binding.MachineID), RunnerID: registered.binding.RunnerID, SessionID: id, LeaseID: newNativeID("lease"), TTL: time.Hour})
			if err != nil {
				return err
			}
			if now.Before(lease.RenewedAt) {
				now = lease.RenewedAt.Add(time.Second)
			}
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: scope.organization, ProjectID: scope.project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: now}); err != nil {
				return err
			}
			run := tracker.NativeRunData{AttemptID: id, RunID: id, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: registered.binding.MachineID, RunnerID: registered.binding.RunnerID, SessionID: id, PolicyID: hubTestPolicy().ID, Sequence: 1, Outcome: status, Runtime: observation}
			kind := "run.started"
			if status == "succeeded" {
				kind = "run.finished"
			}
			data := tracker.CollaborationData{Run: &run}
			if observation.Landing != nil {
				data.FromState, data.ToState = observation.Landing.FromState, observation.Landing.TargetState
			}
			event, err := content.PrepareEvent(t.Context(), scope.organization, marshal(tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: f.issue.WorkItemID, Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: string(registered.binding.MachineID)}, RecordedAt: now, Data: data}))
			if err != nil {
				return err
			}
			payload, err := content.PreparePayload(t.Context(), scope.organization, marshal(run))
			if err != nil {
				return err
			}
			if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: scope.organization, ID: "archive_" + payload.SHA256, Reference: payload, CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: id, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: id, Sequence: 1, Status: status, StartedAt: now.Add(-time.Minute), PayloadObjectKey: payload.Key}); err != nil {
				return err
			}
			if err := w.SetAttemptRuntimeHeartbeat(t.Context(), id, 1, &observation.HeartbeatAt); err != nil {
				return err
			}
			if _, err := w.AppendEvent(t.Context(), event); err != nil {
				return err
			}
			if status == "succeeded" {
				_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	receipt := tracker.NativeLandingReceipt{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, FromState: "Merging", TargetState: "In Progress", RefusalKind: "base_protected", Refusal: "base_protected", ObservedAt: now, Gate: &gate.CommandResult{ReceiptID: "receipt_example", Command: "make check-landing", HeadSHA: version.HeadSHA, TreeSHA: strings.Repeat("d", 40), FinishedAt: now, Output: strings.Repeat("validation output ", 4096)}}
	seedAttempt("succeeded", &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now, Landing: &receipt})
	finding := "Unresolved review: integration actor overwrites the runner actor"
	issue := f.issue
	issue.State, issue.Revision, issue.UpdatedAt = "In Progress", issue.Revision+1, now
	issueVersion, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, marshal(issue))
	if err != nil {
		t.Fatal(err)
	}
	transition, err := content.PrepareEvent(t.Context(), scope.organization, marshal(tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "system", PrincipalID: "example-worker"}, RecordedAt: now, Data: tracker.CollaborationData{FromState: "Merging", ToState: issue.State, Reason: "worker_progress", ReasonDetail: finding, Change: &tracker.NativeChangeReference{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), scope.project, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.WorkflowStateName, row.UpdatedAt, row.NativeUpdatedAt = &issue.State, now, now
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(t.Context(), issueVersion); err != nil {
			return err
		}
		_, err = w.AppendEvent(t.Context(), transition)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Second)
	repair := seedAttempt("running", &tracker.NativeRuntimeObservation{Phase: "rework", HeartbeatAt: now})
	if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	readRuntime := func(t *testing.T, ctx context.Context) (tracker.NativeRuntimeEvidence, []tracedStatement) {
		t.Helper()
		capture := &runtimeHistoryTraceCapture{}
		evidence, err := f.service.readNativeRuntime(context.WithValue(ctx, runtimeHistoryTraceKey{}, capture), scope, string(f.issue.WorkItemID), "")
		if err != nil {
			t.Fatal(err)
		}
		capture.mu.Lock()
		defer capture.mu.Unlock()
		if len(capture.statements) == 0 {
			t.Fatal("runtime PostgreSQL statements were not observed")
		}
		return evidence, append([]tracedStatement(nil), capture.statements...)
	}
	previous, count, explanationCount := 0, 0, 0
	for _, history := range []int{1, 64, 256} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			for i := previous; i < history; i++ {
				decision, err := content.PrepareEvent(t.Context(), scope.organization, marshal(tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateType: "issue", AggregateID: f.issue.WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, Actor: tracker.Actor{Kind: "system", PrincipalID: "example-scheduler"}, RecordedAt: now, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Reason: "recorded refusal", At: now}}}))
				if err != nil {
					t.Fatal(err)
				}
				old := version
				old.ID, old.Number = newNativeID("version"), int64(i+1000)
				oldVersion, err := content.PrepareVersion(t.Context(), scope.organization, marshal(old))
				if err != nil {
					t.Fatal(err)
				}
				var historical []eventstore.Evidence
				for kind, record := range map[string]any{
					"review":     tracker.ChangeReview{ID: newNativeID("review"), VersionID: old.ID, Decision: "approved", Body: strings.Repeat("historical review ", 512), Actor: tracker.Actor{Kind: "human", PrincipalID: "old-reviewer"}, CreatedAt: version.CreatedAt},
					"check":      tracker.ChangeCheck{VersionID: old.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: newNativeID("check"), Conclusion: "failure"}, Actor: tracker.Actor{Kind: "system", PrincipalID: "old-check"}, ReceivedAt: version.CreatedAt},
					"discussion": tracker.ChangeDiscussion{ID: newNativeID("comment"), VersionID: old.ID, Body: strings.Repeat("historical discussion ", 512), Actor: tracker.Actor{Kind: "human", PrincipalID: "old-commenter"}, CreatedAt: version.CreatedAt},
				} {
					row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.organization, ChangeID: old.ChangeID, Kind: kind}, marshal(record))
					if err != nil {
						t.Fatal(err)
					}
					historical = append(historical, row)
				}
				if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.AppendEvent(t.Context(), decision); err != nil {
						return err
					}
					if _, err := w.CreateVersion(t.Context(), oldVersion); err != nil {
						return err
					}
					for _, row := range historical {
						if _, err := w.AppendEvidence(t.Context(), row); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), "ANALYZE change_evidence,change_versions"); err != nil {
				t.Fatal(err)
			}
			previous = history
			evidence, statements := readRuntime(t, t.Context())
			full := f.detail(t)
			want := full.Summary
			want.Messages = nil
			if !reflect.DeepEqual(evidence.Change.Summary, want) || evidence.Change.Change.CurrentVersion != version.ID || len(full.Versions) != history+1 || len(full.Discussion) != history || evidence.Change.Versions != nil || evidence.Change.Reviews != nil || evidence.Change.Checks != nil || evidence.Change.Discussion != nil {
				t.Fatalf("runtime lost current summary or loaded history: %#v", evidence.Change)
			}
			if count == 0 {
				count = len(statements)
			}
			projected := explain.FromNativeEvidence(evidence)
			if evidence.CurrentVersion == nil || evidence.CurrentVersion.VersionID != version.ID || evidence.CurrentVersion.HeadSHA != version.HeadSHA || evidence.CurrentVersion.Landing == nil || !evidence.CurrentVersion.Landing.Gate.OutputTruncated || len(evidence.CurrentVersion.Landing.Gate.Output) > 4096 || projected.RequiredGate.State != explain.GateFailed || projected.RequiredGate.Reason != finding || projected.RequiredGate.ExternalMerge.ObservedAt == nil || !projected.RequiredGate.ExternalMerge.ObservedAt.Equal(receipt.ObservedAt) || projected.RequiredGate.LocalValidation.State != explain.GatePassed || projected.Attempt.NativeID != repair || evidence.CurrentLease == nil || projected.Eligibility.Current.Outcome != "claimed" {
				t.Fatalf("current refusal or claimed repair lost: gate=%#v merge=%#v version=%#v lease=%#v scheduling=%#v now=%s receipt_at=%s change_at=%s", projected.RequiredGate, projected.RequiredGate.ExternalMerge, evidence.CurrentVersion, evidence.CurrentLease, evidence.Scheduling, now.Format(time.RFC3339Nano), receipt.ObservedAt.Format(time.RFC3339Nano), evidence.Change.Change.UpdatedAt.Format(time.RFC3339Nano))
			}
			if len(statements) != count {
				t.Fatalf("runtime query count grew with history: %d -> %d", count, len(statements))
			}
			checked := 0
			selectedVersion, selectedEvidence := false, false
			for _, statement := range statements {
				if !strings.Contains(statement.query, "FROM change_evidence") && !strings.Contains(statement.query, "FROM change_versions") {
					continue
				}
				checked++
				tx, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				var raw []byte
				_, err = tx.Exec(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(scope.organization))
				if err == nil {
					err = tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement.query, statement.args...).Scan(&raw)
				}
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
				rollbackErr := tx.Rollback(cleanup)
				cancel()
				if err := errors.Join(err, rollbackErr); err != nil {
					t.Fatal(err)
				}
				type runtimePlan struct {
					Relation  string        `json:"Relation Name"`
					Rows      float64       `json:"Actual Rows"`
					Removed   float64       `json:"Rows Removed by Filter"`
					Rechecked float64       `json:"Rows Removed by Index Recheck"`
					Loops     float64       `json:"Actual Loops"`
					Plans     []runtimePlan `json:"Plans"`
				}
				var plans []struct{ Plan runtimePlan }
				if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
					t.Fatalf("invalid PostgreSQL evidence plan: %s %v", raw, err)
				}
				currentVersion := false
				for _, arg := range statement.args {
					if id, ok := arg.(string); ok && id == version.ID {
						currentVersion = true
					}
				}
				var inspect func(runtimePlan)
				inspect = func(plan runtimePlan) {
					if plan.Relation == "change_evidence" || plan.Relation == "change_versions" {
						if visits := (plan.Rows + plan.Removed + plan.Rechecked) * plan.Loops; visits > 2 {
							t.Fatalf("current evidence query scans history: visits=%g plan=%s", visits, raw)
						}
						if currentVersion && plan.Rows*plan.Loops > 0 {
							selectedVersion = selectedVersion || plan.Relation == "change_versions"
							selectedEvidence = selectedEvidence || plan.Relation == "change_evidence"
						}
					}
					for _, child := range plan.Plans {
						inspect(child)
					}
				}
				inspect(plans[0].Plan)
			}
			if checked == 0 || !selectedVersion || !selectedEvidence {
				t.Fatalf("runtime plans did not read selected current evidence: queries=%d version=%t evidence=%t", checked, selectedVersion, selectedEvidence)
			}
			ctx := context.WithValue(t.Context(), explanationReadKey{}, true)
			current, explanationStatements := readRuntime(t, ctx)
			if explanationCount == 0 {
				explanationCount = len(explanationStatements)
			}
			result := explain.FromNativeEvidence(current)
			if len(explanationStatements) != explanationCount || result.RequiredGate.State != projected.RequiredGate.State || result.RequiredGate.Reason != finding || result.Attempt.NativeID != repair || current.LatestDecision == nil || current.NativeCandidateExclusion == nil || current.NativeCandidateExclusion.Data.Decision.Reason != "recorded refusal" {
				t.Fatalf("explanation lost current evidence or grew with history: queries=%d gate=%+v", len(explanationStatements), result.RequiredGate)
			}
			t.Logf("explanation history=%d queries=%d latest_decision=%s", history, len(explanationStatements), current.LatestDecision.ID)
			t.Logf("%d historical versions: %d runtime queries; bounded current evidence", history, len(statements))
		})
	}
	now = now.Add(time.Minute)
	newVersion := f.publish(t, "superseding", version.ID)
	evidence, err := f.service.readNativeRuntime(t.Context(), scope, string(f.issue.WorkItemID), "")
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
			service := newPostgresNativeService(t, Config{})
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					f := newChangeFixture(t, service, organization)
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
					prior := f.publish(t, "superseded", "")
					for i := range 66 {
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+prior.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "prior-comment-" + strconv.Itoa(i)}, Decision: "commented"}), http.StatusOK)
					}
					version := f.publish(t, "current", prior.ID)
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
					var beforeComments eventstore.Change
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						beforeComments, err = r.Change(t.Context(), f.change.ID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/explanation", f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var before explain.IssueExplanation
					decodeHubResponse(t, response, &before)
					if before.NativeRuntime == nil || before.NativeRuntime.Change == nil || before.NativeRuntime.CurrentVersion == nil || before.NativeRuntime.CurrentVersion.VersionID != version.ID || before.NativeRuntime.Change.Summary.NativeReview != review {
						t.Fatalf("current-version gate evidence omitted or contaminated by superseded reviews: %+v", before.NativeRuntime)
					}
					if decision != "checks" && (before.NativeRuntime.Change.Summary.Status == "evidence_incomplete" || slices.Contains(before.NativeRuntime.Unavailable, "change_gate_history_truncated")) {
						t.Fatal("superseded review history truncated the current-version gate")
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
					response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/explanation", f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var projected explain.IssueExplanation
					if err := json.Unmarshal(response.Body.Bytes(), &projected); err != nil {
						t.Fatal(err)
					}
					if projected.NativeRuntime == nil {
						t.Fatal("explanation omitted runtime gate evidence")
					}
					evidence := *projected.NativeRuntime
					if evidence.Change == nil {
						t.Fatal("typed runtime omitted the current Change and gate evidence without a prior attempt")
					}
					if evidence.Change.Summary.Status != "evidence_incomplete" || !slices.Contains(evidence.Unavailable, "change_gate_history_truncated") {
						t.Fatalf("history falsely complete: %+v", evidence.Change.Summary)
					}
					if decision == "checks" {
						if evidence.Change.Summary.Checks != "unknown" || evidence.Change.Summary.NativeReview != "approved" {
							t.Fatalf("check truncation changed the independent review gate: %+v", evidence.Change.Summary)
						}
					} else if evidence.Change.Summary.NativeReview != "unknown" || evidence.Change.Summary.ExternalReview != "unknown" || evidence.Change.Summary.Checks != "success" {
						t.Fatalf("review truncation changed the independent check gate: %+v", evidence.Change.Summary)
					}
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						reader, ok := r.(eventstore.ChangeGateEvidenceReader)
						if !ok {
							t.Fatal("current-version gate reader unavailable")
						}
						cut, err := reader.CurrentVersionGateEvidence(t.Context(), f.change.ID, version.ID, beforeComments.EvidenceSequence)
						if err != nil {
							return err
						}
						reviews, checks := 0, 0
						for _, row := range cut {
							if row.VersionID == nil || *row.VersionID != version.ID || row.Sequence > beforeComments.EvidenceSequence {
								t.Fatalf("gate evidence escaped the captured current version: %+v", row)
							}
							if row.Kind == "review" {
								reviews++
								if row.Decision != review {
									t.Fatalf("gate evidence ignored its snapshot sequence: %+v", row)
								}
							} else if row.Kind == "check" {
								checks++
							}
						}
						if reviews != 1 || checks != len(version.Checks) {
							t.Fatalf("snapshot gate rows reviews=%d checks=%d", reviews, checks)
						}
						if decision != "checks" {
							latest, err := reader.CurrentVersionGateEvidence(t.Context(), f.change.ID, version.ID, beforeComments.EvidenceSequence+65)
							if err != nil {
								return err
							}
							reviews = 0
							for _, row := range latest {
								if row.Kind == "review" {
									reviews++
									if row.VersionID == nil || *row.VersionID != version.ID || row.Decision != "commented" || row.Sequence <= beforeComments.EvidenceSequence {
										t.Fatalf("gate reader did not select the newest current-version reviews: %+v", row)
									}
								}
							}
							if reviews != 65 {
								t.Fatalf("newest current-version reviews=%d, want 65", reviews)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
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
		})
	}
}

func newRuntimeCapacityFixture(t *testing.T, name string, now *time.Time) (nativeFixture, *postgres.Repository, *pgxpool.Pool, *sql.DB, nativeScope) {
	t.Helper()
	service := newPostgresNativeService(t, Config{Clock: func() time.Time { return *now }})
	f := newNativeFixture(t, service, "", name)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.States = append(project.States, tracker.NativeState{Name: "Merging", Dispatchable: true})
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	admin := usagePostgresAdmin(t, service)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	config, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(config)
	config.MaxConns = 3
	config.ConnConfig.Tracer = runtimeHistoryTrace{}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}, func() time.Time { return *now })
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	originalTenant, originalAccess, originalPostgres := service.config.TenantRepository, service.config.AccessRepository, service.database.postgres
	t.Cleanup(func() {
		service.config.TenantRepository, service.config.AccessRepository, service.database.postgres = originalTenant, originalAccess, originalPostgres
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	service.config.TenantRepository, service.config.AccessRepository, service.database.postgres = repository, repository, repository
	credential, _, err := service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	return f, repository, pool, admin, scope
}

func runtimeCapacityStatements(statements []tracedStatement) ([]tracedStatement, []tracedStatement) {
	var capacity, data []tracedStatement
	for _, statement := range statements {
		query := strings.ToLower(strings.TrimSpace(statement.query))
		if strings.HasPrefix(query, "begin") || query == "commit" || query == "rollback" || strings.Contains(query, "set_config('detent.organization_id'") {
			continue
		}
		data = append(data, statement)
		if strings.HasPrefix(query, "with capacity_runners as materialized") || strings.HasPrefix(query, "with capacity_owner_window as materialized") || strings.HasPrefix(query, "with provider_owners as materialized") || strings.HasPrefix(query, "with candidates as materialized (select * from leases") {
			capacity = append(capacity, statement)
		}
	}
	return capacity, data
}

func runtimeCapacityQueries(t *testing.T, service *Service, ctx context.Context, scope nativeScope, now time.Time) ([]runnerauth.Runner, bool, []tracedStatement, error) {
	t.Helper()
	capture := &runtimeHistoryTraceCapture{}
	readCtx := context.WithValue(ctx, runtimeHistoryTraceKey{}, capture)
	repository, ok := service.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		t.Fatal("tenant snapshot owner unavailable")
	}
	var snapshot runnerstore.RuntimeCapacitySnapshot
	err := repository.ReadTenantSnapshot(readCtx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(runnerstore.RuntimeCapacityReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		snapshot, err = owner.RuntimeCapacitySnapshot(readCtx, runnerstore.RuntimeCapacityQuery{ProjectID: scope.project, RunnerID: scope.credential.Runner.RunnerID, At: now, Explanation: explanationRead(ctx)})
		return err
	})
	statements, _ := runtimeCapacityStatements(capture.statements)
	if err == nil && len(statements) == 0 {
		t.Fatal("runtime capacity PostgreSQL queries were not observed")
	}
	return runtimeRunnersFromSnapshot(snapshot, scope, now), snapshot.Truncated, statements, err
}

func runtimeRouteCapacityQueries(t *testing.T, service *Service, ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, want int, total *int) (tracker.NativeRuntimeEvidence, []tracedStatement, error) {
	t.Helper()
	capture := &runtimeHistoryTraceCapture{}
	evidence, err := service.readNativeRuntime(context.WithValue(ctx, runtimeHistoryTraceKey{}, capture), scope, string(item), "")
	capacity, data := runtimeCapacityStatements(capture.statements)
	if err == nil && len(capacity) != want {
		t.Fatalf("live runtime capacity owner queries=%d want=%d; total runtime queries=%d", len(capacity), want, len(data))
	}
	if err == nil && total != nil {
		if *total == 0 {
			*total = len(data)
		}
		if len(data) != *total {
			t.Fatalf("total runtime query count grew with runner population: %d -> %d", *total, len(data))
		}
	}
	for index, statement := range data {
		t.Logf("runtime statement %d: %s", index+1, strings.Join(strings.Fields(statement.query), " "))
	}
	t.Logf("live runtime capacity owner queries=%d total runtime queries=%d", len(capacity), len(data))
	return evidence, capacity, err
}

func assertRuntimeCapacityPlans(t *testing.T, pool *pgxpool.Pool, scope nativeScope, statements []tracedStatement, want int) {
	t.Helper()
	found := false
	for _, statement := range statements {
		if !strings.HasPrefix(statement.query, "WITH candidates AS MATERIALIZED (SELECT * FROM leases") {
			continue
		}
		found = true
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(t.Context(), "SELECT current_user, set_config('detent.organization_id', $1, true)", string(scope.organization))
		var raw []byte
		if err == nil {
			err = tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement.query, statement.args...).Scan(&raw)
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
		rollbackErr := tx.Rollback(cleanup)
		cancel()
		if err := errors.Join(err, rollbackErr); err != nil {
			t.Fatal(err)
		}
		type plan struct {
			Relation string  `json:"Relation Name"`
			Subplan  string  `json:"Subplan Name"`
			Rows     float64 `json:"Actual Rows"`
			Removed  float64 `json:"Rows Removed by Filter"`
			Loops    float64 `json:"Actual Loops"`
			Plans    []plan  `json:"Plans"`
		}
		var plans []struct{ Plan plan }
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("invalid capacity plan: %s %v", raw, err)
		}
		candidates, examined := -1.0, -1.0
		var inspect func(plan)
		inspect = func(node plan) {
			if node.Subplan == "CTE candidates" {
				candidates = node.Rows * node.Loops
			}
			if node.Relation == "leases" {
				examined = max(examined, (node.Rows+node.Removed)*node.Loops)
			}
			if node.Relation == "issues" && (node.Rows+node.Removed)*node.Loops > float64(want) {
				t.Fatalf("landing capacity scans project history: %s", raw)
			}
			for _, child := range node.Plans {
				inspect(child)
			}
		}
		inspect(plans[0].Plan)
		if candidates != float64(want) || examined != float64(want) {
			t.Fatalf("examined %g live lease candidates with %g selected; want%d: %s", examined, candidates, want, raw)
		}
	}
	if !found {
		t.Fatal("bounded provider candidate query was not observed")
	}
}

func TestNativeRuntimeReadSharedCapacityCost(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f, repository, pool, admin, scope := newRuntimeCapacityFixture(t, "runtime-capacity", &now)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "selected runtime")
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	first.enroll(t)
	reportRunnerCapacity(t, f.service, f.base, first.redemption.Credential, first.binding.MachineID, "runner", first.redemption.BackendIsolation, 8)
	report := capacityReport(now)
	report.MaxConcurrent = 8
	publishCapacity(t, f, first, report)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", first.redemption.Credential, providerClaim(first, issue, "reserved"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	now = lease.ServerTime
	all := []runnerFixture{first}
	landingResponse := createIndependentNativeIssue(t, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "runtime-landing"}, Title: "Example landing", Body: issueContractTestSections, State: "Merging"}, func(request tracker.CreateIssue) *httptest.ResponseRecorder {
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	})
	var landingIssue tracker.NativeIssue
	decodeHubResponse(t, landingResponse, &landingIssue)
	normalTotal, explanationTotal := 0, 0
	previous := 1
	for _, total := range []int{1, 8, 100, 101} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			for i := previous; i < total; i++ {
				all = append(all, sharedRunner(t, first))
			}
			previous = total
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				for _, runner := range all {
					if err := w.ReplaceRunnerProviderReports(t.Context(), runner.binding.RunnerID, []providercapacity.Report{report}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			runners, truncated, statements, err := runtimeCapacityQueries(t, f.service, t.Context(), scope, now)
			if err != nil || len(runners) != min(total, 100) || truncated != (total > 100) || len(statements) != 4 {
				t.Fatalf("capacity projection runners=%d truncated=%v queries=%d error=%v", len(runners), truncated, len(statements), err)
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
					full, err := f.service.readRunnerRouting(t.Context(), scope, r.RunnerID)
					if err != nil || r.Health != full.Health || !reflect.DeepEqual(r.ProviderCapacity, full.ProviderCapacity) || !reflect.DeepEqual(r.Exclusions(scope.project, policy.Requirements{}, false), full.Exclusions(scope.project, policy.Requirements{}, false)) {
						t.Fatalf("projection differs from routing owner: %#v, %#v, %v", r, full, err)
					}
				}
			}
			evidence, _, err := runtimeRouteCapacityQueries(t, f.service, t.Context(), scope, issue.WorkItemID, 4, &normalTotal)
			if err != nil || slices.Contains(evidence.Unavailable, "capacity_projection_truncated") != truncated || evidence.CurrentLease == nil || evidence.CurrentLease.ID != lease.ID || evidence.Scheduling.Outcome != "claimed" {
				t.Fatalf("runtime truncation or fenced lease lost: unavailable=%v lease=%#v error=%v", evidence.Unavailable, evidence.CurrentLease, err)
			}
			if len(evidence.Capacity) != len(runners) || slices.Contains(evidence.Unavailable, "runner_capacity_projection") {
				t.Fatalf("live runtime capacity count=%d want=%d", len(evidence.Capacity), len(runners))
			}
			for i, runner := range runners {
				view := evidence.Capacity[i]
				codes := []string{}
				for _, exclusion := range runner.Exclusions(scope.project, policy.Requirements{}, false) {
					codes = append(codes, exclusion.Code)
				}
				if view.RunnerID != runner.RunnerID || view.Health != runner.Health || view.Available != max(0, min(runner.HostCapacity-runner.HostUsed, runner.RemainingCapacity(scope.project))) || !reflect.DeepEqual(view.ProviderCapacity, runner.ProviderCapacity) || !reflect.DeepEqual(view.Exclusions, codes) {
					t.Fatalf("live runtime lost capacity owner facts: %#v %#v", view, runner)
				}
			}
			if total == 101 {
				currentScope := scope
				for _, runner := range all {
					if runner.binding.RunnerID > currentScope.credential.Runner.RunnerID {
						currentScope.credential.Runner.RunnerID = runner.binding.RunnerID
					}
				}
				current, truncated, currentStatements, err := runtimeCapacityQueries(t, f.service, t.Context(), currentScope, now)
				if err != nil || !truncated || len(current) != 100 || current[0].RunnerID != currentScope.credential.Runner.RunnerID || len(currentStatements) != 4 {
					t.Fatalf("bounded runtime projection dropped current registered runner: runners=%d truncated=%v queries=%d error=%v", len(current), truncated, len(currentStatements), err)
				}
			}
			explanationCtx := context.WithValue(t.Context(), explanationReadKey{}, true)
			explanationRunners, partial, explanationStatements, err := runtimeCapacityQueries(t, f.service, explanationCtx, scope, now)
			if err != nil || len(explanationRunners) != min(total, 16) || partial != (total > 16) || len(explanationStatements) != 5 {
				t.Fatalf("unbounded explanation capacity: runners=%d partial=%v queries=%d error=%v", len(explanationRunners), partial, len(explanationStatements), err)
			}
			explanationEvidence, _, err := runtimeRouteCapacityQueries(t, f.service, explanationCtx, scope, issue.WorkItemID, 5, nil)
			if err != nil || len(explanationEvidence.Capacity) != len(explanationRunners) || slices.Contains(explanationEvidence.Unavailable, "capacity_projection_truncated") != partial {
				t.Fatalf("live explanation capacity lost owner window: %#v %v", explanationEvidence, err)
			}
			landingEvidence, landingStatements, err := runtimeRouteCapacityQueries(t, f.service, explanationCtx, scope, landingIssue.WorkItemID, 5, &explanationTotal)
			if err != nil || slices.Contains(landingEvidence.Unavailable, "project_landing_capacity") != (total > 64) || len(landingEvidence.Capacity) == 0 {
				t.Fatalf("live landing capacity lost bounded current facts: %#v error=%v", landingEvidence, err)
			}
			for _, view := range landingEvidence.Capacity {
				if slices.Contains(view.Exclusions, "landing_capacity_unknown") != (total > 64) {
					t.Fatalf("live landing readiness differs from bounded owner: %#v", view)
				}
			}
			if total <= 64 {
				assertRuntimeCapacityPlans(t, pool, scope, landingStatements, 1)
			}
			t.Logf("%d runners sharing a host/provider: %d capacity queries", total, len(statements))
			if total == 101 {
				t.Run("normal landing lease outside runner window", func(t *testing.T) {
					outside := all[0]
					for _, runner := range all {
						if runner.binding.RunnerID > outside.binding.RunnerID {
							outside = runner
						}
					}
					machineID := newNativeID("machine")
					if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: scope.organization, ID: machineID, Hostname: "Example outside host", Capacity: 8, Version: "test"})
					}); err != nil {
						t.Fatal(err)
					}
					originalNow := now
					var hiddenLease leasestore.Lease
					t.Cleanup(func() {
						now = originalNow
						cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
						defer cancel()
						if hiddenLease.LeaseID != "" {
							if err := repository.WriteTenant(cleanup, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
								_, err := w.Release(cleanup, leasestore.Fence{LeaseID: hiddenLease.LeaseID, FencingToken: hiddenLease.FencingToken}, "completed")
								return err
							}); err != nil {
								t.Error(err)
							}
						}
						if _, err := admin.ExecContext(cleanup, `WITH enrollment AS (UPDATE runner_enrollments SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 RETURNING id) UPDATE runner_identities SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 AND enrollment_id IN(SELECT id FROM enrollment)`, scope.organization, outside.binding.RunnerID, outside.binding.MachineID); err != nil {
							t.Error(err)
						}
					})
					if _, err := admin.ExecContext(t.Context(), `WITH enrollment AS (UPDATE runner_enrollments SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 RETURNING id) UPDATE runner_identities SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 AND enrollment_id IN(SELECT id FROM enrollment)`, scope.organization, outside.binding.RunnerID, machineID); err != nil {
						t.Fatal(err)
					}
					hiddenIssue := f.create(t, "Example outside landing lease")
					if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						item, err := w.WorkItem(t.Context(), scope.project, hiddenIssue.WorkItemID)
						if err != nil {
							return err
						}
						hiddenLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machineID, RunnerID: outside.binding.RunnerID, SessionID: "landing/outside-window", LeaseID: newNativeID("lease"), TTL: time.Hour, LandingCapacity: 1})
						if err != nil {
							return err
						}
						return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: scope.organization, ProjectID: scope.project, LeaseID: tracker.LeaseID(hiddenLease.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: hiddenLease.RenewedAt})
					}); err != nil {
						t.Fatal(err)
					}
					now = hiddenLease.RenewedAt
					selected, partial, ownerStatements, err := runtimeCapacityQueries(t, f.service, t.Context(), scope, now)
					if err != nil || !partial || len(selected) != 100 || len(ownerStatements) != 4 {
						t.Fatalf("normal runner window lost its bound: count=%d partial=%v statements=%d error=%v", len(selected), partial, len(ownerStatements), err)
					}
					for _, runner := range selected {
						if runner.RunnerID == outside.binding.RunnerID || string(runner.MachineID) == machineID {
							t.Fatalf("landing lease was not outside selected runner machines: %#v", runner)
						}
					}
					landing, _, err := runtimeRouteCapacityQueries(t, f.service, t.Context(), scope, landingIssue.WorkItemID, 4, nil)
					if err != nil || len(landing.Capacity) != 100 || !slices.Contains(landing.Unavailable, "capacity_projection_truncated") || !slices.Contains(landing.Unavailable, "project_landing_capacity") {
						t.Fatalf("normal truncated window reported complete landing capacity: %#v error=%v", landing, err)
					}
					for _, view := range landing.Capacity {
						if !slices.Contains(view.Exclusions, "landing_capacity_unknown") {
							t.Fatalf("out-of-window landing lease became ready: %#v", view)
						}
					}
					for _, entry := range landing.Admission {
						if entry.Outcome == "ready" {
							t.Fatalf("normal incomplete landing facts became ready: %#v", entry)
						}
					}
				})
			}

		})
	}
	for _, sparse := range []string{"ungranted", "removed"} {
		t.Run(sparse, func(t *testing.T) {
			currentScope := scope
			for _, runner := range all {
				if runner.binding.RunnerID > currentScope.credential.Runner.RunnerID {
					currentScope.credential.Runner.RunnerID = runner.binding.RunnerID
				}
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
				defer cancel()
				if _, err := admin.ExecContext(cleanup, `UPDATE runner_identities SET removed_at=NULL WHERE organization_id=$1`, scope.organization); err != nil {
					t.Error(err)
				}
				if _, err := admin.ExecContext(cleanup, `INSERT INTO token_grants(organization_id,token_id,project_id) SELECT organization_id,token_id,$2 FROM runner_identities WHERE organization_id=$1 ON CONFLICT DO NOTHING`, scope.organization, scope.project); err != nil {
					t.Error(err)
				}
			})
			var err error
			if sparse == "ungranted" {
				_, err = admin.ExecContext(t.Context(), `DELETE FROM token_grants WHERE organization_id=$1 AND token_id IN(SELECT token_id FROM runner_identities WHERE organization_id=$1 AND runner_id<>$2)`, scope.organization, currentScope.credential.Runner.RunnerID)
			} else {
				_, err = admin.ExecContext(t.Context(), `UPDATE runner_identities SET removed_at=$3 WHERE organization_id=$1 AND runner_id<>$2`, scope.organization, currentScope.credential.Runner.RunnerID, now)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), explanationReadKey{}, true)
			candidates, partial, _, err := runtimeCapacityQueries(t, f.service, ctx, scope, now)
			if err != nil || !partial || len(candidates) != 0 {
				t.Fatalf("candidate search escaped sparse owner window: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
			candidates, partial, _, err = runtimeCapacityQueries(t, f.service, ctx, currentScope, now)
			if err != nil || !partial || len(candidates) != 1 || candidates[0].RunnerID != currentScope.credential.Runner.RunnerID {
				t.Fatalf("selected runner lost priority: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
			if _, err := admin.ExecContext(t.Context(), `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=(SELECT token_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2)`, scope.organization, currentScope.credential.Runner.RunnerID); err != nil {
				t.Fatal(err)
			}
			candidates, partial, _, err = runtimeCapacityQueries(t, f.service, ctx, currentScope, now)
			if err != nil || !partial || len(candidates) != 0 {
				t.Fatalf("selected runner bypassed grants: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
		})
	}

	foreign := newNativeFixture(t, f.service, "org_first_second", "runtime-foreign")
	foreignCredential, _, err := f.service.authenticateTenantAPIHash(t.Context(), foreign.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil {
		t.Fatal(err)
	}
	runners, _, _, err := runtimeCapacityQueries(t, f.service, t.Context(), nativeScope{organization: foreign.project.OrganizationID, project: foreign.project.ID, credential: foreignCredential}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("cross-organization capacity leaked: %#v %v", runners, err)
	}
	other := newNativeFixture(t, f.service, scope.organization, "runtime-ungranted")
	runners, _, _, err = runtimeCapacityQueries(t, f.service, t.Context(), nativeScope{organization: scope.organization, project: other.project.ID, credential: scope.credential}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("ungranted project capacity leaked: %#v %v", runners, err)
	}
	runners, _, statements, err := runtimeCapacityQueries(t, f.service, t.Context(), scope, now.Add(91*time.Second))
	if err != nil || len(statements) != 4 {
		t.Fatal(err)
	}
	for _, r := range runners {
		if r.HostUsed != 0 || r.Used != 0 || r.ProviderCapacity[0].Used != 0 {
			t.Fatalf("expired reservation remains occupied: %#v", r)
		}
	}
	originalNow := now
	now = now.Add(91 * time.Second)
	evidence, err := f.service.readNativeRuntime(t.Context(), scope, string(issue.WorkItemID), "")
	now = originalNow
	if err != nil || evidence.Scheduling.Outcome != "unknown" || !slices.Contains(evidence.Unavailable, "capacity_projection_truncated") {
		t.Fatalf("truncated exclusions became a complete scheduling decision: %#v %v", evidence.Scheduling, err)
	}
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=(SELECT token_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2) AND project_id=$3`, scope.organization, first.binding.RunnerID, scope.project); err != nil {
		t.Fatal(err)
	}
	runners, truncated, _, err := runtimeCapacityQueries(t, f.service, t.Context(), scope, now)
	if err != nil || truncated || len(runners) != 100 {
		t.Fatalf("project grant removal did not restrict the page: runners=%d truncated=%v error=%v", len(runners), truncated, err)
	}
	for _, r := range runners {
		if r.RunnerID == first.binding.RunnerID || r.HostUsed != 1 || r.Used != 0 || r.ProviderCapacity[0].Used != 1 {
			t.Fatalf("occupancy outside the granted runner page was lost: %#v", r)
		}
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET scope='organization' WHERE organization_id=$1 AND runner_id=$2`, scope.organization, first.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	organizationScope := scope
	organizationScope.credential.Runner.RunnerID = first.binding.RunnerID
	runners, truncated, _, err = runtimeCapacityQueries(t, f.service, t.Context(), organizationScope, now)
	if err != nil || !truncated || len(runners) != 100 || runners[0].RunnerID != first.binding.RunnerID || !runners[0].AllowsProject(scope.project) {
		t.Fatalf("organization-scoped runner required a project grant: %#v partial=%v error=%v", runners, truncated, err)
	}
	organizationEvidence, _, err := runtimeRouteCapacityQueries(t, f.service, t.Context(), organizationScope, issue.WorkItemID, 4, nil)
	if err != nil || len(organizationEvidence.Capacity) != 100 || organizationEvidence.Capacity[0].RunnerID != first.binding.RunnerID || slices.Contains(organizationEvidence.Capacity[0].Exclusions, "project_access_denied") {
		t.Fatalf("live runtime dropped organization-scoped runner without project grant: %#v %v", organizationEvidence, err)
	}
}

func TestExplanationProviderCapacityCandidateWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	f, repository, pool, admin, scope := newRuntimeCapacityFixture(t, "capacity-window", &now)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	first.enroll(t)
	reportRunnerCapacity(t, f.service, f.base, first.redemption.Credential, first.binding.MachineID, "runner", first.redemption.BackendIsolation, 8)
	publishCapacity(t, f, first, capacityReport(now))
	issue := f.create(t, "reserved")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", first.redemption.Credential, providerClaim(first, issue, "reserved"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	now = lease.ServerTime
	var reservation claimstore.ProviderReservation
	if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		reservation, err = r.ProviderReservation(t.Context(), lease.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), explanationReadKey{}, true)
	previous := 1
	for _, total := range []int{1024, 2048} {
		_, err := admin.ExecContext(t.Context(), `WITH added AS (
INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name,native_created_at,native_updated_at)
SELECT $1,$2,'candidate_'||n,n+1,'Example candidate','Todo',$5,$5 FROM generate_series($3::integer,$4::integer)n RETURNING organization_id,project_id,id,native_id
), owned AS (
INSERT INTO leases(organization_id,project_id,runner_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at)
SELECT organization_id,project_id,$7,'capacity_'||native_id,id,$6,native_id,1,$5,$5,$5::timestamptz+interval '2 minutes' FROM added RETURNING organization_id,lease_id,runner_id,acquired_at
) INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at) SELECT organization_id,lease_id,runner_id,acquired_at FROM owned`, scope.organization, scope.project, previous, total-1, now, first.binding.MachineID, first.binding.RunnerID)
		if err != nil {
			t.Fatal(err)
		}
		previous = total
		if _, err := admin.ExecContext(t.Context(), "ANALYZE leases,issues,provider_reservations"); err != nil {
			t.Fatal(err)
		}
		for _, absent := range []bool{false, true} {
			t.Run(fmt.Sprintf("leases=%d/absent=%v", total, absent), func(t *testing.T) {
				if absent {
					if _, err := admin.ExecContext(t.Context(), `DELETE FROM provider_reservations WHERE organization_id=$1`, scope.organization); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
						defer cancel()
						if err := repository.WriteTenant(cleanup, scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return w.CreateProviderReservation(cleanup, reservation) }); err != nil {
							t.Error(err)
						}
					})
				}
				capture := &runtimeHistoryTraceCapture{}
				readCtx := context.WithValue(ctx, runtimeHistoryTraceKey{}, capture)
				var facts runnerstore.RuntimeCapacitySnapshot
				err := repository.ReadTenantSnapshot(readCtx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					owner, ok := r.(runnerstore.RuntimeCapacityReader)
					if !ok {
						return nativeRunEventStorageUnavailable()
					}
					var err error
					facts, err = owner.RuntimeCapacitySnapshot(readCtx, runnerstore.RuntimeCapacityQuery{ProjectID: scope.project, At: now, Explanation: true})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				assertRuntimeCapacityPlans(t, pool, scope, capture.statements, min(total, 1025))
				snapshot := runtimeProviderCapacity(facts.Providers)
				wantReservations := 1
				if absent {
					wantReservations = 0
				}
				if snapshot.truncated != (total > 1024) || len(snapshot.reservations) != wantReservations {
					t.Fatalf("reservations=%d partial=%v error=%v", len(snapshot.reservations), snapshot.truncated, err)
				}
				evidence, routeStatements, err := runtimeRouteCapacityQueries(t, f.service, ctx, scope, issue.WorkItemID, 5, nil)
				assertRuntimeCapacityPlans(t, pool, scope, routeStatements, min(total, 1025))
				if err != nil || len(evidence.Capacity) != 1 || len(evidence.Capacity[0].ProviderCapacity) != 1 || evidence.Capacity[0].ProviderCapacity[0].Used != wantReservations || slices.Contains(evidence.Unavailable, "capacity_projection_truncated") != (total > 1024) {
					t.Fatalf("unavailable=%v error=%v", evidence.Unavailable, err)
				}
				if total > 1024 {
					for _, entry := range evidence.Admission {
						if entry.Outcome == "ready" {
							t.Fatalf("truncated provider facts became ready: %#v", entry)
						}
					}
				}
			})
		}
	}
}
