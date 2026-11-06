package hubserver

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreExecutionImportPreservesHistoricalFenceAndRevisionZero(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	objects := &memoryRunDetails{}
	for _, organization := range []tracker.OrganizationID{"org_execution_import", "org_execution_import_other"} {
		t.Run(string(organization), func(t *testing.T) {
			project := tracker.ProjectID("prj_execution_import")
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
					return fmt.Errorf("create import test organization: %w", err)
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_execution_import"}); err != nil {
					return fmt.Errorf("create import test tenant: %w", err)
				}
				for _, credential := range []accessstore.Credential{
					{OrganizationID: organization, ID: "owner_token_execution_import", Name: "owner token", Hash: coreExecutionHash('1'), Fingerprint: "owner", Scope: "admin", ProjectAccess: "selected", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Kind: "service"},
					{OrganizationID: organization, ID: "runner_token_execution_import", Name: "runner token", Hash: coreExecutionHash('2'), Fingerprint: "runner", Scope: "worker", ProjectAccess: "selected", NativeOnly: true, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: coreExecutionTimePointer(time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)), Kind: "service"},
				} {
					if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
						return fmt.Errorf("create import test credential %s: %w", credential.ID, err)
					}
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
					Core:     corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"},
					Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project},
					States:   nativeFixtureStates(),
				}); err != nil {
					return fmt.Errorf("create import test project: %w", err)
				}
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{
					OrganizationID: organization, ID: 41, NativeID: "wi_execution_import", ProjectID: project, Number: 1,
					Revision: 1, EventSequence: 1, Title: "Example issue", ActorKind: coreExecutionStringPointer("human"), ActorPrincipalID: coreExecutionStringPointer("example-owner"),
					CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
					NativeCreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), NativeUpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
					LastActivityAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), WorkflowStateName: coreExecutionStringPointer("Todo"),
				})
				if err != nil {
					return fmt.Errorf("create import test work item: %w", err)
				}
				return nil
			}); err != nil {
				var postgresErr *pq.Error
				if errors.As(err, &postgresErr) {
					t.Fatalf("initialize import fixture: constraint %s: %s", postgresErr.Constraint, postgresErr.Message)
				}
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO policy_revisions (organization_id,project_id,policy_id,approved_by,approved_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES ($1,$2,'policy_example','owner_token_execution_import',$3,$4,$5,1,1)`, organization, project, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "orgs/"+string(organization)+"/blobs/sha256/aa/"+coreExecutionHash('a'), coreExecutionHash('a')); err != nil {
				t.Fatalf("seed import test policy: %v", err)
			}
			copyPath := coreExecutionSourceCopy(t, organization)
			source := CoreExecutionImportSource{CopyPath: copyPath, OrganizationID: organization, SourceObjects: objects, Objects: objects}
			for range 2 {
				count, err := ImportCoreExecution(t.Context(), repository, source)
				if err != nil || count != 7 {
					t.Fatalf("execution import count=%d error=%v", count, err)
				}
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				lease, err := r.Lease(t.Context(), "lease_execution_import")
				if err != nil {
					return err
				}
				if lease.FencingToken != 40 || lease.WorkItemRevision != 0 || lease.ReleasedAt == nil {
					return errors.New("historical lease fencing or zero revision was not preserved")
				}
				attempt, err := r.Attempt(t.Context(), "attempt_execution_import")
				if err != nil {
					return err
				}
				if attempt.FencingToken != lease.FencingToken || attempt.WorkItemRevision != 0 || attempt.Status != "failed" || attempt.PayloadObjectKey == "" {
					return errors.New("historical attempt identity or zero revision was not preserved")
				}
				enrollment, err := r.RunnerEnrollment(t.Context(), "enrollment_execution_import")
				if err != nil {
					return err
				}
				if enrollment.RedeemedAt == nil || enrollment.RevokedAt != nil || enrollment.RunnerID != "runner_execution_import" || enrollment.ProjectIDs[0] != project {
					return errors.New("runner enrollment state or project scope was not preserved")
				}
				identity, err := r.RunnerIdentityByCredential(t.Context(), "runner_token_execution_import")
				if err != nil {
					return err
				}
				if identity.RunnerID != "runner_execution_import" || identity.TokenID != "runner_token_execution_import" || identity.Scope != "projects" || identity.State != "active" {
					return errors.New("runner identity binding was not preserved")
				}
				assigned, err := r.RunnerForLease(t.Context(), "lease_execution_import")
				if err != nil {
					return err
				}
				if assigned.RunnerID != "runner_execution_import" {
					return errors.New("lease runner assignment was not preserved")
				}
				pinned, err := r.LeasePolicy(t.Context(), "lease_execution_import")
				if err != nil {
					return err
				}
				if pinned.ProjectID != project || pinned.PolicyID != "policy_example" || !pinned.BoundAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
					return errors.New("lease policy binding was not preserved")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var sourceID, issueID int64
			var fencingToken sql.NullInt64
			var machineID, kind, payload string
			if err := admin.QueryRowContext(t.Context(), `SELECT source_id,issue_id,fencing_token,machine_id,kind,payload_json FROM core_work_events WHERE organization_id=$1`, organization).Scan(&sourceID, &issueID, &fencingToken, &machineID, &kind, &payload); err != nil {
				t.Fatal(err)
			}
			if sourceID != 7 || issueID != 41 || !fencingToken.Valid || fencingToken.Int64 != 40 || machineID != "machine_execution_import" || kind != "progress" || payload != `{"step":"compile"}` {
				t.Fatalf("imported work event source=%d issue=%d fence=%v machine=%s kind=%s payload=%s", sourceID, issueID, fencingToken, machineID, kind, payload)
			}
			var claimed leasestore.Lease
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				claimed, err = w.Claim(t.Context(), leasestore.Claim{IssueID: 41, ExpectedRevision: 1, MachineID: "machine_execution_import", SessionID: "session_after_import", LeaseID: "lease_after_import", TTL: time.Hour})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if claimed.FencingToken <= 40 {
				t.Fatalf("new lease fencing token %d did not advance beyond imported token 40", claimed.FencingToken)
			}

		})
	}
}

func coreExecutionSourceCopy(t *testing.T, organization tracker.OrganizationID) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "execution-source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE issues(id INTEGER,organization_id TEXT,project_id TEXT,native_id TEXT)`,
		`CREATE TABLE machines(id TEXT,organization_id TEXT,hostname TEXT,display_name TEXT,capacity INTEGER,version TEXT,sprite_name TEXT,last_heartbeat_at TEXT,registered_at TEXT,updated_at TEXT)`,
		`CREATE TABLE leases(fencing_token INTEGER,lease_id TEXT,issue_id INTEGER,machine_id TEXT,session_id TEXT,work_item_revision INTEGER,landing_capacity INTEGER,acquired_at TEXT,renewed_at TEXT,expires_at TEXT,released_at TEXT)`,
		`CREATE TABLE native_attempts(id TEXT,organization_id TEXT,project_id TEXT,work_item_id TEXT,lease_id TEXT,fencing_token INTEGER,run_id TEXT,sequence INTEGER,status TEXT,work_item_revision INTEGER,dispatch_generation INTEGER,data_json TEXT,checkpoint_json TEXT,artifact_ids_json TEXT,started_at TEXT,updated_at TEXT,payload_object_key TEXT,checkpoint_object_key TEXT)`,
		`CREATE TABLE native_attempt_events(attempt_id TEXT,sequence INTEGER,request_hash TEXT)`,
		`CREATE TABLE change_content_objects(organization_id TEXT,object_key TEXT,object_sha256 TEXT,object_bytes INTEGER,content_bytes INTEGER)`,
		`CREATE TABLE attachments(organization_id TEXT,object_key TEXT,sha256 TEXT,size INTEGER,uncompressed_size INTEGER,kind TEXT,customer_owned INTEGER,storage_binding_id TEXT)`,
		`CREATE TABLE api_tokens(id TEXT,expires_at TEXT)`,
		`CREATE TABLE runner_enrollments(id TEXT,organization_id TEXT,runner_id TEXT,machine_id TEXT,token_hash TEXT,scope TEXT,shared_machine INTEGER,operations_json TEXT,created_at TEXT,expires_at TEXT,created_by TEXT,redeemed_at TEXT,revoked_at TEXT)`,
		`CREATE TABLE runner_enrollment_projects(enrollment_id TEXT,project_id TEXT)`,
		`CREATE TABLE runner_identities(id TEXT,organization_id TEXT,machine_id TEXT,token_id TEXT,enrollment_id TEXT,scope TEXT,state TEXT,revision INTEGER,tags_json TEXT,operations_json TEXT,display_name TEXT,capacity_limit INTEGER,reported_capacity INTEGER,os TEXT,architecture TEXT,created_at TEXT,last_heartbeat_at TEXT,removed_at TEXT,routing_settings_json TEXT,capacity_configuration_json TEXT)`,
		`CREATE TABLE runner_project_capacity_limits(organization_id TEXT,runner_id TEXT,project_id TEXT,capacity_limit INTEGER)`,
		`CREATE TABLE runner_project_ranks(runner_id TEXT,project_id TEXT,rank INTEGER)`,
		`CREATE TABLE runner_identity_events(id INTEGER,runner_id TEXT,actor_id TEXT,kind TEXT,occurred_at TEXT)`,
		`CREATE TABLE lease_runners(lease_id TEXT,runner_id TEXT)`,
		`CREATE TABLE lease_policies(lease_id TEXT PRIMARY KEY,scope TEXT,policy_id TEXT)`,
		`CREATE TABLE work_events(id INTEGER,issue_id INTEGER,fencing_token INTEGER,machine_id TEXT,session_id TEXT,run_id TEXT,kind TEXT,payload_json TEXT,occurred_at TEXT,recorded_at TEXT)`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	const acquired = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	const expired = "2026-10-01T01:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO issues VALUES(41,?,'prj_execution_import','wi_execution_import')`, organization); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO machines VALUES('machine_execution_import',?,'example-host','Example host',2,'v0.173.4','',?,?,?)`, organization, acquired, acquired, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO api_tokens VALUES('runner_token_execution_import',?)`, "2027-10-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runner_enrollments VALUES('enrollment_execution_import',?,'runner_execution_import','machine_execution_import',?,'projects',0,'["read","claim"]',?,?, 'owner_token_execution_import',?,NULL)`, organization, coreExecutionHash('3'), acquired, "2027-10-01T00:00:00Z", acquired); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runner_enrollment_projects VALUES('enrollment_execution_import','prj_execution_import')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runner_identities VALUES('runner_execution_import',?,'machine_execution_import','runner_token_execution_import','enrollment_execution_import','projects','active',1,'[]','["read","claim"]','Example runner',2,2,'darwin','arm64',?,? ,NULL,'{}','null')`, organization, acquired, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO runner_identity_events VALUES(9,'runner_execution_import','owner_token_execution_import','enrolled',?)`, acquired); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO leases VALUES(40,'lease_execution_import',41,'machine_execution_import','session_execution_import',0,0,?,?,?,?)`, acquired, acquired, expired, updated); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO lease_runners VALUES('lease_execution_import','runner_execution_import')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO lease_policies VALUES('lease_execution_import',?,'policy_example')`, string(organization)+"/prj_execution_import"); err != nil {
		t.Fatal(err)
	}
	run := tracker.NativeRunData{AttemptID: "attempt_execution_import", LeaseID: "lease_execution_import", FencingToken: 40, RunID: "run_execution_import", Sequence: 1, PolicyID: "policy_example"}
	runData, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO native_attempts VALUES('attempt_execution_import',?, 'prj_execution_import','wi_execution_import','lease_execution_import',40,'run_execution_import',1,'failed',0,0,?,NULL,'[]',?,?, '', '')`, organization, string(runData), updated, updated); err != nil {
		t.Fatal(err)
	}
	requestHash := hex.EncodeToString(make([]byte, 32))
	if _, err := db.ExecContext(t.Context(), `INSERT INTO native_attempt_events VALUES('attempt_execution_import',1,?)`, requestHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO work_events VALUES(7,41,40,'machine_execution_import','session_execution_import','run_execution_import','progress','{"step":"compile"}',?,?)`, updated, updated); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	return path
}

func coreExecutionHash(char byte) string {
	return strings.Repeat(string(char), 64)
}

func coreExecutionTimePointer(value time.Time) *time.Time {
	return &value
}

func coreExecutionStringPointer(value string) *string {
	return &value
}
