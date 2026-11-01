package platformstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func decodeImportJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrImport
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return ErrImport
	}
	return nil
}

func (i *Importer) decodeRow(name string, cells map[string]storagecatalog.Cell) (importRow, error) {
	row := importRow{name: name, values: map[string]any{}, text: map[string]string{}}
	for column, definition := range i.source.Catalog.Tables[name].Columns {
		cell := cells[column]
		if definition.Disposition == "pending" {
			if !slices.Contains([]string{"auth/sessions/identity_json", "auth/authorizations/identity_json", "auth/access_keys/organizations_json", "auth/access_key_organizations/blocked_projects_json"}, name+"/"+column) || cell.Null || !json.Valid(cell.Data) {
				return row, storagecatalog.ErrUnmapped
			}
			row.values[column] = string(cell.Data)
			row.text[column] = string(cell.Data)
			continue
		}
		if definition.Disposition != "column" || len(definition.Targets) != 1 || definition.Targets[0].Table != "platform."+importTarget(name) || definition.Targets[0].Column != column {
			return row, storagecatalog.ErrUnmapped
		}
		value, err := i.source.Catalog.Decode(name, column, cell)
		if err != nil {
			return row, err
		}
		var scalar any
		if !value.Null {
			switch definition.Codec {
			case "text":
				scalar = value.Text
				row.text[column] = value.Text
			case "int64":
				scalar = value.Integer
			case "bool01":
				scalar = value.Boolean
			case "timestamp":
				scalar = value.Time
			default:
				return row, storagecatalog.ErrUnmapped
			}
		}
		row.values[column] = scalar
		if column == "organization_id" || column == "service_organization_id" || name == "registry/organizations" && column == "id" || name == "auth/transactions" && column == "invitation_organization" {
			if value.Text != "" && !i.organizations[value.Text] {
				return row, storagecatalog.ErrOrganization
			}
		}
	}
	return row, nil
}

func (i *Importer) prepareRow(ctx context.Context, row *importRow) error {
	v := row.values
	switch row.name {
	case "auth/sessions", "auth/authorizations":
		var identity auth.HostedIdentity
		data := []byte(row.text["identity_json"])
		if err := decodeImportJSON(data, &identity); err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return ErrImport
		}
		for _, field := range []string{"created_at", "expires_at"} {
			var value string
			if json.Unmarshal(fields[field], &value) != nil {
				return ErrImport
			}
			if _, err := i.source.Catalog.Decode(row.name, field, storagecatalog.Cell{Encoding: "utf8", Data: []byte(value)}); err != nil {
				return err
			}
		}
		if identity.Subject == "" || identity.SessionID == "" || identity.CreatedAt.IsZero() || !identity.ExpiresAt.After(identity.CreatedAt) || identity.CreatedAt.Nanosecond()%1000 != 0 || identity.ExpiresAt.Nanosecond()%1000 != 0 {
			return ErrImport
		}
		if row.name == "auth/sessions" && identity.Subject != v["subject"] {
			return ErrImport
		}
		if row.name == "auth/authorizations" {
			if v["binding"] != cloudassert.AuthorizationBinding(row.text["session_hash"], row.text["organization_id"], identity.SessionID) || v["support"] != (identity.SupportActor != "") {
				return ErrImport
			}
			for _, field := range []string{"access_token", "refresh_token"} {
				purpose := "provider-access"
				if field == "refresh_token" {
					purpose = "provider-refresh"
				}
				if err := i.checkCipher(purpose, row.text["binding"], row.text[field]); err != nil {
					return err
				}
			}
		}
		delete(v, "identity_json")
		v["identity_subject"], v["provider_organization_id"], v["provider_session_id"] = identity.Subject, identity.OrganizationID, identity.SessionID
		v["identity_created_at"], v["identity_expires_at"] = identity.CreatedAt, identity.ExpiresAt
		v["support_actor"], v["support_reason"] = identity.SupportActor, identity.SupportReason
	case "auth/transactions":
		for _, field := range []string{"verifier", "invitation_token"} {
			purpose := "login-verifier"
			if field == "invitation_token" {
				purpose = "login-invitation"
			}
			sealed, err := i.repository.secrets.seal(purpose, row.text["token_hash"], row.text[field])
			if err != nil {
				return err
			}
			v[field] = sealed
		}
	case "auth/access_keys":
		if err := i.prepareKey(row); err != nil {
			return err
		}
	case "auth/access_key_organizations":
		var blocked []string
		if err := decodeImportJSON([]byte(row.text["blocked_projects_json"]), &blocked); err != nil {
			return err
		}
		if len(blocked) > 200 || len(apikey.NormalizeProjectIDs(blocked)) != len(blocked) {
			return ErrImport
		}
		delete(v, "blocked_projects_json")
		for _, project := range blocked {
			if strings.TrimSpace(project) != project || len(project) > 256 {
				return ErrImport
			}
			row.writes = append(row.writes, importWrite{table: "access_key_blocked_projects", values: map[string]any{"organization_id": v["organization_id"], "key_id": v["key_id"], "project_id": project}})
		}
	case "auth/github_user_connections":
		if err := i.checkCipher("github-user-access", row.text["owner_subject"], row.text["access_token"]); err != nil {
			return err
		}
	case "auth/oauth_grants":
		if v["access_hash"] == "" {
			v["access_hash"] = nil
		}
	case "auth/audit":
		return i.prepareAudit(ctx, row)
	}
	return nil
}

