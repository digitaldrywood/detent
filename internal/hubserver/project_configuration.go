package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) localProjectConfiguration(ctx context.Context, name string, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	scope.project = tracker.ProjectID(args.ProjectID)
	tx, err := e.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		return operatortool.Result{}, err
	}
	defer tx.Rollback()
	if err := authorizeNativeProject(ctx, tx, scope); err != nil {
		return operatortool.Result{}, err
	}
	path := "$." + jsonPathKey(args.ProjectID)
	rows, err := tx.QueryContext(ctx, "SELECT id, json_extract(project_configuration_json, ?), routing_settings_json FROM runner_identities WHERE organization_id = ? AND json_type(project_configuration_json, ?) = 'object'", path, scope.organization, path)
	if err != nil {
		return operatortool.Result{}, err
	}
	type observation struct {
		runner   string
		raw      string
		settings string
	}
	var observations []observation
	for rows.Next() {
		var item observation
		if err := rows.Scan(&item.runner, &item.raw, &item.settings); err != nil {
			rows.Close()
			return operatortool.Result{}, err
		}
		observations = append(observations, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return operatortool.Result{}, err
	}
	now, err := e.service.database.currentTime()
	if err != nil {
		return operatortool.Result{}, err
	}
	view := project.MissingConfigurationOwner(args.ProjectID)
	var selected runnerauth.Runner
	for _, item := range observations {
		if args.RunnerID != "" && args.RunnerID != item.runner {
			continue
		}
		r, err := readRunner(ctx, tx, scope.organization, item.runner, now)
		if err != nil || r.ConnectionHealth != "online" || !slices.Contains(r.ProjectIDs, scope.project) {
			continue
		}
		var candidate runnerauth.ProjectConfiguration
		if json.Unmarshal([]byte(item.raw), &candidate) != nil || candidate.ProjectID != args.ProjectID || candidate.ObservedAt.Before(r.LastHeartbeatAt.Add(-runnerauth.HeartbeatTimeout)) {
			continue
		}
		var settings runnerSettings
		if json.Unmarshal([]byte(item.settings), &settings) != nil {
			return operatortool.Result{}, errProjectServiceUnavailable
		}
		if pending := settings.ProjectConfigurationCommand; pending != nil && pending.Request.ProjectID == args.ProjectID {
			candidate.RequestID, candidate.Pending, candidate.Applied, candidate.Saved = pending.Request.RequestID, true, false, false
		}
		if selected.RunnerID != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		selected, view = r, candidate
		view.RunnerID, view.RunnerRevision = r.RunnerID, r.Revision
	}
	if name == operatortool.LocalProjectConfiguration || selected.RunnerID == "" {
		return hubProjectResult(view)
	}
	operation := name + " " + args.RunnerID + " " + args.ProjectID
	hash, err := nativeCommandHash(args)
	if err != nil {
		return operatortool.Result{}, err
	}
	if raw, found, err := nativeCommandReceipt(ctx, tx, scope, operation, args.RequestID, hash); err != nil {
		return operatortool.Result{}, err
	} else if found {
		return hubProjectResult(json.RawMessage(raw))
	}
	if args.RunnerID != selected.RunnerID || args.ExpectedRunnerRevision != selected.Revision {
		return operatortool.Result{}, nativeConflict(tracker.Revision(selected.Revision))
	}
	if err := e.service.configurationRequestAuthority(ctx, tx, scope, now); err != nil {
		return operatortool.Result{}, err
	}
	if view.Constraint != "" || view.ConfigRevision != args.ExpectedConfigRevision || view.EffectivePolicy == nil || view.EffectivePolicy.ID != args.ExpectedPolicyID {
		return operatortool.Result{}, nativeConflict(tracker.Revision(selected.Revision))
	}
	if name == "apply_local_project_policy" {
		if view.UnsettledAttempts != 0 {
			return operatortool.Result{}, policyMismatch("Active leases retain their approved policy; finish or cancel them before approving a different revision")
		}
		approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
		if err != nil || approval.Policy.ID != args.PolicyID || approval.Policy.SourceRevision != args.SourceRevision {
			return operatortool.Result{}, policyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
		}
		if args.AllowLocalBinding != nil {
			candidate := view.RestrictedBindingPolicy
			if *args.AllowLocalBinding {
				candidate = view.LocalBindingPolicy
			}
			if candidate == nil || candidate.ID != args.PolicyID {
				return operatortool.Result{}, policyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
			}
		}
	}
	request := runnerauth.ProjectConfigurationRequest{RequestID: args.RequestID, Operation: name, ProjectID: args.ProjectID, ExpectedConfigRevision: args.ExpectedConfigRevision, ExpectedPolicyID: args.ExpectedPolicyID, PolicyID: args.PolicyID, SourceRevision: args.SourceRevision, Checkpoint: args.Checkpoint, AllowLocalBinding: args.AllowLocalBinding}
	rawAuthority, err := json.Marshal(scope.credential)
	if err != nil {
		return operatortool.Result{}, err
	}
	settings := settingsFromRouting(selected.Routing)
	settings.ProjectConfigurationCommand = &runnerauth.ProjectConfigurationCommand{Request: request, Issuer: rawAuthority, RunnerRevision: selected.Revision + 1}
	rawSettings, err := json.Marshal(settings)
	if err != nil {
		return operatortool.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET routing_settings_json = ?, revision = revision + 1 WHERE organization_id = ? AND id = ?", string(rawSettings), scope.organization, selected.RunnerID); err != nil {
		return operatortool.Result{}, err
	}
	view.RunnerRevision = selected.Revision + 1
	view.RequestID, view.Applied, view.Saved, view.Pending = args.RequestID, false, false, true
	raw, err := json.Marshal(view)
	if err != nil {
		return operatortool.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO native_commands (organization_id, actor_id, operation, command_key, request_hash, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)", scope.organization, scope.credential.ID, operation, args.RequestID, hash, string(raw), formatHubTime(now)); err != nil {
		return operatortool.Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return operatortool.Result{}, err
	}
	return hubProjectResult(view)
}

