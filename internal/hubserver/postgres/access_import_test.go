package postgres_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var accessFixtureTables = []string{
	"organizations", "hosted_tenant", "api_tokens", "hosted_members", "hosted_release_preferences", "daily_summary_preferences", "daily_summary_receipts", "hosted_project_grants", "token_grants",
	"hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations",
	"hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets",
	"project_secret_audit", "hosted_audit", "hosted_audit_times",
}

func accessImportSourceFor(t *testing.T, db *sql.DB, schema string, organization tracker.OrganizationID, malformedInvitation bool) (postgres.AccessImportSource, *historyObjects) {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	source := postgres.AccessImportSource{DB: db, Catalog: catalog, Source: storagecatalog.Source{Schema: schema, Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}, Schema: storagecatalog.Schema{}, SnapshotID: "example-frozen-access-snapshot"}
	for name, table := range catalog.Tables {
		name, ok := strings.CutPrefix(name, "hub/")
		if !ok {
			continue
		}
		source.Schema[name] = make(map[string]string, len(table.Columns))
		for column, definition := range table.Columns {
			source.Schema[name][column] = definition.Source
		}
	}
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+pq.QuoteIdentifier(schema)); err != nil {
		t.Fatal(err)
	}
	keys := make(map[string][]string, len(accessFixtureTables))
	for _, name := range accessFixtureTables {
		definition := catalog.Tables["hub/"+name]
		columns := make([]string, 0, len(definition.Columns))
		for column, sourceDefinition := range definition.Columns {
			var metadata []json.RawMessage
			if err := json.Unmarshal([]byte(sourceDefinition.Source), &metadata); err != nil || len(metadata) < 3 {
				t.Fatalf("decode source column metadata for %s.%s: %v", name, column, err)
			}
			var sourceType string
			if err := json.Unmarshal(metadata[2], &sourceType); err != nil {
				t.Fatal(err)
			}
			typeName := `text COLLATE "C"`
			if sourceType == "BLOB" {
				typeName = "bytea"
			}
			columns = append(columns, pq.QuoteIdentifier(column)+" "+typeName)
			var row []json.RawMessage
			if err := json.Unmarshal([]byte(sourceDefinition.Source), &row); err != nil || len(row) < 6 {
				t.Fatalf("decode source key metadata for %s.%s: %v", name, column, err)
			}
			var position int
			if err := json.Unmarshal(row[5], &position); err != nil {
				t.Fatal(err)
			}
			if position > 0 {
				for len(keys[name]) < position {
					keys[name] = append(keys[name], "")
				}
				keys[name][position-1] = column
			}
		}
		slices.Sort(columns)
		for n, key := range keys[name] {
			if key == "" {
				t.Fatalf("missing primary key position %d for %s", n+1, name)
			}
		}
		quotedKeys := make([]string, len(keys[name]))
		for n, key := range keys[name] {
			quotedKeys[n] = pq.QuoteIdentifier(key)
		}
		statement := "CREATE TABLE " + pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name) + " (" + strings.Join(columns, ",") + ", PRIMARY KEY (" + strings.Join(quotedKeys, ",") + "))"
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}

	identity := `{"subject":"user_first","organization_id":"provider_first","session_id":"session_first","created_at":"2026-10-31T12:00:00Z","expires_at":"2026-11-02T12:00:00Z"}`
	grants := `[{"project_id":"prj_first","write":true,"runner":false}]`
	if malformedInvitation {
		grants = `[{"project_id":]`
	}
	stamp := "2026-10-31T12:00:00Z"
	rows := map[string]map[string]any{
		"organizations":              {"id": string(organization), "name": "Example organization", "slug": "example", "local": "0", "created_at": stamp},
		"hosted_tenant":              {"organization_id": string(organization), "singleton": "1", "provider_id": "provider_first", "bootstrap_subject": "bootstrap_first", "deployment": "example-deployment", "public_url": "https://example.test", "allocation_generation": "1"},
		"api_tokens":                 {"id": "token_first", "name": "Example credential", "token_hash": "hash_first", "token_fingerprint": "fingerprint_first", "scope": "operator", "native_only": "0", "created_at": stamp, "updated_at": stamp, "hosted_user_id": "user_first", "hosted_organization_id": string(organization), "hosted_membership_id": "membership_first", "operator_key_scope": "project", "operator_project_access": "selected", "key_kind": "personal"},
		"hosted_members":             {"user_id": "user_first", "email": "user@example.test", "membership_id": "membership_first", "role": "member", "active": "1", "principal_id": "token_first", "notify_issues": "1", "created_at": stamp, "updated_at": stamp},
		"hosted_release_preferences": {"organization_id": string(organization), "user_id": "user_first", "version": "v1.2.3", "build_time": stamp},
		"daily_summary_preferences":  {"organization_id": string(organization), "user_id": "user_first", "enabled": "1", "send_minute": "390", "time_zone": "America/Chicago", "weekdays": "1", "token": "unsub_" + string(organization), "window_end": "1790000000", "due_at": "1790000300"},
		"daily_summary_receipts":     {"organization_id": string(organization), "user_id": "user_first", "window_end": "1790000000", "status": "sent", "attempts": "2", "recorded_at": "1790000001"},
		"hosted_project_grants":      {"organization_id": string(organization), "user_id": "user_first", "project_id": "prj_first", "can_write": "1", "manage_runner": "0"},
		"token_grants":               {"organization_id": string(organization), "token_id": "token_first", "project_id": "prj_first"},
		"hosted_sessions":            {"token_hash": "session_hash_first", "email": "user@example.test", "identity_json": identity, "identity_subject": "user_first", "created_at": stamp, "expires_at": "2026-11-02T12:00:00Z"},
		"hosted_transactions":        {"token_hash": "transaction_hash_first", "state": "state_first", "verifier": "verifier_first", "organization_id": "provider_first", "support_actor": "", "support_session": "", "invitation_token": "", "expires_at": "2026-11-01T12:00:00Z"},
		"hosted_invitations":         {"id": "invitation_first", "organization_id": string(organization), "email": "invite@example.test", "role": "viewer", "accepted_user_id": "", "created_at": stamp, "expires_at": "2026-11-07T12:00:00Z", "grants_json": grants},
		"hosted_member_reservations": {"email": "reserve@example.test", "expires_at": "1790000000"},
		"hosted_binding_migrations":  {"organization_id": string(organization), "id": "1", "from_public_url": "https://old.example.test", "to_public_url": "https://new.example.test", "from_deployment": "old", "from_generation": "1", "to_generation": "2", "migrated_at": stamp, "applied": "1"},
		"organization_secrets":       {"organization_id": string(organization), "kind": "github", "organization_slug": "example", "ciphertext": []byte{0, 1, 2, 255}, "nonce": []byte{4, 5, 6}, "wrapped_data_key": []byte{7, 8, 9}, "master_key_version": "2", "updated_at": stamp},
		"organization_secret_audit":  {"organization_id": string(organization), "id": "1", "actor": "user_first", "kind": "github", "key_version": "2", "event": "updated", "recorded_at": stamp},
		"project_secrets":            {"organization_id": string(organization), "project_id": "prj_first", "kind": "github", "organization_slug": "example", "ciphertext": []byte{10, 11, 12}, "nonce": []byte{13, 14}, "wrapped_data_key": []byte{15, 16}, "master_key_version": "2", "updated_at": stamp},
		"project_secret_audit":       {"organization_id": string(organization), "id": "1", "project_id": "prj_first", "actor": "user_first", "kind": "github", "key_version": "2", "event": "updated", "recorded_at": stamp},
		"hosted_audit":               {"organization_id": string(organization), "id": "1", "session_id": "token_first", "actual_actor": "user_first", "effective_user": "user_first", "reason": "", "event": "credential_created", "route": "mcp credential_create", "project_id": "prj_first", "status": "200", "started_at": stamp, "expires_at": "2026-11-01T12:00:00Z", "recorded_at": stamp, "recorded_at_ns": "1790000000000000000", "mutation_json": `{"action":"example","changed":true}`, "account_kind": "", "account_retry": ""},
		"hosted_audit_times":         {"audit_id": "1", "recorded_at_ns": "1790000000000000000"},
	}
	for _, name := range accessFixtureTables {
		insertAccessSourceRow(t, db, schema, name, source.Schema[name], rows[name])
	}
	objects := newHistoryObjects()
	source.Objects = objects
	return source, objects
}

