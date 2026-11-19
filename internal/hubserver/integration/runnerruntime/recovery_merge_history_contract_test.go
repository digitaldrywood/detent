package runnerruntime_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRecoveryReportsWhenMergeHistoryMayBeOmitted(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	for _, test := range []struct {
		name              string
		comments          int
		legacyTransitions int
		omitted           bool
	}{
		{name: "short final page", legacyTransitions: corestore.MaxRelationshipPreparationPages},
		{name: "older merge transition", legacyTransitions: corestore.MaxRelationshipPreparationPages, omitted: true},
		{name: "full final page", comments: corestore.MaxPageSize - corestore.MaxRelationshipPreparationPages, legacyTransitions: corestore.MaxRelationshipPreparationPages, omitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := f.Create(t, test.name)
			content := eventstore.NewContent(f.Hub.Config.RunDetails)
			var records []eventstore.Event
			record := func(kind, from string) {
				t.Helper()
				event := tracker.CollaborationEvent{
					ID: hubapi.NewID("event"), OrganizationID: f.Organization(), ProjectID: f.Project.ID,
					AggregateType: "issue", AggregateID: item.WorkItemID, Type: kind, SchemaVersion: 1,
					RecordedAt: f.Hub.Now(), Actor: item.Actor, Data: tracker.CollaborationData{FromState: from},
				}
				raw, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := content.PrepareEvent(t.Context(), f.Organization(), raw)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, prepared)
			}
			if test.name == "older merge transition" {
				record("workflow.transitioned", "Merging")
			}
			for range test.comments {
				record("comment.created", "")
			}
			for range test.legacyTransitions {
				record("workflow.transitioned", "")
			}
			if err := f.Hub.Repository.WriteTenant(t.Context(), f.Organization(), corestore.Serving, func(w tenantstore.Writer) error {
				for _, event := range records {
					if _, err := w.AppendEvent(t.Context(), event); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			response := f.Request(t, http.MethodGet, f.Base+"/work-items/"+string(item.WorkItemID)+"?view=recovery", nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var recovery tracker.NativeRecovery
			hubfixture.Decode(t, response, &recovery)
			if len(recovery.History) != 0 || recovery.Read == nil || slices.Contains(recovery.Read.Omitted, "history") != test.omitted {
				t.Fatalf("history=%+v, read=%+v, want omission=%v", recovery.History, recovery.Read, test.omitted)
			}
		})
	}
}
