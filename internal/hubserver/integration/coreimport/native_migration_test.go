package coreimport_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRuntimeMigrationPreservesHistory(t *testing.T) {
	t.Parallel()
	migrations, err := postgres.MigrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	historical := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		if _, err := db.ExecContext(ctx, postgres.RolesSQL()); err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
			return err
		}
		provider, err := hubMigrationProvider(db)
		if err != nil {
			return err
		}
		_, err = provider.UpTo(ctx, 20261107212205)
		return err
	})
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := historical.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	admin := historical.Database(t)
	objects := newKeyedDetails()
	content := eventstore.NewContent(objects)
	stamp := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	expected := map[tracker.OrganizationID][]tracker.CollaborationEvent{}
	for _, organization := range []tracker.OrganizationID{"org_history_first", "org_history_second"} {
		for _, statement := range []string{
			`INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`,
			`INSERT INTO projects(organization_id,id,name,profile,created_at) VALUES($1,'prj_history','Example history','native',$2)`,
			`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,created_at,updated_at) VALUES($1,41,'wi_history','prj_history',1,'Retained history',$2,$2)`,
			`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,'machine_history','example',1,'example',$2,$2,$2)`,
		} {
			if _, err := admin.ExecContext(t.Context(), statement, organization, stamp); err != nil {
				t.Fatal(err)
			}
		}
		var fence int64
		if err := admin.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) VALUES($1,'lease_history',41,'machine_history','session_history',1,$2,$2,$2::timestamptz+interval '1 hour') RETURNING fencing_token`, organization, stamp).Scan(&fence); err != nil {
			t.Fatal(err)
		}
		run := tracker.NativeRunData{AttemptID: "attempt_history", RunID: "run_history", LeaseID: "lease_history", FencingToken: tracker.FencingToken(fence), Sequence: 1, Runtime: &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: stamp}}
		observed := run
		observed.Sequence = 2
		observed.Runtime = &tracker.NativeRuntimeObservation{Phase: "validation", HeartbeatAt: stamp.Add(time.Second)}
		values := []tracker.CollaborationEvent{
			{ID: "history_created", Type: "issue.created", Data: tracker.CollaborationData{Revision: 1}},
			{ID: "history_claimed", Type: "scheduler.decision", Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "claimed"}}},
			{ID: "history_started", Type: "run.started", Data: tracker.CollaborationData{Run: &run}},
			{ID: "history_observed", Type: "run.observed", Data: tracker.CollaborationData{Run: &observed}},
		}
		for index := range values {
			value := &values[index]
			value.OrganizationID, value.ProjectID, value.AggregateID = organization, "prj_history", "wi_history"
			value.AggregateType, value.SchemaVersion, value.AggregateSequence = "issue", 1, int64(index+1)
			value.RecordedAt, value.Actor = stamp.Add(time.Duration(index)*time.Second), tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			event, err := content.PrepareEvent(t.Context(), organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,run_id,attempt_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,event_revision) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, organization, event.ProjectID, event.WorkItemID, event.ID, event.Sequence, event.Type, event.ActorKind, event.ActorPrincipalID, event.RunID, event.AttemptID, event.RecordedAt, event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes, event.EventRevision); err != nil {
				t.Fatal(err)
			}
			if value.Type == "run.started" {
				rawAttempt, err := json.Marshal(observed)
				if err != nil {
					t.Fatal(err)
				}
				payload, err := objects.StoreDetail(t.Context(), string(organization), rawAttempt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,'run_history',41)`, organization); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at) VALUES($1,'attempt_history',41,'lease_history',$2,'run_history',2,'running',1,0,$3,$4,$4)`, organization, fence, payload.Key, stamp); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,id,native_id,project_id,number,title) VALUES($1,42,'wi_retained','prj_history',2,'Retained identity')`, organization); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `DELETE FROM issues WHERE organization_id=$1 AND id=42`, organization); err != nil {
			t.Fatal(err)
		}
		expected[organization] = values
	}
	admin.SetMaxOpenConns(1)
	if _, err := admin.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	provider, err := hubMigrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 20261107212206); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET runtime_heartbeat_at=$1,runtime_summary_sequence=2`, stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	const guardedFile = "20261107060339_retain_work_item_identities.sql"
	guarded, err := fs.ReadFile(migrations, guardedFile)
	if err != nil {
		t.Fatal(err)
	}
	const ledger = "retained_history_rollback_proof"
	rollback, err := goose.NewProvider(goose.DialectPostgres, admin, postgres.RoleMigrations(fstest.MapFS{guardedFile: &fstest.MapFile{Data: guarded}}), goose.WithDisableGlobalRegistry(true), goose.WithTableName(ledger))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rollback.GetVersions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "INSERT INTO "+ledger+"(version_id,is_applied) VALUES(20261107060339,true)"); err != nil {
		t.Fatal(err)
	}
	if _, err := rollback.DownTo(t.Context(), 0); err == nil || !strings.Contains(err.Error(), "retained work item history prevents downgrade") {
		t.Fatalf("rollback did not protect retained history: %v", err)
	}
	version, _, err := rollback.GetVersions(t.Context())
	if err != nil || version != 20261107060339 {
		t.Fatalf("failed rollback changed schema: %d %v", version, err)
	}
	if _, err := admin.ExecContext(t.Context(), "DROP TABLE "+ledger); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM work_item_identities i LEFT JOIN issues live USING(organization_id,id) WHERE i.organization_id IN ('org_history_first','org_history_second') AND i.id=42 AND live.id IS NULL`).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("failed rollback changed retained identities: %d %v", retained, err)
	}
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_attempts WHERE runtime_summary_sequence=2 AND runtime_heartbeat_at=$1`, stamp.Add(time.Second)).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("failed rollback changed runtime evidence: %d %v", retained, err)
	}
	var schemaVersion int64
	if err := admin.QueryRowContext(t.Context(), `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&schemaVersion); err != nil || schemaVersion != 20261107212206 {
		t.Fatalf("failed rollback changed migration head: %d %v", schemaVersion, err)
	}
	migration := roleConnection(t, historical, admin, "detent_migration_v2")
	config := postgres.Config{Connection: roleConnection(t, historical, admin, "detent_runtime_v2"), Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
	if err := postgres.Migrate(t.Context(), migration, migrations); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err := openHubRepository(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			for organization, want := range expected {
				if err := store.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
					events, err := reader.Events(t.Context(), "prj_history", "wi_history", 0, 10)
					if err != nil {
						return err
					}
					if len(events) != len(want) {
						return fmt.Errorf("history length=%d want=%d", len(events), len(want))
					}
					for index, event := range events {
						value, err := content.Event(t.Context(), event)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(value, want[index]) {
							return fmt.Errorf("migration changed retained event %s: %+v", event.ID, value)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				for _, statement := range []string{"UPDATE collaboration_events SET type='issue.edited' WHERE organization_id=$1 AND id='history_created'", "DELETE FROM collaboration_events WHERE organization_id=$1 AND id='history_created'"} {
					if _, err := admin.ExecContext(t.Context(), statement, organization); err == nil {
						t.Fatal("migration lost append-only history enforcement")
					}
				}
			}
		}()
	}
	t.Run("activity backfill", func(t *testing.T) {
		names := []string{"updated_at fallback", "issue edit", "comment", "Workpad", "run start", "run finish", "history", "legacy event", "change version", "change review", "change check", "change discussion", "change landing", "linked PR", "external PR"}
		for index, name := range names {
			t.Run(name, func(t *testing.T) {
				repository, _ := coreImportPostgresWithAdmin(t, 3)
				for _, organization := range []tracker.OrganizationID{"org_activity_first", "org_activity_second"} {
					project := tracker.ProjectID("prj_activity")
					coreimporttest.SeedHistoricalTarget(t, repository, organization, project, "native")
					fixture := coreimporttest.WorkItems(t, os.DirFS("../../../.."), organization, project, false, coreimporttest.WorkItemBody())
					fixture.AddTables(t, os.DirFS("../../../.."), "projects", "native_attempts", "work_events", "change_issue_links", "change_requests", "change_versions", "change_evidence", "pull_requests")
					fixture.Add("projects", map[string]any{"id": project, "organization_id": organization, "name": "Example activity", "profile": "native", "repository_id": 1, "created_at": "2026-10-01T00:00:00Z"})
					base := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
					at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * time.Microsecond)
					stamp := at.Format(time.RFC3339Nano)
					nativeUpdated := base
					item := tracker.NativeWorkItemID("wi_core_import")
					fixture.Add("change_issue_links", map[string]any{"change_id": "activity-change", "organization_id": organization, "project_id": project, "work_item_id": item})
					fixture.Add("change_requests", map[string]any{"id": "activity-change", "organization_id": organization, "project_id": project, "work_item_id": item, "record_json": `{"updated_at":"2026-10-01T00:00:00Z"}`})
					fixture.Add("change_versions", map[string]any{"id": "activity-version", "change_id": "activity-change", "number": 1, "record_json": `{"created_at":"2026-10-01T00:00:00Z","external":{"provider":"github","id":"1","url":"https://github.com/example-org/example-project/pull/1"}}`})
					switch name {
					case "updated_at fallback":
						fixture.Set("issues", map[string]string{"id": "41"}, map[string]any{"updated_at": stamp})
					case "issue edit":
						fixture.Set("issues", map[string]string{"id": "41"}, map[string]any{"native_updated_at": stamp})
						value := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: item, Number: 1, Revision: 2, Profile: "native"}, Title: "Updated title", Body: coreimporttest.WorkItemBody(), State: "Todo", Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: base.Add(-24 * time.Hour), UpdatedAt: at, LastActivityAt: at}
						raw, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						fixture.Set("collaboration_versions", map[string]string{"record_id": string(item), "revision": "2"}, map[string]any{"encoding": "snapshot", "record_json": string(raw)})
						nativeUpdated = at
					case "comment", "Workpad":
						body := "Example comment"
						if name == "Workpad" {
							body = "## Codex Workpad"
						}
						fixture.Set("native_comments", map[string]string{"id": "comment_core_import"}, map[string]any{"updated_at": stamp, "body": body})
						value := tracker.NativeComment{OrganizationID: organization, ProjectID: project, WorkItemID: item, ID: "comment_core_import", Revision: 1, Sequence: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-commenter"}, Body: body, CreatedAt: base.Add(-24 * time.Hour), UpdatedAt: at}
						raw, err := json.Marshal(value)
						if err != nil {
							t.Fatal(err)
						}
						fixture.Set("collaboration_versions", map[string]string{"record_id": "comment_core_import", "revision": "1"}, map[string]any{"record_json": string(raw)})
					case "run start", "run finish":
						started, updated := "2026-10-01T00:00:00Z", "2026-10-01T00:00:00Z"
						if name == "run start" {
							started = stamp
						} else {
							updated = stamp
						}
						fixture.Add("native_attempts", map[string]any{"id": "activity-attempt", "organization_id": organization, "project_id": project, "work_item_id": item, "lease_id": "activity-lease", "fencing_token": 1, "run_id": "activity-run", "sequence": 1, "status": "succeeded", "data_json": "{}", "started_at": started, "updated_at": updated})
					case "history":
						actor, err := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: "example-owner"})
						if err != nil {
							t.Fatal(err)
						}
						fixture.Add("collaboration_events", map[string]any{"id": "activity-history", "organization_id": organization, "project_id": project, "work_item_id": item, "sequence": 3, "type": "run.finished", "schema_version": 1, "actor_json": string(actor), "data_json": "{}", "recorded_at": stamp})
						fixture.Set("issues", map[string]string{"id": "41"}, map[string]any{"event_sequence": 3})
					case "legacy event":
						fixture.Add("work_events", map[string]any{"id": 1, "issue_id": 41, "kind": "activity-event", "payload_json": "{}", "occurred_at": stamp, "recorded_at": stamp})
					case "change version":
						fixture.Set("change_versions", map[string]string{"id": "activity-version"}, map[string]any{"record_json": `{"created_at":"` + stamp + `"}`})
					case "change review", "change check", "change discussion":
						kind := "review"
						payload := `{"created_at":"` + stamp + `"}`
						if name == "change check" {
							kind = "check"
							payload = `{"received_at":"` + stamp + `","completed_at":"2020-01-01T00:00:00Z"}`
						}
						if name == "change discussion" {
							kind = "discussion"
						}
						fixture.Add("change_evidence", map[string]any{"sequence": 1, "change_id": "activity-change", "version_id": "activity-version", "kind": kind, "record_json": payload})
					case "change landing":
						fixture.Set("change_requests", map[string]string{"id": "activity-change"}, map[string]any{"record_json": `{"updated_at":"` + stamp + `"}`})
					case "linked PR", "external PR":
						var issue any = int64(41)
						if name == "external PR" {
							issue = nil
						}
						updated, synchronized := stamp, "2026-10-01T00:00:00Z"
						if name == "external PR" {
							updated, synchronized = synchronized, stamp
						}
						fixture.Add("pull_requests", map[string]any{"id": 1, "repository_id": 1, "issue_id": issue, "github_node_id": "PR_activity", "github_number": 1, "title": "Example PR", "url": "https://github.com/example-org/example-project/pull/1", "github_state": "open", "head_ref": "change/example", "head_sha": strings.Repeat("a", 40), "base_ref": "develop", "source_version": "source-v1", "source_updated_at": updated, "synchronized_at": synchronized, "created_at": "2026-10-01T00:00:00Z", "updated_at": updated})
					}
					objects := newKeyedDetails()
					source := coreimport.CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: organization, ProjectID: project, Objects: objects}
					for range 2 {
						if count, err := coreimport.ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 2 {
							t.Fatalf("activity source import rows=%d error=%v", count, err)
						}
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
						current, err := reader.WorkItem(t.Context(), project, item)
						if err != nil {
							return err
						}
						if !current.LastActivityAt.Equal(at) || !current.NativeUpdatedAt.Equal(nativeUpdated) {
							return fmt.Errorf("backfill activity=%s native_updated=%s want=%s/%s", current.LastActivityAt, current.NativeUpdatedAt, at, nativeUpdated)
						}
						other, err := reader.WorkItem(t.Context(), project, "wi_core_import_second")
						if err != nil {
							return err
						}
						if !other.LastActivityAt.Equal(base) {
							return fmt.Errorf("backfill changed unrelated activity=%s", other.LastActivityAt)
						}
						receipt, err := reader.Receipt(t.Context(), "production-import", "core-work-item-source", "41")
						if err != nil {
							return err
						}
						raw, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
						if err != nil {
							return err
						}
						var decoded struct {
							ActivityRows []historicalActivityReceiptRow
						}
						if err := json.Unmarshal(raw, &decoded); err != nil {
							return err
						}
						found := false
						for _, row := range decoded.ActivityRows {
							for _, timestamp := range row.Timestamps {
								if timestamp == stamp {
									found = true
								}
							}
							for _, cell := range row.Row.Values {
								if !cell.Null && bytes.Contains(cell.Data, []byte(stamp)) {
									found = true
								}
							}
						}
						if !found {
							return errors.New("original activity timestamp bytes are missing")
						}

						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	})
}

func TestHubMigrationPreservesExistingData(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	config := postgres.Config{Connection: hubtest.PostgresConfig(t, admin).Connection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
	catalog, err := storagecatalog.Read(os.DirFS("../../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	objects := newKeyedDetails()
	const rawProgress = `{"policy_approved":true}`
	const updated = "2026-10-01T00:00:00Z"
	for _, organization := range []tracker.OrganizationID{"org_aggregate_a", "org_aggregate_b"} {
		fixture := neutralimporttest.Read(t, coreTenantAggregateSourceCopy(t, organization, false), organization)
		fixture.AddTables(t, os.DirFS("../../../.."), "project_onboarding")
		suffix := strings.TrimPrefix(string(organization), "org_aggregate_")
		project := tracker.ProjectID("prj_aggregate_" + suffix)
		fixture.Add("project_onboarding", map[string]any{"organization_id": organization, "project_id": project, "revision": 7, "progress_json": rawProgress, "updated_at": updated})
		fixture.Add("api_tokens", map[string]any{"id": "legacy-key", "name": "Original selected key", "scope": "operator", "token_hash": apikey.HashToken("legacy-key-" + string(organization)), "token_fingerprint": "legacy-fingerprint", "native_only": 1, "operator_key_scope": "project", "operator_project_access": "selected", "key_kind": "service", "created_at": updated, "updated_at": updated})
		fixture.Add("token_grants", map[string]any{"token_id": "legacy-key", "organization_id": organization, "project_id": project})
		identity := `{"subject":"reviewer","organization_id":"provider_` + string(organization) + `","session_id":"original-session","created_at":"2026-10-01T00:00:00Z","expires_at":"2027-01-01T00:00:00Z"}`
		fixture.Add("hosted_sessions", map[string]any{"token_hash": "existing-session", "email": "reviewer@example.test", "identity_json": identity, "identity_subject": "reviewer", "created_at": updated, "expires_at": "2027-01-01T00:00:00Z"})
		source := hubserver.TenantCoreImportSource{BundlePath: fixture.Write(), OrganizationID: organization, Catalog: catalog, Objects: objects}
		for range 2 {
			result, err := hubserver.ImportTenantCoreCopy(t.Context(), config, source)
			if err != nil || !result.CoreImportComplete || result.WorkItems != 1 || result.ExecutionRows != 3 {
				t.Fatalf("historical tenant import=%+v error=%v", result, err)
			}
		}
	}
	for range 2 {
		store, err := openHubRepository(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			for _, organization := range []tracker.OrganizationID{"org_aggregate_a", "org_aggregate_b"} {
				suffix := strings.TrimPrefix(string(organization), "org_aggregate_")
				project := tracker.ProjectID("prj_aggregate_" + suffix)
				if err := store.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
					credential, err := reader.CredentialByHash(t.Context(), apikey.HashToken("legacy-key-"+string(organization)))
					if err != nil || credential.ID != "legacy-key" || credential.ProjectAccess != "selected" {
						return fmt.Errorf("legacy selected key=%+v error=%v", credential, err)
					}
					grant, err := reader.AccessTokenGrant(t.Context(), credential.ID, project, accessstore.NoRoleCeiling)
					if err != nil || grant.ProjectID != project {
						return fmt.Errorf("legacy key grant=%+v error=%v", grant, err)
					}
					progress, err := reader.ProjectOnboardingProgress(t.Context(), project)
					if err != nil || progress.Revision != 7 || progress.UpdatedAt != updated {
						return fmt.Errorf("historical onboarding progress=%+v error=%v", progress, err)
					}
					receipt, err := reader.Receipt(t.Context(), "production-import", "core-onboarding", string(project))
					if err != nil {
						return err
					}
					raw, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
					if err != nil {
						return err
					}
					var original struct {
						ProgressJSON string
						Revision     int64
						UpdatedAt    string
					}
					if err := json.Unmarshal(raw, &original); err != nil {
						return err
					}
					if original.ProgressJSON != rawProgress || original.Revision != 7 || original.UpdatedAt != updated {
						return fmt.Errorf("onboarding approval source changed: %+v", original)
					}
					if _, err := reader.ProjectPolicy(t.Context(), project); !errors.Is(err, corestore.ErrNotFound) {
						return fmt.Errorf("historical flag became current approval: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var sessions, allowances int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_sessions WHERE organization_id=$1 AND token_hash='existing-session' AND email='reviewer@example.test'`, organization).Scan(&sessions); err != nil || sessions != 1 {
					t.Fatalf("historical hosted session=%d error=%v", sessions, err)
				}
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_plan_assignments WHERE organization_id=$1`, organization).Scan(&allowances); err != nil || allowances != 0 {
					t.Fatalf("migration manufactured allowances=%d error=%v", allowances, err)
				}
			}
		}()
	}
	failedOrg := tracker.OrganizationID("org_aggregate_incomplete")
	failed := neutralimporttest.Read(t, coreTenantAggregateSourceCopy(t, failedOrg, false), failedOrg)
	failed.AddTables(t, os.DirFS("../../../.."), "project_onboarding")
	failed.Add("project_onboarding", map[string]any{"organization_id": failedOrg, "project_id": "prj_aggregate_incomplete", "revision": 7, "progress_json": "{", "updated_at": updated})
	result, err := hubserver.ImportTenantCoreCopy(t.Context(), config, hubserver.TenantCoreImportSource{BundlePath: failed.Write(), OrganizationID: failedOrg, Catalog: catalog, Objects: objects})
	if err == nil || result.CoreImportComplete {
		t.Fatalf("unaccounted onboarding source marked complete: %+v error=%v", result, err)
	}
}

func TestNativeMigrationPreservesCompatibilityIdentity(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	config := postgres.Config{Connection: hubtest.PostgresConfig(t, admin).Connection, Pool: postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}
	open := func() *postgres.Repository {
		store, err := openHubRepository(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		return store
	}
	store := open()
	objects := newKeyedDetails()
	for _, organization := range []tracker.OrganizationID{"org_legacy_first", "org_legacy_second"} {
		project := tracker.ProjectID("prj_legacy")
		coreimporttest.SeedHistoricalTarget(t, store, organization, project, "github_compatible")
		fixture := neutralimporttest.HistoricalV7(t, organization)
		fixture.Add("workflow_states", map[string]any{"id": 2, "repository_id": 1, "source_name": "Done", "detent_state": "Done", "terminal": 1, "created_at": "2026-09-01T12:00:00Z", "updated_at": "2026-09-01T12:00:00Z"})
		fixture.Set("issues", map[string]string{"id": "41"}, map[string]any{"workflow_state_id": 2, "github_state": "open"})
		fixture.Set("issues", map[string]string{"id": "43"}, map[string]any{"workflow_state_id": nil, "github_state": "closed"})

		if _, present := fixture.Tables["issues"].Columns["native_id"]; present {
			t.Fatal("pre-native fixture already contains manufactured native identity")
		}
		path := fixture.Write()
		source := coreimport.CoreWorkItemImportSource{BundlePath: path, OrganizationID: organization, ProjectID: project, Objects: objects}
		if err := neutralimport.Read(t.Context(), path, organization, func(bundle *neutralimport.Bundle) error {
			for _, id := range []int64{41, 43} {
				source.IdentityMappings = append(source.IdentityMappings, coreimport.CoreWorkItemIdentityMapping{SourceSHA256: bundle.Identity().SourceSHA256, OrganizationID: organization, SourceIssueID: id, SourceRepositoryID: 1, SourceRepositoryNodeID: "R_original", SourceGitHubNodeID: "I_original_" + strconv.FormatInt(id, 10), TargetProjectID: project, TargetWorkItemID: tracker.NativeWorkItemID("wi_original_" + strconv.FormatInt(id, 10))})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if count, err := coreimport.ImportCoreWorkItems(t.Context(), store, source); err != nil || count != 2 {
				t.Fatalf("verified pre-native import=%d error=%v", count, err)
			}
		}
		changed := source
		changed.IdentityMappings = append([]coreimport.CoreWorkItemIdentityMapping(nil), source.IdentityMappings...)
		changed.IdentityMappings[0].TargetWorkItemID = "wi_changed"
		if _, err := coreimport.ImportCoreWorkItems(t.Context(), store, changed); !errors.Is(err, mutation.ErrConflict) && !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("changed mapping was accepted: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = open()
		if err := store.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			record, err := reader.ProjectRecord(t.Context(), project)
			if err != nil || record.Core.Profile != "github_compatible" || record.Settings.Repository != "example-org/example-project" {
				return fmt.Errorf("repository/profile identity changed: %+v error=%v", record, err)
			}
			for index, id := range []int64{41, 43} {
				itemID := tracker.NativeWorkItemID("wi_original_" + strconv.FormatInt(id, 10))
				item, err := reader.WorkItem(t.Context(), project, itemID)
				if err != nil {
					return err
				}
				if item.ID != id || item.NativeID != itemID || item.Number != int64(index+1) || item.ProvenanceProvider == nil || *item.ProvenanceProvider != "github" || item.ProvenanceExternalID == nil || *item.ProvenanceExternalID != "I_original_"+strconv.FormatInt(id, 10) {
					return fmt.Errorf("original GitHub/numeric identity changed: %+v", item)
				}
				if item.BodyObjectKey == nil {
					return errors.New("imported current body reference is missing")
				}
				objects.mu.Lock()
				encodedBody := append([]byte(nil), objects.data[*item.BodyObjectKey]...)
				objects.mu.Unlock()
				var body string
				if err := json.Unmarshal(encodedBody, &body); err != nil {
					return err
				}
				if body != "Original body\r\n"+strconv.FormatInt(id, 10) {
					return fmt.Errorf("imported current body changed: %q", body)
				}
				version, err := reader.CollaborationVersion(t.Context(), string(itemID), 1)
				if err != nil {
					return err
				}
				originalVersion, err := eventstore.NewContent(objects).CollaborationVersion(t.Context(), version)
				if err != nil {
					return err
				}
				var snapshot tracker.NativeIssue
				if err := json.Unmarshal(originalVersion, &snapshot); err != nil {
					return err
				}
				if snapshot.WorkItemID != itemID || snapshot.Body != body || snapshot.Revision != 1 || snapshot.Provenance == nil || snapshot.Provenance.ExternalID != "I_original_"+strconv.FormatInt(id, 10) {
					return fmt.Errorf("historical native version changed: %+v", snapshot)
				}

				expectedState, expectedTerminal := "Done", true
				if id == 43 {
					expectedState, expectedTerminal = "", false
				}
				storedState := ""
				if item.WorkflowStateName != nil {
					storedState = *item.WorkflowStateName
				}
				if (item.WorkflowStateName == nil) != (expectedState == "") || storedState != expectedState || snapshot.State != expectedState || snapshot.Terminal != expectedTerminal {
					return fmt.Errorf("historical workflow authority changed: item=%+v snapshot=%+v", item, snapshot)
				}
				lease, err := reader.Lease(t.Context(), "legacy-lease-"+strconv.FormatInt(id, 10))
				if err != nil || lease.IssueID != id || lease.FencingToken != int64(index+1) {
					return fmt.Errorf("historical lease fence changed: %+v error=%v", lease, err)
				}
				receipt, err := reader.Receipt(t.Context(), "production-import", "core-work-item-source", strconv.FormatInt(id, 10))
				if err != nil {
					return err
				}
				raw, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
				if err != nil {
					return err
				}
				var original struct {
					Mapping coreimport.CoreWorkItemIdentityMapping
					Rows    []historicalActivityReceiptRow
				}
				if err := json.Unmarshal(raw, &original); err != nil {
					return err
				}
				if original.Mapping != source.IdentityMappings[index] {
					return fmt.Errorf("mapping changed across reopen: %+v", original.Mapping)
				}
			}
			if _, err := reader.WorkItem(t.Context(), project, "wi_changed"); !errors.Is(err, corestore.ErrNotFound) {
				return fmt.Errorf("rejected mapping created identity: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,id,native_id,project_id,number,title) VALUES($1,9999,'wi_invalid','prj_missing',1,'Invalid reference')`, organization); err == nil {
			t.Fatal("PostgreSQL foreign key enforcement was lost")
		} else {
			var state interface{ SQLState() string }
			if !errors.As(err, &state) || state.SQLState() != "23503" {
				t.Fatalf("foreign key refusal=%v", err)
			}
		}
		var issue, fence, compressed, contentBytes int64
		var key, digest string
		if err := admin.QueryRowContext(t.Context(), `SELECT issue_id,fencing_token,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM core_work_events WHERE organization_id=$1 AND source_id=7`, organization).Scan(&issue, &fence, &key, &digest, &compressed, &contentBytes); err != nil {
			t.Fatal(err)
		}
		raw, err := objects.ReadDetail(t.Context(), string(organization), eventstore.Reference(key, digest, compressed, contentBytes))
		if err != nil || issue != 41 || fence != 1 || string(raw) != `{"reference":"preserved"}` {
			t.Fatalf("historical work event changed: issue=%d fence=%d body=%q error=%v", issue, fence, raw, err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET native_id='changed' WHERE organization_id=$1 AND project_id=$2 AND native_id='wi_original_41'`, organization, project); err == nil {
			t.Fatal("native identity was mutable")
		}
		metadata, err := objects.StoreDetail(t.Context(), string(organization), []byte(`{"status":"historical"}`))
		if err != nil {
			t.Fatal(err)
		}
		for _, control := range []struct {
			name     string
			metadata bool
		}{{name: "metadata only", metadata: true}, {name: "unchanged title/body replay"}} {
			if err := store.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
				current, err := writer.WorkItem(t.Context(), project, "wi_original_41")
				if err != nil {
					return err
				}
				before := current.LastActivityAt
				if control.metadata {
					current.ReportContextObjectKey = &metadata.Key
				}
				after, err := writer.UpdateWorkItem(t.Context(), current)
				if err != nil {
					return err
				}
				if !after.LastActivityAt.Equal(before) {
					return fmt.Errorf("%s advanced activity: %s -> %s", control.name, before, after.LastActivityAt)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		var databaseNow time.Time
		if err := admin.QueryRowContext(t.Context(), "SELECT clock_timestamp()").Scan(&databaseNow); err != nil {
			t.Fatal(err)
		}
		injected := databaseNow.Add(-time.Hour).UTC().Truncate(time.Microsecond)
		for _, control := range []struct {
			name        string
			clock, want time.Time
		}{{name: "injected clock precedes database", clock: injected, want: injected}, {name: "older edit retains high water", clock: injected.Add(-time.Minute), want: injected}} {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			config.LeaseClock = func() time.Time { return control.clock }
			store = open()
			if err := store.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
				current, err := writer.WorkItem(t.Context(), project, "wi_original_41")
				if err != nil {
					return err
				}
				current.Title = control.name
				after, err := writer.UpdateWorkItem(t.Context(), current)
				if err != nil {
					return err
				}
				if !after.LastActivityAt.Equal(control.want) {
					return fmt.Errorf("%s activity=%s want=%s", control.name, after.LastActivityAt, control.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		config.LeaseClock = nil
		store = open()
		editedBody, err := json.Marshal("Imported edit")
		if err != nil {
			t.Fatal(err)
		}
		ref, err := objects.StoreDetail(t.Context(), string(organization), editedBody)
		if err != nil {
			t.Fatal(err)
		}
		var edited corestore.WorkItem
		if err := store.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			current, err := writer.WorkItem(t.Context(), project, "wi_original_41")
			if err != nil {
				return err
			}
			current.BodyObjectKey = &ref.Key
			edited, err = writer.UpdateWorkItem(t.Context(), current)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		sourceStamp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		if !edited.LastActivityAt.Equal(edited.NativeUpdatedAt) || !edited.LastActivityAt.After(sourceStamp) {
			t.Fatalf("import edit activity=%s native updated=%s", edited.LastActivityAt, edited.NativeUpdatedAt)
		}
		if err := store.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			receipt, err := reader.Receipt(t.Context(), "production-import", "core-work-item-source", "41")
			if err != nil {
				return err
			}
			raw, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
			if err != nil {
				return err
			}
			var original struct {
				SourceRows []historicalSourceReceiptReference
			}
			if err := json.Unmarshal(raw, &original); err != nil {
				return err
			}
			for _, source := range original.SourceRows {
				if source.Table != "issues" {
					continue
				}
				rowReceipt, err := reader.Receipt(t.Context(), "production-import", "core-work-item-source-row", source.CommandKey)
				if err != nil {
					return err
				}
				rawRow, err := eventstore.NewContent(objects).Receipt(t.Context(), rowReceipt)
				if err != nil {
					return err
				}
				var row neutralimport.Row
				if err := json.Unmarshal(rawRow, &row); err != nil {
					return err
				}
				var updated, body string
				if err := row.Decode(map[string]any{"updated_at": &updated, "body": &body}); err != nil {
					return err
				}
				if updated != "2026-09-01T12:00:00Z" || body != "Original body\r\n41" {
					return fmt.Errorf("native edit rewrote source clock/body: %s/%q", updated, body)
				}
				return nil
			}

			return errors.New("original issue bytes are missing")
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store = open()
		if err := store.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			current, err := reader.WorkItem(t.Context(), project, "wi_original_41")
			if err != nil {
				return err
			}
			if current.NativeID != "wi_original_41" {
				return errors.New("restart changed native identity")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			if _, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_native", Name: "Coexisting native", Profile: "native"}, States: hubtest.States()}); err != nil {
				return err
			}
			_, err := writer.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_native", NativeID: "wi_native", Number: 1, Title: "Native work"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

type historicalActivityReceiptRow struct {
	Table        string
	Row          neutralimport.Row
	RecordSHA256 string
	Timestamps   []string
}

type historicalSourceReceiptReference struct {
	Table      string
	CommandKey string
}
