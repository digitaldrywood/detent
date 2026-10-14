package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func migrateCloudWorkflowLanes(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id, organization_id, workflow_markdown, states_json FROM projects WHERE profile='native' AND workflow_source='' AND workflow_markdown<>''")
	if err != nil {
		return err
	}
	defer rows.Close()
	type rewrite struct {
		id           string
		organization string
		before       []byte
		markdown     []byte
	}
	var rewrites []rewrite
	for rows.Next() {
		var id, organization, markdown, statesJSON string
		if err := rows.Scan(&id, &organization, &markdown, &statesJSON); err != nil {
			return err
		}
		var states []tracker.NativeState
		if err := json.Unmarshal([]byte(statesJSON), &states); err != nil {
			return fmt.Errorf("project %s workflow states: %w", id, err)
		}
		order := make([]string, 0, len(states))
		for _, state := range states {
			order = append(order, state.Name)
		}
		out, changed, err := workflowconfig.MigrateTrackerLanes([]byte(markdown), order...)
		if err != nil {
			return fmt.Errorf("project %s workflow lanes: %w", id, err)
		}
		if changed {
			rewrites = append(rewrites, rewrite{id: id, organization: organization, before: []byte(markdown), markdown: out})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range rewrites {
		if err := migrateCloudWorkflowApproval(ctx, tx, item.organization+"/"+item.id, item.before, item.markdown); err != nil {
			return fmt.Errorf("project %s workflow approval: %w", item.id, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE projects SET workflow_markdown=?, integration_revision=integration_revision+1 WHERE id=?", string(item.markdown), item.id); err != nil {
			return err
		}
	}
	return nil
}

func migrateCloudWorkflowApproval(ctx context.Context, tx *sql.Tx, scope string, before, after []byte) error {
	var raw, approvedBy, approvedAt string
	err := tx.QueryRowContext(ctx, `SELECT r.metadata_json, r.approved_by, r.approved_at
FROM project_policies p JOIN policy_revisions r ON r.scope=p.scope AND r.policy_id=p.policy_id WHERE p.scope=?`, scope).Scan(&raw, &approvedBy, &approvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var descriptor policy.Descriptor
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return err
	}
	if descriptor.Workflow != nil || descriptor.Configuration == nil {
		return nil
	}
	oldWorkflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: before})
	if err != nil {
		return err
	}
	if descriptor.Configuration.DefinitionDigest != oldWorkflow.SourceHash {
		return nil
	}
	approved, err := workflowconfig.ApplyNativePolicy(oldWorkflow, descriptor)
	if err != nil {
		return err
	}
	newWorkflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: after})
	if err != nil {
		return err
	}
	approved.SourceHash = newWorkflow.SourceHash
	approved.Authored, approved.DefinitionSources = newWorkflow.Authored, newWorkflow.DefinitionSources
	approved.Config.Tracker.Lanes = newWorkflow.Config.Tracker.Lanes
	updated, err := workflowconfig.ResolvePolicy(approved)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO policy_revisions(scope,policy_id,metadata_json,approved_by,approved_at) VALUES (?,?,?,?,?)
ON CONFLICT(scope,policy_id) DO NOTHING`, scope, updated.ID, string(encoded), approvedBy, approvedAt); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE project_policies SET policy_id=? WHERE scope=? AND policy_id=?", updated.ID, scope, descriptor.ID)
	return err
}
