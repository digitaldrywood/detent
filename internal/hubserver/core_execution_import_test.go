package hubserver

import (
	"bytes"
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

	"github.com/digitaldrywood/detent/internal/storagecatalog"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreExecutionImportPreservesHistoricalFenceAndRevisionZero(t *testing.T) {
	var native *pgxpool.Config
	repository, admin := coreImportPostgresWithAdmin(t, 3, func(config *pgxpool.Config) { native = config.Copy() })
	objects := &memoryRunDetails{}
	closeAfterTenants := t.Cleanup
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
					{OrganizationID: organization, ID: "runner_token_execution_import_second", Name: "second runner token", Hash: coreExecutionHash('5'), Fingerprint: "runner-second", Scope: "worker", ProjectAccess: "selected", NativeOnly: true, CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: coreExecutionTimePointer(time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC)), Kind: "service"},
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
				for index, suffix := range []string{"", "_second"} {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{
						OrganizationID: organization, ID: int64(41 + index), NativeID: tracker.NativeWorkItemID("wi_execution_import" + suffix), ProjectID: project, Number: int64(1 + index),
						Revision: 1, EventSequence: 1, Title: "Example issue", ActorKind: coreExecutionStringPointer("human"), ActorPrincipalID: coreExecutionStringPointer("example-owner"),
						CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
						NativeCreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), NativeUpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
						LastActivityAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), WorkflowStateName: coreExecutionStringPointer("Todo"),
					})
					if err != nil {
						return fmt.Errorf("create import test work item: %w", err)
					}
				}
				return nil
			}); err != nil {
				var postgresErr *pgconn.PgError
				if errors.As(err, &postgresErr) {
					t.Fatalf("initialize import fixture: constraint %s: %s", postgresErr.ConstraintName, postgresErr.Message)
				}
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO policy_revisions (organization_id,project_id,policy_id,approved_by,approved_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES ($1,$2,'policy_example','owner_token_execution_import',$3,$4,$5,1,1)`, organization, project, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "orgs/"+string(organization)+"/blobs/sha256/aa/"+coreExecutionHash('a'), coreExecutionHash('a')); err != nil {
				t.Fatalf("seed import test policy: %v", err)
			}
			copyPath := coreExecutionSourceCopy(t, organization)
			source := CoreExecutionImportSource{BundlePath: copyPath, OrganizationID: organization, SourceObjects: objects, Objects: objects}
			historical := historicalCoreAttemptReceipts(t, source, "")
			for _, receipt := range historical {
				if err := repository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error { _, err := w.PutReceipt(t.Context(), receipt); return err }); err != nil {
					t.Fatal(err)
				}
			}
			for pass := range 3 {
				count, err := ImportCoreExecution(t.Context(), repository, source)
				if err != nil || count != 14 {
					t.Fatalf("execution import count=%d error=%v", count, err)
				}
				if pass == 0 {
					if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET local_attempt_id=NULL,local_attempt_id_recorded=false WHERE organization_id=$1`, organization); err != nil {
						t.Fatal(err)
					}
					wrongSource := historicalCoreAttemptReceipts(t, source, strings.Repeat("f", 64))
					for i, receipt := range historical {
						var original leasestore.Attempt
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							original, err = r.Attempt(t.Context(), receipt.CommandKey)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						if original.LocalAttemptID != 0 || original.LocalAttemptIDRecorded {
							t.Fatal("upgrade fixture already has recorded correlation coverage")
						}
						for _, test := range []struct {
							name string
							edit func(*postgres.CoreAttemptImport)
						}{
							{"source fingerprint", func(record *postgres.CoreAttemptImport) { record.Receipt = wrongSource[i] }},
							{"source fence", func(record *postgres.CoreAttemptImport) { record.Attempt.FencingToken++ }},
							{"source payload", func(record *postgres.CoreAttemptImport) {
								record.Attempt.PayloadObjectKey = "unverified/source/payload"
							}},
						} {
							t.Run(receipt.CommandKey+"/reject "+test.name, func(t *testing.T) {
								requested := postgres.CoreAttemptImport{Attempt: original, Receipt: receipt}
								requested.Attempt.LocalAttemptID, requested.Attempt.LocalAttemptIDRecorded = 42, true
								test.edit(&requested)
								if err := repository.ImportCoreAttempt(t.Context(), requested); !errors.Is(err, mutation.ErrConflict) {
									t.Fatalf("mismatched provenance accepted: %v", err)
								}
								if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
									current, err := r.Attempt(t.Context(), original.ID)
									if err == nil && (current.LocalAttemptID != 0 || current.LocalAttemptIDRecorded || current.FencingToken != original.FencingToken || current.PayloadObjectKey != original.PayloadObjectKey) {
										t.Fatal("rejected import changed correlation or source identity")
									}
									return err
								}); err != nil {
									t.Fatal(err)
								}
							})
						}
					}
					if err := repository.Close(); err != nil {
						t.Fatal(err)
					}
					pool, err := pgxpool.NewWithConfig(t.Context(), native.Copy())
					if err != nil {
						t.Fatal(err)
					}
					repository, err = postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
					if err != nil {
						pool.Close()
						t.Fatal(err)
					}
					upgraded := repository
					closeAfterTenants(func() {
						if err := upgraded.Close(); err != nil {
							t.Error(err)
						}
					})
				}
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), native.Copy())
			if err != nil {
				t.Fatal(err)
			}
			repository, err = postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
			if err != nil {
				pool.Close()
				t.Fatal(err)
			}
			reopened := repository
			closeAfterTenants(func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})

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
				if attempt.LocalAttemptID != 42 || attempt.FencingToken != lease.FencingToken || attempt.WorkItemRevision != 0 || attempt.Status != "failed" || attempt.PayloadObjectKey == "" {
					return errors.New("historical attempt identity or zero revision was not preserved")
				}
				for _, suffix := range []string{"", "_second"} {
					matches, err := r.LocalAttemptMatches(t.Context(), project, tracker.NativeWorkItemID("wi_execution_import"+suffix), 42)
					if err != nil {
						return err
					}
					if len(matches) != 1 || matches[0] != "attempt_execution_import"+suffix {
						return fmt.Errorf("verified historical correlation missing: %v", matches)
					}
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
				secondLease, err := r.Lease(t.Context(), "lease_execution_import_second")
				if err != nil {
					return err
				}
				secondAttempt, err := r.Attempt(t.Context(), "attempt_execution_import_second")
				if err != nil {
					return err
				}
				secondRunner, err := r.RunnerIdentityByCredential(t.Context(), "runner_token_execution_import_second")
				if err != nil {
					return err
				}
				secondEnrollment, err := r.RunnerEnrollment(t.Context(), "enrollment_execution_import_second")
				if err != nil {
					return err
				}
				if secondLease.FencingToken != 41 || secondLease.IssueID != 42 || secondAttempt.FencingToken != 41 || secondAttempt.WorkItemRevision != 0 || secondRunner.RunnerID != "runner_execution_import_second" || secondEnrollment.RunnerID != secondRunner.RunnerID || secondRunner.ExpiresAt != identity.ExpiresAt {
					return errors.New("second imported execution identity was not preserved after reopen")
				}

				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var sourceID, issueID int64
			var fencingToken sql.NullInt64
			var machineID, kind string
			var ref attachment.ArchiveReference
			var legacy sql.NullString
			if err := admin.QueryRowContext(t.Context(), `SELECT source_id,issue_id,fencing_token,machine_id,kind,payload_json,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM core_work_events WHERE organization_id=$1 AND source_id=7`, organization).Scan(&sourceID, &issueID, &fencingToken, &machineID, &kind, &legacy, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize); err != nil {
				t.Fatal(err)
			}
			ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention = "gzip", 1, "archive", attachment.ArchiveRetention
			raw, err := objects.ReadDetail(t.Context(), string(organization), ref)
			if err != nil || legacy.Valid {
				t.Fatalf("work event object read=%v inline=%t", err, legacy.Valid)
			}
			payload := string(raw)
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
			if claimed.FencingToken <= 41 {
				t.Fatalf("new lease fencing token %d did not advance beyond imported token 41", claimed.FencingToken)
			}

		})
	}
}

