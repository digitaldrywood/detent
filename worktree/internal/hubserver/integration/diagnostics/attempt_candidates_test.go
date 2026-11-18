package diagnostics_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reports"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const project tracker.ProjectID = "prj_security"

type tenant struct {
	repository   *postgres.Repository
	admin        *sql.DB
	organization tracker.OrganizationID
}

func tenants(t *testing.T) []tenant {
	t.Helper()
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(4))
	out := []tenant{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization})
		hubtest.Project(t, repository, organization, hubtest.ProjectOptions{ID: project, Name: "Example project"})
		out = append(out, tenant{repository: repository, admin: admin, organization: organization})
	}
	return out
}

func seedAttempts(t *testing.T, f tenant, item tracker.NativeWorkItemID, prefix string, population int, started, updated time.Time) {
	t.Helper()
	if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		state := "Todo"
		number, err := w.NextWorkItemNumber(t.Context(), project)
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.organization, ProjectID: project, NativeID: item, Number: number, Title: "Diagnostic source", WorkflowStateName: &state, NativeCreatedAt: started, NativeUpdatedAt: updated, CreatedAt: started, UpdatedAt: updated})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,'diagnostics-machine','example',1,'example',$2,$2,$2) ON CONFLICT DO NOTHING`, []any{f.organization, started}},
		{`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) SELECT $1,$4||'-run-'||n,i.id FROM generate_series(1,$5::integer)n JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3`, []any{f.organization, project, item, prefix, population}},
		{`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT $1,$4||'-lease-'||n,i.id,'diagnostics-machine',$4||'-session-'||n,1,$6,$6,greatest($6::timestamptz,$7::timestamptz)+interval '1 hour',greatest($6::timestamptz,$7::timestamptz) FROM generate_series(1,$5::integer)n JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3`, []any{f.organization, project, item, prefix, population, started, updated}},
		{`INSERT INTO native_attempts(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at,project_id) SELECT $1,$4||'-attempt-'||n,i.id,l.lease_id,l.fencing_token,$4||'-run-'||n,1,'failed',1,1,'fixture/payload',$6,$7,i.project_id FROM generate_series(1,$5::integer)n JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 JOIN leases l ON l.organization_id=$1 AND l.lease_id=$4||'-lease-'||n`, []any{f.organization, project, item, prefix, population, started, updated}},
	} {
		if _, err := f.admin.ExecContext(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func readSnapshot(t *testing.T, f tenant, w operatortool.AnalyticsWindow) diagnosticstore.Snapshot {
	t.Helper()
	var out diagnosticstore.Snapshot
	if err := f.repository.ReadTenantSnapshot(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		out, err = r.(diagnosticstore.Reader).DiagnosticsSnapshot(t.Context(), []tracker.ProjectID{project}, w.From, w.To, w.To, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDiagnosticsAttemptCandidatesBound(t *testing.T) {
	t.Parallel()
	for _, population := range []int{4000, 40000} {
		for _, f := range tenants(t) {
			now := time.Now().UTC().Truncate(time.Hour)
			w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
			seedAttempts(t, f, "candidate-source", "candidate", population, now.Add(-2*time.Hour), now.Add(-time.Hour))
			for _, test := range []struct {
				name             string
				started, updated time.Time
				observed         int
			}{
				{"matching", now.Add(-2 * time.Hour), now.Add(-time.Hour), reports.MaxAnalyticsPopulation},
				{"before exact window", w.From.Add(-time.Hour), w.From.Add(-time.Minute), 0},
				{"started at exclusive end", w.To, w.To.Add(time.Hour), 0},
				{"overlapping old start", w.From.Add(-365 * 24 * time.Hour), w.From, reports.MaxAnalyticsPopulation},
			} {
				t.Run(fmt.Sprintf("%d/%s/%s", population, f.organization, test.name), func(t *testing.T) {
					if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_attempts SET started_at=$2,updated_at=$3 WHERE organization_id=$1`, f.organization, test.started, test.updated); err != nil {
						t.Fatal(err)
					}
					snapshot := readSnapshot(t, f, w)
					if snapshot.Attempts != test.observed || len(snapshot.AttemptRecords) != test.observed || snapshot.AttemptsPartial != (test.observed == reports.MaxAnalyticsPopulation || test.name == "started at exclusive end" && population > diagnosticstore.Limit) {
						t.Fatalf("observed=%d partial=%t, want observed=%d and exact candidate clipping", snapshot.Attempts, snapshot.AttemptsPartial, test.observed)
					}
					if test.name == "matching" {
						first := "candidate-attempt-999"
						if population == 40000 {
							first = "candidate-attempt-9999"
						}
						if len(snapshot.AttemptRecords) == 0 {
							t.Fatal("equal-time attempts lost every candidate")
						}
						if snapshot.AttemptRecords[0].ID != first {
							t.Fatalf("equal-time attempts lost id tie-break: first=%s want=%s", snapshot.AttemptRecords[0].ID, first)
						}
					}
				})
			}
		}
	}
}