func insertAccessSourceRow(t *testing.T, db *sql.DB, schema, table string, schemaColumns map[string]string, values map[string]any) {
	t.Helper()
	columns := make([]string, 0, len(schemaColumns))
	for name := range schemaColumns {
		columns = append(columns, name)
	}
	slices.Sort(columns)
	quoted, placeholders, args := make([]string, len(columns)), make([]string, len(columns)), make([]any, len(columns))
	for n, name := range columns {
		quoted[n], placeholders[n], args[n] = pq.QuoteIdentifier(name), fmt.Sprintf("$%d", n+1), values[name]
	}
	statement := "INSERT INTO " + pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table) + " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
	if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("insert raw source row for %s: %v", table, err)
	}
}

func seedAccessTarget(t *testing.T, db *sql.DB, repository *postgres.Repository, organization tracker.OrganizationID) {
	t.Helper()
	createdAt := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,slug,local,created_at) VALUES($1,'Example organization','example',false,$2)`, organization, createdAt); err != nil {
		t.Fatal(err)
	}
	seedRepository(t, repository, organization)
}

func finishAccessImport(t *testing.T, r *postgres.Repository, source postgres.AccessImportSource) postgres.AccessImportProgress {
	t.Helper()
	previous := int64(0)
	for {
		importer, err := r.NewAccessImporter(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		progress, err := importer.Step(t.Context(), 2)
		if err != nil {
			t.Fatal(err)
		}
		if progress.Processed-previous > 2 {
			t.Fatalf("import page exceeded bound: processed=%d previous=%d", progress.Processed, previous)
		}
		previous = progress.Processed
		if progress.Done {
			return progress
		}
	}
}

func TestAccessImportRetainsTenantAccessAndSecretParity(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	sources := make([]postgres.AccessImportSource, len(organizations))
	objects := make([]*historyObjects, len(organizations))
	for n, organization := range organizations {
		sources[n], objects[n] = accessImportSourceFor(t, db, fmt.Sprintf("raw_access_%d", n), organization, false)
		if n == 0 {
			if _, err := db.ExecContext(t.Context(), `UPDATE "raw_access_0".hosted_invitations SET expires_at='' WHERE id='invitation_first'`); err != nil {
				t.Fatal(err)
			}
		}
	}
	for n, source := range sources {
		organization := organizations[n]
		copySource := postgres.AccessCopyImportSource{CopyPath: accessSQLiteCopy(t, source), OrganizationID: organization, Catalog: source.Catalog, Objects: source.Objects}
		if err := r.InitializeAccessCopyOrganization(t.Context(), copySource); err != nil {
			t.Fatalf("initialize tenant organization: %v", err)
		}
		seedRepository(t, r, organization)
		processed, err := r.ImportAccessCopy(t.Context(), copySource)
		if err != nil || processed != int64(len(accessFixtureTables)) {
			t.Fatalf("tenant %s import = %d, %v", organizations[n], processed, err)
		}
		if err := r.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			_, err := reader.CollaborationBytes(t.Context())
			if !errors.Is(err, corestore.ErrNotFound) {
				return errors.Join(err, errors.New("import initialized unaccounted history to zero"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var invitationExpiry time.Time
		if err := db.QueryRowContext(t.Context(), `SELECT expires_at FROM hosted_invitations WHERE organization_id=$1 AND id='invitation_first'`, organization).Scan(&invitationExpiry); err != nil {
			t.Fatal(err)
		}
		wantExpiry := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		if n == 0 {
			wantExpiry = time.Date(9999, time.December, 31, 23, 59, 59, 999999000, time.UTC)
		}
		if !invitationExpiry.Equal(wantExpiry) {
			t.Fatalf("tenant %s invitation expiry=%v, want %v", organization, invitationExpiry, wantExpiry)
		}
		acceptAt := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		acceptErr := r.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.AcceptAccessInvitation(t.Context(), "invitation_first", "invite@example.test", "user_accept", acceptAt)
		})
		if n == 0 && acceptErr != nil || n == 1 && !errors.Is(acceptErr, corestore.ErrNotFound) {
			t.Fatalf("tenant %s invitation acceptance after original finite-expiry horizon: %v", organization, acceptErr)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',$2)`, organization, 101+n); err != nil {
			t.Fatal(err)
		}
		replay, err := r.ImportAccessCopy(t.Context(), copySource)
		if err != nil || replay != processed {
			t.Fatalf("tenant %s replay = %+v, %v", organizations[n], replay, err)
		}
		var retainedBytes int64
		if err := db.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes'`, organization).Scan(&retainedBytes); err != nil {
			t.Fatal(err)
		}
		if retainedBytes != int64(101+n) {
			t.Fatalf("import reset retained collaboration bytes to %d", retainedBytes)
		}
		var importedName, importedSlug string
		var importedLocal bool
		if err := db.QueryRowContext(t.Context(), `SELECT name,slug,local FROM organizations WHERE organization_id=$1`, organization).Scan(&importedName, &importedSlug, &importedLocal); err != nil || importedName != "Example organization" || importedSlug != "example" || importedLocal {
			t.Fatalf("tenant organization import name=%q slug=%q local=%v error=%v", importedName, importedSlug, importedLocal, err)
		}
		var tokenHash, scope, hostedUser, hostedMembership, keyKind string
		var nativeOnly bool
		if err := db.QueryRowContext(t.Context(), `SELECT token_hash,scope,native_only,hosted_user_id,hosted_membership_id,key_kind FROM api_tokens WHERE organization_id=$1 AND id='token_first'`, organization).Scan(&tokenHash, &scope, &nativeOnly, &hostedUser, &hostedMembership, &keyKind); err != nil {
			t.Fatal(err)
		}
		if tokenHash != "hash_first" || scope != "operator" || nativeOnly || hostedUser != "user_first" || hostedMembership != "membership_first" || keyKind != "personal" {
			t.Fatalf("tenant %s credential metadata changed: %s %s %t %s %s %s", organization, tokenHash, scope, nativeOnly, hostedUser, hostedMembership, keyKind)
		}
		var role, principal string
		var active, notify bool
		if err := db.QueryRowContext(t.Context(), `SELECT role,principal_id,active,notify_issues FROM hosted_members WHERE organization_id=$1 AND user_id='user_first'`, organization).Scan(&role, &principal, &active, &notify); err != nil || role != "member" || principal != "token_first" || !active || !notify {
			t.Fatalf("tenant %s membership parity: role=%s principal=%s active=%t notify=%t err=%v", organization, role, principal, active, notify, err)
		}
		var releaseVersion, timeZone, unsubscribeToken, receiptStatus string
		var buildTime time.Time
		if err := db.QueryRowContext(t.Context(), `SELECT version,build_time FROM hosted_release_preferences WHERE organization_id=$1 AND user_id='user_first'`, organization).Scan(&releaseVersion, &buildTime); err != nil || releaseVersion != "v1.2.3" || !buildTime.Equal(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("tenant %s release preference version=%q build_time=%v err=%v", organization, releaseVersion, buildTime, err)
		}
		var dailyEnabled, weekdays bool
		var sendMinute int
		var windowEnd, dueAt time.Time
		if err := db.QueryRowContext(t.Context(), `SELECT enabled,send_minute,time_zone,weekdays,token,window_end,due_at FROM daily_summary_preferences WHERE organization_id=$1 AND user_id='user_first'`, organization).Scan(&dailyEnabled, &sendMinute, &timeZone, &weekdays, &unsubscribeToken, &windowEnd, &dueAt); err != nil || !dailyEnabled || sendMinute != 390 || timeZone != "America/Chicago" || !weekdays || unsubscribeToken != "unsub_"+string(organization) || windowEnd.Unix() != 1790000000 || dueAt.Unix() != 1790000300 {
			t.Fatalf("tenant %s daily preference enabled=%t minute=%d zone=%q weekdays=%t token=%q window=%v due=%v err=%v", organization, dailyEnabled, sendMinute, timeZone, weekdays, unsubscribeToken, windowEnd, dueAt, err)
		}
		var attempts int
		var recordedAt time.Time
		if err := db.QueryRowContext(t.Context(), `SELECT status,attempts,recorded_at FROM daily_summary_receipts WHERE organization_id=$1 AND user_id='user_first' AND window_end=$2`, organization, time.Unix(1790000000, 0).UTC()).Scan(&receiptStatus, &attempts, &recordedAt); err != nil || receiptStatus != "sent" || attempts != 2 || recordedAt.Unix() != 1790000001 {
			t.Fatalf("tenant %s daily receipt status=%q attempts=%d recorded_at=%v err=%v", organization, receiptStatus, attempts, recordedAt, err)
		}
		var sessionSubject, providerOrganization, sessionID, sessionEmail string
		var identityCreated, identityExpires time.Time
		if err := db.QueryRowContext(t.Context(), `SELECT identity_subject,provider_organization_id,session_id,email,identity_created_at,identity_expires_at FROM hosted_sessions WHERE organization_id=$1 AND token_hash='session_hash_first'`, organization).Scan(&sessionSubject, &providerOrganization, &sessionID, &sessionEmail, &identityCreated, &identityExpires); err != nil {
			t.Fatal(err)
		}
		if sessionSubject != "user_first" || providerOrganization != "provider_first" || sessionID != "session_first" || sessionEmail != "user@example.test" || !identityCreated.Equal(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)) || !identityExpires.Equal(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("tenant %s session identity changed: %s %s %s %s %v %v", organization, sessionSubject, providerOrganization, sessionID, sessionEmail, identityCreated, identityExpires)
		}
		var reservedUntil time.Time
		if err := db.QueryRowContext(t.Context(), `SELECT expires_at FROM hosted_member_reservations WHERE organization_id=$1 AND email='reserve@example.test'`, organization).Scan(&reservedUntil); err != nil || !reservedUntil.Equal(time.Unix(1790000000, 0).UTC()) {
			t.Fatalf("tenant %s reservation expiry=%v, err=%v", organization, reservedUntil, err)
		}
		var ciphertext, nonce, wrapped []byte
		if err := db.QueryRowContext(t.Context(), `SELECT ciphertext,nonce,wrapped_data_key FROM organization_secrets WHERE organization_id=$1 AND kind='github'`, organization).Scan(&ciphertext, &nonce, &wrapped); err != nil {
			t.Fatal(err)
		}
		if string(ciphertext) != string([]byte{0, 1, 2, 255}) || string(nonce) != string([]byte{4, 5, 6}) || string(wrapped) != string([]byte{7, 8, 9}) {
			t.Fatalf("tenant %s organization secret envelope changed", organization)
		}
		var projectCiphertext []byte
		if err := db.QueryRowContext(t.Context(), `SELECT ciphertext FROM project_secrets WHERE organization_id=$1 AND project_id='prj_first' AND kind='github'`, organization).Scan(&projectCiphertext); err != nil || string(projectCiphertext) != string([]byte{10, 11, 12}) {
			t.Fatalf("tenant %s project secret envelope = %v, %v", organization, projectCiphertext, err)
		}
		var mutationKey, digest string
		var compressed, content int64
		var version int
		if err := db.QueryRowContext(t.Context(), `SELECT mutation_object_key,mutation_sha256,mutation_bytes,mutation_content_bytes,mutation_format_version FROM hosted_audit WHERE organization_id=$1 AND id=1`, organization).Scan(&mutationKey, &digest, &compressed, &content, &version); err != nil {
			t.Fatal(err)
		}
		ref := attachment.ArchiveReference{Key: mutationKey, SHA256: digest, CompressedSize: compressed, UncompressedSize: content, Codec: "gzip", SchemaVersion: version, Kind: "archive", Retention: attachment.ArchiveRetention}
		payload, err := objects[n].ReadDetail(t.Context(), string(organization), ref)
		if err != nil || string(payload) != `{"action":"example","changed":true}` {
			t.Fatalf("tenant %s audit payload = %s, %v", organization, payload, err)
		}
		var grants int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_invitation_grants WHERE organization_id=$1`, organization).Scan(&grants); err != nil || grants != 1 {
			t.Fatalf("tenant %s invitation grants=%d, err=%v", organization, grants, err)
		}
		var canWrite, manageRunner bool
		if err := db.QueryRowContext(t.Context(), `SELECT can_write,manage_runner FROM hosted_invitation_grants WHERE organization_id=$1 AND invitation_id='invitation_first' AND project_id='prj_first'`, organization).Scan(&canWrite, &manageRunner); err != nil || !canWrite || manageRunner {
			t.Fatalf("tenant %s invitation grant parity: write=%t runner=%t err=%v", organization, canWrite, manageRunner, err)
		}
	}
	for _, table := range []string{"api_tokens", "hosted_members", "hosted_project_grants", "token_grants", "hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations", "hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets", "project_secret_audit", "hosted_audit", "hosted_audit_times"} {
		var count int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+pq.QuoteIdentifier(table)).Scan(&count); err != nil || count != 2 {
			t.Errorf("shared %s rows=%d, err=%v", table, count, err)
		}
	}
}

func TestAccessImportMissingProjectReferenceDoesNotAdvanceCheckpoint(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	organization := tracker.OrganizationID("org_missing_project")
	seedAccessTarget(t, db, r, organization)
	source, _ := accessImportSourceFor(t, db, "raw_access_missing_project", organization, false)
	if _, err := db.ExecContext(t.Context(), `UPDATE "raw_access_missing_project".token_grants SET project_id='prj_missing'`); err != nil {
		t.Fatal(err)
	}
	importer, err := r.NewAccessImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := importer.Step(t.Context(), postgres.AccessImportLimit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := importer.Step(t.Context(), postgres.AccessImportLimit); !errors.Is(err, postgres.ErrAccessImport) {
		t.Fatalf("missing project reference error = %v", err)
	}
	var stage, processed int64
	if err := db.QueryRowContext(t.Context(), `SELECT stage,processed FROM access_imports WHERE organization_id=$1`, organization).Scan(&stage, &processed); err != nil {
		t.Fatal(err)
	}
	if stage != 5 || processed != 5 {
		t.Fatalf("missing project page advanced checkpoint: stage=%d processed=%d", stage, processed)
	}
	var grants int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM token_grants WHERE organization_id=$1`, organization).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("missing project token grants=%d, err=%v", grants, err)
	}
}