func coreExecutionSourceCopy(t *testing.T, organization tracker.OrganizationID) string {
	t.Helper()
	return coreExecutionNeutralFixture(t, organization).write()
}

func coreExecutionNeutralFixture(t *testing.T, organization tracker.OrganizationID) *coreNeutralFixture {
	t.Helper()
	fixture := newCoreNeutralFixture(t, organization, "issues", "machines", "leases", "native_attempts", "native_attempt_events", "change_content_objects", "attachments", "api_tokens", "runner_enrollments", "runner_enrollment_projects", "runner_identities", "runner_project_capacity_limits", "runner_project_ranks", "runner_identity_events", "lease_runners", "lease_policies", "work_events", "attempt_usage")
	const acquired = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	const expired = "2026-10-01T01:00:00Z"
	for index, suffix := range []string{"", "_second"} {
		id := func(value string) string { return value + suffix }
		fixture.add("issues", map[string]any{"id": 41 + index, "organization_id": organization, "project_id": "prj_execution_import", "native_id": id("wi_execution_import")})
		fixture.add("machines", map[string]any{"id": id("machine_execution_import"), "organization_id": organization, "hostname": "example-host", "display_name": "Example host", "capacity": 2, "version": "v0.173.4", "sprite_name": "", "last_heartbeat_at": acquired, "registered_at": acquired, "updated_at": updated})
		var expiry any = "2027-10-01T00:00:00Z"
		if index == 1 {
			expiry = nil
		}
		fixture.add("api_tokens", map[string]any{"id": id("runner_token_execution_import"), "expires_at": expiry})
		fixture.add("runner_enrollments", map[string]any{"id": id("enrollment_execution_import"), "organization_id": organization, "runner_id": id("runner_execution_import"), "machine_id": id("machine_execution_import"), "token_hash": coreExecutionHash(byte('3' + index)), "scope": "projects", "shared_machine": 0, "operations_json": "[\"read\",\"claim\"]", "created_at": acquired, "expires_at": "2027-10-01T00:00:00Z", "created_by": "owner_token_execution_import", "redeemed_at": acquired, "revoked_at": nil})
		fixture.add("runner_enrollment_projects", map[string]any{"enrollment_id": id("enrollment_execution_import"), "project_id": "prj_execution_import"})
		fixture.add("runner_identities", map[string]any{"id": id("runner_execution_import"), "organization_id": organization, "machine_id": id("machine_execution_import"), "token_id": id("runner_token_execution_import"), "enrollment_id": id("enrollment_execution_import"), "scope": "projects", "state": "active", "revision": 1, "tags_json": "[]", "operations_json": "[\"read\",\"claim\"]", "display_name": "Example runner", "capacity_limit": 2, "reported_capacity": 2, "os": "darwin", "architecture": "arm64", "created_at": acquired, "last_heartbeat_at": updated, "removed_at": nil, "routing_settings_json": "{}", "capacity_configuration_json": "null"})
		fixture.add("runner_identity_events", map[string]any{"id": 9 + index, "runner_id": id("runner_execution_import"), "actor_id": "owner_token_execution_import", "kind": "enrolled", "occurred_at": acquired})
		fixture.add("leases", map[string]any{"fencing_token": 40 + index, "lease_id": id("lease_execution_import"), "issue_id": 41 + index, "machine_id": id("machine_execution_import"), "session_id": id("session_execution_import"), "work_item_revision": 0, "landing_capacity": 0, "acquired_at": acquired, "renewed_at": acquired, "expires_at": expired, "released_at": updated})
		fixture.add("lease_runners", map[string]any{"lease_id": id("lease_execution_import"), "runner_id": id("runner_execution_import")})
		fixture.add("lease_policies", map[string]any{"lease_id": id("lease_execution_import"), "scope": string(organization) + "/prj_execution_import", "policy_id": "policy_example"})
		run := tracker.NativeRunData{AttemptID: id("attempt_execution_import"), LeaseID: tracker.LeaseID(id("lease_execution_import")), FencingToken: tracker.FencingToken(40 + index), RunID: id("run_execution_import"), Sequence: 1, PolicyID: "policy_example", Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 42}}
		runData, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		fixture.add("native_attempts", map[string]any{"id": id("attempt_execution_import"), "organization_id": organization, "project_id": "prj_execution_import", "work_item_id": id("wi_execution_import"), "lease_id": id("lease_execution_import"), "fencing_token": 40 + index, "run_id": id("run_execution_import"), "sequence": 1, "status": "failed", "work_item_revision": 0, "dispatch_generation": 0, "data_json": string(runData), "checkpoint_json": nil, "artifact_ids_json": "[]", "started_at": updated, "updated_at": updated, "payload_object_key": "", "checkpoint_object_key": ""})
		requestHash := hex.EncodeToString(make([]byte, 32))
		fixture.add("native_attempt_events", map[string]any{"attempt_id": id("attempt_execution_import"), "sequence": 1, "request_hash": requestHash})
		fixture.add("attempt_usage", map[string]any{"attempt_id": id("attempt_execution_import"), "project_id": "prj_execution_import", "period": acquired, "provider": "codex", "model": "example-model", "input": 100, "cached_input": 20, "output": 30, "cost_estimate": 0.25, "currency": "USD", "updated_at": updated})

		fixture.add("work_events", map[string]any{"id": 7 + index, "issue_id": 41 + index, "fencing_token": 40 + index, "machine_id": id("machine_execution_import"), "session_id": id("session_execution_import"), "run_id": id("run_execution_import"), "kind": "progress", "payload_json": "{\"step\":\"compile\"}", "occurred_at": updated, "recorded_at": updated})
	}
	return fixture
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

func TestCoreExecutionImportRefusesLegacyCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "historical-source.db")
	original := []byte("preserved verified historical source")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	for _, bundle := range []string{"", "unused-neutral-bundle"} {
		_, err := ImportCoreExecution(t.Context(), &postgres.Repository{}, CoreExecutionImportSource{CopyPath: path, BundlePath: bundle, OrganizationID: "org_example", SourceObjects: &memoryRunDetails{}, Objects: &memoryRunDetails{}})
		if !errors.Is(err, postgres.ErrNeutralImport) {
			t.Fatalf("legacy selector error=%v", err)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("historical source changed: %v", err)
	}
}

func TestCoreExecutionImportRejectsUnsupportedSchemaBeforePin(t *testing.T) {
	repository, admin := coreImportPostgresWithAdmin(t, 3)
	for _, scenario := range []struct{ table, column, change string }{
		{"native_attempts", "data_json", "extra"},
		{"native_attempts", "activity_started_at", "generated"},
		{"native_attempts", "id", "primary"},
		{"runner_identities", "id", "primary"},
		{"leases", "fencing_token", "primary"},
		{"runner_enrollments", "scope", "type"},
	} {
		t.Run(scenario.table+"/"+scenario.change, func(t *testing.T) {
			fixture := coreExecutionNeutralFixture(t, "org_execution_schema")
			table := fixture.tables[scenario.table]
			if scenario.change == "extra" {
				table.Columns["unaccounted"] = fmt.Sprintf(`[%d,"unaccounted","TEXT",1,null,0,0]`, len(table.Columns))
				for _, row := range fixture.records[scenario.table] {
					row.Values["unaccounted"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("must survive migration")}
				}
			} else {
				var signature []any
				if err := json.Unmarshal([]byte(table.Columns[scenario.column]), &signature); err != nil {
					t.Fatal(err)
				}
				switch scenario.change {
				case "generated":
					signature[6] = float64(0)
				case "primary":
					signature[5] = float64(0)
				case "type":
					signature[2] = "BLOB"
				}
				raw, err := json.Marshal(signature)
				if err != nil {
					t.Fatal(err)
				}
				table.Columns[scenario.column] = string(raw)
			}
			fixture.tables[scenario.table] = table
			objects := &memoryRunDetails{}
			_, err := ImportCoreExecution(t.Context(), repository, CoreExecutionImportSource{BundlePath: fixture.write(), OrganizationID: fixture.organization, SourceObjects: objects, Objects: objects})
			if !errors.Is(err, storagecatalog.ErrUnmapped) {
				t.Fatalf("unsupported schema error=%v", err)
			}
			var count int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM neutral_import_sources").Scan(&count); err != nil || count != 0 {
				t.Fatalf("source pinned before schema validation: count=%d error=%v", count, err)
			}
		})
	}
}

