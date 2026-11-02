package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrAccessImport = errors.New("invalid tenant access import")

const AccessImportLimit = 100

type AccessImportSource struct {
	DB         *sql.DB
	copyTx     *sql.Tx
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	SnapshotID string
	Objects    attachment.DetailStore
}

type AccessImportProgress struct {
	Processed int64
	Done      bool
}

type AccessImporter struct {
	repository   *Repository
	source       AccessImportSource
	binding      *storagecatalog.Binding
	digest       string
	organization tracker.OrganizationID
	tables       []accessImportTable
}

type accessImportTable struct {
	name string
	keys []string
}

type accessImportCheckpoint struct {
	sourceSchema  string
	snapshotID    string
	catalogDigest string
	stage         int64
	processed     int64
	revision      int64
	after         [3]string
	hasAfter      bool
	done          bool
}

type accessImportRow struct {
	keyValues [3]string
	values    map[string]storagecatalog.Value
	raw       map[string]storagecatalog.Cell
	objects   []accessImportObject
}

type accessImportObject struct {
	field string
	ref   attachment.ArchiveReference
}

var accessImportTableNames = [...]string{
	"organizations", "hosted_tenant", "api_tokens", "hosted_members", "hosted_project_grants", "token_grants",
	"hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations",
	"hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets",
	"project_secret_audit", "hosted_audit", "hosted_audit_times",
}

var accessImportTargets = map[string]map[string]string{
	"organizations":              {"id": "organization_id", "name": "name", "slug": "slug", "local": "local", "created_at": "created_at"},
	"hosted_tenant":              {"provider_id": "provider_id", "bootstrap_subject": "bootstrap_subject", "deployment": "deployment", "public_url": "public_url", "allocation_generation": "allocation_generation"},
	"api_tokens":                 {"id": "id", "name": "name", "token_hash": "token_hash", "token_fingerprint": "token_fingerprint", "scope": "scope", "native_only": "native_only", "created_at": "created_at", "updated_at": "updated_at", "rotated_at": "rotated_at", "revoked_at": "revoked_at", "expires_at": "expires_at", "last_used_at": "last_used_at", "hosted_user_id": "hosted_user_id", "hosted_membership_id": "hosted_membership_id", "operator_key_scope": "operator_key_scope", "operator_project_access": "operator_project_access", "key_kind": "key_kind"},
	"hosted_members":             {"user_id": "user_id", "email": "email", "membership_id": "membership_id", "role": "role", "active": "active", "principal_id": "principal_id", "notify_issues": "notify_issues", "created_at": "created_at", "updated_at": "updated_at"},
	"hosted_project_grants":      {"user_id": "user_id", "project_id": "project_id", "can_write": "can_write", "manage_runner": "manage_runner"},
	"token_grants":               {"token_id": "token_id", "project_id": "project_id"},
	"hosted_sessions":            {"token_hash": "token_hash", "email": "email", "identity_subject": "identity_subject", "created_at": "created_at", "expires_at": "expires_at", "revoked_at": "revoked_at"},
	"hosted_transactions":        {"token_hash": "token_hash", "state": "state", "verifier": "verifier", "organization_id": "provider_organization_id", "support_actor": "support_actor", "support_session": "support_session", "invitation_token": "invitation_token", "expires_at": "expires_at", "consumed_at": "consumed_at"},
	"hosted_invitations":         {"id": "id", "email": "email", "role": "role", "accepted_user_id": "accepted_user_id", "created_at": "created_at", "expires_at": "expires_at"},
	"hosted_member_reservations": {"email": "email", "expires_at": "expires_at"},
	"hosted_binding_migrations":  {"id": "id", "from_public_url": "from_public_url", "to_public_url": "to_public_url", "from_deployment": "from_deployment", "from_generation": "from_generation", "to_generation": "to_generation", "migrated_at": "migrated_at", "applied": "applied"},
	"organization_secrets":       {"kind": "kind", "organization_slug": "organization_slug", "ciphertext": "ciphertext", "nonce": "nonce", "wrapped_data_key": "wrapped_data_key", "master_key_version": "master_key_version", "updated_at": "updated_at"},
	"organization_secret_audit":  {"id": "id", "actor": "actor", "kind": "kind", "key_version": "key_version", "event": "event", "recorded_at": "recorded_at"},
	"project_secrets":            {"project_id": "project_id", "kind": "kind", "organization_slug": "organization_slug", "ciphertext": "ciphertext", "nonce": "nonce", "wrapped_data_key": "wrapped_data_key", "master_key_version": "master_key_version", "updated_at": "updated_at"},
	"project_secret_audit":       {"id": "id", "project_id": "project_id", "actor": "actor", "kind": "kind", "key_version": "key_version", "event": "event", "recorded_at": "recorded_at"},
	"hosted_audit":               {"id": "id", "session_id": "session_id", "actual_actor": "actual_actor", "effective_user": "effective_user", "reason": "reason", "event": "event", "route": "route", "project_id": "project_id", "status": "status", "started_at": "started_at", "expires_at": "expires_at", "recorded_at": "recorded_at", "recorded_at_ns": "recorded_at_ns", "account_kind": "account_kind", "account_retry": "account_retry"},
	"hosted_audit_times":         {"audit_id": "audit_id", "recorded_at_ns": "recorded_at_ns"},
}

