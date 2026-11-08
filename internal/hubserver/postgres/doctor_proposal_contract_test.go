package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDoctorProposalContractInitializesWithAtomicOutcome(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	content := eventstore.NewContent(newHistoryObjects())
	filer := tracker.Actor{Kind: "human", PrincipalID: "example"}
	var requests []postgres.DoctorProposalInitialization
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error { return seedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"first", "second"} {
			project := tracker.ProjectID("prj_" + suffix)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: suffix, Profile: "native", ContentSettingsRevision: 4, WorkflowSource: "example", WorkflowSourceRevision: "workflow-1"}, States: []tracker.NativeState{{Name: "Backlog", Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true, Transitions: []string{"Backlog"}}}})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			policy, err := content.PrepareProjectPolicy(t.Context(), org, importedCurrentPolicyDescriptor())
			if err != nil {
				t.Fatal(err)
			}
			policy.ProjectID, policy.ApprovedBy, policy.ApprovedAt = project, "operator", historyAt()
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.ApproveProjectPolicy(t.Context(), policy, "") }); err != nil {
				t.Fatal(err)
			}
			var stored projectstore.Record
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				stored, err = r.ProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for number := int64(1); number <= 2; number++ {
				item := tracker.NativeWorkItemID(fmt.Sprintf("proposal_%s_%d", suffix, number))
				state := "Backlog"
				if number == 2 {
					state = "Todo"
				}
				before, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 1}, historyJSON(t, map[string]any{"id": item, "project": project, "revision": 1, "state": state}))
				if err != nil {
					t.Fatal(err)
				}
				after, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 2}, historyJSON(t, map[string]any{"id": item, "project": project, "revision": 2, "state": "Todo"}))
				if err != nil {
					t.Fatal(err)
				}
				at := time.Now().UTC().Truncate(time.Microsecond)
				event, err := content.PrepareEvent(t.Context(), org, historyJSON(t, tracker.CollaborationEvent{ID: "init_" + string(item), OrganizationID: org, ProjectID: project, AggregateType: "issue", AggregateID: item, Type: "workflow.transitioned", SchemaVersion: 1, Actor: filer, RecordedAt: at, Data: tracker.CollaborationData{Revision: 2, FromState: state, ToState: "Todo"}}))
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "operator-key", Operation: postgres.DoctorProposalOperation, CommandKey: string(project) + "/" + string(item), RequestHash: artifact.Digest([]byte(string(project) + "/" + string(item))), CreatedAt: at}, []byte(`{"initialized":true}`))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: number, Title: "Example proposal", ActorKind: new(filer.Kind), ActorPrincipalID: new(filer.PrincipalID), WorkflowStateName: new(state)}); err != nil {
						return err
					}
					_, err := w.CreateCollaborationVersion(t.Context(), before)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, postgres.DoctorProposalInitialization{OrganizationID: org, ProjectID: project, WorkItemID: item, Filer: filer, ExpectedRevision: 1, ExpectedVersionSHA256: before.PayloadSHA256, ExpectedState: state, TargetState: "Todo", ContentSettingsRevision: stored.Core.ContentSettingsRevision, SettingsRevision: stored.Settings.Revision, WorkflowSourceRevision: stored.Core.WorkflowSourceRevision, PolicyID: policy.PolicyID, PolicySHA256: policy.PayloadSHA256, At: at, Version: after, Event: event, Receipt: receipt})
			}
		}
	}
	apply := func(arg postgres.DoctorProposalInitialization) postgres.DoctorProposalApply {
		return func(ctx context.Context, w tenantstore.Writer) error {
			row, err := w.WorkItem(ctx, arg.ProjectID, arg.WorkItemID)
			if err != nil {
				return err
			}
			row.WorkflowStateName = new(arg.TargetState)
			row.UpdatedAt, row.NativeUpdatedAt, row.LastActivityAt = arg.At, arg.At, arg.At
			_, err = w.UpdateWorkItem(ctx, row)
			return err
		}
	}
	invoke := func(arg postgres.DoctorProposalInitialization, change postgres.DoctorProposalApply) error {
		return repository.WriteDoctorProposals(t.Context(), arg.OrganizationID, corestore.Serving, func(w postgres.DoctorProposalWriter) error {
			_, err := w.InitializeDoctorProposalLane(t.Context(), arg, change)
			return err
		})
	}
	for _, arg := range requests {
		if arg.ExpectedState != arg.TargetState {
			for _, tc := range []struct {
				name   string
				mutate func(*postgres.DoctorProposalInitialization)
			}{
				{"revision", func(a *postgres.DoctorProposalInitialization) { a.ExpectedRevision = 2 }},
				{"source", func(a *postgres.DoctorProposalInitialization) {
					a.ExpectedVersionSHA256 = artifact.Digest([]byte("stale"))
				}},
				{"source key", func(a *postgres.DoctorProposalInitialization) { a.ExpectedSourceKey = new("foreign") }},
				{"policy", func(a *postgres.DoctorProposalInitialization) { a.PolicyID = "other" }},
				{"policy source", func(a *postgres.DoctorProposalInitialization) { a.PolicySHA256 = artifact.Digest([]byte("stale")) }},
				{"workflow source", func(a *postgres.DoctorProposalInitialization) { a.WorkflowSourceRevision = "other" }},
				{"settings", func(a *postgres.DoctorProposalInitialization) { a.SettingsRevision++ }},
				{"filer", func(a *postgres.DoctorProposalInitialization) { a.Filer.PrincipalID = "other" }},
				{"foreign scope", func(a *postgres.DoctorProposalInitialization) { a.OrganizationID = "foreign" }},
				{"fence", func(a *postgres.DoctorProposalInitialization) {
					a.Fence = leasestore.Fence{LeaseID: "stale", FencingToken: 1}
				}},
			} {
				changed := arg
				tc.mutate(&changed)
				if err := invoke(changed, apply(changed)); err == nil {
					t.Fatalf("%s initialization accepted", tc.name)
				}
			}
			bad := arg
			bad.Receipt.PayloadBytes = 0
			if err := invoke(bad, apply(bad)); err == nil {
				t.Fatal("invalid outcome receipt committed initialization")
			}
			if err := repository.ReadTenant(t.Context(), arg.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), arg.ProjectID, arg.WorkItemID)
				if err != nil {
					return err
				}
				if row.Revision != 1 || row.EventSequence != 0 || row.WorkflowStateName == nil || *row.WorkflowStateName != "Backlog" {
					t.Fatalf("failed initialization changed item=%+v", row)
				}
				if _, err := r.CollaborationVersion(t.Context(), string(arg.WorkItemID), 2); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("failed initialization retained version: %v", err)
				}
				events, err := r.Events(t.Context(), arg.ProjectID, arg.WorkItemID, 0, 10)
				if err == nil && len(events) != 0 {
					t.Fatalf("failed initialization retained events=%+v", events)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if arg.ExpectedState == arg.TargetState {
			machine := "machine_" + string(arg.ProjectID)
			var item corestore.WorkItem
			if err := repository.ReadTenant(t.Context(), arg.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				item, err = r.WorkItem(t.Context(), arg.ProjectID, arg.WorkItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteLeases(t.Context(), arg.OrganizationID, corestore.Serving, func(w leasestore.Writer) error {
				return w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: arg.OrganizationID, ID: machine, Hostname: "example", Capacity: 1, Version: "example"})
			}); err != nil {
				t.Fatal(err)
			}
			lease := claimLease(t, repository, arg.OrganizationID, leasestore.Claim{IssueID: item.ID, ExpectedRevision: 1, MachineID: machine, SessionID: "session_" + string(arg.WorkItemID), LeaseID: "lease_" + string(arg.WorkItemID), TTL: time.Hour})
			changed := arg
			changed.TargetState = "Backlog"
			var err error
			changed.Version, err = content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: arg.OrganizationID, ProjectID: arg.ProjectID, WorkItemID: arg.WorkItemID, RecordID: string(arg.WorkItemID), Revision: 2}, historyJSON(t, map[string]any{"id": arg.WorkItemID, "project": arg.ProjectID, "revision": 2, "state": "Backlog"}))
			if err != nil {
				t.Fatal(err)
			}
			changed.Event, err = content.PrepareEvent(t.Context(), arg.OrganizationID, historyJSON(t, tracker.CollaborationEvent{ID: arg.Event.ID, OrganizationID: arg.OrganizationID, ProjectID: arg.ProjectID, AggregateType: "issue", AggregateID: arg.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: filer, RecordedAt: arg.At, Data: tracker.CollaborationData{Revision: 2, FromState: "Todo", ToState: "Backlog"}}))
			if err != nil {
				t.Fatal(err)
			}
			changed.Receipt.RequestHash = artifact.Digest([]byte("live lease request"))
			called := false
			if err := invoke(changed, func(ctx context.Context, w tenantstore.Writer) error { called = true; return apply(changed)(ctx, w) }); !errors.Is(err, tracker.ErrLeaseConflict) || called {
				t.Fatalf("live lease initialized called=%v error=%v", called, err)
			}
			if err := repository.WriteLeases(t.Context(), arg.OrganizationID, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), leaseFence(lease), "released")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := invoke(arg, apply(arg)); err != nil {
			t.Fatal(err)
		}
		if err := invoke(arg, func(context.Context, tenantstore.Writer) error { t.Fatal("committed replay called apply"); return nil }); err != nil {
			t.Fatal(err)
		}
		changed := arg
		changed.Receipt.RequestHash = artifact.Digest([]byte("different request"))
		if err := invoke(changed, apply(changed)); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("changed retry content=%v", err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, arg := range requests {
		if err := repository.ReadTenant(t.Context(), arg.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.WorkItem(t.Context(), arg.ProjectID, arg.WorkItemID)
			if err != nil {
				return err
			}
			revision, eventsWant := int64(2), 1
			if arg.ExpectedState == arg.TargetState {
				revision, eventsWant = 1, 0
			}
			if row.Revision != revision || row.WorkflowStateName == nil || *row.WorkflowStateName != "Todo" || row.EventSequence != int64(eventsWant) {
				t.Fatalf("durable initialized item=%+v", row)
			}
			events, err := r.Events(t.Context(), arg.ProjectID, arg.WorkItemID, 0, 10)
			if err != nil {
				return err
			}
			if len(events) != eventsWant || len(events) > 0 && (events[0].FromState != "Backlog" || events[0].ToState != "Todo") {
				t.Fatalf("durable initialization events=%+v", events)
			}
			receipt, err := r.Receipt(t.Context(), arg.Receipt.ActorID, arg.Receipt.Operation, arg.Receipt.CommandKey)
			if err != nil {
				return err
			}
			if receipt.RequestHash != arg.Receipt.RequestHash {
				t.Fatal("durable outcome receipt changed")
			}
			dispatch, err := r.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), arg.ProjectID, arg.WorkItemID)
			if err == nil && dispatch.Generation != int64(eventsWant) {
				t.Fatalf("dispatch intent=%+v", dispatch)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
