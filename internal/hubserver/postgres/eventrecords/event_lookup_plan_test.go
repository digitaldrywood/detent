package eventrecords_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type eventLookupTrace struct {
	mu    sync.Mutex
	query string
	args  []any
}

func (e *eventLookupTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "-- name: RecordGetEvent :one") {
		e.mu.Lock()
		e.query, e.args = data.SQL, append([]any(nil), data.Args...)
		e.mu.Unlock()
	}
	return ctx
}

func (*eventLookupTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (e *eventLookupTrace) take() (string, []any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	query, args := e.query, e.args
	e.query, e.args = "", nil
	return query, args
}

type eventLookupPlan struct {
	Relation string  `json:"Relation Name"`
	Index    string  `json:"Index Name"`
	Rows     float64 `json:"Actual Rows"`
	Removed  float64 `json:"Rows Removed by Filter"`
	Recheck  float64 `json:"Rows Removed by Index Recheck"`
	Loops    float64 `json:"Actual Loops"`
	Plans    []eventLookupPlan
}

type eventReader interface {
	RunEvent(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (eventstore.Event, error)
	SchedulerDecision(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (eventstore.Event, error)
}

func TestEventLookupStaysOnPrimaryKeyInEveryStatisticsState(t *testing.T) {
	t.Parallel()
	const events = 200000
	admin := pgstoretest.Template.Database(t)
	original, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	var database string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 12"); err != nil {
		t.Fatal(err)
	}
	trace := &eventLookupTrace{}
	repository, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, trace), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	runtime := pgstoretest.RuntimeFor(t, pool, pool)
	for _, statement := range []string{`ALTER TABLE collaboration_events SET (autovacuum_enabled=false)`, `ALTER TABLE collaboration_events DISABLE TRIGGER USER`} {
		if _, err := admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, original, org)
	}
	for _, seed := range []struct {
		org                       tracker.OrganizationID
		project, item             string
		count, idOffset, sequence int
	}{
		{"org_first", "prj_1", "wi_1", events, 0, 0},
		{"org_first", "prj_2", "wi_2", 10, 1000000, 0},
		{"org_second", "prj_1", "wi_1", 10, 100000, 7},
	} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events (organization_id,project_id,work_item_id,id,sequence,schema_version,type,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT $1,$2,$3,'event_'||(g+$5::bigint),g+$6::bigint,1,CASE WHEN g%10=0 THEN 'scheduler.decision' ELSE 'run.observed' END,now(),'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(1,$4::bigint) g`, seed.org, seed.project, seed.item, seed.count, seed.idOffset, seed.sequence); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), `ALTER TABLE collaboration_events ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"never analyzed", "analyzed", "vacuumed and analyzed"} {
		switch state {
		case "never analyzed":
			var tuples float64
			if err := admin.QueryRowContext(t.Context(), `SELECT reltuples FROM pg_class WHERE oid='collaboration_events'::regclass`).Scan(&tuples); err != nil || tuples >= 0 {
				t.Fatalf("collaboration_events already has statistics: reltuples=%v error=%v", tuples, err)
			}
		case "analyzed":
			if _, err := admin.ExecContext(t.Context(), `ANALYZE collaboration_events`); err != nil {
				t.Fatal(err)
			}
		default:
			if _, err := admin.ExecContext(t.Context(), `VACUUM (ANALYZE) collaboration_events`); err != nil {
				t.Fatal(err)
			}
		}
		for _, lookup := range []struct {
			name, project, item, id string
			scheduler, found        bool
			sequence                int64
		}{
			{name: "present", project: "prj_1", item: "wi_1", id: "event_100001", found: true, sequence: 100001},
			{name: "scheduler decision", project: "prj_1", item: "wi_1", id: "event_100000", scheduler: true, found: true, sequence: 100000},
			{name: "absent", project: "prj_1", item: "wi_1", id: "event_absent"},
			{name: "other item", project: "prj_2", item: "wi_2", id: "event_100001"},
			{name: "other project and item of a present event", project: "prj_1", item: "wi_2", id: "event_1000001"},
		} {
			t.Run(state+"/"+lookup.name, func(t *testing.T) {
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
				if lookup.found != (err == nil) || !lookup.found && !errors.Is(err, corestore.ErrNotFound) || lookup.found && (event.ID != lookup.id || event.Sequence != lookup.sequence || event.ProjectID != tracker.ProjectID(lookup.project) || event.WorkItemID != tracker.NativeWorkItemID(lookup.item)) {
					t.Fatalf("lookup event=%+v error=%v", event, err)
				}
				query, args := trace.take()
				if query == "" {
					t.Fatal("event lookup did not run RecordGetEvent")
				}
				tx := pgstoretest.TenantTx(t, runtime, pool, "org_first")
				var raw []byte
				if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query, args...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var plans []struct{ Plan eventLookupPlan }
				if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
					t.Fatalf("plan=%s error=%v", raw, err)
				}
				var walk func(eventLookupPlan)
				walk = func(p eventLookupPlan) {
					if visits := (p.Rows + p.Removed + p.Recheck) * p.Loops; p.Relation == "collaboration_events" && visits > 1 {
						t.Errorf("%s visited %.0f collaboration_events rows through %q: %s", state, visits, p.Index, raw)
					}
					for _, child := range p.Plans {
						walk(child)
					}
				}
				walk(plans[0].Plan)
			})
		}
	}
}
