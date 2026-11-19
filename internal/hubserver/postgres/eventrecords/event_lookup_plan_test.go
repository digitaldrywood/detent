package eventrecords_test

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type eventReader interface {
	RunEvent(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (eventstore.Event, error)
	SchedulerDecision(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (eventstore.Event, error)
}

func TestEventLookupScopesPresentAndAbsentEvents(t *testing.T) {
	t.Parallel()
	const events = 11
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
	}
	for _, seed := range []struct {
		org                       tracker.OrganizationID
		project, item             string
		count, idOffset, sequence int
	}{
		{"org_first", "prj_1", "wi_1", events, 0, 0},
		{"org_first", "prj_2", "wi_2", 10, 1000000, 0},
		{"org_second", "prj_1", "wi_1", 10, 10, 7},
	} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events (organization_id,project_id,work_item_id,id,sequence,schema_version,type,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT $1,$2,$3,'event_'||(g+$5::bigint),g+$6::bigint,1,CASE WHEN g%10=0 THEN 'scheduler.decision' ELSE 'run.observed' END,now(),'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(1,$4::bigint) g`, seed.org, seed.project, seed.item, seed.count, seed.idOffset, seed.sequence); err != nil {
			t.Fatal(err)
		}
	}
	for _, lookup := range []struct {
		name, project, item, id string
		scheduler, found        bool
		sequence                int64
	}{
		{name: "present", project: "prj_1", item: "wi_1", id: "event_11", found: true, sequence: 11},
		{name: "scheduler decision", project: "prj_1", item: "wi_1", id: "event_10", scheduler: true, found: true, sequence: 10},
		{name: "absent", project: "prj_1", item: "wi_1", id: "event_absent"},
		{name: "other item", project: "prj_2", item: "wi_2", id: "event_11"},
		{name: "other project and item of a present event", project: "prj_1", item: "wi_2", id: "event_1000001"},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			var event eventstore.Event
			err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				reader, ok := r.(eventReader)
				if !ok {
					return errors.New("tenant reader does not serve event lookups")
				}
				var err error
				if lookup.scheduler {
					event, err = reader.SchedulerDecision(t.Context(), tracker.ProjectID(lookup.project), tracker.NativeWorkItemID(lookup.item), lookup.id)
				} else {
					event, err = reader.RunEvent(t.Context(), tracker.ProjectID(lookup.project), tracker.NativeWorkItemID(lookup.item), lookup.id)
				}
				return err
			})
			if lookup.found != (err == nil) || !lookup.found && !errors.Is(err, corestore.ErrNotFound) || lookup.found && (event.OrganizationID != "org_first" || event.ID != lookup.id || event.Sequence != lookup.sequence || event.ProjectID != tracker.ProjectID(lookup.project) || event.WorkItemID != tracker.NativeWorkItemID(lookup.item)) {
				t.Fatalf("lookup event=%+v error=%v", event, err)
			}
		})
	}
}
