package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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

func TestNativeRuntimeReadPreservesCurrentEvidence(t *testing.T) {
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
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1}, func() time.Time { return f.service.config.Clock() })
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
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	path := f.path + "/versions/" + version.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-current"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, changeTestResult(version)), http.StatusOK)
	now := f.service.config.Clock().Add(time.Second).UTC().Truncate(time.Microsecond)
	f.service.config.Clock = func() time.Time { return now }
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
		id := hubapi.NewID("attempt")
		if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(t.Context(), scope.Project, f.issue.WorkItemID)
			if err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(registered.binding.MachineID), RunnerID: registered.binding.RunnerID, SessionID: id, LeaseID: hubapi.NewID("lease"), TTL: time.Hour})
			if err != nil {
				return err
			}
			if now.Before(lease.RenewedAt) {
				now = lease.RenewedAt.Add(time.Second)
			}
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: scope.Organization, ProjectID: scope.Project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: now}); err != nil {
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
			event, err := content.PrepareEvent(t.Context(), scope.Organization, marshal(tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: f.issue.WorkItemID, Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: string(registered.binding.MachineID)}, RecordedAt: now, Data: data}))
			if err != nil {
				return err
			}
			payload, err := content.PreparePayload(t.Context(), scope.Organization, marshal(run))
			if err != nil {
				return err
			}
			if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: "archive_" + payload.SHA256, Reference: payload, CreatedAt: now}); err != nil {
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
	issueVersion, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, marshal(issue))
	if err != nil {
		t.Fatal(err)
	}
	transition, err := content.PrepareEvent(t.Context(), scope.Organization, marshal(tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "system", PrincipalID: "example-worker"}, RecordedAt: now, Data: tracker.CollaborationData{FromState: "Merging", ToState: issue.State, Reason: "worker_progress", ReasonDetail: finding, Change: &tracker.NativeChangeReference{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), scope.Project, issue.WorkItemID)
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
	readRuntime := func(t *testing.T, ctx context.Context) tracker.NativeRuntimeEvidence {
		t.Helper()
		evidence, err := f.service.readNativeRuntime(ctx, scope, string(f.issue.WorkItemID), "")
		if err != nil {
			t.Fatal(err)
		}
		return evidence
	}
	previous := 0
	for _, history := range []int{1, 64, 256} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			for i := previous; i < history; i++ {
				decision, err := content.PrepareEvent(t.Context(), scope.Organization, marshal(tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: f.issue.WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, Actor: tracker.Actor{Kind: "system", PrincipalID: "example-scheduler"}, RecordedAt: now, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Reason: "recorded refusal", At: now}}}))
				if err != nil {
					t.Fatal(err)
				}
				old := version
				old.ID, old.Number = hubapi.NewID("version"), int64(i+1000)
				oldVersion, err := content.PrepareVersion(t.Context(), scope.Organization, marshal(old))
				if err != nil {
					t.Fatal(err)
				}
				var historical []eventstore.Evidence
				for kind, record := range map[string]any{
					"review":     tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: old.ID, Decision: "approved", Body: strings.Repeat("historical review ", 512), Actor: tracker.Actor{Kind: "human", PrincipalID: "old-reviewer"}, CreatedAt: version.CreatedAt},
					"check":      tracker.ChangeCheck{VersionID: old.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: hubapi.NewID("check"), Conclusion: "failure"}, Actor: tracker.Actor{Kind: "system", PrincipalID: "old-check"}, ReceivedAt: version.CreatedAt},
					"discussion": tracker.ChangeDiscussion{ID: hubapi.NewID("comment"), VersionID: old.ID, Body: strings.Repeat("historical discussion ", 512), Actor: tracker.Actor{Kind: "human", PrincipalID: "old-commenter"}, CreatedAt: version.CreatedAt},
				} {
					row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: old.ChangeID, Kind: kind}, marshal(record))
					if err != nil {
						t.Fatal(err)
					}
					historical = append(historical, row)
				}
				if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
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
			previous = history
			evidence := readRuntime(t, t.Context())
			var full tracker.ChangeDetail
			if history+1 <= corestore.MaxPageSize {
				full = f.detail(t)
			} else {
				summary, err := f.service.selectedCurrentChangeSummary(t.Context(), scope, f.issue.WorkItemID, f.change.ID, f.service.config.Clock())
				if err != nil {
					t.Fatal(err)
				}
				full.Summary = summary
				if err := repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					for after := int64(0); ; {
						page, err := r.Versions(t.Context(), f.change.ID, after, corestore.MaxPageSize)
						if err != nil || len(page) == 0 {
							return err
						}
						for _, row := range page {
							full.Versions = append(full.Versions, tracker.ChangeVersion{ID: row.ID})
						}
						after = page[len(page)-1].Number
					}
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					for after := int64(0); ; {
						page, err := r.Evidence(t.Context(), f.change.ID, after, corestore.MaxPageSize)
						if err != nil || len(page) == 0 {
							return err
						}
						for _, row := range page {
							if row.Kind == "discussion" {
								full.Discussion = append(full.Discussion, tracker.ChangeDiscussion{VersionID: hubapi.ValueOrEmpty(row.VersionID)})
							}
						}
						after = page[len(page)-1].Sequence
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			want := full.Summary
			want.Messages = nil
			if !reflect.DeepEqual(evidence.Change.Summary, want) || evidence.Change.Change.CurrentVersion != version.ID || len(full.Versions) != history+1 || len(full.Discussion) != history || evidence.Change.Versions != nil || evidence.Change.Reviews != nil || evidence.Change.Checks != nil || evidence.Change.Discussion != nil {
				t.Fatalf("runtime lost current summary or loaded history: %#v", evidence.Change)
			}
			projected := explain.FromNativeEvidence(evidence)
			if evidence.CurrentVersion == nil || evidence.CurrentVersion.VersionID != version.ID || evidence.CurrentVersion.HeadSHA != version.HeadSHA || evidence.CurrentVersion.Landing == nil || !evidence.CurrentVersion.Landing.Gate.OutputTruncated || len(evidence.CurrentVersion.Landing.Gate.Output) > 4096 || projected.RequiredGate.State != explain.GateFailed || projected.RequiredGate.Reason != finding || projected.RequiredGate.ExternalMerge.ObservedAt == nil || !projected.RequiredGate.ExternalMerge.ObservedAt.Equal(receipt.ObservedAt) || projected.RequiredGate.LocalValidation.State != explain.GatePassed || projected.Attempt.NativeID != repair || evidence.CurrentLease == nil || projected.Eligibility.Current.Outcome != "claimed" {
				t.Fatalf("current refusal or claimed repair lost: gate=%#v merge=%#v version=%#v lease=%#v scheduling=%#v now=%s receipt_at=%s change_at=%s", projected.RequiredGate, projected.RequiredGate.ExternalMerge, evidence.CurrentVersion, evidence.CurrentLease, evidence.Scheduling, now.Format(time.RFC3339Nano), receipt.ObservedAt.Format(time.RFC3339Nano), evidence.Change.Change.UpdatedAt.Format(time.RFC3339Nano))
			}
			ctx := context.WithValue(t.Context(), hubcontent.ExplanationReadKey{}, true)
			current := readRuntime(t, ctx)
			result := explain.FromNativeEvidence(current)
			if result.RequiredGate.State != projected.RequiredGate.State || result.RequiredGate.Reason != finding || result.Attempt.NativeID != repair || current.LatestDecision == nil || current.NativeCandidateExclusion == nil || current.NativeCandidateExclusion.Data.Decision.Reason != "recorded refusal" {
				t.Fatalf("explanation lost current evidence: gate=%+v", result.RequiredGate)
			}
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
					for range 2 {
						if _, err := f.service.config.TenantRepository.(*postgres.Repository).BackfillCurrentChangeReviews(t.Context(), organization, eventstore.NewContent(f.service.runDetailStore()), corestore.MaxPageSize); err != nil {
							t.Fatal(err)
						}
					}
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
							switch row.Kind {
							case "review":
								reviews++
								if row.Decision != review {
									t.Fatalf("gate evidence ignored its snapshot sequence: %+v", row)
								}
							case "check":
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

func newRuntimeCapacityFixture(t *testing.T, name string, now *time.Time) (nativeFixture, *postgres.Repository, *pgxpool.Pool, *sql.DB, hubscope.NativeScope) {
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
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	return f, repository, pool, admin, scope
}

func runtimeCapacityQueries(t *testing.T, service *Service, ctx context.Context, scope hubscope.NativeScope, now time.Time) ([]runnerauth.Runner, bool, error) {
	t.Helper()
	repository, ok := service.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		t.Fatal("tenant snapshot owner unavailable")
	}
	var snapshot runnerstore.RuntimeCapacitySnapshot
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(runnerstore.RuntimeCapacityReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		snapshot, err = owner.RuntimeCapacitySnapshot(ctx, runnerstore.RuntimeCapacityQuery{ProjectID: scope.Project, RunnerID: scope.Credential.Runner.RunnerID, At: now, Explanation: hubcontent.ExplanationRead(ctx)})
		return err
	})
	return runtimeRunnersFromSnapshot(snapshot, scope, now), snapshot.Truncated, err
}

func TestNativeRuntimeReadPreservesSharedCapacity(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f, repository, _, admin, scope := newRuntimeCapacityFixture(t, "runtime-capacity", &now)
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
	previous := 1
	for _, total := range []int{1, 8, 100, 101} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			for i := previous; i < total; i++ {
				all = append(all, sharedRunner(t, first))
			}
			previous = total
			if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
				for _, runner := range all {
					if err := w.ReplaceRunnerProviderReports(t.Context(), runner.binding.RunnerID, []providercapacity.Report{report}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			runners, truncated, err := runtimeCapacityQueries(t, f.service, t.Context(), scope, now)
			if err != nil || len(runners) != min(total, 100) || truncated != (total > 100) {
				t.Fatalf("capacity projection runners=%d truncated=%v error=%v", len(runners), truncated, err)
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
					if err != nil || r.Health != full.Health || !reflect.DeepEqual(r.ProviderCapacity, full.ProviderCapacity) || !reflect.DeepEqual(r.Exclusions(scope.Project, policy.Requirements{}, false), full.Exclusions(scope.Project, policy.Requirements{}, false)) {
						t.Fatalf("projection differs from routing owner: %#v, %#v, %v", r, full, err)
					}
				}
			}
			evidence, err := f.service.readNativeRuntime(t.Context(), scope, string(issue.WorkItemID), "")
			if err != nil || slices.Contains(evidence.Unavailable, "capacity_projection_truncated") != truncated || evidence.CurrentLease == nil || evidence.CurrentLease.ID != lease.ID || evidence.Scheduling.Outcome != "claimed" {
				t.Fatalf("runtime truncation or fenced lease lost: unavailable=%v lease=%#v error=%v", evidence.Unavailable, evidence.CurrentLease, err)
			}
			if len(evidence.Capacity) != len(runners) || slices.Contains(evidence.Unavailable, "runner_capacity_projection") {
				t.Fatalf("live runtime capacity count=%d want=%d", len(evidence.Capacity), len(runners))
			}
			for i, runner := range runners {
				view := evidence.Capacity[i]
				codes := []string{}
				for _, exclusion := range runner.Exclusions(scope.Project, policy.Requirements{}, false) {
					codes = append(codes, exclusion.Code)
				}
				if view.RunnerID != runner.RunnerID || view.Health != runner.Health || view.Available != max(0, min(runner.HostCapacity-runner.HostUsed, runner.RemainingCapacity(scope.Project))) || !reflect.DeepEqual(view.ProviderCapacity, runner.ProviderCapacity) || !reflect.DeepEqual(view.Exclusions, codes) {
					t.Fatalf("live runtime lost capacity owner facts: %#v %#v", view, runner)
				}
			}
			if total == 101 {
				currentScope := scope
				for _, runner := range all {
					if runner.binding.RunnerID > currentScope.Credential.Runner.RunnerID {
						currentScope.Credential.Runner.RunnerID = runner.binding.RunnerID
					}
				}
				current, truncated, err := runtimeCapacityQueries(t, f.service, t.Context(), currentScope, now)
				if err != nil || !truncated || len(current) != 100 || current[0].RunnerID != currentScope.Credential.Runner.RunnerID {
					t.Fatalf("bounded runtime projection dropped current registered runner: runners=%d truncated=%v error=%v", len(current), truncated, err)
				}
			}
			explanationCtx := context.WithValue(t.Context(), hubcontent.ExplanationReadKey{}, true)
			explanationRunners, partial, err := runtimeCapacityQueries(t, f.service, explanationCtx, scope, now)
			if err != nil || len(explanationRunners) != min(total, 16) || partial != (total > 16) {
				t.Fatalf("unbounded explanation capacity: runners=%d partial=%v error=%v", len(explanationRunners), partial, err)
			}
			explanationEvidence, err := f.service.readNativeRuntime(explanationCtx, scope, string(issue.WorkItemID), "")
			if err != nil || len(explanationEvidence.Capacity) != len(explanationRunners) || slices.Contains(explanationEvidence.Unavailable, "capacity_projection_truncated") != partial {
				t.Fatalf("live explanation capacity lost owner window: %#v %v", explanationEvidence, err)
			}
			landingEvidence, err := f.service.readNativeRuntime(explanationCtx, scope, string(landingIssue.WorkItemID), "")
			if err != nil || slices.Contains(landingEvidence.Unavailable, "project_landing_capacity") != (total > 64) || len(landingEvidence.Capacity) == 0 {
				t.Fatalf("live landing capacity lost bounded current facts: %#v error=%v", landingEvidence, err)
			}
			for _, view := range landingEvidence.Capacity {
				if slices.Contains(view.Exclusions, "landing_capacity_unknown") != (total > 64) {
					t.Fatalf("live landing readiness differs from bounded owner: %#v", view)
				}
			}
			if total == 101 {
				t.Run("normal landing lease outside runner window", func(t *testing.T) {
					outside := all[0]
					for _, runner := range all {
						if runner.binding.RunnerID > outside.binding.RunnerID {
							outside = runner
						}
					}
					machineID := hubapi.NewID("machine")
					if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: scope.Organization, ID: machineID, Hostname: "Example outside host", Capacity: 8, Version: "test"})
					}); err != nil {
						t.Fatal(err)
					}
					originalNow := now
					var hiddenLease leasestore.Lease
					t.Cleanup(func() {
						now = originalNow
						cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
						defer cancel()
						if hiddenLease.LeaseID != "" {
							if err := repository.WriteTenant(cleanup, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
								_, err := w.Release(cleanup, leasestore.Fence{LeaseID: hiddenLease.LeaseID, FencingToken: hiddenLease.FencingToken}, "completed")
								return err
							}); err != nil {
								t.Error(err)
							}
						}
						if _, err := admin.ExecContext(cleanup, `WITH enrollment AS (UPDATE runner_enrollments SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 RETURNING id) UPDATE runner_identities SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 AND enrollment_id IN(SELECT id FROM enrollment)`, scope.Organization, outside.binding.RunnerID, outside.binding.MachineID); err != nil {
							t.Error(err)
						}
					})
					if _, err := admin.ExecContext(t.Context(), `WITH enrollment AS (UPDATE runner_enrollments SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 RETURNING id) UPDATE runner_identities SET machine_id=$3 WHERE organization_id=$1 AND runner_id=$2 AND enrollment_id IN(SELECT id FROM enrollment)`, scope.Organization, outside.binding.RunnerID, machineID); err != nil {
						t.Fatal(err)
					}
					hiddenIssue := f.create(t, "Example outside landing lease")
					if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						item, err := w.WorkItem(t.Context(), scope.Project, hiddenIssue.WorkItemID)
						if err != nil {
							return err
						}
						hiddenLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machineID, RunnerID: outside.binding.RunnerID, SessionID: "landing/outside-window", LeaseID: hubapi.NewID("lease"), TTL: time.Hour, LandingCapacity: 1})
						if err != nil {
							return err
						}
						return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: scope.Organization, ProjectID: scope.Project, LeaseID: tracker.LeaseID(hiddenLease.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: hiddenLease.RenewedAt})
					}); err != nil {
						t.Fatal(err)
					}
					now = hiddenLease.RenewedAt
					selected, partial, err := runtimeCapacityQueries(t, f.service, t.Context(), scope, now)
					if err != nil || !partial || len(selected) != 100 {
						t.Fatalf("normal runner window lost its bound: count=%d partial=%v error=%v", len(selected), partial, err)
					}
					for _, runner := range selected {
						if runner.RunnerID == outside.binding.RunnerID || string(runner.MachineID) == machineID {
							t.Fatalf("landing lease was not outside selected runner machines: %#v", runner)
						}
					}
					landing, err := f.service.readNativeRuntime(t.Context(), scope, string(landingIssue.WorkItemID), "")
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
				if runner.binding.RunnerID > currentScope.Credential.Runner.RunnerID {
					currentScope.Credential.Runner.RunnerID = runner.binding.RunnerID
				}
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
				defer cancel()
				if _, err := admin.ExecContext(cleanup, `UPDATE runner_identities SET removed_at=NULL WHERE organization_id=$1`, scope.Organization); err != nil {
					t.Error(err)
				}
				if _, err := admin.ExecContext(cleanup, `INSERT INTO token_grants(organization_id,token_id,project_id) SELECT organization_id,token_id,$2 FROM runner_identities WHERE organization_id=$1 ON CONFLICT DO NOTHING`, scope.Organization, scope.Project); err != nil {
					t.Error(err)
				}
			})
			var err error
			if sparse == "ungranted" {
				_, err = admin.ExecContext(t.Context(), `DELETE FROM token_grants WHERE organization_id=$1 AND token_id IN(SELECT token_id FROM runner_identities WHERE organization_id=$1 AND runner_id<>$2)`, scope.Organization, currentScope.Credential.Runner.RunnerID)
			} else {
				_, err = admin.ExecContext(t.Context(), `UPDATE runner_identities SET removed_at=$3 WHERE organization_id=$1 AND runner_id<>$2`, scope.Organization, currentScope.Credential.Runner.RunnerID, now)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), hubcontent.ExplanationReadKey{}, true)
			candidates, partial, err := runtimeCapacityQueries(t, f.service, ctx, scope, now)
			if err != nil || !partial || len(candidates) != 0 {
				t.Fatalf("candidate search escaped sparse owner window: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
			candidates, partial, err = runtimeCapacityQueries(t, f.service, ctx, currentScope, now)
			if err != nil || !partial || len(candidates) != 1 || candidates[0].RunnerID != currentScope.Credential.Runner.RunnerID {
				t.Fatalf("selected runner lost priority: count=%d partial=%v err=%v", len(candidates), partial, err)
			}
			if _, err := admin.ExecContext(t.Context(), `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=(SELECT token_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2)`, scope.Organization, currentScope.Credential.Runner.RunnerID); err != nil {
				t.Fatal(err)
			}
			candidates, partial, err = runtimeCapacityQueries(t, f.service, ctx, currentScope, now)
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
	runners, _, err := runtimeCapacityQueries(t, f.service, t.Context(), hubscope.NativeScope{Organization: foreign.project.OrganizationID, Project: foreign.project.ID, Credential: foreignCredential}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("cross-organization capacity leaked: %#v %v", runners, err)
	}
	other := newNativeFixture(t, f.service, scope.Organization, "runtime-ungranted")
	runners, _, err = runtimeCapacityQueries(t, f.service, t.Context(), hubscope.NativeScope{Organization: scope.Organization, Project: other.project.ID, Credential: scope.Credential}, now)
	if err != nil || len(runners) != 0 {
		t.Fatalf("ungranted project capacity leaked: %#v %v", runners, err)
	}
	runners, _, err = runtimeCapacityQueries(t, f.service, t.Context(), scope, now.Add(91*time.Second))
	if err != nil {
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
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=(SELECT token_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2) AND project_id=$3`, scope.Organization, first.binding.RunnerID, scope.Project); err != nil {
		t.Fatal(err)
	}
	runners, truncated, err := runtimeCapacityQueries(t, f.service, t.Context(), scope, now)
	if err != nil || truncated || len(runners) != 100 {
		t.Fatalf("project grant removal did not restrict the page: runners=%d truncated=%v error=%v", len(runners), truncated, err)
	}
	for _, r := range runners {
		if r.RunnerID == first.binding.RunnerID || r.HostUsed != 1 || r.Used != 0 || r.ProviderCapacity[0].Used != 1 {
			t.Fatalf("occupancy outside the granted runner page was lost: %#v", r)
		}
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET scope='organization' WHERE organization_id=$1 AND runner_id=$2`, scope.Organization, first.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	organizationScope := scope
	organizationScope.Credential.Runner.RunnerID = first.binding.RunnerID
	runners, truncated, err = runtimeCapacityQueries(t, f.service, t.Context(), organizationScope, now)
	if err != nil || !truncated || len(runners) != 100 || runners[0].RunnerID != first.binding.RunnerID || !runners[0].AllowsProject(scope.Project) {
		t.Fatalf("organization-scoped runner required a project grant: %#v partial=%v error=%v", runners, truncated, err)
	}
	organizationEvidence, err := f.service.readNativeRuntime(t.Context(), organizationScope, string(issue.WorkItemID), "")
	if err != nil || len(organizationEvidence.Capacity) != 100 || organizationEvidence.Capacity[0].RunnerID != first.binding.RunnerID || slices.Contains(organizationEvidence.Capacity[0].Exclusions, "project_access_denied") {
		t.Fatalf("live runtime dropped organization-scoped runner without project grant: %#v %v", organizationEvidence, err)
	}
}

func TestExplanationProviderCapacityCandidateWindow(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f, repository, _, admin, scope := newRuntimeCapacityFixture(t, "capacity-window", &now)
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
	if err := repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		reservation, err = r.ProviderReservation(t.Context(), lease.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), hubcontent.ExplanationReadKey{}, true)
	previous := 1
	for _, total := range []int{1024, 2048} {
		_, err := admin.ExecContext(t.Context(), `WITH added AS (
INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name,native_created_at,native_updated_at)
SELECT $1,$2,'candidate_'||n,n+1,'Example candidate','Todo',$5,$5 FROM generate_series($3::integer,$4::integer)n RETURNING organization_id,project_id,id,native_id
), owned AS (
INSERT INTO leases(organization_id,project_id,runner_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at)
SELECT organization_id,project_id,$7,'capacity_'||native_id,id,$6,native_id,1,$5,$5,$5::timestamptz+interval '2 minutes' FROM added RETURNING organization_id,lease_id,runner_id,acquired_at
) INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at) SELECT organization_id,lease_id,runner_id,acquired_at FROM owned`, scope.Organization, scope.Project, previous, total-1, now, first.binding.MachineID, first.binding.RunnerID)
		if err != nil {
			t.Fatal(err)
		}
		previous = total
		for _, absent := range []bool{false, true} {
			t.Run(fmt.Sprintf("leases=%d/absent=%v", total, absent), func(t *testing.T) {
				if absent {
					if _, err := admin.ExecContext(t.Context(), `DELETE FROM provider_reservations WHERE organization_id=$1`, scope.Organization); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
						defer cancel()
						if err := repository.WriteTenant(cleanup, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return w.CreateProviderReservation(cleanup, reservation) }); err != nil {
							t.Error(err)
						}
					})
				}
				var facts runnerstore.RuntimeCapacitySnapshot
				err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					owner, ok := r.(runnerstore.RuntimeCapacityReader)
					if !ok {
						return hubapi.NativeRunEventStorageUnavailable()
					}
					var err error
					facts, err = owner.RuntimeCapacitySnapshot(ctx, runnerstore.RuntimeCapacityQuery{ProjectID: scope.Project, At: now, Explanation: true})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				snapshot := runtimeProviderCapacity(facts.Providers)
				wantReservations := 1
				if absent {
					wantReservations = 0
				}
				if snapshot.truncated != (total > 1024) || len(snapshot.reservations) != wantReservations {
					t.Fatalf("reservations=%d partial=%v error=%v", len(snapshot.reservations), snapshot.truncated, err)
				}
				evidence, err := f.service.readNativeRuntime(ctx, scope, string(issue.WorkItemID), "")
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