type accessImportTransform struct {
	name    string
	targets []storagecatalog.Target
}

var accessImportTransforms = map[string]map[string]accessImportTransform{
	"api_tokens":         {"hosted_organization_id": {name: "provider-binding", targets: []storagecatalog.Target{{Table: "public.hosted_tenant", Column: "provider_id", Type: "text"}}}},
	"hosted_tenant":      {"singleton": {name: "tenant-singleton"}},
	"hosted_sessions":    {"identity_json": {name: "hosted-identity", targets: []storagecatalog.Target{{Table: "public.hosted_sessions", Column: "identity_subject", Type: "text"}, {Table: "public.hosted_sessions", Column: "provider_organization_id", Type: "text"}, {Table: "public.hosted_sessions", Column: "session_id", Type: "text"}, {Table: "public.hosted_sessions", Column: "identity_created_at", Type: "timestamp with time zone"}, {Table: "public.hosted_sessions", Column: "identity_expires_at", Type: "timestamp with time zone"}, {Table: "public.hosted_sessions", Column: "support_actor", Type: "text"}, {Table: "public.hosted_sessions", Column: "support_reason", Type: "text"}}}},
	"hosted_invitations": {"grants_json": {name: "invitation-grants", targets: []storagecatalog.Target{{Table: "public.hosted_invitation_grants", Column: "project_id", Type: "text"}, {Table: "public.hosted_invitation_grants", Column: "can_write", Type: "boolean"}, {Table: "public.hosted_invitation_grants", Column: "manage_runner", Type: "boolean"}}}},
	"hosted_audit":       {"mutation_json": {name: "archive-json", targets: []storagecatalog.Target{{Table: "public.hosted_audit", Column: "mutation_object_key", Type: "text"}, {Table: "public.hosted_audit", Column: "mutation_sha256", Type: "text"}, {Table: "public.hosted_audit", Column: "mutation_bytes", Type: "bigint"}, {Table: "public.hosted_audit", Column: "mutation_content_bytes", Type: "bigint"}, {Table: "public.hosted_audit", Column: "mutation_format_version", Type: "integer"}}}},
}

