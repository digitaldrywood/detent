package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coordinatorActionProject(t *testing.T, f coordinatorActionFixture, change func(*projectstore.Record)) {
	t.Helper()
	f.writeTenant(t, func(w tenantstore.Writer) error {
		row, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		change(&row)
		_, err = w.UpdateProjectRecord(t.Context(), row)
		return err
	})
}
func coordinatorActionState(t *testing.T, f coordinatorActionFixture, name string, transitions []string) {
	t.Helper()
	coordinatorActionProject(t, f, func(p *projectstore.Record) {
		for i := range p.States {
			if p.States[i].Name == "Todo" && name == "Blocked" {
				p.States[i].Transitions = append(p.States[i].Transitions, name)
			}
			if p.States[i].Name == name {
				p.States[i].Transitions = transitions
				return
			}
		}
		p.States = append(p.States, tracker.NativeState{Name: name, Transitions: transitions})
	})
}
func coordinatorActionItem(t *testing.T, f coordinatorActionFixture, id string, change func(*corestore.WorkItem)) {
	t.Helper()
	coordinatorActionChange(t, f.service, "org_security", f.project, id, change)
}
func coordinatorActionChange(t *testing.T, service *Service, org tracker.OrganizationID, project tracker.ProjectID, id string, change func(*corestore.WorkItem)) {
	t.Helper()
	row, version, err := service.readWorkItemRecord(t.Context(), nativeScope{organization: org, project: project}, id)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := service.hydrateWorkItemRecord(t.Context(), row, version)
	if err != nil {
		t.Fatal(err)
	}
	current := row
	change(&row)
	issue.Revision = tracker.Revision(row.Revision + 1)
	issue.Title = row.Title
	issue.State = valueOrEmpty(row.WorkflowStateName)
	issue.Archived = row.Archived
	issue.Priority = row.Priority
	var projectRecord projectstore.Record
	if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		projectRecord, err = r.ProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	plan := workItemRecordPlan{project: projectRecord, current: current, issue: issue}
	fields, err := service.prepareWorkItemFields(t.Context(), org, plan)
	if err != nil {
		t.Fatal(err)
	}
	version.Revision = row.Revision + 1
	version, err = eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), version, coordinatorActionJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	writeCoordinatorActionTenant(t, service, org, func(w tenantstore.Writer) error {
		if err := writeWorkItemRecord(t.Context(), w, plan, fields); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	})
}

func coordinatorActionJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func coordinatorActionPark(t *testing.T, f coordinatorActionFixture, id string, human bool) {
	t.Helper()
	coordinatorActionRecordPark(t, f, id, human, "initial-park")
}

func coordinatorActionRecordPark(t *testing.T, f coordinatorActionFixture, id string, human bool, suffix string) {
	t.Helper()
	leaseID := "park-lease-" + id + "-" + suffix
	var lease leasestore.Lease
	f.writeTenant(t, func(w tenantstore.Writer) error {
		item, err := w.WorkItemByNativeID(t.Context(), tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		machine := leasestore.Machine{OrganizationID: "org_security", ID: "park-machine-" + id, Hostname: "example-host", Capacity: 1, Version: "test"}
		if err := w.RegisterMachine(t.Context(), machine); err != nil {
			return err
		}
		if err := w.HeartbeatMachine(t.Context(), machine, f.service.config.now()); err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machine.ID, SessionID: leaseID, LeaseID: leaseID, TTL: time.Hour})
		return err
	})
	now := f.service.config.now()
	run := tracker.NativeRunData{AttemptID: "attempt_" + id + "_" + suffix, RunID: "run_" + id + "_" + suffix, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: tracker.MachineID(lease.MachineID), Sequence: 1, Outcome: "succeeded", Disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: human}}
	ref, err := f.service.runDetailStore().StoreDetail(t.Context(), "org_security", coordinatorActionJSON(t, run))
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), "org_security", coordinatorActionJSON(t, tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: "org_security", ProjectID: f.project, AggregateID: tracker.NativeWorkItemID(id), Type: "run.observed", SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: lease.MachineID}, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}))
	if err != nil {
		t.Fatal(err)
	}
	f.writeTenant(t, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: run.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: run.RunID, Sequence: 1, Status: "succeeded", StartedAt: now, PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
		return err
	})
}

