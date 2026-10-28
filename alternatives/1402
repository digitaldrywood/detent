package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readAccountReceipt(ctx context.Context, query nativeQueryer, organization, actor, retry string) (string, error) {
	var latestID int64
	var latest string
	err := query.QueryRowContext(ctx, `SELECT id,event FROM hosted_audit WHERE organization_id=? AND actual_actor=? AND account_kind='account_administration' AND account_retry=? ORDER BY id DESC LIMIT 1`, organization, actor, retry).Scan(&latestID, &latest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := query.QueryContext(ctx, `SELECT event,account_kind,account_retry FROM hosted_audit WHERE organization_id=? AND actual_actor=? AND id>? AND (account_kind IS NULL OR account_retry IS NULL) ORDER BY id DESC LIMIT ?`, organization, actor, latestID, hostedImportFallbackRows+1)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > hostedImportFallbackRows {
			return "", errors.New("legacy account receipts await bounded maintenance")
		}
		var raw string
		var kind, identity sql.NullString
		if err := rows.Scan(&raw, &kind, &identity); err != nil {
			return "", err
		}
		if !kind.Valid || !identity.Valid {
			value := decodeAccountFields(raw)
			if !kind.Valid {
				kind.String = value.Kind
			}
			if !identity.Valid {
				identity.String = value.Retry
			}
		}
		if kind.String == "account_administration" && identity.String == retry {
			return raw, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if latestID != 0 {
		return latest, nil
	}
	return "", sql.ErrNoRows
}

type legacyHostedSession struct{ hash, raw string }

func legacyHostedSessions(ctx context.Context, query nativeQueryer, subject string) ([]legacyHostedSession, error) {
	rows, err := query.QueryContext(ctx, `SELECT token_hash,identity_json FROM hosted_sessions WHERE identity_subject IS NULL AND revoked_at IS NULL LIMIT ?`, hostedImportFallbackRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []legacyHostedSession
	count := 0
	for rows.Next() {
		count++
		if count > hostedImportFallbackRows {
			return nil, errors.New("legacy session subjects await bounded maintenance")
		}
		var session legacyHostedSession
		if err := rows.Scan(&session.hash, &session.raw); err != nil {
			return nil, err
		}
		var identity auth.HostedIdentity
		if err := json.Unmarshal([]byte(session.raw), &identity); err != nil {
			return nil, err
		}
		if identity.Subject == subject {
			sessions = append(sessions, session)
		}
	}
	return sessions, rows.Err()
}

func legacyHostedArtifactBinding(ctx context.Context, query nativeQueryer, service, organization, token string) (string, error) {
	rows, err := query.QueryContext(ctx, `SELECT s.binding_json,s.binding_mode,s.hosted_opt_in FROM artifact_services s JOIN token_grants g ON g.token_id=s.publisher_token_id AND g.project_id=s.project_id AND g.organization_id=s.organization_id WHERE s.id=? AND s.organization_id=? AND s.publisher_token_id=? AND (s.binding_mode IS NULL OR s.hosted_opt_in IS NULL)`, service, organization, token)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var mode sql.NullString
		var optIn sql.NullBool
		if err := rows.Scan(&raw, &mode, &optIn); err != nil {
			return "", err
		}
		var binding artifact.Binding
		if err := json.Unmarshal([]byte(raw), &binding); err != nil {
			return "", err
		}
		if mode.Valid {
			binding.Mode = mode.String
		}
		if optIn.Valid {
			binding.HostedOptIn = optIn.Bool
		}
		if binding.Mode == "hosted" && binding.HostedOptIn {
			return raw, nil
		}
	}
	return "", rows.Err()
}

func (d *database) readHostedArtifactUsage(ctx context.Context, query nativeQueryer) (int64, int64, error) {
	var retained, reserved sql.NullInt64
	err := query.QueryRowContext(ctx, `SELECT retained_bytes,reserved_bytes FROM hosted_artifact_usage WHERE singleton=1`).Scan(&retained, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	if !retained.Valid || !reserved.Valid {
		var raw string
		if err := d.reader.QueryRowContext(ctx, `SELECT usage_json FROM hosted_artifact_usage WHERE singleton=1`).Scan(&raw); err != nil {
			return 0, 0, err
		}
		var value artifact.Usage
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return 0, 0, err
		}
		if !retained.Valid {
			retained.Int64 = value.RetainedBytes
		}
		if !reserved.Valid {
			reserved.Int64 = value.ReservedBytes
		}
	}
	return retained.Int64, reserved.Int64, nil
}

func findExistingGitHubIssue(ctx context.Context, query nativeQueryer, scope nativeScope, repository int64, preview tracker.GitHubIssuePreview) (string, error) {
	var existing string
	err := query.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND
(native_source_key IN (?,?) OR github_node_id=? OR (provenance_provider='github' AND provenance_external_id=?) OR (repository_id=? AND github_number=?))`, scope.organization, scope.project, "github:"+preview.URL, "github:"+preview.ID, preview.ID, preview.ID, repository, preview.Number).Scan(&existing)
	if !errors.Is(err, sql.ErrNoRows) {
		return existing, err
	}
	rows, err := query.QueryContext(ctx, `SELECT native_id,provenance_provider,provenance_external_id,provenance_json FROM issues WHERE organization_id=? AND project_id=? AND (provenance_provider IS NULL OR provenance_external_id IS NULL)`, scope.organization, scope.project)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var provider, external, raw sql.NullString
		if err := rows.Scan(&id, &provider, &external, &raw); err != nil {
			return "", err
		}
		payload := "null"
		if raw.Valid {
			payload = raw.String
		}
		fields, err := evaluatedHostedImportFields(ctx, query, "issues", payload)
		if err != nil {
			return "", err
		}
		if !provider.Valid {
			provider.String = fields.provenance.Provider
		}
		if !external.Valid {
			external.String = fields.provenance.ExternalID
		}
		if provider.String == "github" && external.String == preview.ID {
			return id, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", sql.ErrNoRows
}