func historicalCoreAttemptReceipts(t *testing.T, source CoreExecutionImportSource, copyHash string) []eventstore.Receipt {
	t.Helper()
	var receipts []eventstore.Receipt
	err := postgres.ReadNeutralImportBundle(t.Context(), source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if copyHash == "" {
			copyHash = bundle.Identity().SourceSHA256
		}
		rows, err := readCoreAttemptImportPage(t.Context(), newCoreNeutralImport(bundle), source.OrganizationID, 0)
		if err != nil {
			return err
		}
		for _, row := range rows {
			a := row.Attempt
			legacy := struct {
				Attempt struct {
					ClaimDisposition       *string
					RuntimeHeartbeatAt     *time.Time
					RuntimeSummarySequence int64
					CleanupAcknowledgedAt  *time.Time
					OrganizationID         tracker.OrganizationID
					ID                     string
					IssueID                int64
					LeaseID                string
					FencingToken           int64
					RunID                  string
					Sequence               int64
					Status                 string
					WorkItemRevision       int64
					DispatchGeneration     int64
					PayloadObjectKey       string
					CheckpointObjectKey    *string
					ArtifactsObjectKey     *string
					StartedAt              time.Time
					UpdatedAt              time.Time
				}
				Payload          string
				Checkpoint       sql.NullString
				ArtifactIDs      string
				PayloadObject    sql.NullString
				CheckpointObject sql.NullString
			}{Payload: row.Payload, Checkpoint: row.Checkpoint, ArtifactIDs: row.ArtifactIDs, PayloadObject: row.PayloadObject, CheckpointObject: row.CheckpointObject}
			legacy.Attempt.ClaimDisposition = a.ClaimDisposition
			legacy.Attempt.RuntimeHeartbeatAt = a.RuntimeHeartbeatAt
			legacy.Attempt.RuntimeSummarySequence = a.RuntimeSummarySequence
			legacy.Attempt.CleanupAcknowledgedAt = a.CleanupAcknowledgedAt
			legacy.Attempt.OrganizationID = a.OrganizationID
			legacy.Attempt.ID = a.ID
			legacy.Attempt.IssueID = a.IssueID
			legacy.Attempt.LeaseID = a.LeaseID
			legacy.Attempt.FencingToken = a.FencingToken
			legacy.Attempt.RunID = a.RunID
			legacy.Attempt.Sequence = a.Sequence
			legacy.Attempt.Status = a.Status
			legacy.Attempt.WorkItemRevision = a.WorkItemRevision
			legacy.Attempt.DispatchGeneration = a.DispatchGeneration
			legacy.Attempt.PayloadObjectKey = a.PayloadObjectKey
			legacy.Attempt.CheckpointObjectKey = a.CheckpointObjectKey
			legacy.Attempt.ArtifactsObjectKey = a.ArtifactsObjectKey
			legacy.Attempt.StartedAt = a.StartedAt
			legacy.Attempt.UpdatedAt = a.UpdatedAt
			receipt, err := prepareCoreExecutionReceipt(t.Context(), eventstore.NewContent(source.Objects), source.OrganizationID, copyHash, "core-attempt", a.ID, legacy)
			if err != nil {
				return err
			}
			t.Logf("pre-correlation receipt attempt=%s request_hash=%s", a.ID, receipt.RequestHash)
			receipts = append(receipts, receipt)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipts
}

func TestCoreWorkEventObjectPreservesImportReceipt(t *testing.T) {
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			content := eventstore.NewContent(&memoryRunDetails{})
			row := coreWorkEventImportRow{Event: postgres.CoreWorkEventImport{OrganizationID: org, ID: 7, IssueID: 41, Kind: "progress", PayloadJSON: ` {"step": "compile"} `, OccurredAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), RecordedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}}
			before, err := prepareCoreExecutionReceipt(t.Context(), content, org, strings.Repeat("a", 64), "core-work-event", "7", row)
			if err != nil {
				t.Fatal(err)
			}
			row.Event.Payload, err = content.PreparePayload(t.Context(), org, []byte(row.Event.PayloadJSON))
			if err != nil {
				t.Fatal(err)
			}
			after, err := prepareCoreExecutionReceipt(t.Context(), content, org, strings.Repeat("a", 64), "core-work-event", "7", row)
			if err != nil || before.RequestHash != after.RequestHash || before.PayloadObjectKey != after.PayloadObjectKey || before.PayloadSHA256 != after.PayloadSHA256 {
				t.Fatalf("object preparation changed historical receipt identity: before=%s after=%s error=%v", before.RequestHash, after.RequestHash, err)
			}
		})
	}
}