func coordinatorActionLease(t *testing.T, service *Service, id string, durations ...time.Duration) {
	t.Helper()
	ttl := time.Hour
	if len(durations) > 0 {
		ttl = durations[0]
	}
	org := tracker.OrganizationID(service.appOrganizationID())
	writeCoordinatorActionTenant(t, service, org, func(w tenantstore.Writer) error {
		item, err := w.WorkItemByNativeID(t.Context(), tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		now := service.config.now()
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "archive-machine", Hostname: "example-host", DisplayName: "example", Capacity: 1, Version: "test"}); err != nil {
			return err
		}
		if err := w.HeartbeatMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "archive-machine", Hostname: "example-host", Capacity: 1, Version: "test"}, now); err != nil {
			return err
		}
		_, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "archive-machine", SessionID: "archive-session", LeaseID: "archive-lease", TTL: ttl})
		return err
	})
}
func coordinatorActionIssue(t *testing.T, f coordinatorActionFixture, id tracker.NativeWorkItemID) tracker.NativeIssue {
	t.Helper()
	row, version, err := f.service.readWorkItemRecord(t.Context(), nativeScope{organization: "org_security", project: f.project}, string(id))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := f.service.hydrateWorkItemRecord(t.Context(), row, version)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func coordinatorActionEvents(t *testing.T, f coordinatorActionFixture, id tracker.NativeWorkItemID) []tracker.CollaborationEvent {
	t.Helper()
	var rows []eventstore.Event
	f.readTenant(t, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.Events(t.Context(), f.project, id, 0, 200)
		return err
	})
	events := make([]tracker.CollaborationEvent, 0, len(rows))
	content := eventstore.NewContent(f.service.runDetailStore())
	for _, row := range rows {
		event, err := content.Event(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
func coordinatorActionComments(t *testing.T, f coordinatorActionFixture, id tracker.NativeWorkItemID) []tracker.NativeComment {
	t.Helper()
	var rows []corestore.Comment
	var versions []eventstore.CollaborationVersion
	f.readTenant(t, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.Comments(t.Context(), f.project, id, 0, 200)
		if err != nil {
			return err
		}
		for _, row := range rows {
			version, err := r.CollaborationVersion(t.Context(), row.ID, row.Revision)
			if err != nil {
				return err
			}
			versions = append(versions, version)
		}
		return nil
	})
	comments := make([]tracker.NativeComment, 0, len(rows))
	for i, row := range rows {
		comment, err := f.service.hydrateCommentRecord(t.Context(), row, versions[i])
		if err != nil {
			t.Fatal(err)
		}
		comments = append(comments, comment)
	}
	return comments
}
func coordinatorActionCandidates(t *testing.T, f coordinatorActionFixture) []tracker.WorkItemID {
	t.Helper()
	var ids []tracker.WorkItemID
	f.readTenant(t, func(r tenantstore.Reader) error {
		page, err := r.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: f.project, Scope: string(f.project), AvailableAt: f.service.config.now(), Limit: 200})
		for _, row := range page.Items {
			ids = append(ids, tracker.WorkItemID(row.ID))
		}
		return err
	})
	return ids
}
func coordinatorActionFault(t *testing.T, f coordinatorActionFixture, table, event, condition, reason string) {
	t.Helper()
	db := f.admin
	if _, err := db.ExecContext(t.Context(), "CREATE FUNCTION tf_conv_actions_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '%', TG_ARGV[0]; END $$"); err != nil {
		t.Fatal(err)
	}
	query := "CREATE TRIGGER tf_conv_actions_fault BEFORE " + event + " ON " + pgx.Identifier{table}.Sanitize() + " FOR EACH ROW WHEN (NEW.organization_id='org_security'" + condition + ") EXECUTE FUNCTION tf_conv_actions_fault(" + pgtransport.QuoteLiteral(reason) + ")"
	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}

type coordinatorActionFixture struct{ operatorSecurityFixture }

func (f coordinatorActionFixture) readTenant(t *testing.T, read func(tenantstore.Reader) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, read); err != nil {
		t.Fatal(err)
	}
}

func (f coordinatorActionFixture) writeTenant(t *testing.T, write func(tenantstore.Writer) error) {
	t.Helper()
	writeCoordinatorActionTenant(t, f.service, "org_security", write)
}

func writeCoordinatorActionTenant(t *testing.T, service *Service, org tracker.OrganizationID, write func(tenantstore.Writer) error) {
	t.Helper()
	if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, write); err != nil {
		t.Fatal(err)
	}
}

func coordinatorActionMember(t *testing.T, f coordinatorActionFixture, subject, role string, active bool) {
	t.Helper()
	f.writeTenant(t, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), subject)
		if err != nil {
			return err
		}
		member.Role, member.Active = role, active
		return w.UpdateAccessMember(t.Context(), member)
	})
}

func coordinatorActionReplaceLease(t *testing.T, f coordinatorActionFixture) {
	t.Helper()
	f.writeTenant(t, func(w tenantstore.Writer) error {
		lease, err := w.Lease(t.Context(), "archive-lease")
		if err != nil {
			return err
		}
		if _, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "replaced"); err != nil {
			return err
		}
		item, err := w.WorkItemByID(t.Context(), f.project, lease.IssueID)
		if err != nil {
			return err
		}
		_, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: lease.MachineID, SessionID: "replacement-session", LeaseID: "replacement-lease", TTL: time.Hour})
		return err
	})
}

func coordinatorActionHumanPark(t *testing.T, f coordinatorActionFixture, id string) {
	t.Helper()
	coordinatorActionRecordPark(t, f, id, true, "human-question")
}

func seedCoordinatorActionIssues(t *testing.T, service *Service, scope nativeScope, count int, state string, archivedLast ...bool) []tracker.NativeIssue {
	t.Helper()
	seedNativeFixturePolicy(t, service, string(scope.organization)+"/"+string(scope.project))
	issues := make([]tracker.NativeIssue, 0, count)
	for i := range count {
		request := tracker.CreateIssue{Title: fmt.Sprintf("Seed %d", i), Body: nativeContractTestBody, State: state}
		plan, err := service.loadWorkItemPlan(t.Context(), scope, "", "", request)
		if err != nil {
			t.Fatal(err)
		}
		plan, err = service.planWorkItemRecord(t.Context(), scope, plan, request)
		if err != nil {
			t.Fatal(err)
		}
		plan.issue.Archived = len(archivedLast) > 0 && archivedLast[0] && i == count-1
		fields, err := service.prepareWorkItemFields(t.Context(), scope.organization, plan)
		if err != nil {
			t.Fatal(err)
		}
		version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: string(plan.issue.WorkItemID), Revision: 1}, coordinatorActionJSON(t, plan.issue))
		if err != nil {
			t.Fatal(err)
		}
		writeCoordinatorActionTenant(t, service, scope.organization, func(w tenantstore.Writer) error {
			if err := writeWorkItemRecord(t.Context(), w, plan, fields); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		})
		issues = append(issues, plan.issue)
	}
	return issues
}
