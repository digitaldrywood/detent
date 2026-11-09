package postgres_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRecordedRecoveryRejectsChangedSourceAndDependencies(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	content := eventstore.NewContent(newHistoryObjects())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		recovery := seedRecordedRecovery(t, repository, admin, content, organization)
		for _, test := range []struct {
			name   string
			mutate func(*leasestore.RecordedRecovery)
			edit   bool
			claim  bool
			newer  bool
			want   error
		}{
			{name: "current source"},
			{name: "sequence", mutate: func(r *leasestore.RecordedRecovery) { r.Attempt.Sequence++ }, want: tracker.ErrStaleFencingToken},
			{name: "payload", mutate: func(r *leasestore.RecordedRecovery) { r.Attempt.PayloadObjectKey += "/changed" }, want: tracker.ErrStaleFencingToken},
			{name: "checkpoint", mutate: func(r *leasestore.RecordedRecovery) { r.Attempt.CheckpointObjectKey = new("different") }, want: tracker.ErrStaleFencingToken},
			{name: "lease", mutate: func(r *leasestore.RecordedRecovery) { r.Attempt.LeaseID += "_different" }, want: tracker.ErrStaleFencingToken},
			{name: "fence", mutate: func(r *leasestore.RecordedRecovery) { r.Attempt.FencingToken++ }, want: tracker.ErrStaleFencingToken},
			{name: "report", mutate: func(r *leasestore.RecordedRecovery) { r.ReportEventID += "_different" }, want: corestore.ErrRevisionConflict},
			{name: "report sequence", mutate: func(r *leasestore.RecordedRecovery) { r.ReportSequence++ }, want: corestore.ErrRevisionConflict},
			{name: "workflow", mutate: func(r *leasestore.RecordedRecovery) { r.WorkflowObjectKey += "/changed" }, want: corestore.ErrRevisionConflict},
			{name: "workflow sequence", mutate: func(r *leasestore.RecordedRecovery) { r.WorkflowSequence++ }, want: corestore.ErrRevisionConflict},
			{name: "missing relation", mutate: func(r *leasestore.RecordedRecovery) { r.DependencyNumbers = []int64{3} }, want: corestore.ErrRevisionConflict},
			{name: "duplicate relation", mutate: func(r *leasestore.RecordedRecovery) { r.DependencyNumbers = []int64{2, 2} }, want: corestore.ErrRevisionConflict},
			{name: "reported reference overflow", mutate: func(r *leasestore.RecordedRecovery) { r.DependencyNumbers = make([]int64, corestore.MaxPageSize+1) }, want: corestore.ErrRevisionConflict},
			{name: "tenant", mutate: func(r *leasestore.RecordedRecovery) { r.Attempt.OrganizationID = "org_other" }, want: corestore.ErrRevisionConflict},
			{name: "source edited after report", edit: true, want: corestore.ErrRevisionConflict},
			{name: "current source after refused edit"},
			{name: "new active claim", claim: true, want: tracker.ErrStaleFencingToken},
			{name: "newer released attempt", claim: true, newer: true, want: tracker.ErrStaleFencingToken},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				changed := recovery
				if test.mutate != nil {
					test.mutate(&changed)
				}
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockRelationshipGraph(t.Context()); err != nil {
						return err
					}
					if _, err := w.LockWorkItem(t.Context(), recovery.ProjectID, recovery.WorkItemID); err != nil {
						return err
					}
					if test.edit {
						value := tracker.CollaborationEvent{ID: "later_edit", OrganizationID: organization, ProjectID: recovery.ProjectID, AggregateID: recovery.WorkItemID, Type: "issue.edited", SchemaVersion: 1, Actor: historyActor(), RecordedAt: recovery.Now, Data: tracker.CollaborationData{Revision: 3}}
						event, err := content.PrepareEvent(t.Context(), organization, historyJSON(t, value))
						if err != nil {
							return err
						}
						if _, err := w.AppendEvent(t.Context(), event); err != nil {
							return err
						}
					}
					if test.claim {
						request := leaseClaim(1)
						request.LeaseID, request.SessionID = "new_lease", "new_session"
						lease, err := w.Claim(t.Context(), request)
						if err != nil {
							return err
						}
						if test.newer {
							data := attemptEvent(lease)
							data.Status = "succeeded"
							if _, err := w.RecordAttempt(t.Context(), data); err != nil {
								return err
							}
							if _, err := w.Release(t.Context(), leaseFence(lease), "completed"); err != nil {
								return err
							}
						}
					}
					return w.(leasestore.RecordedRecoveryWriter).AuthorizeRecordedRecovery(t.Context(), changed)
				})
				if !errors.Is(err, test.want) {
					t.Fatalf("recovery error=%v want %v", err, test.want)
				}
			})
		}
	}
}

func seedRecordedRecovery(t *testing.T, repository *postgres.Repository, admin *sql.DB, content *eventstore.Content, organization tracker.OrganizationID) leasestore.RecordedRecovery {
	t.Helper()
	seedLeaseFamily(t, repository, organization)
	lease := claimLease(t, repository, organization, leaseClaim(1))
	for _, statement := range []string{
		`INSERT INTO project_workflow_states(organization_id,project_id,name,board_rank,terminal,dispatchable,operator_only) VALUES($1,'prj_1','Done',0,true,false,false)`,
		`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,workflow_state_name) VALUES($1,3,'wi_dependency','prj_1',2,'Example prerequisite','Done')`,
		`INSERT INTO issue_dependencies(organization_id,dependent_project_id,dependent_issue_id,blocker_project_id,blocker_issue_id) VALUES($1,'prj_1',1,'prj_1',3)`,
	} {
		if _, err := admin.ExecContext(t.Context(), statement, organization); err != nil {
			t.Fatal(err)
		}
	}
	var attempt leasestore.Attempt
	var report, lane eventstore.Event
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		data := attemptEvent(lease)
		data.Status, data.CheckpointObjectKey = "succeeded", new("example/checkpoint")
		var err error
		attempt, err = w.RecordAttempt(t.Context(), data)
		if err != nil {
			return err
		}
		for index, kind := range []string{"run.finished", "workflow.transitioned"} {
			value := tracker.CollaborationEvent{ID: kind, OrganizationID: organization, ProjectID: "prj_1", AggregateID: "wi_1", Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example"}, RecordedAt: lease.AcquiredAt, Data: tracker.CollaborationData{Revision: tracker.Revision(index + 1)}}
			if index == 0 {
				value.Data.Run = &tracker.NativeRunData{AttemptID: attempt.ID}
			}
			event, err := content.PrepareEvent(t.Context(), organization, historyJSON(t, value))
			if err != nil {
				return err
			}
			event, err = w.AppendEvent(t.Context(), event)
			if err != nil {
				return err
			}
			if index == 0 {
				report = event
			} else {
				lane = event
			}
		}
		_, err = w.Release(t.Context(), leaseFence(lease), "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return leasestore.RecordedRecovery{ProjectID: "prj_1", WorkItemID: "wi_1", Attempt: attempt, ReportEventID: report.ID, ReportSequence: report.Sequence, ReportRevision: 1, WorkflowEventID: lane.ID, WorkflowSequence: lane.Sequence, WorkflowObjectKey: lane.PayloadObjectKey, DependencyNumbers: []int64{2}, Now: time.Now().UTC()}
}