func (r *Repository) NewAccessImporter(ctx context.Context, source AccessImportSource) (*AccessImporter, error) {
	if (source.DB == nil) == (source.copyTx == nil) || source.Source.Store != "hub" || source.SnapshotID == "" || len(source.SnapshotID) > 200 || strings.TrimSpace(source.SnapshotID) != source.SnapshotID || strings.TrimSpace(source.Source.OrganizationID) != source.Source.OrganizationID {
		return nil, ErrAccessImport
	}
	encoded, err := json.Marshal(struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}{source.Catalog, source.Schema})
	if err != nil {
		return nil, ErrAccessImport
	}
	var frozen struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return nil, ErrAccessImport
	}
	source.Catalog, source.Schema = frozen.Catalog, frozen.Schema
	if err := validateAccessImportCatalog(source.Catalog); err != nil {
		return nil, err
	}
	binding, err := source.Catalog.Bind(source.Source, source.Schema)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	i := &AccessImporter{repository: r, source: source, binding: binding, digest: hex.EncodeToString(digest[:]), organization: tracker.OrganizationID(source.Source.OrganizationID)}
	if err := i.validateSource(ctx); err != nil {
		return nil, mapError(ctx, err)
	}
	return i, nil
}

func validateAccessImportCatalog(catalog storagecatalog.Catalog) error {
	if err := catalog.Validate(); err != nil {
		return err
	}
	for tableName, expected := range accessImportTargets {
		table, ok := catalog.Tables["hub/"+tableName]
		if !ok || table.Family != "access" || table.Disposition != "target" {
			return ErrAccessImport
		}
		expectedTargets := make(map[string]string, len(expected))
		for name, target := range expected {
			expectedTargets[name] = target
		}
		for columnName, column := range table.Columns {
			targetName, direct := expectedTargets[columnName]
			if !direct {
				transform, expectedTransform := accessImportTransforms[tableName][columnName]
				if expectedTransform {
					if column.Disposition != "transform" || column.Transform != transform.name || !slices.Equal(column.Targets, transform.targets) {
						return ErrAccessImport
					}
				} else if column.Disposition == "transform" || column.Disposition == "column" || column.Disposition == "object" {
					return ErrAccessImport
				}
				continue
			}
			if column.Disposition != "column" || len(column.Targets) != 1 || column.Targets[0].Table != "public."+tableName || column.Targets[0].Column != targetName {
				return ErrAccessImport
			}
			delete(expectedTargets, columnName)
		}
		if len(expectedTargets) != 0 {
			return ErrAccessImport
		}
		for columnName := range accessImportTransforms[tableName] {
			if _, ok := table.Columns[columnName]; !ok {
				return ErrAccessImport
			}
		}
	}
	return nil
}

func (i *AccessImporter) Step(ctx context.Context, limit int) (AccessImportProgress, error) {
	if limit < 1 || limit > AccessImportLimit {
		return AccessImportProgress{}, ErrAccessImport
	}
	checkpoint, err := i.start(ctx)
	if err != nil {
		return AccessImportProgress{}, fmt.Errorf("start access import checkpoint: %w", err)
	}
	progress := AccessImportProgress{Processed: checkpoint.processed, Done: checkpoint.done}
	if progress.Done {
		return progress, nil
	}
	table := i.tables[checkpoint.stage]
	rows, err := i.readPage(ctx, table, checkpoint, limit)
	if err != nil {
		return progress, fmt.Errorf("read %s access import page: %w", table.name, err)
	}
	for n := range rows {
		if table.name == "hosted_audit" {
			if err := i.prepareAuditObject(ctx, &rows[n]); err != nil {
				return progress, mapError(ctx, err)
			}
		}
	}
	err = i.repository.transaction(ctx, i.organization, corestore.Background, false, func(s *session) error {
		current, err := readAccessCheckpoint(ctx, s.db, i.organization, true)
		if err != nil {
			return err
		}
		if current.revision != checkpoint.revision || current.stage != checkpoint.stage || current.after != checkpoint.after || current.hasAfter != checkpoint.hasAfter || current.done {
			return corestore.ErrRevisionConflict
		}
		for _, row := range rows {
			if table.name == "organizations" {
				if err := i.validateOrganization(ctx, s, row); err != nil {
					return err
				}
			} else if err := i.importRow(ctx, s, table.name, row); err != nil {
				return fmt.Errorf("import %s row: %w", table.name, err)
			}
		}
		next := checkpoint
		next.processed += int64(len(rows))
		if len(rows) > 0 {
			next.after = rows[len(rows)-1].keyValues
			next.hasAfter = true
		}
		if len(rows) < limit {
			next.stage++
			next.after = [3]string{}
			next.hasAfter = false
			if next.stage == int64(len(i.tables)) {
				next.done = true
			}
		}
		result, err := s.db.ExecContext(ctx, `UPDATE access_imports SET stage=$1,processed=$2,revision=revision+1,after_key_1=$3,after_key_2=$4,after_key_3=$5,has_after=$6,done=$7 WHERE organization_id=$8 AND revision=$9`, next.stage, next.processed, next.after[0], next.after[1], next.after[2], next.hasAfter, next.done, i.organization, checkpoint.revision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return corestore.ErrRevisionConflict
		}
		progress = AccessImportProgress{Processed: next.processed, Done: next.done}
		return nil
	})
	if err != nil {
		return AccessImportProgress{Processed: checkpoint.processed}, fmt.Errorf("commit %s access import page: %w", table.name, err)
	}
	return progress, nil
}