func TestAccessImportRejectsCredentialBoundToAnotherOrganization(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	organization := tracker.OrganizationID("org_access_binding")
	seedAccessTarget(t, db, r, organization)
	source, _ := accessImportSourceFor(t, db, "raw_access_binding", organization, false)
	if _, err := db.ExecContext(t.Context(), `UPDATE raw_access_binding.api_tokens SET hosted_organization_id='org_other'`); err != nil {
		t.Fatal(err)
	}
	importer, err := r.NewAccessImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := importer.Step(t.Context(), postgres.AccessImportLimit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := importer.Step(t.Context(), postgres.AccessImportLimit); !errors.Is(err, postgres.ErrAccessImport) {
		t.Fatalf("cross-organization credential import error = %v", err)
	}
	var stage, processed int64
	if err := db.QueryRowContext(t.Context(), `SELECT stage,processed FROM access_imports WHERE organization_id=$1`, organization).Scan(&stage, &processed); err != nil {
		t.Fatal(err)
	}
	if stage != 2 || processed != 2 {
		t.Fatalf("cross-organization credential advanced checkpoint: stage=%d processed=%d", stage, processed)
	}
	var credentials int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM api_tokens WHERE organization_id=$1`, organization).Scan(&credentials); err != nil || credentials != 0 {
		t.Fatalf("cross-organization credentials imported=%d, err=%v", credentials, err)
	}
}

func TestAccessImportMalformedInvitationDoesNotAdvanceCheckpoint(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	organization := tracker.OrganizationID("org_malformed")
	seedAccessTarget(t, db, r, organization)
	source, _ := accessImportSourceFor(t, db, "raw_access_malformed", organization, true)
	importer, err := r.NewAccessImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := importer.Step(t.Context(), postgres.AccessImportLimit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := importer.Step(t.Context(), postgres.AccessImportLimit); !errors.Is(err, postgres.ErrAccessImport) {
		t.Fatalf("malformed invitation error = %v", err)
	}
	var stage, processed int64
	if err := db.QueryRowContext(t.Context(), `SELECT stage,processed FROM access_imports WHERE organization_id=$1`, organization).Scan(&stage, &processed); err != nil {
		t.Fatal(err)
	}
	if stage != 8 || processed != 8 {
		t.Fatalf("failed invitation page advanced checkpoint: stage=%d processed=%d", stage, processed)
	}
	var invitations int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_invitations WHERE organization_id=$1`, organization).Scan(&invitations); err != nil || invitations != 0 {
		t.Fatalf("malformed invitation rows=%d, err=%v", invitations, err)
	}
}

