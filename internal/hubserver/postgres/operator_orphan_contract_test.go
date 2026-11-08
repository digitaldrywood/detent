package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type operatorOrphanProbe struct {
	pgquery.DBTX
	query string
	args  []any
}

func (p *operatorOrphanProbe) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	p.query, p.args = query, args
	return p.DBTX.QueryRow(ctx, query, args...)
}

func TestOperatorOrphanScanAdvancesPastOtherRunners(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	objects := newHistoryObjects()
	now := time.Now().UTC().Truncate(time.Microsecond)
	wants := make(map[string][]int64)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedLeaseFamily(t, repository, org)
		for id := int64(1); id <= 2; id++ {
			seedRunnerIdentity(t, repository, org, fmt.Sprintf("runner_%d", id), fmt.Sprintf("machine_%d", id), now, false)
		}
		for id := int64(1); id <= 2; id++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			runner := fmt.Sprintf("runner_%d", id)
			lease := claimLease(t, repository, org, leaseClaim(id))
			event := attemptEvent(lease)
			if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.RecordAttempt(t.Context(), event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: runner, LeaseID: lease.LeaseID, AssignedAt: lease.AcquiredAt})
			}); err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(t.Context(), string(org), []byte("saved-provider-session"))
			if err != nil {
				t.Fatal(err)
			}
			create := func(key, attempt, state, provider string) int64 {
				t.Helper()
				var row runnersession.Record
				if err := repository.WriteOperatorProcesses(t.Context(), org, corestore.Serving, func(w postgres.OperatorProcessWriter) error {
					var err error
					row, err = w.CreateRunnerSession(t.Context(), runnersession.Record{Organization: org, Project: project, IssueID: fmt.Sprintf("wi_%d", id), AttemptID: attempt, StartedAt: now, Reference: ref, FinalState: state, ProviderSessionID: provider}, runnersession.History{Key: key, Reference: ref})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return row.ID
			}
			first := create("first", event.ID, "running", "provider_first")
			second := create("second", event.ID, "running", "provider_second")
			wants[string(org)+string(project)] = []int64{second, first}
			create("failed", event.ID, "failed", "provider_failed")
			create("empty", event.ID, "running", "")
			completed := create("completed", event.ID, "running", "provider_completed")
			if _, err := admin.ExecContext(t.Context(), `UPDATE runner_sessions SET completed_at=$1 WHERE organization_id=$2 AND id=$3`, now, org, completed); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), leaseFence(lease), "complete")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			other := 3 - id
			for _, statement := range []string{
				`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT organization_id,'other_'||lease_id,issue_id,$3,'other_'||session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at FROM leases WHERE organization_id=$1 AND lease_id=$2`,
				`INSERT INTO native_attempts(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at) SELECT a.organization_id,'other_'||a.id,a.issue_id,l.lease_id,l.fencing_token,a.run_id,1,'interrupted',a.work_item_revision,a.dispatch_generation,a.payload_object_key,a.started_at,a.updated_at FROM native_attempts a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id='other_'||a.lease_id WHERE a.organization_id=$1 AND a.lease_id=$2 AND l.machine_id=$3`,
				`INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at) SELECT l.organization_id,l.lease_id,r.runner_id,l.acquired_at FROM leases l JOIN runner_identities r ON r.organization_id=l.organization_id AND r.machine_id=l.machine_id WHERE l.organization_id=$1 AND l.lease_id='other_'||$2 AND l.machine_id=$3`,
			} {
				if _, err := admin.ExecContext(t.Context(), statement, org, lease.LeaseID, fmt.Sprintf("machine_%d", other)); err != nil {
					t.Fatal(err)
				}
			}
			create("other", "other_"+event.ID, "running", "provider_other")
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, pool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for id := int64(1); id <= 2; id++ {
			query := postgres.OperatorOrphanQuery{OrganizationID: org, ProjectID: tracker.ProjectID(fmt.Sprintf("prj_%d", id)), RunnerID: fmt.Sprintf("runner_%d", id), EnrollmentID: fmt.Sprintf("enrollment_runner_%d", id), MachineID: tracker.MachineID(fmt.Sprintf("machine_%d", id)), Now: now.Add(2 * time.Hour), Limit: 1}
			scan := func(q postgres.OperatorOrphanQuery) (postgres.OperatorOrphanPage, error) {
				var page postgres.OperatorOrphanPage
				err := repository.ReadOperatorOrphans(t.Context(), org, corestore.Serving, func(r postgres.OperatorOrphanReader) error {
					var err error
					page, err = r.ScanOrphanedAgentSessions(t.Context(), q)
					return err
				})
				return page, err
			}
			page, err := scan(query)
			if err != nil || page.Scanned != 1 || len(page.Sessions) != 0 || page.Done || page.Next == nil {
				t.Fatalf("foreign runner page=%+v: %v", page, err)
			}
			query.Before = page.Next
			for _, want := range wants[string(org)+string(query.ProjectID)] {
				page, err = scan(query)
				if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != want || page.Sessions[0].OrganizationID != org || page.Sessions[0].ProjectID != query.ProjectID || page.Sessions[0].PayloadSHA256 == "" || page.Sessions[0].WorkerPid != 0 || page.Sessions[0].AttemptStatus != "cancelled" {
					t.Fatalf("saved session want=%d page=%+v: %v", want, page, err)
				}
				query.Before = page.Next
			}
			page, err = scan(query)
			if err != nil || !page.Done || page.Scanned != 0 || len(page.Sessions) != 0 {
				t.Fatalf("end page=%+v: %v", page, err)
			}
			query.Before = nil
			query.Limit = 200
			for _, tc := range []struct {
				name string
				now  time.Time
				want int
			}{
				{"live", now.Add(time.Hour - time.Microsecond), 0},
				{"expiry", now.Add(time.Hour), 2},
			} {
				if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=NULL,expires_at=$3 WHERE organization_id=$1 AND lease_id=$2`, org, fmt.Sprintf("lease_%d", id), now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				q := query
				q.Now = tc.now
				page, err := scan(q)
				if err != nil || len(page.Sessions) != tc.want || !page.Done {
					t.Fatalf("%s page=%+v: %v", tc.name, page, err)
				}
			}
			for _, change := range []func(*postgres.OperatorOrphanQuery){
				func(q *postgres.OperatorOrphanQuery) { q.OrganizationID = "foreign" },
				func(q *postgres.OperatorOrphanQuery) { q.EnrollmentID = "foreign" },
				func(q *postgres.OperatorOrphanQuery) { q.MachineID = "foreign" },
				func(q *postgres.OperatorOrphanQuery) { q.Limit = 201 },
				func(q *postgres.OperatorOrphanQuery) { q.Before = &postgres.OperatorOrphanCursor{StartedAt: now} },
			} {
				q := query
				change(&q)
				if _, err := scan(q); err == nil {
					t.Fatal("invalid recovery scope accepted")
				}
			}
		}
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO runner_sessions(organization_id,project_id,issue_id,attempt_id,start_key,started_at,completed_at,backend_id,backend_kind,role,model,policy_id,final_state,provider_session_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT s.organization_id,s.project_id,s.issue_id,s.attempt_id,'closed_'||n,s.started_at,s.started_at,s.backend_id,s.backend_kind,s.role,s.model,s.policy_id,'completed','provider',s.payload_object_key,s.payload_sha256,s.payload_bytes,s.payload_content_bytes FROM runner_sessions s CROSS JOIN generate_series(1,10000) n WHERE s.organization_id='org_first' AND s.project_id='prj_1' AND s.start_key='first'`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `ANALYZE runner_sessions`); err != nil {
		t.Fatal(err)
	}
	tx := tenantTx(t, runtimeFor(t, pool, pool), pool, "org_first")
	defer tx.Rollback(t.Context())
	probe := &operatorOrphanProbe{DBTX: tx}
	_, err := pgquery.New(probe).OperatorOrphanScanThrough(t.Context(), pgquery.OperatorOrphanScanThroughParams{OrganizationID: "org_first", ProjectID: "prj_1", BeforeTime: now.Add(time.Second), BeforeID: math.MaxInt64, PageLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.query, probe.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct{ Plan searchPlanNode }
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !plans[0].Plan.usesIndex("runner_sessions_orphan_scan") {
		t.Fatalf("orphan discovery missed partial index: %s", raw)
	}
	var inspect func(searchPlanNode)
	inspect = func(p searchPlanNode) {
		if p.Relation == "runner_sessions" && (p.Rows+p.Removed+p.IndexRemoved)*p.Loops > 1 {
			t.Errorf("orphan page scanned history: %s", raw)
		}
		for _, child := range p.Plans {
			inspect(child)
		}
	}
	inspect(plans[0].Plan)
}