func (s *Service) configurationRequestAuthority(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	if scope.credential.Runner.RunnerID != "" {
		return nativeNotFound()
	}
	scope.requireHostedAdmin = true
	if scope.credential.Hosted != nil {
		return s.recheckHostedMutation(ctx, tx, scope)
	}
	if scope.credential.Scope != apiScopeAdmin {
		return nativeNotFound()
	}
	var active int
	err := tx.QueryRowContext(ctx, "SELECT count(*) FROM api_tokens WHERE id = ? AND token_hash = ? AND scope = 'admin' AND revoked_at IS NULL AND julianday(created_at) <= julianday(?) AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))", scope.credential.ID, scope.credential.Hash, formatHubTime(now), formatHubTime(now)).Scan(&active)
	if err != nil {
		return err
	}
	if active != 1 {
		return nativeNotFound()
	}
	return authorizeNativeProject(ctx, tx, scope)
}

func (s *Service) runnerProjectConfiguration(ctx context.Context, tx *sql.Tx, scope nativeScope, observation *runnerauth.ProjectConfiguration, now time.Time) (*runnerauth.ProjectConfigurationRequest, error) {
	path := "$." + jsonPathKey(string(scope.project))
	if observation != nil {
		if err := observation.Validate(); err != nil || observation.ProjectID != string(scope.project) {
			return nil, nativeInvalid("Invalid project configuration observation")
		}
		observation.ObservedAt = now
		observation.RunnerID, observation.RunnerRevision = "", 0
		observation.Pending = false
		raw, err := json.Marshal(observation)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET project_configuration_json = json_set(project_configuration_json, ?, json(?)) WHERE organization_id = ? AND id = ?", path, string(raw), scope.organization, scope.credential.Runner.RunnerID); err != nil {
			return nil, err
		}
	}
	r, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
	if err != nil {
		return nil, err
	}
	command := r.Routing.ProjectConfigurationCommand
	if command == nil || command.Request.ProjectID != string(scope.project) {
		return nil, nil
	}
	request := command.Request
	var credential apiCredential
	if json.Unmarshal(command.Issuer, &credential) != nil {
		return nil, nativeInvalid("Invalid project configuration request")
	}
	finish := func(view runnerauth.ProjectConfiguration) (*runnerauth.ProjectConfigurationRequest, error) {
		view.RunnerID, view.RunnerRevision = r.RunnerID, command.RunnerRevision
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		operation := request.Operation + " " + r.RunnerID + " " + string(scope.project)
		if _, err := tx.ExecContext(ctx, "UPDATE native_commands SET response_json = ? WHERE organization_id = ? AND actor_id = ? AND operation = ? AND command_key = ?", string(raw), scope.organization, credential.ID, operation, request.RequestID); err != nil {
			return nil, err
		}
		settings := settingsFromRouting(r.Routing)
		settings.ProjectConfigurationCommand = nil
		rawSettings, err := json.Marshal(settings)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE runner_identities SET routing_settings_json = ?, project_configuration_json = json_set(project_configuration_json, ?, json(?)) WHERE organization_id = ? AND id = ?", string(rawSettings), path, string(raw), scope.organization, r.RunnerID)
		return nil, err
	}
	if observation != nil && observation.RequestID == request.RequestID {
		return finish(*observation)
	}
	refuse := func() (*runnerauth.ProjectConfigurationRequest, error) {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT json_extract(project_configuration_json, ?) FROM runner_identities WHERE organization_id = ? AND id = ?", path, scope.organization, r.RunnerID).Scan(&raw); err != nil {
			return nil, err
		}
		var view runnerauth.ProjectConfiguration
		if err := json.Unmarshal([]byte(raw), &view); err != nil {
			return nil, err
		}
		view.RequestID, view.Applied, view.Saved, view.Pending = request.RequestID, false, false, false
		view.Constraint = "The selected policy was not applied; verify approval and supported workflow through the existing policy owner."
		return finish(view)
	}
	if r.Revision != command.RunnerRevision || !slices.Contains(r.ProjectIDs, scope.project) || request.ProjectID != string(scope.project) {
		return refuse()
	}
	issuer := nativeScope{organization: scope.organization, project: scope.project, credential: credential}
	if s.configurationRequestAuthority(ctx, tx, issuer, now) != nil {
		return refuse()
	}
	if request.Operation == "apply_local_project_policy" {
		approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
		if err != nil || approval.Policy.ID != request.PolicyID {
			return refuse()
		}
	}
	return &request, nil
}

func jsonPathKey(key string) string {
	raw, _ := json.Marshal(key)
	return string(raw)
}