func (i *AccessImporter) start(ctx context.Context) (accessImportCheckpoint, error) {
	var checkpoint accessImportCheckpoint
	err := i.repository.transaction(ctx, i.organization, corestore.Background, false, func(s *session) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO access_imports(organization_id,source_schema,snapshot_id,catalog_digest) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id) DO NOTHING`, i.organization, i.source.Source.Schema, i.source.SnapshotID, i.digest)
		if err != nil {
			return err
		}
		checkpoint, err = readAccessCheckpoint(ctx, s.db, i.organization, false)
		return err
	})
	if err != nil {
		return accessImportCheckpoint{}, err
	}
	if checkpoint.sourceSchema != i.source.Source.Schema || checkpoint.snapshotID != i.source.SnapshotID || checkpoint.catalogDigest != i.digest {
		return accessImportCheckpoint{}, ErrAccessImport
	}
	return checkpoint, nil
}

func readAccessCheckpoint(ctx context.Context, db pgqueryDBTX, organization tracker.OrganizationID, lock bool) (accessImportCheckpoint, error) {
	query := `SELECT source_schema,snapshot_id,catalog_digest,stage,processed,revision,after_key_1,after_key_2,after_key_3,has_after,done FROM access_imports WHERE organization_id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var c accessImportCheckpoint
	err := db.QueryRowContext(ctx, query, organization).Scan(&c.sourceSchema, &c.snapshotID, &c.catalogDigest, &c.stage, &c.processed, &c.revision, &c.after[0], &c.after[1], &c.after[2], &c.hasAfter, &c.done)
	if errors.Is(err, sql.ErrNoRows) {
		return accessImportCheckpoint{}, ErrAccessImport
	}
	return c, err
}

type pgqueryDBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (i *AccessImporter) validateSource(ctx context.Context) error {
	for _, name := range accessImportTableNames {
		if _, ok := i.source.Schema[name]; !ok {
			return fmt.Errorf("validate access source %s schema: %w", name, storagecatalog.ErrUnmapped)
		}
		if err := i.validateSourceTable(ctx, name); err != nil {
			return fmt.Errorf("validate access source %s columns: %w", name, err)
		}
		keys, err := accessSourceKeys(i.source.Catalog.Tables["hub/"+name])
		if err != nil || len(keys) == 0 || len(keys) > 3 {
			return fmt.Errorf("validate access source %s primary key: %w", name, ErrAccessImport)
		}
		if err := i.validateSourceIndex(ctx, name, keys); err != nil {
			return fmt.Errorf("validate access source %s index: %w", name, err)
		}
		i.tables = append(i.tables, accessImportTable{name: name, keys: keys})
	}
	statement := strings.Join([]string{"SELECT id FROM", i.sourceTable("organizations"), "ORDER BY id COLLATE " + i.sourceCollation() + " LIMIT 2"}, " ")
	rows, err := i.querySource(ctx, statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return storagecatalog.ErrOrganization
	}
	var organization string
	if err := rows.Scan(&organization); err != nil || organization != string(i.organization) || rows.Next() {
		return storagecatalog.ErrOrganization
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return nil
}

func accessSourceKeys(table storagecatalog.Table) ([]string, error) {
	keys := make([]string, 0, 3)
	for name, column := range table.Columns {
		var metadata []json.RawMessage
		if json.Unmarshal([]byte(column.Source), &metadata) != nil || len(metadata) < 6 {
			return nil, ErrAccessImport
		}
		var sourceName string
		var primary int
		if json.Unmarshal(metadata[1], &sourceName) != nil || json.Unmarshal(metadata[5], &primary) != nil || sourceName != name || primary < 0 || primary > 3 {
			return nil, ErrAccessImport
		}
		if primary > 0 {
			for len(keys) < primary {
				keys = append(keys, "")
			}
			if keys[primary-1] != "" {
				return nil, ErrAccessImport
			}
			keys[primary-1] = name
		}
	}
	if slices.Contains(keys, "") {
		return nil, ErrAccessImport
	}
	return keys, nil
}

func (i *AccessImporter) sourceTable(table string) string {
	if i.source.copyTx != nil {
		return pq.QuoteIdentifier(table)
	}
	return pq.QuoteIdentifier(i.source.Source.Schema) + "." + pq.QuoteIdentifier(table)
}

func (i *AccessImporter) validateSourceTable(ctx context.Context, table string) error {
	if i.source.copyTx != nil {
		return i.validateCopyTable(ctx, table)
	}
	statement := strings.Join([]string{"SELECT * FROM", i.sourceTable(table), "LIMIT 0"}, " ")
	rows, err := i.source.DB.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	expected := i.source.Schema[table]
	if len(columns) != len(expected) {
		return storagecatalog.ErrUnmapped
	}
	for _, column := range columns {
		definition, ok := i.source.Catalog.Tables["hub/"+table].Columns[column.Name()]
		if !ok || expected[column.Name()] != definition.Source {
			return storagecatalog.ErrUnmapped
		}
		var metadata []json.RawMessage
		var sourceType string
		if json.Unmarshal([]byte(definition.Source), &metadata) != nil || len(metadata) < 3 || json.Unmarshal(metadata[2], &sourceType) != nil {
			return ErrAccessImport
		}
		expectedType := "TEXT"
		if sourceType == "BLOB" {
			expectedType = "BYTEA"
		}
		if column.DatabaseTypeName() != expectedType {
			return storagecatalog.ErrUnmapped
		}
	}
	return rows.Close()
}

func (i *AccessImporter) validateSourceIndex(ctx context.Context, table string, keys []string) error {
	if i.source.copyTx != nil {
		return nil
	}
	var valid bool
	err := i.source.DB.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
WHERE i.indrelid=$1::regclass AND i.indisvalid AND i.indisprimary AND i.indpred IS NULL AND i.indexprs IS NULL
AND am.amname='btree' AND i.indnkeyatts=cardinality($2::text[])
AND NOT EXISTS (SELECT 1 FROM unnest(i.indoption) option WHERE option<>0)
AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum,n)
JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE k.n<=i.indnkeyatts ORDER BY k.n)=$2::text[]
AND NOT EXISTS (SELECT 1 FROM unnest(i.indcollation) WITH ORDINALITY k(coll,n)
WHERE k.n<=i.indnkeyatts AND k.coll<>'"C"'::regcollation))`, i.sourceTable(table), pq.Array(keys)).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrAccessImport
	}
	return nil
}

func (i *AccessImporter) readPage(ctx context.Context, table accessImportTable, checkpoint accessImportCheckpoint, limit int) ([]accessImportRow, error) {
	columns := make([]string, 0, len(i.source.Schema[table.name]))
	for name := range i.source.Schema[table.name] {
		columns = append(columns, name)
	}
	slices.Sort(columns)
	quoted := make([]string, len(columns))
	for n, name := range columns {
		quoted[n] = pq.QuoteIdentifier(name)
	}
	order := make([]string, len(table.keys))
	args := make([]any, 0, len(table.keys)+1)
	for n, key := range table.keys {
		order[n] = pq.QuoteIdentifier(key) + " COLLATE " + i.sourceCollation()
	}
	queryParts := []string{"SELECT", strings.Join(quoted, ","), "FROM", i.sourceTable(table.name)}
	if checkpoint.hasAfter {
		predicate := "(" + strings.Join(order, ",") + ") > ("
		placeholders := make([]string, len(table.keys))
		for n := range table.keys {
			placeholders[n] = fmt.Sprintf("$%d::text COLLATE \"C\"", n+1)
			if i.source.copyTx != nil {
				placeholders[n] = "?"
			}
			args = append(args, checkpoint.after[n])
		}
		predicate += strings.Join(placeholders, ",") + ")"
		queryParts = append(queryParts, "WHERE", predicate)
	}
	limitClause := fmt.Sprintf("LIMIT $%d::integer", len(args)+1)
	if i.source.copyTx != nil {
		limitClause = "LIMIT ?"
	}
	queryParts = append(queryParts, "ORDER BY", strings.Join(order, ","), limitClause)
	args = append(args, limit)
	query := strings.Join(queryParts, " ")
	rows, err := i.querySource(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("execute access source page for %s: %w", table.name, mapError(ctx, err))
	}
	defer rows.Close()
	var page []accessImportRow
	for rows.Next() {
		data := make([][]byte, len(columns))
		destinations := make([]any, len(columns))
		for n := range data {
			destinations[n] = &data[n]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		raw := make(map[string]storagecatalog.Cell, len(columns))
		values := make(map[string]storagecatalog.Value, len(columns))
		for n, name := range columns {
			cell := storagecatalog.Cell{Encoding: "utf8", Data: data[n]}
			if data[n] == nil {
				cell = storagecatalog.Cell{Null: true}
			} else {
				var sourceType string
				var metadata []json.RawMessage
				if json.Unmarshal([]byte(i.source.Catalog.Tables["hub/"+table.name].Columns[name].Source), &metadata) != nil || len(metadata) < 3 || json.Unmarshal(metadata[2], &sourceType) != nil {
					return nil, ErrAccessImport
				}
				if sourceType == "BLOB" {
					cell.Encoding = "binary"
				}
			}
			raw[name] = cell
			disposition := i.source.Catalog.Tables["hub/"+table.name].Columns[name].Disposition
			if disposition == "column" || disposition == "object" {
				value, err := i.source.Catalog.Decode("hub/"+table.name, name, cell)
				if err != nil {
					return nil, fmt.Errorf("decode %s.%s: %w", table.name, name, err)
				}
				values[name] = value
			}
		}
		if _, err := i.binding.ValidateMappedRow(table.name, raw); err != nil {
			return nil, err
		}
		row := accessImportRow{values: values, raw: raw}
		for n, key := range table.keys {
			if raw[key].Null || raw[key].Encoding != "utf8" || strings.IndexByte(string(raw[key].Data), 0) >= 0 {
				return nil, storagecatalog.ErrValue
			}
			row.keyValues[n] = string(raw[key].Data)
		}
		page = append(page, row)
	}
	return page, rows.Err()
}

func (i *AccessImporter) prepareAuditObject(ctx context.Context, row *accessImportRow) error {
	if i.source.Objects == nil {
		return ErrAccessImport
	}
	cell := row.raw["mutation_json"]
	if cell.Null || !json.Valid(cell.Data) || len(cell.Data) == 0 || len(cell.Data) > attachment.MaxArchiveBytes {
		return storagecatalog.ErrValue
	}
	ref, err := i.source.Objects.StoreDetail(ctx, string(i.organization), bytes.Clone(cell.Data))
	if err != nil || ref.Validate(string(i.organization)) != nil {
		return errors.Join(ErrAccessImport, err)
	}
	stored, err := i.source.Objects.ReadDetail(ctx, string(i.organization), ref)
	if err != nil || !bytes.Equal(stored, cell.Data) {
		return errors.Join(ErrAccessImport, err)
	}
	row.objects = append(row.objects, accessImportObject{field: "mutation_json", ref: ref})
	return nil
}

func (i *AccessImporter) validateOrganization(ctx context.Context, s *session, row accessImportRow) error {
	if len(row.raw) == 0 || row.raw["id"].Null || string(row.raw["id"].Data) != string(i.organization) {
		return storagecatalog.ErrOrganization
	}
	values := row.values
	var name, slug string
	var local bool
	var created time.Time
	err := s.db.QueryRowContext(ctx, `SELECT name,slug,local,created_at FROM organizations WHERE organization_id=$1`, i.organization).Scan(&name, &slug, &local, &created)
	if err != nil {
		return errors.Join(storagecatalog.ErrOrganization, err)
	}
	if values["name"].Text != name || values["slug"].Text != slug || values["local"].Boolean != local || !values["created_at"].Time.Equal(created) {
		return storagecatalog.ErrOrganization
	}
	return nil
}

func (i *AccessImporter) importRow(ctx context.Context, s *session, table string, row accessImportRow) error {
	values := row.values
	columns := map[string]any{"organization_id": i.organization}
	for name, value := range values {
		column := i.source.Catalog.Tables["hub/"+table].Columns[name]
		targetName := name
		if len(column.Targets) == 1 {
			targetName = column.Targets[0].Column
		}
		if value.Null {
			columns[targetName] = nil
			continue
		}
		switch column.Codec {
		case "text":
			columns[targetName] = value.Text
		case "int64":
			columns[targetName] = value.Integer
		case "bool01":
			columns[targetName] = value.Boolean
		case "timestamp", "unix-seconds":
			columns[targetName] = value.Time
		case "bytes":
			columns[targetName] = bytes.Clone(value.Bytes)
		default:
			return ErrAccessImport
		}
	}
	var extra []map[string]any
	switch table {
	case "hosted_tenant":
		if row.raw["singleton"].Null || string(row.raw["singleton"].Data) != "1" {
			return ErrAccessImport
		}
	case "api_tokens":
		if columns["hosted_user_id"] == nil {
			columns["hosted_user_id"] = ""
		}
		if columns["hosted_membership_id"] == nil {
			columns["hosted_membership_id"] = ""
		}
		if columns["operator_key_scope"] == nil {
			columns["operator_key_scope"] = ""
		}
		provider := string(row.raw["hosted_organization_id"].Data)
		if provider != "" {
			matches, err := i.providerMatches(ctx, s, provider)
			if err != nil || !matches {
				return errors.Join(ErrAccessImport, err)
			}
		}
	case "hosted_sessions":
		identity, err := decodeHostedImportIdentity(row.raw["identity_json"].Data)
		if err != nil {
			return err
		}
		if subject := string(row.raw["identity_subject"].Data); subject != "" && subject != identity.Subject {
			return ErrAccessImport
		}
		if identity.Subject == "" && row.values["revoked_at"].Null || identity.Subject != "" && (identity.SessionID == "" || identity.CreatedAt.IsZero() || identity.ExpiresAt.IsZero()) {
			return ErrAccessImport
		}
		provider := identity.OrganizationID
		if provider != "" {
			matches, err := i.providerMatches(ctx, s, provider)
			if err != nil || !matches {
				return errors.Join(ErrAccessImport, err)
			}
		}
		columns["identity_subject"] = identity.Subject
		columns["provider_organization_id"] = provider
		columns["session_id"] = identity.SessionID
		if identity.Subject == "" {
			columns["identity_created_at"] = values["created_at"].Time
			columns["identity_expires_at"] = values["expires_at"].Time
		} else {
			columns["identity_created_at"] = identity.CreatedAt
			columns["identity_expires_at"] = identity.ExpiresAt
		}
		columns["support_actor"] = identity.SupportActor
		columns["support_reason"] = identity.SupportReason
	case "hosted_transactions":
		provider := string(row.raw["organization_id"].Data)
		if provider != "" {
			matches, err := i.providerMatches(ctx, s, provider)
			if err != nil || !matches {
				return errors.Join(ErrAccessImport, err)
			}
		}
		columns["provider_organization_id"] = provider
	case "hosted_invitations":
		grants, err := decodeInvitationImportGrants(row.raw["grants_json"].Data)
		if err != nil {
			return err
		}
		for _, grant := range grants {
			extra = append(extra, map[string]any{"organization_id": i.organization, "invitation_id": columns["id"], "project_id": grant.ProjectID, "can_write": grant.Write, "manage_runner": grant.Runner})
		}
	case "hosted_member_reservations":
		columns["expires_at"] = values["expires_at"].Time
	case "hosted_audit":
		if len(row.objects) != 1 {
			return ErrAccessImport
		}
		ref := row.objects[0].ref
		columns["mutation_object_key"] = ref.Key
		columns["mutation_sha256"] = ref.SHA256
		columns["mutation_bytes"] = ref.CompressedSize
		columns["mutation_content_bytes"] = ref.UncompressedSize
		columns["mutation_format_version"] = ref.SchemaVersion
	}
	if err := i.validateProjectReference(ctx, s, table, columns); err != nil {
		return err
	}
	if err := insertAccessRow(ctx, s.db, table, columns); err != nil {
		return err
	}
	for _, child := range extra {
		if err := i.validateProjectReference(ctx, s, "hosted_invitation_grants", child); err != nil {
			return err
		}
		if err := insertAccessRow(ctx, s.db, "hosted_invitation_grants", child); err != nil {
			return err
		}
	}
	return nil
}

func decodeHostedImportIdentity(data []byte) (auth.HostedIdentity, error) {
	var identity auth.HostedIdentity
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return identity, ErrAccessImport
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return identity, errors.Join(ErrAccessImport, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return identity, ErrAccessImport
	}
	return identity, nil
}

type accessImportGrant struct {
	ProjectID string `json:"project_id"`
	Write     bool   `json:"write"`
	Runner    bool   `json:"runner"`
}

func decodeInvitationImportGrants(data []byte) ([]accessImportGrant, error) {
	var grants []accessImportGrant
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '[' {
		return nil, ErrAccessImport
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grants); err != nil {
		return nil, errors.Join(ErrAccessImport, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrAccessImport
	}
	seen := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		if strings.TrimSpace(grant.ProjectID) == "" {
			return nil, ErrAccessImport
		}
		if _, ok := seen[grant.ProjectID]; ok {
			return nil, ErrAccessImport
		}
		seen[grant.ProjectID] = struct{}{}
	}
	return grants, nil
}

func (i *AccessImporter) providerMatches(ctx context.Context, s *session, provider string) (bool, error) {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT provider_id FROM hosted_tenant WHERE organization_id=$1`, i.organization).Scan(&stored)
	if err != nil {
		return false, err
	}
	return stored == provider, nil
}

func (i *AccessImporter) validateProjectReference(ctx context.Context, s *session, table string, values map[string]any) error {
	project, ok := values["project_id"].(string)
	if !ok || project == "" {
		return nil
	}
	if table != "hosted_project_grants" && table != "token_grants" && table != "hosted_invitation_grants" && table != "project_secrets" && table != "project_secret_audit" && table != "hosted_audit" {
		return nil
	}
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR KEY SHARE`, i.organization, project).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccessImport
	}
	if err != nil {
		return err
	}
	return nil
}

func insertAccessRow(ctx context.Context, db pgqueryDBTX, table string, values map[string]any) error {
	columns := make([]string, 0, len(values))
	for name := range values {
		columns = append(columns, name)
	}
	slices.Sort(columns)
	quoted, args, placeholders := make([]string, len(columns)), make([]any, len(columns)), make([]string, len(columns))
	for n, name := range columns {
		quoted[n] = pq.QuoteIdentifier(name)
		args[n] = values[name]
		placeholders[n] = fmt.Sprintf("$%d", n+1)
	}
	statement := "INSERT INTO " + pq.QuoteIdentifier(table) + " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
	_, err := db.ExecContext(ctx, statement, args...)
	return err
}