func TestAccessImportContinuesAfterEmptyTextKey(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	organization := tracker.OrganizationID("org_empty_key")
	seedAccessTarget(t, db, r, organization)
	source, _ := accessImportSourceFor(t, db, "raw_access_empty_key", organization, false)
	insertAccessSourceRow(t, db, "raw_access_empty_key", "hosted_invitations", source.Schema["hosted_invitations"], map[string]any{
		"id": "", "organization_id": string(organization), "email": "empty-id@example.test", "role": "viewer", "accepted_user_id": "",
		"created_at": "2026-10-31T12:00:00Z", "expires_at": "2026-11-07T12:00:00Z", "grants_json": "[]",
	})
	importer, err := r.NewAccessImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	var progress postgres.AccessImportProgress
	for range 100 {
		progress, err = importer.Step(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if progress.Done {
			break
		}
	}
	if !progress.Done || progress.Processed != int64(len(accessFixtureTables)+1) {
		t.Fatalf("empty text key import = %+v", progress)
	}
	var invitations int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_invitations WHERE organization_id=$1`, organization).Scan(&invitations); err != nil || invitations != 2 {
		t.Fatalf("empty text key invitation count=%d, err=%v", invitations, err)
	}
}

func TestAccessImportRejectsSourceColumnTypeDrift(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	organization := tracker.OrganizationID("org_source_type")
	seedAccessTarget(t, db, r, organization)
	source, _ := accessImportSourceFor(t, db, "raw_access_source_type", organization, false)
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE raw_access_source_type.hosted_tenant ALTER COLUMN deployment TYPE bytea USING convert_to(deployment,'UTF8')`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.NewAccessImporter(t.Context(), source); !errors.Is(err, storagecatalog.ErrUnmapped) {
		t.Fatalf("source column type drift error = %v", err)
	}
}

func TestAccessImportPreservesInvitationBeyondServingPage(t *testing.T) {
	db := template.Database(t)
	r, _ := repositoryFor(t, db, smallPool())
	organization := tracker.OrganizationID("org_many_grants")
	seedAccessTarget(t, db, r, organization)
	source, _ := accessImportSourceFor(t, db, "raw_access_many_grants", organization, false)
	grants := make([]map[string]any, 0, corestore.MaxPageSize+1)
	if err := r.Write(t.Context(), organization, corestore.Serving, func(w corestore.Writer) error {
		for index := range corestore.MaxPageSize + 1 {
			id := tracker.ProjectID(fmt.Sprintf("prj_grant_%03d", index))
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Slug: string(id), Profile: "native"}); err != nil {
				return err
			}
			grants = append(grants, map[string]any{"project_id": string(id), "write": index%2 == 0, "runner": false})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(grants)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE raw_access_many_grants.hosted_invitations SET grants_json=$1 WHERE id='invitation_first'`, string(raw)); err != nil {
		t.Fatal(err)
	}
	finishAccessImport(t, r, source)
	var total, writable int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER (WHERE can_write) FROM hosted_invitation_grants WHERE organization_id=$1 AND invitation_id='invitation_first'`, organization).Scan(&total, &writable); err != nil || total != 201 || writable != 101 {
		t.Fatalf("imported grant counts=%d,%d want 201,101: %v", total, writable, err)
	}
}
