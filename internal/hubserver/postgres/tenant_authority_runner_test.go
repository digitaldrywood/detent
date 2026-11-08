package postgres_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerAuthorityPreservesRenewalAndProjectBounds(t *testing.T) {
	db, pools := tenantAuthorityFixture(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	authority := tenantauth.New(pools, func() time.Time { return now })
	runnerID := "runner_" + strings.Repeat("1", 32)
	machineID := "machine_" + strings.Repeat("2", 32)
	for _, org := range []string{"first", "second"} {
		for _, statement := range []string{
			`UPDATE api_tokens SET scope='worker',expires_at='2026-11-06' WHERE organization_id=$1`,
			`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,$2,'example',1,'fixture','2026-01-01','2026-01-01','2026-01-01')`,
		} {
			args := []any{org}
			if strings.Contains(statement, "$2") {
				args = append(args, machineID)
			}
			if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO runner_enrollments(organization_id,id,token_hash,scope,operations,created_at,expires_at,created_by) VALUES($1,'same',$2,'projects',ARRAY['read'],'2026-01-01','2026-11-06','fixture')`, org, strings.Repeat("3", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO runner_identities(organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,capacity_limit,reported_capacity,created_at,expires_at,last_heartbeat_at) VALUES($1,$2,$3,'same','same','projects',ARRAY['read'],1,1,'2026-01-01','2026-11-06','2026-01-01')`, org, runnerID, machineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) VALUES($1,'same','same','native');`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO token_grants VALUES($1,'same','same')`, org); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name       string
		org        tracker.OrganizationID
		renewalOrg tracker.OrganizationID
		renewal    string
		denied     bool
	}{
		{"expired", "first", "", "", true},
		{"matching renewal", "first", "first", runnerID, false},
		{"second tenant renewal", "second", "second", runnerID, false},
		{"wrong organization", "first", "second", runnerID, true},
		{"wrong runner", "first", "first", "runner_wrong", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope, err := authority.Resolve(t.Context(), tenantauth.RequestIdentity{Organization: test.org, CredentialHash: "same-proof", RenewalRunner: test.renewal, RenewalOrganization: test.renewalOrg})
			if errors.Is(err, tenantauth.ErrDenied) != test.denied {
				t.Fatalf("Resolve()=%v, denied=%v", err, test.denied)
			}
			if !test.denied && (scope.RunnerID() != runnerID || len(scope.Runner().ProjectIDs) != 1 || scope.Runner().OrganizationID != test.org || !scope.Renewal()) {
				t.Fatalf("incorrect renewal identity: %+v", scope.Runner())
			}
		})
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) SELECT 'first','project_'||n,'project_'||n,'native' FROM generate_series(1,2000) n;
INSERT INTO token_grants SELECT organization_id,'same',id FROM projects WHERE organization_id='first' AND id<>'same';`); err != nil {
		t.Fatal(err)
	}
	input := tenantauth.RequestIdentity{Organization: "first", CredentialHash: "same-proof", RenewalRunner: runnerID, RenewalOrganization: "first"}
	if _, err := authority.Resolve(t.Context(), input); !errors.Is(err, runnerstore.ErrLimitExceeded) {
		t.Fatalf("project overflow silently accepted: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE api_tokens SET revoked_at='2026-11-06' WHERE organization_id='second'`); err != nil {
		t.Fatal(err)
	}
	input.Organization = "second"
	input.RenewalOrganization = "second"
	if _, err := authority.Resolve(t.Context(), input); !errors.Is(err, tenantauth.ErrDenied) {
		t.Fatalf("renewal accepted revoked credential: %v", err)
	}
}
