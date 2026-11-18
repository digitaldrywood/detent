package hubserver

import (
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func BenchmarkNativeIssueProjection(b *testing.B) {
	ctx, fixture, service, _, project, _ := benchmarkPostgresReportFacts(b)
	scope := hubscope.NativeScope{Organization: fixture.organization, Project: project}
	var row corestore.WorkItem
	if err := fixture.repository.ReadTenant(ctx, fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(ctx, project, "diagnostics-current")
		return err
	}); err != nil {
		b.Fatal(err)
	}
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.NativeID, Revision: tracker.Revision(row.Revision), Number: int(row.Number)}, Title: row.Title, State: hubapi.StringValue(row.WorkflowStateName), Archived: row.Archived, Actor: tracker.Actor{Kind: hubapi.StringValue(row.ActorKind), PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}, CreatedAt: row.NativeCreatedAt, UpdatedAt: row.NativeUpdatedAt}
	raw, err := json.Marshal(issue)
	if err != nil {
		b.Fatal(err)
	}
	content := eventstore.NewContent(fixture.details)
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.NativeID, RecordID: string(row.NativeID), Revision: row.Revision}, raw)
	if err != nil {
		b.Fatal(err)
	}
	if err := fixture.repository.WriteTenant(ctx, fixture.organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.CreateCollaborationVersion(ctx, version); return err }); err != nil {
		b.Fatal(err)
	}
	for _, hydrate := range []bool{false, true} {
		name := "record"
		if hydrate {
			name = "snapshot"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				got, version, err := service.readWorkItemRecord(ctx, scope, string(row.NativeID))
				if err != nil || got.NativeID != row.NativeID {
					b.Fatalf("read projection: item=%s error=%v", got.NativeID, err)
				}
				if hydrate {
					value, err := service.hydrateCanonicalWorkItemRecord(ctx, got, version)
					if err != nil || value.WorkItemID != row.NativeID {
						b.Fatalf("read projection: item=%s error=%v", value.WorkItemID, err)
					}
				}
			}
		})
	}
}
