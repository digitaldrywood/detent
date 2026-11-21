package runnerruntime_test

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestPostgresBoardLivenessProbesActiveRows(t *testing.T) {
	f := hubfixture.NewLeasedRunners(t)
	f.Hub.RefreshBoard(t, f.Organization)
	now := f.Hub.Now().UTC()
	for _, tc := range []struct {
		name       string
		idle       int
		additional int
	}{
		{"idle_cards", 1000, 0},
		{"active_runners", 0, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.additional {
				suffix := strconv.Itoa(i)
				pgstoretest.SeedRunnerIdentity(t, f.Hub.Repository, f.Organization, "runner_fixture_"+suffix, "machine_fixture_"+suffix, now, true)
			}
			if _, err := f.Admin.ExecContext(t.Context(), `
WITH source AS (SELECT g, $3::text || g::text AS id FROM generate_series(0,$2::int-1) g),
 inserted AS (
 INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name)
 SELECT $1,'prj_runner',id,g+$4::int,'Example','Todo' FROM source RETURNING native_id
)
INSERT INTO native_board_items(organization_id,project_id,work_item_id,source_revision,source_event_sequence,source_project_revision,
 state,archived,terminal,running,dispatchable,lane_rank,priority_rank,activity_missing,identifier,execution_fence,
 actor_principal_id,issue_type,verification_state,created_at,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
SELECT $1,'prj_runner',inserted.native_id,1,0,1,'Todo',false,false,false,true,0,0,true,inserted.native_id,0,'','','',$5,$5,
 b.payload_object_key,b.payload_sha256,b.payload_bytes,b.payload_content_bytes
FROM inserted CROSS JOIN native_board_items b WHERE b.organization_id=$1 AND b.work_item_id='wi_1'`,
				f.Organization, tc.idle+tc.additional, "wi_"+tc.name+"_", 100+tc.idle, now); err != nil {
				t.Fatal(err)
			}
			if tc.additional > 0 {
				if _, err := f.Admin.ExecContext(t.Context(), `
WITH claimed AS (
 INSERT INTO leases(organization_id,project_id,lease_id,issue_id,machine_id,runner_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at)
 SELECT i.organization_id,i.project_id,'lease_fixture_'||g,i.id,'machine_fixture_'||g,'runner_fixture_'||g,'session_fixture_'||g,1,$3,$3,$4
 FROM generate_series(0,$2::int-1) g
 JOIN issues i ON i.organization_id=$1 AND i.project_id='prj_runner' AND i.native_id='wi_active_runners_'||g
 RETURNING issue_id,lease_id,fencing_token,expires_at
)
UPDATE native_board_items b SET execution_fence=c.fencing_token,card_execution_id=c.lease_id,card_execution_running=true,card_execution_expires_at=c.expires_at,expires_at=c.expires_at
FROM claimed c JOIN issues i ON i.organization_id=$1 AND i.id=c.issue_id
WHERE b.organization_id=$1 AND b.project_id=i.project_id AND b.work_item_id=i.native_id`,
					f.Organization, tc.additional, now, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.Admin.ExecContext(t.Context(), "ANALYZE native_board_items, issues, leases, runner_identities"); err != nil {
				t.Fatal(err)
			}
			query := "EXPLAIN (ANALYZE, FORMAT JSON) WITH " + boardviewsql.Liveness("$1", "$2::text", "$3", "$4") + " SELECT count(*) FROM board_liveness"
			var raw []byte
			if err := f.Admin.QueryRowContext(t.Context(), query, f.Organization, "prj_runner", now, int64(runnerauth.HeartbeatTimeout/time.Second)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			type plan struct {
				Relation string  `json:"Relation Name"`
				Subplan  string  `json:"Subplan Name"`
				Rows     float64 `json:"Actual Rows"`
				Loops    float64 `json:"Actual Loops"`
				Removed  float64 `json:"Rows Removed by Filter"`
				Plans    []plan  `json:"Plans"`
			}
			var report []struct{ Plan plan }
			if err := json.Unmarshal(raw, &report); err != nil {
				t.Fatal(err)
			}
			var boardRows, candidates, owners float64
			var visit func(plan)
			visit = func(p plan) {
				switch p.Subplan {
				case "CTE board_liveness":
					candidates = p.Rows
				case "CTE fresh_owners":
					owners = p.Rows
				}
				if p.Relation == "native_board_items" {
					boardRows += (p.Rows + p.Removed) * p.Loops
				}
				for _, child := range p.Plans {
					visit(child)
				}
			}
			visit(report[0].Plan)
			want := float64(len(f.Leases) + tc.additional)
			if candidates != want || owners != want || boardRows != want {
				t.Fatalf("lease candidates=%g owners=%g board rows=%g, want %g of each with %d idle cards: %s", candidates, owners, boardRows, want, tc.idle, raw)
			}
		})
	}
}
