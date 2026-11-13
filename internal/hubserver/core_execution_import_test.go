package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/diffbody"

	"github.com/digitaldrywood/detent/internal/storagecatalog"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreExecutionImportPreservesHistoricalFenceAndRevisionZero(t *testing.T) {
	t.Parallel()
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
			copyPath := coreExecutionSourceCopy(t, organization, objects)
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
					if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET local_attempt_id=NULL,local_attempt_id_recorded=false,checkpoint_preserves_source=NULL WHERE organization_id=$1`, organization); err != nil {
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
							{"source key", func(record *postgres.CoreAttemptImport) {
								record.Attempt.CheckpointObjectKey = new("unverified/source/checkpoint")
								record.Attempt.CheckpointPreservesSource = new(true)
							}},
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

			wantDetailUploaders := map[tracker.OrganizationID]string{"org_execution_import": "", "org_execution_import_other": "runner_token_execution_import_second"}
			var detailUploaders string
			if err := admin.QueryRowContext(t.Context(), `SELECT coalesce(string_agg(uploader, ',' ORDER BY id),'') FROM attachments WHERE organization_id=$1 AND kind='archive'`, organization).Scan(&detailUploaders); err != nil {
				t.Fatal(err)
			}
			if detailUploaders != wantDetailUploaders[organization] {
				t.Fatalf("imported checkpoint detail uploaders = %q, want %q", detailUploaders, wantDetailUploaders[organization])
			}
			for _, suffix := range []string{"", "_second"} {
				var snapshot hubrecords.TenantAttemptSnapshot
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					snapshot.Attempt, err = r.Attempt(t.Context(), "attempt_execution_import"+suffix)
					if err != nil {
						return err
					}
					snapshot.Event = eventstore.Event{OrganizationID: organization, ProjectID: project}
					return hubrecords.ReadTenantAttemptCheckpointPayload(t.Context(), r, &snapshot)
				}); err != nil {
					t.Fatal(err)
				}
				reader := &Service{config: Config{TenantRepository: repository, RunDetails: objects}}
				checkpoint, err := reader.readNativeAttemptCheckpointTenant(t.Context(), snapshot)
				if err != nil || checkpoint == nil || checkpoint.PreservesSource() != (suffix == "") {
					t.Fatalf("imported verified format hydration %s: %+v err=%v", suffix, checkpoint, err)
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
				if attempt.LocalAttemptID != 42 || attempt.FencingToken != lease.FencingToken || attempt.WorkItemRevision != 0 || attempt.Status != "failed" || attempt.PayloadObjectKey == "" {
					return errors.New("historical attempt identity or zero revision was not preserved")
				}
				for _, suffix := range []string{"", "_second"} {
					imported, err := r.Attempt(t.Context(), "attempt_execution_import"+suffix)
					if err != nil {
						return err
					}
					if imported.CheckpointObjectKey == nil || imported.CheckpointPreservesSource == nil || *imported.CheckpointPreservesSource != (suffix == "") {
						t.Fatalf("verified imported checkpoint classification %s: %+v", suffix, imported)
					}
					snapshot := hubrecords.TenantAttemptSnapshot{Attempt: imported, Event: eventstore.Event{OrganizationID: organization, ProjectID: project}}
					if err := hubrecords.ReadTenantAttemptCheckpointPayload(t.Context(), r, &snapshot); err != nil {
						return err
					}
					if snapshot.CheckpointPayload == nil {
						t.Fatal("verified imported checkpoint reference was not registered")
					}

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

func coreExecutionSourceCopy(t *testing.T, organization tracker.OrganizationID, objects *memoryRunDetails) string {
	t.Helper()
	fixture := coreExecutionNeutralFixture(t, organization)
	for index, suffix := range []string{"", "_second"} {
		id := "attempt_execution_import" + suffix
		checkpoint := &tracker.NativeCheckpoint{Resume: "fresh_checkout", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"}
		if index == 0 {
			checkpoint.WorktreeState = "unpushed"
		}
		var value any = checkpoint
		if organization == "org_execution_import" && index == 0 {
			value = checkpointPayload{Checkpoint: checkpoint}
		}
		if organization == "org_execution_import_other" {
			run := tracker.NativeRunData{AttemptID: id, LeaseID: tracker.LeaseID("lease_execution_import" + suffix), FencingToken: tracker.FencingToken(40 + index), RunID: "run_execution_import" + suffix, Sequence: 1, Handoff: checkpoint}
			var data any = run
			if index == 1 {
				data = map[string]tracker.NativeRunData{hubrecords.RunDetailEntry(run): run}
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(t.Context(), string(organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			fixture.Add("change_content_objects", map[string]any{"object_key": ref.Key, "object_sha256": ref.SHA256, "object_bytes": ref.CompressedSize, "content_bytes": ref.UncompressedSize, "pending_content": raw})
			detail := hubrecords.NewRunDetailReference(ref, string(organization), hubrecords.RunDetailEntry(run))
			if index == 0 {
				detail.ObjectKey = ref.Key
			} else {
				fixture.Add("attachments", map[string]any{"id": detail.AttachmentID, "project_id": "prj_execution_import", "kind": "archive", "object_key": ref.Key, "sha256": ref.SHA256, "size": ref.CompressedSize, "uncompressed_size": ref.UncompressedSize, "codec": ref.Codec, "schema_version": ref.SchemaVersion, "retention_class": ref.Retention, "storage_binding_id": "", "customer_owned": 0, "uploader": "runner_token_execution_import_second", "name": "archive.gz", "content_type": "application/gzip", "created_at": "2026-10-01T00:00:00Z"})
			}
			value = storedRunCheckpoint{Detail: detail}
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Set("native_attempts", map[string]string{"id": id}, map[string]any{"checkpoint_json": string(raw)})
	}
	return fixture.Write()
}

func coreExecutionNeutralFixture(t *testing.T, organization tracker.OrganizationID) *neutralimporttest.Fixture {
	t.Helper()
	fixture := newCoreNeutralFixture(t, organization, "issues", "machines", "leases", "native_attempts", "native_attempt_events", "change_content_objects", "attachments", "api_tokens", "runner_enrollments", "runner_enrollment_projects", "runner_identities", "runner_project_capacity_limits", "runner_project_ranks", "runner_identity_events", "lease_runners", "lease_policies", "work_events", "attempt_usage", "attempt_diffs", "attempt_diff_files", "attempt_diff_file_archives")
	const acquired = "2026-10-01T00:00:00Z"
	const updated = "2026-10-02T00:00:00Z"
	const expired = "2026-10-01T01:00:00Z"
	for index, suffix := range []string{"", "_second"} {
		id := func(value string) string { return value + suffix }
		fixture.Add("issues", map[string]any{"id": 41 + index, "organization_id": organization, "project_id": "prj_execution_import", "native_id": id("wi_execution_import")})
		fixture.Add("machines", map[string]any{"id": id("machine_execution_import"), "organization_id": organization, "hostname": "example-host", "display_name": "Example host", "capacity": 2, "version": "v0.173.4", "sprite_name": "", "last_heartbeat_at": acquired, "registered_at": acquired, "updated_at": updated})
		var expiry any = "2027-10-01T00:00:00Z"
		if index == 1 {
			expiry = nil
		}
		fixture.Add("api_tokens", map[string]any{"id": id("runner_token_execution_import"), "expires_at": expiry})
		fixture.Add("runner_enrollments", map[string]any{"id": id("enrollment_execution_import"), "organization_id": organization, "runner_id": id("runner_execution_import"), "machine_id": id("machine_execution_import"), "token_hash": coreExecutionHash(byte('3' + index)), "scope": "projects", "shared_machine": 0, "operations_json": "[\"read\",\"claim\"]", "created_at": acquired, "expires_at": "2027-10-01T00:00:00Z", "created_by": "owner_token_execution_import", "redeemed_at": acquired, "revoked_at": nil})
		fixture.Add("runner_enrollment_projects", map[string]any{"enrollment_id": id("enrollment_execution_import"), "project_id": "prj_execution_import"})
		fixture.Add("runner_identities", map[string]any{"id": id("runner_execution_import"), "organization_id": organization, "machine_id": id("machine_execution_import"), "token_id": id("runner_token_execution_import"), "enrollment_id": id("enrollment_execution_import"), "scope": "projects", "state": "active", "revision": 1, "tags_json": "[]", "operations_json": "[\"read\",\"claim\"]", "display_name": "Example runner", "capacity_limit": 2, "reported_capacity": 2, "os": "darwin", "architecture": "arm64", "created_at": acquired, "last_heartbeat_at": updated, "removed_at": nil, "routing_settings_json": "{}", "capacity_configuration_json": "null"})
		fixture.Add("runner_identity_events", map[string]any{"id": 9 + index, "runner_id": id("runner_execution_import"), "actor_id": "owner_token_execution_import", "kind": "enrolled", "occurred_at": acquired})
		fixture.Add("leases", map[string]any{"fencing_token": 40 + index, "lease_id": id("lease_execution_import"), "issue_id": 41 + index, "machine_id": id("machine_execution_import"), "session_id": id("session_execution_import"), "work_item_revision": 0, "landing_capacity": 0, "acquired_at": acquired, "renewed_at": acquired, "expires_at": expired, "released_at": updated})
		fixture.Add("lease_runners", map[string]any{"lease_id": id("lease_execution_import"), "runner_id": id("runner_execution_import")})
		fixture.Add("lease_policies", map[string]any{"lease_id": id("lease_execution_import"), "scope": string(organization) + "/prj_execution_import", "policy_id": "policy_example"})
		run := tracker.NativeRunData{AttemptID: id("attempt_execution_import"), LeaseID: tracker.LeaseID(id("lease_execution_import")), FencingToken: tracker.FencingToken(40 + index), RunID: id("run_execution_import"), Sequence: 1, PolicyID: "policy_example", Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 42}}
		runData, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("native_attempts", map[string]any{"id": id("attempt_execution_import"), "organization_id": organization, "project_id": "prj_execution_import", "work_item_id": id("wi_execution_import"), "lease_id": id("lease_execution_import"), "fencing_token": 40 + index, "run_id": id("run_execution_import"), "sequence": 1, "status": "failed", "work_item_revision": 0, "dispatch_generation": 0, "data_json": string(runData), "checkpoint_json": nil, "artifact_ids_json": "[]", "started_at": updated, "updated_at": updated, "payload_object_key": "", "checkpoint_object_key": ""})
		requestHash := hex.EncodeToString(make([]byte, 32))
		fixture.Add("native_attempt_events", map[string]any{"attempt_id": id("attempt_execution_import"), "sequence": 1, "request_hash": requestHash})
		fixture.Add("attempt_usage", map[string]any{"attempt_id": id("attempt_execution_import"), "project_id": "prj_execution_import", "period": acquired, "provider": "codex", "model": "example-model", "input": 100, "cached_input": 20, "output": 30, "cost_estimate": 0.25, "currency": "USD", "updated_at": updated})

		fixture.Add("work_events", map[string]any{"id": 7 + index, "issue_id": 41 + index, "fencing_token": 40 + index, "machine_id": id("machine_execution_import"), "session_id": id("session_execution_import"), "run_id": id("run_execution_import"), "kind": "progress", "payload_json": "{\"step\":\"compile\"}", "occurred_at": updated, "recorded_at": updated})
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
	t.Parallel()
	path := filepath.Join(t.TempDir(), "historical-source.db")
	original := []byte("preserved verified historical source")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	for _, bundle := range []string{"", "unused-neutral-bundle"} {
		_, err := ImportCoreExecution(t.Context(), &postgres.Repository{}, CoreExecutionImportSource{CopyPath: path, BundlePath: bundle, OrganizationID: "org_example", SourceObjects: &memoryRunDetails{}, Objects: &memoryRunDetails{}})
		if !errors.Is(err, neutralimport.ErrInvalid) {
			t.Fatalf("legacy selector error=%v", err)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("historical source changed: %v", err)
	}
}

func TestCoreExecutionImportRejectsUnsupportedSchemaBeforePin(t *testing.T) {
	t.Parallel()
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
			table := fixture.Tables[scenario.table]
			if scenario.change == "extra" {
				table.Columns["unaccounted"] = fmt.Sprintf(`[%d,"unaccounted","TEXT",1,null,0,0]`, len(table.Columns))
				for _, row := range fixture.Records[scenario.table] {
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
			fixture.Tables[scenario.table] = table
			objects := &memoryRunDetails{}
			_, err := ImportCoreExecution(t.Context(), repository, CoreExecutionImportSource{BundlePath: fixture.Write(), OrganizationID: fixture.Organization, SourceObjects: objects, Objects: objects})
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
	err := neutralimport.Read(t.Context(), source.BundlePath, source.OrganizationID, func(bundle *neutralimport.Bundle) error {
		if copyHash == "" {
			copyHash = bundle.Identity().SourceSHA256
		}
		rows, err := readCoreAttemptImportPage(t.Context(), neutralimport.NewQuery(bundle), source.OrganizationID, 0)
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
	t.Parallel()
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

func seedCoreExecutionPayloadTarget(t *testing.T, repository *postgres.Repository, admin *sql.DB, organization tracker.OrganizationID) {
	t.Helper()
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
}

func TestCoreExecutionDiffImportPreservesBodiesAndRejectsIncompleteCustody(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"inline", "manifest", "file_archive", "body", "expired", "missing_body", "corrupt_body", "wrong_producer"} {
		t.Run(shape, func(t *testing.T) {
			repository, admin := coreImportPostgresWithAdmin(t, 3)
			objects := &memoryRunDetails{}
			sourceBodies := &coreExecutionDiffBodyStorage{data: map[string][]byte{}}
			targetBodies := &coreExecutionDiffBodyStorage{data: map[string][]byte{}}
			for _, organization := range []tracker.OrganizationID{"org_diff_import_first", "org_diff_import_second"} {
				seedCoreExecutionPayloadTarget(t, repository, admin, organization)
				fixture := coreExecutionNeutralFixture(t, organization)
				expected := map[string][]tracker.AttemptDiffFile{}
				for index, suffix := range []string{"", "_second"} {
					diffID := "diff_payload_import" + suffix
					patch := "@@ -1 +1 @@\n-old\n+" + strings.Repeat("payload", 1024) + "\n"
					file := tracker.AttemptDiffFile{Path: "example.go", Status: tracker.DiffStatusModified, Patch: patch}
					parent := map[string]any{"id": diffID, "organization_id": organization, "project_id": "prj_execution_import", "work_item_id": "wi_execution_import" + suffix, "attempt_id": "attempt_execution_import" + suffix, "source": "attempt", "source_id": "", "seq": 1, "base_sha": strings.Repeat("a", 40), "head_sha": strings.Repeat("b", 40), "producer_kind": "attempt", "producer_id": "attempt_execution_import" + suffix, "producer_runner_id": "runner_execution_import" + suffix, "producer_lease_id": "lease_execution_import" + suffix, "producer_fencing_token": 40 + index, "file_count": 1, "patch_bytes": len(patch), "posted_bytes": len(patch), "created_at": "2026-10-02T00:00:00Z"}
					body := diffbody.New(string(organization), "prj_execution_import", diffID, 0, patch)
					if shape == "body" || shape == "missing_body" || shape == "corrupt_body" || shape == "expired" {
						file.Patch = ""
						file.PatchBody = &body
						if shape != "missing_body" && shape != "expired" {
							sourceBodies.data[body.Key] = []byte(patch)
						}
						if shape == "corrupt_body" {
							sourceBodies.data[body.Key] = []byte("corrupt")
						}
						if shape == "expired" {
							file.PatchBody = nil
							file.PatchExpired = true
						}
					}
					if shape == "wrong_producer" {
						parent["producer_fencing_token"] = 99
					}
					switch shape {
					case "manifest":
						raw, _ := json.Marshal([]tracker.AttemptDiffFile{file})
						ref, err := objects.StoreDetail(t.Context(), string(organization), raw)
						if err != nil {
							t.Fatal(err)
						}
						parent["object_key"], parent["object_sha256"], parent["object_bytes"], parent["content_bytes"] = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
					case "file_archive":
						raw, _ := json.Marshal(struct {
							DiffID   string                  `json:"diff_id"`
							Position int                     `json:"position"`
							File     tracker.AttemptDiffFile `json:"file"`
						}{diffID, 0, file})
						ref, err := objects.StoreDetail(t.Context(), string(organization), raw)
						if err != nil {
							t.Fatal(err)
						}
						fixture.Add("attempt_diff_file_archives", map[string]any{"organization_id": organization, "project_id": "prj_execution_import", "diff_id": diffID, "position": 0, "object_key": ref.Key, "object_sha256": ref.SHA256, "object_bytes": ref.CompressedSize, "content_bytes": ref.UncompressedSize})
					default:
						values := map[string]any{"diff_id": diffID, "position": 0, "path": file.Path, "old_path": file.OldPath, "status": file.Status, "additions": 0, "deletions": 0, "binary": false, "patch": file.Patch, "truncated": false, "denied": false, "patch_expired": file.PatchExpired}
						if file.PatchBody != nil || shape == "expired" {
							values["patch_object"], values["patch_sha256"], values["patch_size"] = body.Key, body.SHA256, body.Bytes
						}
						fixture.Add("attempt_diff_files", values)
					}
					fixture.Add("attempt_diffs", parent)
					expected[diffID] = []tracker.AttemptDiffFile{file}
				}
				source := CoreExecutionImportSource{BundlePath: fixture.Write(), OrganizationID: organization, SourceObjects: objects, Objects: objects, SourceDiffBodies: sourceBodies, DiffBodies: targetBodies}
				failure := shape == "missing_body" || shape == "corrupt_body" || shape == "wrong_producer"
				for pass := range 2 {
					count, err := ImportCoreExecution(t.Context(), repository, source)
					if failure {
						if err == nil {
							t.Fatal("historical diff import accepted incomplete custody")
						}
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							for _, suffix := range []string{"", "_second"} {
								_, err := r.AttemptDiff(t.Context(), "prj_execution_import", "attempt_execution_import"+suffix, tracker.DiffGeneration{Source: "attempt", Seq: 1})
								if !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("failed diff import persisted row: %v", err)
								}
								files := expected["diff_payload_import"+suffix]
								if len(files) == 1 && files[0].PatchBody != nil {
									referenced, _, err := r.AttemptDiffBodyReferenced(t.Context(), "prj_execution_import", files[0].PatchBody.Key)
									if err != nil {
										return err
									}
									if referenced {
										t.Fatal("failed diff import persisted body ownership")
									}
								}
								_, err = r.Receipt(t.Context(), "production-import", "core-attempt-diff", "diff_payload_import"+suffix)
								if !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("failed diff import persisted receipt: %v", err)
								}
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						continue
					}
					if err != nil || count != 16 {
						t.Fatalf("diff import pass=%d count=%d error=%v", pass, count, err)
					}
					var records []struct {
						row     leasestore.AttemptDiff
						archive artifactstore.ArchiveObject
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						for _, suffix := range []string{"", "_second"} {
							row, err := r.AttemptDiff(t.Context(), "prj_execution_import", "attempt_execution_import"+suffix, tracker.DiffGeneration{Source: "attempt", Seq: 1})
							if err != nil {
								return err
							}
							archive, err := r.ArchiveObject(t.Context(), row.ArchiveID)
							if err != nil {
								return err
							}
							records = append(records, struct {
								row     leasestore.AttemptDiff
								archive artifactstore.ArchiveObject
							}{row, archive})
							files := expected[row.ID]
							if len(files) == 1 && files[0].PatchBody != nil {
								reference := *files[0].PatchBody
								referenced, complete, err := r.AttemptDiffBodyReferenced(t.Context(), "prj_execution_import", reference.Key)
								if err != nil {
									return err
								}
								if !referenced || !complete {
									t.Fatal("diff body index is incomplete")
								}
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					for _, record := range records {
						raw, err := objects.ReadDetail(t.Context(), string(organization), record.archive.Reference)
						if err != nil {
							t.Fatal(err)
						}
						var files []tracker.AttemptDiffFile
						if err := json.Unmarshal(raw, &files); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(files, expected[record.row.ID]) {
							t.Fatalf("historical diff changed files: %+v", files)
						}
						if len(files) == 1 && files[0].PatchBody != nil {
							reference := *files[0].PatchBody
							got := targetBodies.data[reference.Key]
							if int64(len(got)) != reference.Bytes || artifact.Digest(got) != reference.SHA256 {
								t.Fatal("diff body custody was not copied exactly")
							}
						}
					}

				}
			}
		})
	}
}

type coreExecutionDiffBodyStorage struct {
	attachment.Storage
	data map[string][]byte
}

func (s *coreExecutionDiffBodyStorage) Put(_ context.Context, key string, body io.ReadSeeker, size int64, _, digest string) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(raw)) != size || artifact.Digest(raw) != digest {
		return artifact.ErrIntegrity
	}
	s.data[key] = append([]byte(nil), raw...)
	return nil
}
func (s *coreExecutionDiffBodyStorage) Open(_ context.Context, key string) (io.ReadCloser, error) {
	raw, ok := s.data[key]
	if !ok {
		return nil, artifact.ErrStorage
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func TestCoreExecutionImportRetainsArtifactsAfterTerminalPayloadClearsIDs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		object   bool
		latest   []string
		retained string
		refused  bool
	}{
		{name: "inline retained artifacts", retained: `["artifact_example","artifact_other"]`},
		{name: "object retained artifacts", object: true, retained: `["artifact_example","artifact_other"]`},
		{name: "matching artifacts", latest: []string{"artifact_example"}, retained: `["artifact_example"]`},
		{name: "conflicting nonempty artifacts", latest: []string{"artifact_conflict"}, retained: `["artifact_example"]`, refused: true},
		{name: "missing retained artifacts", latest: []string{"artifact_example"}, retained: `[]`, refused: true},
		{name: "invalid retained artifacts", retained: `{}`, refused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, admin := coreImportPostgresWithAdmin(t, 3)
			objects := &memoryRunDetails{}
			for _, organization := range []tracker.OrganizationID{"org_artifact_import_first", "org_artifact_import_second"} {
				seedCoreExecutionPayloadTarget(t, repository, admin, organization)
				fixture := coreExecutionNeutralFixture(t, organization)
				expected := map[string]attachment.ArchiveReference{}
				originals := map[string][]byte{}
				for index, suffix := range []string{"", "_second"} {
					attemptID := "attempt_execution_import" + suffix
					run := tracker.NativeRunData{AttemptID: attemptID, LeaseID: tracker.LeaseID("lease_execution_import" + suffix), FencingToken: tracker.FencingToken(40 + index), RunID: "run_execution_import" + suffix, Sequence: 1, PolicyID: "policy_example", ArtifactIDs: test.latest, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 42}}
					raw, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatal(err)
					}
					fields["historical_detail"] = json.RawMessage(`{"preserved":true}`)
					raw, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					updates := map[string]any{"data_json": string(raw), "artifact_ids_json": test.retained}
					if test.object {
						ref, err := objects.StoreDetail(t.Context(), string(organization), raw)
						if err != nil {
							t.Fatal(err)
						}
						fixture.Add("change_content_objects", map[string]any{"object_key": ref.Key, "object_sha256": ref.SHA256, "object_bytes": ref.CompressedSize, "content_bytes": ref.UncompressedSize, "pending_content": raw})
						updates["payload_object_key"] = ref.Key
						updates["data_json"] = "{}"
						originals[ref.Key] = raw
					}
					fixture.Set("native_attempts", map[string]string{"id": attemptID}, updates)
					if !test.refused {
						if len(test.latest) == 0 {
							fields["artifact_ids"] = json.RawMessage(test.retained)
						}
						expectedRaw, err := json.Marshal(fields)
						if err != nil {
							t.Fatal(err)
						}
						ref, _, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader(expectedRaw))
						if err != nil {
							t.Fatal(err)
						}
						expected[attemptID] = ref
					}
				}
				source := CoreExecutionImportSource{BundlePath: fixture.Write(), OrganizationID: organization, SourceObjects: objects, Objects: objects}
				for pass := range 2 {
					count, err := ImportCoreExecution(t.Context(), repository, source)
					if test.refused {
						if err == nil {
							t.Fatal("execution import accepted conflicting artifact custody")
						}
					} else if err != nil || count != 14 {
						t.Fatalf("artifact import pass=%d count=%d error=%v", pass, count, err)
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						for _, suffix := range []string{"", "_second"} {
							attemptID := "attempt_execution_import" + suffix
							row, err := r.Attempt(t.Context(), attemptID)
							if test.refused {
								if !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("refused artifact import persisted attempt: %v", err)
								}
								_, err = r.Receipt(t.Context(), "production-import", "core-attempt", attemptID)
								if !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("refused artifact import persisted receipt: %v", err)
								}
								continue
							}
							if err != nil {
								return err
							}
							if row.PayloadObjectKey != expected[attemptID].Key || row.ArtifactsObjectKey == nil || row.LocalAttemptID != 42 || row.Sequence != 1 {
								t.Fatalf("retained artifact snapshot changed metadata: %+v", row)
							}
							payloadArchive, err := r.ArchiveObject(t.Context(), "archive_"+expected[attemptID].SHA256)
							if err != nil || payloadArchive.Reference != expected[attemptID] {
								t.Fatalf("imported payload archive custody=%+v error=%v", payloadArchive, err)
							}
							artifactRef, _, err := attachment.PrepareArchive(string(organization), 1, strings.NewReader(test.retained))
							if err != nil {
								return err
							}
							artifactArchive, err := r.ArchiveObject(t.Context(), "archive_"+artifactRef.SHA256)
							if err != nil || artifactArchive.Reference != artifactRef || *row.ArtifactsObjectKey != artifactRef.Key {
								t.Fatalf("imported artifact archive custody=%+v error=%v", artifactArchive, err)
							}
							metadataReader, ok := r.(tenantstore.AttemptMetadataReader)
							if !ok {
								t.Fatal("imported attempt has no typed metadata owner")
							}
							metadata, err := metadataReader.AttemptMetadata(t.Context(), "prj_execution_import", tracker.NativeWorkItemID("wi_execution_import"+suffix), []leasestore.Attempt{row}, false)
							if err != nil {
								return err
							}
							if metadata.Archives[artifactRef.Key].Reference != artifactRef || metadata.Archives[row.PayloadObjectKey].Reference != expected[attemptID] {
								t.Fatal("typed attempt snapshot omitted imported artifact or payload custody")
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					for attemptID, ref := range expected {
						raw, err := objects.ReadDetail(t.Context(), string(organization), ref)
						if err != nil {
							t.Fatal(err)
						}
						var run tracker.NativeRunData
						if err := json.Unmarshal(raw, &run); err != nil {
							t.Fatal(err)
						}
						var retained []string
						if err := json.Unmarshal([]byte(test.retained), &retained); err != nil {
							t.Fatal(err)
						}
						if run.AttemptID != attemptID || !sameStringSlice(run.ArtifactIDs, retained) {
							t.Fatalf("retained artifact payload=%+v", run)
						}
						var fields map[string]json.RawMessage
						if err := json.Unmarshal(raw, &fields); err != nil {
							t.Fatal(err)
						}
						if string(fields["historical_detail"]) != `{"preserved":true}` {
							t.Fatal("artifact normalization discarded historical payload fields")
						}
					}
					for key, original := range originals {
						ref, _, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader(original))
						if err != nil {
							t.Fatal(err)
						}
						raw, err := objects.ReadDetail(t.Context(), string(organization), ref)
						if err != nil || key != ref.Key || !bytes.Equal(raw, original) {
							t.Fatalf("artifact normalization changed source object: %v", err)
						}
					}
				}
			}
		})
	}
}
