package hubserver

import (
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresHealthSnapshotObservesTenantRunnersAndCandidates(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	value, ok := projectRecordDatabases.Load(service.database.postgres)
	if !ok {
		t.Fatal("PostgreSQL fixture database is missing")
	}
	admin := value.(*sql.DB)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		f := newNativeFixture(t, service, org, "Example")
		for _, title := range []string{"First candidate", "Second candidate"} {
			f.create(t, title)
		}
		wantRunners := map[string]bool{}
		for range 2 {
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			wantRunners[r.binding.RunnerID] = true
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=$2,created_at=$3 WHERE organization_id=$1`, org, now.Add(-6*time.Minute), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		snapshot, err := service.readHealthSnapshotRecords(t.Context(), org, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runners) != 2 || len(snapshot.Projects) != 1 || snapshot.Projects[0].Candidates != 2 {
			t.Fatalf("organization=%s snapshot=%+v", org, snapshot)
		}
		for _, runner := range snapshot.Runners {
			if !wantRunners[runner.ID] {
				t.Fatalf("foreign runner=%s", runner.ID)
			}
		}
		findings := evaluateHealth(now, snapshot)
		gaps := 0
		for _, finding := range findings {
			if finding.Signal == "runner_heartbeat_gap" {
				gaps++
			}
		}
		if gaps != 2 {
			t.Fatalf("organization=%s findings=%+v", org, findings)
		}
		if err := service.publishHealthEvaluation(t.Context(), org, now, findings); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/health/findings", testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=$2 WHERE organization_id=$1`, org, now); err != nil {
			t.Fatal(err)
		}
		snapshot, err = service.readHealthSnapshotRecords(t.Context(), org, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range evaluateHealth(now, snapshot) {
			if finding.Signal == "runner_heartbeat_gap" {
				t.Fatalf("healthy runner remains stale: %+v", finding)
			}
		}
		if _, err := admin.ExecContext(t.Context(), `DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2`, org, f.project.ID); err != nil {
			t.Fatal(err)
		}
		snapshot, err = service.readHealthSnapshotRecords(t.Context(), org, now)
		if err != nil {
			t.Fatalf("health without approved policy: %v", err)
		}
		if len(snapshot.Runners) != 2 || len(snapshot.Projects) != 1 || snapshot.Projects[0].Candidates != 2 {
			t.Fatalf("missing policy lost observation: %+v", snapshot)
		}
	}
}
