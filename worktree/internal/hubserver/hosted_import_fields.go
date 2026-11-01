package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const hostedImportRowsPerTick = 100

type hostedImportProjection struct {
	table, payload string
	columns        []string
	update         string
}

var hostedImportProjections = []hostedImportProjection{
	{"hosted_plans", "record_json", []string{"collaboration_bytes"}, `UPDATE hosted_plans SET collaboration_bytes=COALESCE(collaboration_bytes,?) WHERE rowid=? AND COALESCE(record_json,'null')=? AND (collaboration_bytes IS NULL)`},
	{"hosted_artifact_usage", "usage_json", []string{"retained_bytes", "reserved_bytes"}, `UPDATE hosted_artifact_usage SET retained_bytes=COALESCE(retained_bytes,?),reserved_bytes=COALESCE(reserved_bytes,?) WHERE rowid=? AND COALESCE(usage_json,'null')=? AND (retained_bytes IS NULL OR reserved_bytes IS NULL)`},
	{"hosted_sessions", "identity_json", []string{"identity_subject"}, `UPDATE hosted_sessions SET identity_subject=COALESCE(identity_subject,?) WHERE rowid=? AND COALESCE(identity_json,'null')=? AND (identity_subject IS NULL)`},
	{"artifact_services", "binding_json", []string{"binding_mode", "hosted_opt_in"}, `UPDATE artifact_services SET binding_mode=COALESCE(binding_mode,?),hosted_opt_in=COALESCE(hosted_opt_in,?) WHERE rowid=? AND COALESCE(binding_json,'null')=? AND (binding_mode IS NULL OR hosted_opt_in IS NULL)`},
	{"usage_cost_observations", "observation_json", []string{"voided"}, `UPDATE usage_cost_observations SET voided=COALESCE(voided,?) WHERE rowid=? AND COALESCE(observation_json,'null')=? AND (voided IS NULL)`},
	{"hosted_audit", "event", []string{"account_kind", "account_retry"}, `UPDATE hosted_audit SET account_kind=COALESCE(account_kind,?),account_retry=COALESCE(account_retry,?) WHERE rowid=? AND COALESCE(event,'null')=? AND (account_kind IS NULL OR account_retry IS NULL)`},
	{"github_import_records", "record_json", []string{"dependency_id", "provenance_external_id"}, `UPDATE github_import_records SET dependency_id=COALESCE(dependency_id,?),provenance_external_id=COALESCE(provenance_external_id,?) WHERE rowid=? AND COALESCE(record_json,'null')=? AND (dependency_id IS NULL OR provenance_external_id IS NULL)`},
	{"issues", "provenance_json", []string{"provenance_provider", "provenance_external_id"}, `UPDATE issues SET provenance_provider=COALESCE(provenance_provider,?),provenance_external_id=COALESCE(provenance_external_id,?) WHERE rowid=? AND COALESCE(provenance_json,'null')=? AND (provenance_provider IS NULL OR provenance_external_id IS NULL)`},
	{"onboarding_issue_intake", "request_json", []string{"hub_owned", "batch_status"}, `UPDATE onboarding_issue_intake SET hub_owned=COALESCE(hub_owned,?),batch_status=COALESCE(batch_status,?) WHERE rowid=? AND COALESCE(request_json,'null')=? AND (hub_owned IS NULL OR batch_status IS NULL)`},
	{"github_outbox", "desired_json", []string{"desired_issue_number", "desired_summary"}, `UPDATE github_outbox SET desired_issue_number=COALESCE(desired_issue_number,?),desired_summary=COALESCE(desired_summary,?) WHERE rowid=? AND COALESCE(desired_json,'null')=? AND (desired_issue_number IS NULL OR desired_summary IS NULL)`},
	{"runner_identities", "operations_json", []string{"operations_known"}, `UPDATE runner_identities SET operations_known=COALESCE(operations_known,?) WHERE rowid=? AND COALESCE(operations_json,'null')=? AND (operations_known IS NULL)`},
}

func provenanceProvider(value *tracker.Provenance) string {
	if value == nil {
		return ""
	}
	return value.Provider
}

func provenanceExternalID(value *tracker.Provenance) string {
	if value == nil {
		return ""
	}
	return value.ExternalID
}

type outboxFields struct {
	IssueNumber int  `json:"issue_number"`
	Summary     bool `json:"summary"`
}

func decodeOutboxFields(raw []byte) (outboxFields, error) {
	var value outboxFields
	err := json.Unmarshal(raw, &value)
	return value, err
}

func decodeAccountFields(event string) accountReceipt {
	var value accountReceipt
	if strings.HasPrefix(strings.TrimSpace(event), "{") {
		if err := json.Unmarshal([]byte(event), &value); err != nil {
			return accountReceipt{}
		}
	}
	return value
}

