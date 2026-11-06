package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func storeConfigurationCommandFacts(ctx context.Context, tx *sql.Tx, runner string, command *runnerauth.ProjectConfigurationCommand) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM runner_configuration_requests WHERE runner_id=?`, runner); err != nil {
		return err
	}
	if command == nil {
		return nil
	}
	r := command.Request
	_, err := tx.ExecContext(ctx, `INSERT INTO runner_configuration_requests(runner_id,runner_revision,allow_local_binding,request_id,operation,project_id,expected_config_revision,expected_policy_id,policy_id,source_revision,checkpoint) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, runner, command.RunnerRevision, r.AllowLocalBinding, r.RequestID, r.Operation, r.ProjectID, r.ExpectedConfigRevision, r.ExpectedPolicyID, r.PolicyID, r.SourceRevision, r.Checkpoint)
	return err
}

func readConfigurationCommandFacts(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, runner string) (*runnerauth.ProjectConfigurationCommand, error) {
	var ready sql.NullBool
	var raw sql.NullString
	command := new(runnerauth.ProjectConfigurationCommand)
	var revision sql.NullInt64
	var binding sql.NullBool
	var requestID sql.NullString
	var operation sql.NullString
	var projectID sql.NullString
	var expectedConfigRevision sql.NullString
	var expectedPolicyID sql.NullString
	var policyID sql.NullString
	var sourceRevision sql.NullString
	var checkpoint sql.NullString
	err := q.QueryRowContext(ctx, `SELECT r.routing_facts_ready,CASE WHEN r.routing_facts_ready IS NULL OR c.runner_id IS NOT NULL THEN r.routing_settings_json END,c.runner_revision,c.allow_local_binding,c.request_id,c.operation,c.project_id,c.expected_config_revision,c.expected_policy_id,c.policy_id,c.source_revision,c.checkpoint FROM runner_identities r LEFT JOIN runner_configuration_requests c ON c.runner_id=r.id WHERE r.organization_id=? AND r.id=?`, organization, runner).Scan(&ready, &raw, &revision, &binding, &requestID, &operation, &projectID, &expectedConfigRevision, &expectedPolicyID, &policyID, &sourceRevision, &checkpoint)
	if err != nil {
		return nil, err
	}
	if !raw.Valid {
		return nil, sql.ErrNoRows
	}
	var source runnerSettings
	if err := json.Unmarshal([]byte(raw.String), &source); err != nil {
		return nil, err
	}
	if !ready.Valid {
		if source.ProjectConfigurationCommand == nil {
			return nil, sql.ErrNoRows
		}
		return source.ProjectConfigurationCommand, nil
	}
	if !revision.Valid {
		return nil, sql.ErrNoRows
	}
	if source.ProjectConfigurationCommand == nil {
		return nil, nativeInvalid("Project configuration issuer is unavailable")
	}
	command.Issuer = source.ProjectConfigurationCommand.Issuer
	command.RunnerRevision = revision.Int64
	if binding.Valid {
		command.Request.AllowLocalBinding = &binding.Bool
	}
	command.Request.RequestID = requestID.String
	command.Request.Operation = operation.String
	command.Request.ProjectID = projectID.String
	command.Request.ExpectedConfigRevision = expectedConfigRevision.String
	command.Request.ExpectedPolicyID = expectedPolicyID.String
	command.Request.PolicyID = policyID.String
	command.Request.SourceRevision = sourceRevision.String
	command.Request.Checkpoint = checkpoint.String
	return command, nil
}