func (i *Importer) checkCipher(purpose, binding, sealed string) error {
	if strings.HasPrefix(sealed, "hk1.") {
		parts := strings.Split(sealed, ".")
		if len(parts) != 5 {
			return ErrImport
		}
		version, err := strconv.Atoi(parts[1])
		if err != nil || !slices.Contains(i.source.KeyVersions, version) {
			return ErrImport
		}
	}
	if _, err := i.repository.secrets.open(purpose, binding, sealed); err != nil {
		return ErrImport
	}
	return nil
}

func (i *Importer) prepareKey(row *importRow) error {
	v := row.values
	var organizations []struct {
		apikey.ProjectContext
		OrganizationID string `json:"organization_id"`
	}
	if err := decodeImportJSON([]byte(row.text["organizations_json"]), &organizations); err != nil {
		return err
	}
	k := Key{ID: row.text["id"], Hash: row.text["token_hash"], Name: row.text["name"], Kind: row.text["kind"], OwnerSubject: row.text["owner_subject"], Permission: row.text["permission"], AccessContext: row.text["access_context"]}
	if id := row.text["service_organization_id"]; id != "" {
		organization := tracker.OrganizationID(id)
		k.ServiceOrganizationID = &organization
	} else {
		v["service_organization_id"] = nil
	}
	for _, o := range organizations {
		if !i.organizations[o.OrganizationID] {
			return storagecatalog.ErrOrganization
		}
		selection := KeyOrganization{OrganizationID: tracker.OrganizationID(o.OrganizationID), ProjectAccess: o.Access}
		for _, p := range o.Projects {
			selection.Projects = append(selection.Projects, tracker.ProjectID(p))
		}
		k.Organizations = append(k.Organizations, selection)
		row.writes = append(row.writes, importWrite{table: "access_key_organizations", values: map[string]any{"key_id": k.ID, "organization_id": o.OrganizationID, "project_access": o.Access, "selected": true}})
		for _, project := range o.Projects {
			row.writes = append(row.writes, importWrite{table: "access_key_projects", values: map[string]any{"key_id": k.ID, "organization_id": o.OrganizationID, "project_id": project}})
		}
	}
	if !validKey(k) {
		return ErrImport
	}
	delete(v, "organizations_json")
	return nil
}