type hostedImportFieldValues struct {
	columns      []any
	dependencyID string
	provenance   tracker.Provenance
	outbox       outboxFields
	voided       bool
	operations   []string
}

func hostedImportFields(table, raw string) (hostedImportFieldValues, error) {
	decode := func(value any) error { return json.Unmarshal([]byte(raw), value) }
	switch table {
	case "hosted_plans":
		var value HostedPlan
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		return hostedImportFieldValues{columns: []any{value.Allowances["collaboration_bytes"]}}, nil
	case "hosted_artifact_usage":
		var value artifact.Usage
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		return hostedImportFieldValues{columns: []any{value.RetainedBytes, value.ReservedBytes}}, nil
	case "hosted_sessions":
		var value auth.HostedIdentity
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		return hostedImportFieldValues{columns: []any{value.Subject}}, nil
	case "artifact_services":
		var value artifact.Binding
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		return hostedImportFieldValues{columns: []any{value.Mode, value.HostedOptIn}}, nil
	case "usage_cost_observations":
		var value costObservation
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		return hostedImportFieldValues{columns: []any{value.Voided}, voided: value.Voided}, nil
	case "hosted_audit":
		value := decodeAccountFields(raw)
		return hostedImportFieldValues{columns: []any{value.Kind, value.Retry}}, nil
	case "github_import_records":
		var value GitHubImportRecord
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		return hostedImportFieldValues{columns: []any{value.DependencyID, value.Provenance.ExternalID}, dependencyID: value.DependencyID, provenance: value.Provenance}, nil
	case "issues":
		var value *tracker.Provenance
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		fields := hostedImportFieldValues{columns: []any{provenanceProvider(value), provenanceExternalID(value)}}
		if value != nil {
			fields.provenance = *value
		}
		return fields, nil
	case "onboarding_issue_intake":
		var value tracker.GitHubBatch
		if err := decode(&value); err != nil {
			return hostedImportFieldValues{}, err
		}
		return hostedImportFieldValues{columns: []any{value.HubOwned, value.Status}}, nil
	case "github_outbox":
		value, err := decodeOutboxFields([]byte(raw))
		return hostedImportFieldValues{columns: []any{value.IssueNumber, value.Summary}, outbox: value}, err
	case "runner_identities":
		return decodeRunnerFields(raw)
	default:
		return hostedImportFieldValues{}, errors.New("unknown hosted import projection")
	}
}

func writeRunnerOperations(ctx context.Context, tx *sql.Tx, runner, raw string) error {
	var operations []string
	if err := json.Unmarshal([]byte(raw), &operations); err != nil {
		return err
	}
	return insertRunnerOperations(ctx, tx, runner, operations)
}

func insertRunnerOperations(ctx context.Context, tx *sql.Tx, runner string, operations []string) error {
	for _, operation := range slices.Compact(slices.Sorted(slices.Values(operations))) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO runner_operations(runner_id,operation) VALUES(?,?) ON CONFLICT DO NOTHING`, runner, operation); err != nil {
			return err
		}
	}
	return nil
}

func (d *database) populateHostedImportFields(ctx context.Context) error {
	for range hostedImportRowsPerTick {
		changed, err := d.populateHostedImportField(ctx)
		if err != nil || !changed {
			return err
		}
	}
	return nil
}

func (d *database) populateHostedImportField(ctx context.Context) (bool, error) {
	for _, projection := range hostedImportProjections {
		var rowID int64
		var raw string
		unknown := make([]string, len(projection.columns))
		for i, column := range projection.columns {
			unknown[i] = column + " IS NULL"
		}
		predicate := "(" + strings.Join(unknown, " OR ") + ")"
		err := d.reader.QueryRowContext(ctx, `SELECT rowid,COALESCE(`+projection.payload+`,'null') FROM `+projection.table+` WHERE `+predicate+` LIMIT 1`).Scan(&rowID, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		values, err := hostedImportFields(projection.table, raw)
		if err != nil {
			return false, fmt.Errorf("decode %s typed fields: %w", projection.table, err)
		}
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer tx.Rollback()
		args := append(values.columns[:len(projection.columns):len(projection.columns)], rowID, raw)
		result, err := tx.ExecContext(ctx, projection.update, args...)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if changed != 0 && projection.table == "runner_identities" {
			var runner string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM runner_identities WHERE rowid=?`, rowID).Scan(&runner); err != nil {
				return false, err
			}
			if err := insertRunnerOperations(ctx, tx, runner, values.operations); err != nil {
				return false, err
			}
		}
		return true, d.commit(ctx, tx)
	}
	return false, nil
}
