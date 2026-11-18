package postgres

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerEventFactPlan struct {
	Relation string                `json:"Relation Name"`
	Node     string                `json:"Node Type"`
	Rows     float64               `json:"Actual Rows"`
	Loops    float64               `json:"Actual Loops"`
	Removed  float64               `json:"Rows Removed by Filter"`
	Recheck  float64               `json:"Rows Removed by Index Recheck"`
	Plans    []runnerEventFactPlan `json:"Plans"`
}

func TestRunnerEventFactsResolveKeysThroughIndexes(t *testing.T) {
	t.Parallel()
	const history, items, visitBound = 4000, 1500, 64
	admin := migratedTemplate(t, os.DirFS("migrations"), nil).Database(t)
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for _, org := range organizations {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,CURRENT_TIMESTAMP)`, []any{org}},
			{`INSERT INTO projects(organization_id,id,name,slug,profile) VALUES($1,'prj_example','example','example','native')`, []any{org}},
			{`INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) VALUES($1,'prj_example','Todo',false,true,0)`, []any{org}},
			{`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,native_source_key,workflow_state_name,archived)
			 SELECT $1,n,'wi_'||n,'prj_example',n,'Example','source_'||n,'Todo',false FROM generate_series(1,$2::bigint) n`, []any{org, items}},
			{`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version,attempt_id,run_id)
			 SELECT $1,'prj_example','wi_1','evt_'||n,n,'run.observed',1,'machine','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2,'attempt_'||(n%400),'run_'||(n%400) FROM generate_series(1,$2::bigint) n`, []any{org, history}},
		} {
			if _, err := admin.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
				t.Fatalf("seed %s: %v", org, err)
			}
		}
	}
	if _, err := admin.ExecContext(t.Context(), "ANALYZE organizations,projects,issues,collaboration_events,native_attempts,leases"); err != nil {
		t.Fatal(err)
	}
	for _, org := range organizations {
		for _, readOnly := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s read only %t", org, readOnly), func(t *testing.T) {
				s := &session{organization: org, readOnly: readOnly}
				_, batch := newRunnerEventFacts(s, s, eventstore.RunCommand{Project: "prj_example", LeaseID: "lease_deferred", Operation: "release"})
				for _, query := range batch.QueuedQueries {
					tx, err := admin.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					var raw []byte
					_, err = tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2")
					if err == nil {
						_, err = tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(org))
					}
					if err == nil {
						err = tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, FORMAT JSON) "+query.SQL, query.Arguments...).Scan(&raw)
					}
					if rollback := tx.Rollback(); err == nil {
						err = rollback
					}
					if err != nil {
						t.Fatalf("explain %s: %v", query.SQL, err)
					}
					var plans []struct{ Plan runnerEventFactPlan }
					if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
						t.Fatalf("plan %s: %v", raw, err)
					}
					var inspect func(runnerEventFactPlan)
					inspect = func(node runnerEventFactPlan) {
						switch node.Relation {
						case "collaboration_events", "issues", "native_attempts":
							if visits := (node.Rows + node.Removed + node.Recheck) * node.Loops; visits > visitBound {
								t.Errorf("%s %s visits %.0f rows for a key lookup:\n%s\nplan=%s", node.Relation, node.Node, visits, query.SQL, raw)
							}
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plans[0].Plan)
				}
			})
		}
	}
}
