package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLandingTransitionReference(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corestore.WorkItem, *eventstore.Change, *eventstore.Version, *leasestore.Attempt, *eventstore.Event)
		want   bool
	}{
		{name: "current landing", want: true},
		{name: "older issue intent", mutate: func(i *corestore.WorkItem, c *eventstore.Change, v *eventstore.Version, a *leasestore.Attempt, e *eventstore.Event) {
			i.Revision++
		}},
		{name: "older dispatch intent", mutate: func(i *corestore.WorkItem, c *eventstore.Change, v *eventstore.Version, a *leasestore.Attempt, e *eventstore.Event) {
			i.DispatchGeneration++
		}},
		{name: "different attempt", mutate: func(i *corestore.WorkItem, c *eventstore.Change, v *eventstore.Version, a *leasestore.Attempt, e *eventstore.Event) {
			e.AttemptID = "attempt_other"
		}},
		{name: "different version", mutate: func(i *corestore.WorkItem, c *eventstore.Change, v *eventstore.Version, a *leasestore.Attempt, e *eventstore.Event) {
			e.LandingVersionID = "version_other"
		}},
		{name: "different head", mutate: func(i *corestore.WorkItem, c *eventstore.Change, v *eventstore.Version, a *leasestore.Attempt, e *eventstore.Event) {
			e.LandingHeadSHA = "other"
		}},
		{name: "different issue", mutate: func(i *corestore.WorkItem, c *eventstore.Change, v *eventstore.Version, a *leasestore.Attempt, e *eventstore.Event) {
			a.IssueID++
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := corestore.WorkItem{OrganizationID: "org_example", ProjectID: "prj_example", NativeID: "wi_example", ID: 1, Revision: 2, DispatchGeneration: 3}
			change := eventstore.Change{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: "change_example", CurrentVersionID: new("version_example")}
			version := eventstore.Version{OrganizationID: item.OrganizationID, ChangeID: change.ID, ID: "version_example", HeadSHA: "head"}
			attempt := leasestore.Attempt{OrganizationID: item.OrganizationID, ID: "attempt_example", IssueID: item.ID, WorkItemRevision: item.Revision, DispatchGeneration: item.DispatchGeneration}
			event := eventstore.Event{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, AttemptID: attempt.ID, LandingChangeID: change.ID, LandingVersionID: version.ID, LandingHeadSHA: version.HeadSHA}
			if test.mutate != nil {
				test.mutate(&item, &change, &version, &attempt, &event)
			}
			ref := landingTransitionReference(item, change, version, attempt, event)
			if (ref != nil) != test.want || ref != nil && *ref != (tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}) {
				t.Fatalf("transition source=%+v, want current=%t", ref, test.want)
			}
		})
	}
}
