package coreimport_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestImportedAttemptCheckpointHydration(t *testing.T) {
	t.Parallel()
	const project tracker.ProjectID = "prj_execution_import"
	executions := coreimport.NewExecutions(coreimport.Deps{
		CleanupRequired:  func(tracker.NativeRunData) bool { return false },
		DecodeCheckpoint: hubrecords.DecodeAttemptCheckpoint,
	})
	for _, organization := range []tracker.OrganizationID{"org_execution_import", "org_execution_import_other"} {
		t.Run(string(organization), func(t *testing.T) {
			hub := hubfixture.Open(t, hubfixture.Config{})
			objects := hub.Config.RunDetails
			coreimporttest.SeedExecutionTarget(t, hub.Repository, hub.Admin, organization)
			hubfixture.SeedAdmin(t, hub.Repository, organization, hub.AdminToken(), hub.Now())
			source := coreimport.CoreExecutionImportSource{
				BundlePath:     coreimporttest.ExecutionWithCheckpoints(t, os.DirFS("../../../.."), organization, objects),
				OrganizationID: organization,
				SourceObjects:  objects,
				Objects:        objects,
			}
			if _, err := executions.Import(t.Context(), hub.Repository, source); err != nil {
				t.Fatal(err)
			}
			content := eventstore.NewContent(objects)
			for _, suffix := range []string{"", "_second"} {
				item := tracker.NativeWorkItemID("wi_execution_import" + suffix)
				id := "attempt_execution_import" + suffix
				if err := hub.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					row, err := w.WorkItem(t.Context(), project, item)
					if err != nil {
						return err
					}
					issue := tracker.NativeIssue{
						NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: item, Number: int(row.Number), Revision: tracker.Revision(row.Revision), Profile: "native"},
						Title:           row.Title, State: hubapi.StringValue(row.WorkflowStateName), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
						Actor: tracker.Actor{Kind: hubapi.StringValue(row.ActorKind), PrincipalID: hubapi.StringValue(row.ActorPrincipalID)},
					}
					version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: row.Revision}, historyTestJSON(t, issue))
					if err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					attempt, err := w.Attempt(t.Context(), id)
					if err != nil {
						return err
					}
					event := tracker.CollaborationEvent{
						ID: "event_execution_import" + suffix, OrganizationID: organization, ProjectID: project,
						AggregateType: "issue", AggregateID: item, Type: "run.finished", SchemaVersion: 1, RecordedAt: attempt.UpdatedAt,
						Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine_execution_import" + suffix},
						Data: tracker.CollaborationData{Run: &tracker.NativeRunData{
							AttemptID: id, RunID: attempt.RunID, LeaseID: tracker.LeaseID(attempt.LeaseID),
							FencingToken: tracker.FencingToken(attempt.FencingToken), Sequence: attempt.Sequence,
						}},
					}
					record, err := content.PrepareEvent(t.Context(), organization, historyTestJSON(t, event))
					if err != nil {
						return err
					}
					_, err = w.AppendEvent(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				path := hubfixture.ProjectBase(organization, project) + "/work-items/" + string(item) + "/attempts/" + id
				response := hub.Request(t, http.MethodGet, path, hub.AdminToken(), nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var attempt tracker.NativeAttempt
				hubfixture.Decode(t, response, &attempt)
				if attempt.Checkpoint == nil || attempt.Checkpoint.PreservesSource() != (suffix == "") {
					t.Fatalf("attempt %q returned checkpoint %+v", id, attempt.Checkpoint)
				}
			}
		})
	}
}