func insertImportRow(ctx context.Context, tx *sql.Tx, table string, values map[string]any, suffix string) error {
	columns := slices.Sorted(maps.Keys(values))
	placeholders := make([]string, len(columns))
	args := make([]any, len(columns))
	for n, column := range columns {
		args[n], placeholders[n], columns[n] = values[column], fmt.Sprintf("$%d", n+1), pq.QuoteIdentifier(column)
	}
	var query strings.Builder
	query.WriteString("INSERT INTO platform.")
	query.WriteString(pq.QuoteIdentifier(table))
	query.WriteString("(")
	query.WriteString(strings.Join(columns, ","))
	query.WriteString(") VALUES(")
	query.WriteString(strings.Join(placeholders, ","))
	query.WriteString(")")
	query.WriteString(suffix)
	_, err := tx.ExecContext(ctx, query.String(), args...)
	return err
}

func (i *Importer) writeRow(ctx context.Context, tx *sql.Tx, row importRow) error {
	v := row.values
	if slices.Contains([]string{"auth/oauth_requests", "auth/oauth_refresh_tokens", "auth/oauth_spent_refresh_tokens"}, row.name) {
		rows, err := tx.QueryContext(ctx, `SELECT organization_id FROM platform.oauth_grants WHERE id=$1 LIMIT 2`, v["grant_id"])
		if err != nil {
			return err
		}
		defer rows.Close()
		var organizations []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			organizations = append(organizations, id)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if len(organizations) != 1 || !i.organizations[organizations[0]] {
			return storagecatalog.ErrOrganization
		}
		v["organization_id"] = organizations[0]
	}
	suffix := ""
	if row.name == "auth/access_key_organizations" {
		suffix = ` ON CONFLICT(organization_id,key_id) DO UPDATE SET blocked=excluded.blocked,reached_at=excluded.reached_at,last_used_at=excluded.last_used_at,approved_at=excluded.approved_at`
	}
	if err := insertImportRow(ctx, tx, importTarget(row.name), v, suffix); err != nil {
		return err
	}
	if row.name == "registry/organizations" && v["slug"] == "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM platform.organization_slugs WHERE organization_id=$1`, v["id"]); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE platform.organizations SET slug='' WHERE id=$1`, v["id"]); err != nil {
			return err
		}
	}
	for _, write := range row.writes {
		suffix := ""
		if write.table == "administration_receipts" {
			var hash string
			err := tx.QueryRowContext(ctx, `SELECT input_hash FROM platform.administration_receipts WHERE subject=$1 AND organization_id=$2 AND retry_identity=$3`, write.values["subject"], write.values["organization_id"], write.values["retry_identity"]).Scan(&hash)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil && hash != write.values["input_hash"] {
				return ErrImport
			}
			suffix = ` ON CONFLICT(subject,organization_id,retry_identity) DO UPDATE SET outcome=excluded.outcome,resource_id=excluded.resource_id,url=excluded.url,reconnect=excluded.reconnect,sign_out=excluded.sign_out,signed_out=excluded.signed_out,provider_confirmed=excluded.provider_confirmed,audit_recorded=excluded.audit_recorded,output_object_key=excluded.output_object_key,recorded_at=excluded.recorded_at,import_source_id=excluded.import_source_id WHERE administration_receipts.input_hash=excluded.input_hash AND administration_receipts.import_source_id<excluded.import_source_id`
		}
		if err := insertImportRow(ctx, tx, write.table, write.values, suffix); err != nil {
			return err
		}
	}
	if slices.Contains([]string{"registry/organization_events", "registry/platform_member_changes", "auth/audit", "auth/access_key_notifications"}, row.name) {
		id, ok := v["id"].(int64)
		if !ok || id < 1 {
			return ErrImport
		}
		sequence := "platform." + importTarget(row.name) + "_id_seq"
		var query strings.Builder
		query.WriteString(`SELECT setval($1::regclass,GREATEST($2,(SELECT last_value FROM platform.`)
		query.WriteString(pq.QuoteIdentifier(importTarget(row.name) + "_id_seq"))
		query.WriteString(`)),true)`)
		_, err := tx.ExecContext(ctx, query.String(), sequence, id)
		return err
	}
	return nil
}
