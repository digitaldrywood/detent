package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func appendTestHealthEventAs(t *testing.T, f nativeFixture, actor tracker.Actor, item tracker.NativeIssue, kind string, data tracker.CollaborationData, at time.Time) {
	t.Helper()
	value := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: item.WorkItemID, Type: kind, SchemaVersion: 1, RecordedAt: at, Actor: actor, Data: data}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.accessWrite(t.Context(), f.project.OrganizationID, func(writer tenantstore.Writer) error {
		_, err := writer.AppendEvent(t.Context(), event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
