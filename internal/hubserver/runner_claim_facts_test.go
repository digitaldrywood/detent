package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestRunnerClaimFactsRecoverOnlyVerifiedCurrentSource(t *testing.T) {
	const humanReport = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the plan\n```"
	const permissionReport = "```detent-status\nschema: 1\nstatus: blocked\nreason_code: permission_wait\nblockers: []\nhuman_action: null\n```"
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	details := &memoryRunDetails{}
	service := openPostgresHubTestService(t, repository, Config{RunDetails: details})
	content := eventstore.NewContent(details)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_shared", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index, test := range []struct {
			name, want, body                                                      string
			disposition                                                           *tracker.NativeDisposition
			acceptance, pending, corrupt, changed, newer, projectLock, incoherent bool
		}{
			{name: "human", want: "human_action", disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
			{name: "permission", want: "permission_wait", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait"}},
			{name: "planning", want: "progress", disposition: &tracker.NativeDisposition{Status: "in_progress"}},
			{name: "acceptance", want: "recorded_acceptance", acceptance: true},
			{name: "retained_uncertain_publication", want: "progress", pending: true, disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
			{name: "corrupt_retained_checkpoint", pending: true, corrupt: true, disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
			{name: "source_changes_after_read", changed: true, disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
			{name: "newer_attempt_after_read", newer: true, disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
			{name: "project_lock_order", projectLock: true, want: "human_action", disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}},
			{name: "ordinary_completion"},
			{name: "typed_complete", disposition: &tracker.NativeDisposition{Status: workpad.StatusComplete}},
			{name: "report_only_human", body: humanReport, want: "human_action"},
			{name: "report_only_permission", body: permissionReport, want: "permission_wait"},
			{name: "malformed_report", body: "```detent-status\nschema: 1\nstatus: blocked\nhuman_action: [\n```"},
			{name: "typed_complete_overrides_report", body: humanReport, disposition: &tracker.NativeDisposition{Status: workpad.StatusComplete}},
			{name: "pending_report_only_human", body: humanReport, pending: true, want: "progress"},
			{name: "incoherent_terminal_outcome", incoherent: true},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				t.Cleanup(func() { service.config.TenantRepository = repository })
				project := tracker.ProjectID("prj_shared")
				itemID := tracker.NativeWorkItemID("wi_" + test.name)
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: itemID, Number: index + 1, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: itemID, RecordID: string(itemID), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				var lease leasestore.Lease
				var item corestore.WorkItem
				machine := "machine_" + test.name
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					item, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: itemID, Number: int64(issue.Number), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State})
					if err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machine, Hostname: "example", Capacity: 1}); err != nil {
						return err
					}
					lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machine, SessionID: "session_" + test.name, LeaseID: "lease_" + test.name, TTL: time.Hour})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var checkpoint attachment.ArchiveReference
				if test.pending {
					raw, err := json.Marshal(checkpointPayload{Checkpoint: &tracker.NativeCheckpoint{ExternalEffect: "git_push", EffectState: "pending"}})
					if err != nil {
						t.Fatal(err)
					}
					checkpoint, err = details.StoreDetail(t.Context(), string(organization), raw)
					if err != nil {
						t.Fatal(err)
					}
				}
				record := func(current leasestore.Lease, id, status string, disposition *tracker.NativeDisposition, acceptance bool, retained attachment.ArchiveReference) leasestore.Attempt {
					t.Helper()
					run := tracker.NativeRunData{AttemptID: id, RunID: "run_" + id, LeaseID: tracker.LeaseID(current.LeaseID), FencingToken: tracker.FencingToken(current.FencingToken), Sequence: 1, MachineID: tracker.MachineID(current.MachineID), Disposition: disposition, CompletionBody: test.body}
					kind := "run.started"
					if status == "succeeded" {
						run.Outcome, kind = "succeeded", "run.finished"
					}
					if test.incoherent {
						run.Outcome = "failed"
					}
					if acceptance {
						run.Runtime = &tracker.NativeRuntimeObservation{Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: true}}
					}
					raw, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					payload, err := details.StoreDetail(t.Context(), string(organization), raw)
					if err != nil {
						t.Fatal(err)
					}
					value := tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: organization, ProjectID: project, AggregateType: "issue", AggregateID: itemID, Type: kind, SchemaVersion: 1, Actor: issue.Actor, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}
					raw, err = json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					event, err := content.PrepareEvent(t.Context(), organization, raw)
					if err != nil {
						t.Fatal(err)
					}
					var attempt leasestore.Attempt
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						data := leasestore.AttemptEvent{ID: id, Fence: leasestore.Fence{LeaseID: current.LeaseID, FencingToken: current.FencingToken}, RunID: run.RunID, Sequence: 1, Status: status, StartedAt: current.AcquiredAt, PayloadObjectKey: payload.Key, PayloadReference: payload}
						if retained.Key != "" {
							data.CheckpointObjectKey, data.CheckpointReference = &retained.Key, retained
						}
						var err error
						attempt, err = w.RecordAttempt(t.Context(), data)
						if err != nil {
							return err
						}
						if _, err := w.AppendEvent(t.Context(), event); err != nil {
							return err
						}
						if status == "succeeded" {
							_, err = w.Release(t.Context(), data.Fence, "completed")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return attempt
				}
				attempt := record(lease, "attempt_"+test.name, "succeeded", test.disposition, test.acceptance, checkpoint)
				if attempt.ClaimDisposition != nil {
					t.Fatalf("legacy fixture already has claim facts: %+v", attempt)
				}
				legacyPlan := nativeClaimPlan{candidate: claimstore.Candidate{State: "Todo"}, item: item, latestAttempt: attempt, latestAttemptFound: true}
				_, unknownErr := service.hydrateNativeClaimPlan(t.Context(), nativeScope{organization: organization, project: project}, tracker.NativeClaim{}, lease.RenewedAt.Add(time.Second), legacyPlan, nil)
				var unknown *nativeError
				if !errors.As(unknownErr, &unknown) || unknown.status != http.StatusServiceUnavailable {
					t.Fatalf("unknown facts did not fail closed: %v", unknownErr)
				}

				if test.corrupt {
					details.mu.Lock()
					details.data[checkpoint.Key] = []byte(`{}`)
					details.mu.Unlock()
				}
				if test.changed || test.newer {
					service.config.TenantRepository = &claimFactsRecoveryRepository{Repository: repository, beforeWrite: func() error {
						if test.changed {
							_, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET sequence=sequence+1,payload_object_key=payload_object_key||'/new' WHERE organization_id=$1 AND id=$2`, organization, attempt.ID)
							return err
						}
						var next leasestore.Lease
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							var err error
							next, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machine, SessionID: "session_next_" + test.name, LeaseID: "lease_next_" + test.name, TTL: time.Hour})
							return err
						}); err != nil {
							return err
						}
						record(next, attempt.ID+"_next", "running", nil, false, attachment.ArchiveReference{})
						return nil
					}}
				}
				candidate := viewstore.BoardCandidate{ProjectID: project, WorkItemID: itemID}
				if test.projectLock {
					err = refreshClaimFactsWithBlockedProject(t, service, repository, admin, organization, candidate, attempt.ID, lease.RenewedAt.Add(time.Second))
				} else {
					err = service.refreshBoardRecord(t.Context(), organization, candidate, lease.RenewedAt.Add(time.Second))
				}
				service.config.TenantRepository = repository
				if test.changed || test.newer {
					if !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("changed source recovery error=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					current, err := r.Attempt(t.Context(), attempt.ID)
					if err != nil {
						return err
					}
					if test.corrupt || test.changed || test.newer || test.incoherent {
						if current.ClaimDisposition != nil {
							t.Fatalf("unverified source published claim facts: %+v", current)
						}
					} else if current.ClaimDisposition == nil || *current.ClaimDisposition != test.want {
						t.Fatalf("recovered disposition=%v want %q", current.ClaimDisposition, test.want)
					}
					if current.ClaimDisposition != nil {
						plan := legacyPlan
						plan.latestAttempt = current
						wantHeld := test.want == "human_action" || test.want == "permission_wait" || test.want == "recorded_acceptance"
						if got := nativeClaimDispatchSatisfied(plan); got != wantHeld {
							t.Fatalf("unrequested work held=%t want %t", got, wantHeld)
						}
						plan.item.DispatchRequestedAt = &now
						if got := nativeClaimDispatchSatisfied(plan); got != (test.want != "progress") {
							t.Fatalf("explicit generation consumed=%t for %q", got, test.want)
						}
						plan.item.Revision++
						if nativeClaimDispatchSatisfied(plan) {
							t.Fatal("historical request consumed a newer item revision")
						}
						plan.latestAttempt.WorkItemRevision = plan.item.Revision
						plan.latestAttempt.ClaimDisposition = new("progress")
						if nativeClaimDispatchSatisfied(plan) {
							t.Fatal("new progress consumed a historical request")
						}
						plan.latestAttempt = current
						plan.item = item
						plan.item.DispatchGeneration = current.DispatchGeneration + 1
						if nativeClaimDispatchSatisfied(plan) {
							t.Fatal("older result consumed a newer dispatch generation")
						}
					}
					if test.newer {
						latest, err := r.LatestAttempt(t.Context(), item.ID)
						if err != nil {
							return err
						}
						if latest.ID != attempt.ID+"_next" || latest.Status != "running" || latest.ClaimDisposition != nil {
							t.Fatalf("older recovery changed current attempt: %+v", latest)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if test.name == "planning" {
					if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET claim_disposition=NULL WHERE organization_id=$1 AND id=$2`, organization, attempt.ID); err != nil {
						t.Fatal(err)
					}
					ctx := (hubmaintenance.Budget{Items: 32, Bytes: hubmaintenance.ByteLimit}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
					var after tracker.NativeWorkItemID
					for range 32 {
						next, processed, err := service.MaintainBoardRecord(ctx, organization, after)
						if err != nil {
							t.Fatal(err)
						}
						if !processed {
							break
						}
						after = next
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						got, err := r.Attempt(t.Context(), attempt.ID)
						if err == nil && (got.ClaimDisposition == nil || *got.ClaimDisposition != "progress") {
							t.Fatalf("current board skipped unresolved claim facts: %+v", got)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

type claimFactsRecoveryRepository struct {
	*postgres.Repository
	beforeWrite func() error
	once        sync.Once
	err         error
}

func (r *claimFactsRecoveryRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.once.Do(func() { r.err = r.beforeWrite() })
	if r.err != nil {
		return r.err
	}
	return r.Repository.WriteTenant(ctx, organization, workload, write)
}

type claimFactsProjectionRepository struct {
	*postgres.Repository
	attempted chan struct{}
	once      sync.Once
}

type claimFactsProjectionWriter struct {
	tenantstore.Writer
	order *claimFactsProjectionRepository
}

func (r *claimFactsProjectionRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error { return write(claimFactsProjectionWriter{Writer: w, order: r}) })
}

func (w claimFactsProjectionWriter) ReplaceBoardItem(ctx context.Context, update viewstore.BoardUpdate) error {
	w.order.once.Do(func() { close(w.order.attempted) })
	return w.Writer.ReplaceBoardItem(ctx, update)
}

func refreshClaimFactsWithBlockedProject(t *testing.T, service *Service, repository *postgres.Repository, admin *sql.DB, organization tracker.OrganizationID, candidate viewstore.BoardCandidate, attemptID string, now time.Time) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocker, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var project string
	if err := blocker.QueryRowContext(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organization, candidate.ProjectID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	order := &claimFactsProjectionRepository{Repository: repository, attempted: make(chan struct{})}
	previous := service.config.TenantRepository
	service.config.TenantRepository = order
	finished := make(chan struct{})
	joined := false
	var refreshErr error
	defer func() {
		blocker.Rollback()
		cancel()
		if !joined {
			<-finished
		}
		service.config.TenantRepository = previous
	}()
	go func() {
		defer close(finished)
		refreshErr = service.refreshBoardRecord(ctx, organization, candidate, now)
	}()
	select {
	case <-order.attempted:
	case <-finished:
		joined = true
		t.Fatalf("projection did not reach project lock: %v", refreshErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var attempt string
	if err := blocker.QueryRowContext(ctx, `SELECT id FROM native_attempts WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT`, organization, attemptID).Scan(&attempt); err != nil {
		t.Fatalf("projection locked attempt while waiting for project: %v", err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		joined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return refreshErr
}
