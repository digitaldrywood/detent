package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) localProjectConfiguration(ctx context.Context, name string, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
	return e.projectConfigurationCommand(ctx, name, args, false)
}

func (e hubProjectExecutor) projectConfigurationCommand(ctx context.Context, name string, args operatortool.LocalProjectArguments, preview bool) (operatortool.Result, error) {
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	scope.project = tracker.ProjectID(args.ProjectID)
	pool := e.service.database.db
	txCtx := ctx
	var options *sql.TxOptions
	if name == operatortool.LocalProjectConfiguration || preview {
		pool = e.service.database.reader
		options = &sql.TxOptions{ReadOnly: true}
		var cancel context.CancelFunc
		txCtx, cancel = context.WithTimeout(ctx, hubReadTimeout)
		defer cancel()
	}
	tx, err := pool.BeginTx(txCtx, options)
	if err != nil {
		return operatortool.Result{}, err
	}
	defer tx.Rollback()
	if err := authorizeNativeProject(ctx, tx, scope); err != nil {
		return operatortool.Result{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, project_configuration_json, routing_settings_json FROM runner_identities WHERE organization_id = ?", scope.organization)
	if err != nil {
		return operatortool.Result{}, err
	}
	defer rows.Close()
	type observation struct {
		runner   string
		raw      string
		settings string
	}
	var observations []observation
	for rows.Next() {
		var item observation
		if err := rows.Scan(&item.runner, &item.raw, &item.settings); err != nil {
			return operatortool.Result{}, err
		}
		entry, err := projectConfigurationEntry(item.raw, args.ProjectID)
		if err != nil {
			return operatortool.Result{}, err
		}
		if !entry.Valid {
			continue
		}
		item.raw = entry.String
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
		if json.Unmarshal([]byte(item.raw), &candidate) != nil || candidate.ProjectID != args.ProjectID {
			continue
		}
		if err := overlayProjectConfigurationObservation(ctx, tx, scope, item.runner, &candidate); err != nil {
			return operatortool.Result{}, err
		}
		if candidate.ObservedAt.Before(r.LastHeartbeatAt.Add(-runnerauth.HeartbeatTimeout)) {
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
	if name == operatortool.LocalProjectConfiguration && selected.RunnerID != "" {
		var raw, operation string
		suffix := " " + selected.RunnerID + " " + args.ProjectID
		var err error
		raw, operation, err = readCompletedConfigurationReceipt(ctx, tx, scope.organization, []string{"apply_local_project_policy" + suffix, "drain_local_project" + suffix, "detach_local_project" + suffix, "resume_local_project" + suffix})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return operatortool.Result{}, err
		}
		if err == nil {
			var receipt runnerauth.ProjectConfiguration
			if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
				return operatortool.Result{}, err
			}
			view.LastOperation = &runnerauth.ProjectConfigurationReceipt{RequestID: receipt.RequestID, Operation: strings.TrimSuffix(operation, suffix), ConfigRevision: receipt.ConfigRevision, Applied: receipt.Applied, Saved: receipt.Saved, Constraint: receipt.Constraint, ObservedAt: receipt.ObservedAt}
			if receipt.EffectivePolicy != nil {
				view.LastOperation.PolicyID = receipt.EffectivePolicy.ID
			}
		}
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
	if err := e.service.configurationRequestAuthority(ctx, tx, scope, now, name); err != nil {
		return operatortool.Result{}, err
	}
	if view.Constraint != "" || view.ConfigRevision != args.ExpectedConfigRevision || view.EffectivePolicy == nil || view.EffectivePolicy.ID != args.ExpectedPolicyID {
		return operatortool.Result{}, nativeConflict(tracker.Revision(selected.Revision))
	}
	if name == "apply_local_project_policy" {
		if view.UnsettledAttempts != 0 {
			return operatortool.Result{}, policyMismatch("Active leases retain their approved policy; finish or cancel them before approving a different revision")
		}
		approval, err := requireProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
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
	if preview {
		return hubProjectResult(view)
	}
	request := runnerauth.ProjectConfigurationRequest{RequestID: args.RequestID, Operation: name, ProjectID: args.ProjectID, ExpectedConfigRevision: args.ExpectedConfigRevision, ExpectedPolicyID: args.ExpectedPolicyID, PolicyID: args.PolicyID, SourceRevision: args.SourceRevision, Checkpoint: args.Checkpoint, AllowLocalBinding: args.AllowLocalBinding}
	rawAuthority, err := json.Marshal(scope.credential)
	if scope.credential.EntryKey != nil {
		origin, ok := ctx.Value(operatorEntryOriginKey{}).(string)
		if !ok || origin == "" {
			origin = operatortool.CurrentConnection(ctx).Origin
		}
		if origin == "" {
			return operatortool.Result{}, operatortool.ErrAccessDenied
		}
		rawAuthority, err = json.Marshal(operatorChatIdentity{Identity: operatorIdentity(scope.credential, string(scope.organization)), Origin: origin})
	}
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
	if err := storeRunnerRoutingFacts(ctx, tx, selected.RunnerID, string(rawSettings)); err != nil {
		return operatortool.Result{}, err
	}
	view.RunnerRevision = selected.Revision + 1
	view.RequestID, view.Applied, view.Saved, view.Pending = args.RequestID, false, false, true
	raw, err := json.Marshal(view)
	if err != nil {
		return operatortool.Result{}, err
	}
	if _, err := insertNativeCommandReceipt(ctx, tx, scope.organization, scope.credential.ID, operation, args.RequestID, hash, string(raw), formatHubTime(now)); err != nil {
		return operatortool.Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return operatortool.Result{}, err
	}
	return hubProjectResult(view)
}

func (s *Service) configurationRequestAuthority(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time, operation string) error {
	if operation == "resume_local_project" {
		runnerScope := scope
		runnerScope.project = ""
		runnerScope.requireHostedAdmin = true
		if err := s.requireRunnerAccess(ctx, tx, runnerScope, now, apikey.ScopeAdmin); err != nil {
			return fmt.Errorf("%w: resume_local_project requires runner administration (manage_runner) and admin scope", err)
		}
	}
	if scope.credential.Runner.RunnerID != "" {
		return nativeNotFound()
	}
	scope.requireHostedAdmin = true
	if scope.credential.Hosted != nil {
		return s.recheckHostedMutation(ctx, tx, &scope)
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

type projectConfigurationIssuer struct {
	requestID      string
	runnerRevision int64
	credential     apiCredential
}

func (s *Service) prepareProjectConfigurationIssuer(ctx context.Context, scope nativeScope, observation *runnerauth.ProjectConfiguration) (projectConfigurationIssuer, error) {
	if scope.credential.Runner.RunnerID == "" {
		return projectConfigurationIssuer{}, nil
	}
	command, err := readConfigurationCommandFacts(ctx, s.database.reader, scope.organization, scope.credential.Runner.RunnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return projectConfigurationIssuer{}, nil
	}
	if err != nil {
		return projectConfigurationIssuer{}, err
	}
	if command == nil {
		return projectConfigurationIssuer{}, nil
	}
	if command.Request.ProjectID != string(scope.project) || observation != nil && observation.RequestID == command.Request.RequestID {
		return projectConfigurationIssuer{}, nil
	}
	var identity operatorChatIdentity
	if err := json.Unmarshal(command.Issuer, &identity); err != nil {
		return projectConfigurationIssuer{}, err
	}
	if identity.Origin == "" && !identity.Valid() {
		return projectConfigurationIssuer{}, nil
	}
	issuer := projectConfigurationIssuer{requestID: command.Request.RequestID, runnerRevision: command.RunnerRevision}
	credential, err := s.currentEntryOriginCredential(ctx, identity.Origin)
	if err == nil && operatorIdentity(credential, string(scope.organization)) == identity.Identity {
		issuer.credential = credential
	}
	return issuer, nil
}

type preparedProjectConfiguration struct {
	view         *runnerauth.ProjectConfiguration
	digest       string
	raw          string
	policy       string
	legacyRaw    sql.NullString
	legacyDigest string
	document     sql.NullString
	replacement  string
	mapRevision  int64
}

func (s *Service) prepareProjectConfiguration(ctx context.Context, scope nativeScope, observation *runnerauth.ProjectConfiguration, now time.Time) (preparedProjectConfiguration, error) {
	if observation == nil {
		return preparedProjectConfiguration{}, nil
	}
	if err := observation.Validate(); err != nil || observation.ProjectID != string(scope.project) {
		return preparedProjectConfiguration{}, nativePublicInvalid("Invalid project configuration observation")
	}
	if observation.Diagnostics != nil {
		observation.Diagnostics.Redact()
		pruneExpiredProjectDiagnostics(observation.Diagnostics, now)
	}
	observation.ObservedAt = now
	observation.RunnerID, observation.RunnerRevision = "", 0
	observation.Pending = false
	observation.LastOperation = nil
	prepared, err := encodeProjectConfiguration(*observation)
	if err != nil {
		return preparedProjectConfiguration{}, err
	}
	if observation.SelectedPolicy != nil {
		raw, err := json.Marshal(observation.SelectedPolicy)
		if err != nil {
			return preparedProjectConfiguration{}, err
		}
		prepared.policy = string(raw)
	}
	return prepareLegacyProjectConfiguration(ctx, s.database.reader, scope, prepared)
}

func pruneExpiredProjectDiagnostics(diagnostics *runnerauth.ProjectDiagnostics, now time.Time) bool {
	previous := len(diagnostics.Records)
	diagnostics.Records = slices.DeleteFunc(diagnostics.Records, func(record runnerauth.DiagnosticAttempt) bool {
		return !record.OperationPending && !record.LeaseExpiresAt.After(now) && !slices.ContainsFunc(record.Membership, func(membership string) bool {
			return membership != "durable_active"
		})
	})
	return len(diagnostics.Records) != previous
}

func encodeProjectConfiguration(view runnerauth.ProjectConfiguration) (preparedProjectConfiguration, error) {
	digest, err := projectConfigurationDigest(view)
	if err != nil {
		return preparedProjectConfiguration{}, err
	}
	raw, err := json.Marshal(view)
	return preparedProjectConfiguration{view: &view, digest: digest, raw: string(raw)}, err
}

func (s *Service) runnerProjectConfiguration(ctx context.Context, tx *sql.Tx, scope nativeScope, prepared preparedProjectConfiguration, preparedIssuer projectConfigurationIssuer, now time.Time) (runnerauth.ProjectConfigurationRequest, error) {
	observation := prepared.view
	if observation != nil {
		if observation.Diagnostics != nil {
			if pruneExpiredProjectDiagnostics(observation.Diagnostics, now) {
				encoded, err := encodeProjectConfiguration(*observation)
				if err != nil {
					return runnerauth.ProjectConfigurationRequest{}, err
				}
				prepared.digest, prepared.raw = encoded.digest, encoded.raw
				if prepared.document.Valid {
					prepared.replacement, err = replaceProjectConfigurationEntry(prepared.document.String, string(scope.project), prepared.raw)
					if err != nil {
						return runnerauth.ProjectConfigurationRequest{}, err
					}
				}
			}
			for _, admission := range observation.Diagnostics.Admissions {
				if admission.Result != "skipped" || admission.WorkItemRevision <= 0 || admission.ObservedAt.After(now) {
					continue
				}
				var id tracker.WorkItemID
				err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=? AND revision=?`, scope.organization, scope.project, admission.IssueID, admission.WorkItemRevision).Scan(&id)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return runnerauth.ProjectConfigurationRequest{}, err
				}
				newer, err := hasNewerRunnerDecision(ctx, tx, scope, admission.IssueID, scope.credential.Runner.RunnerID, admission.ObservedAt)
				if err != nil {
					return runnerauth.ProjectConfigurationRequest{}, err
				}
				if newer {
					continue
				}
				if err := recordNativeSchedulingOutcome(ctx, tx, &scope, id, tracker.NativeSchedulerDecision{Source: "native_runner_admission", Outcome: admission.Result, Reason: admission.Predicate}, admission.ObservedAt); err != nil {
					return runnerauth.ProjectConfigurationRequest{}, err
				}
			}
		}
		if err := storePreparedProjectConfiguration(ctx, tx, scope, prepared); err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		if observation.SelectedPolicy != nil {
			if err := storeEncodedObservedPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project), scope.credential.Runner.RunnerID, *observation.SelectedPolicy, prepared.policy, "null", now); err != nil {
				return runnerauth.ProjectConfigurationRequest{}, err
			}
		}
	}
	pendingCommand, err := readConfigurationCommandFacts(ctx, tx, scope.organization, scope.credential.Runner.RunnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return runnerauth.ProjectConfigurationRequest{}, nil
	}
	if err != nil {
		return runnerauth.ProjectConfigurationRequest{}, err
	}
	if pendingCommand == nil || pendingCommand.Request.ProjectID != string(scope.project) {
		return runnerauth.ProjectConfigurationRequest{}, nil
	}
	r, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
	if err != nil {
		return runnerauth.ProjectConfigurationRequest{}, err
	}
	command := pendingCommand
	if command == nil || command.Request.ProjectID != string(scope.project) {
		return runnerauth.ProjectConfigurationRequest{}, nil
	}
	request := command.Request
	var identity operatorChatIdentity
	var credential apiCredential
	if json.Unmarshal(command.Issuer, &identity) != nil {
		return runnerauth.ProjectConfigurationRequest{}, nativePublicInvalid("Invalid project configuration request")
	}
	entryIssuer := identity.Origin != "" || identity.Valid()
	if entryIssuer {
		credential.ID = identity.PrincipalID
	} else if json.Unmarshal(command.Issuer, &credential) != nil {
		return runnerauth.ProjectConfigurationRequest{}, nativePublicInvalid("Invalid project configuration request")
	}
	finish := func(view runnerauth.ProjectConfiguration) (runnerauth.ProjectConfigurationRequest, error) {
		view.RunnerID, view.RunnerRevision = r.RunnerID, command.RunnerRevision
		raw, err := json.Marshal(view)
		if err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		operation := request.Operation + " " + r.RunnerID + " " + string(scope.project)
		if _, err := tx.ExecContext(ctx, "UPDATE native_commands SET response_json = ?,response_pending=0 WHERE organization_id = ? AND actor_id = ? AND operation = ? AND command_key = ?", string(raw), scope.organization, credential.ID, operation, request.RequestID); err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		settings := settingsFromRouting(r.Routing)
		settings.ProjectConfigurationCommand = nil
		rawSettings, err := json.Marshal(settings)
		if err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		if err := storeProjectConfigurationObservation(ctx, tx, scope, view); err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE runner_identities SET routing_settings_json = ? WHERE organization_id = ? AND id = ?", string(rawSettings), scope.organization, r.RunnerID)
		if err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		if err := storeRunnerRoutingFacts(ctx, tx, r.RunnerID, string(rawSettings)); err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		return runnerauth.ProjectConfigurationRequest{}, nil
	}
	if observation != nil && observation.RequestID == request.RequestID {
		return finish(*observation)
	}
	refuse := func() (runnerauth.ProjectConfigurationRequest, error) {
		raw, err := readProjectConfigurationEntry(ctx, tx, scope, r.RunnerID)
		if err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		var view runnerauth.ProjectConfiguration
		if err := json.Unmarshal([]byte(raw.String), &view); err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		if err := overlayProjectConfigurationObservation(ctx, tx, scope, r.RunnerID, &view); err != nil {
			return runnerauth.ProjectConfigurationRequest{}, err
		}
		view.RequestID, view.Applied, view.Saved, view.Pending = request.RequestID, false, false, false
		view.Constraint = "The selected policy was not applied; verify approval and supported workflow through the existing policy owner."
		return finish(view)
	}
	if r.Revision != command.RunnerRevision || !slices.Contains(r.ProjectIDs, scope.project) || request.ProjectID != string(scope.project) {
		return refuse()
	}
	if entryIssuer {
		if preparedIssuer.requestID != request.RequestID || preparedIssuer.runnerRevision != command.RunnerRevision {
			return runnerauth.ProjectConfigurationRequest{}, nil
		}
		if operatorIdentity(preparedIssuer.credential, string(scope.organization)) != identity.Identity {
			return refuse()
		}
		credential = preparedIssuer.credential
	} else if credential.EntryKey != nil {
		return refuse()
	}
	issuer := nativeScope{organization: scope.organization, project: scope.project, credential: credential}
	if s.configurationRequestAuthority(ctx, tx, issuer, now, request.Operation) != nil {
		return refuse()
	}
	if request.Operation == "apply_local_project_policy" {
		approval, err := requireProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
		if err != nil || approval.Policy.ID != request.PolicyID {
			return refuse()
		}
	}
	return request, nil
}

func jsonPathKey(key string) string {
	raw, err := json.Marshal(key)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func projectConfigurationDigest(view runnerauth.ProjectConfiguration) (string, error) {
	view.ObservedAt = time.Time{}
	if view.Diagnostics != nil {
		diagnostics := *view.Diagnostics
		diagnostics.ObservedAt, diagnostics.RuntimeObservedAt = time.Time{}, time.Time{}
		view.Diagnostics = &diagnostics
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func prepareLegacyProjectConfiguration(ctx context.Context, q nativeQueryer, scope nativeScope, prepared preparedProjectConfiguration) (preparedProjectConfiguration, error) {
	var document sql.NullString
	var legacy bool
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN o.snapshot_digest IS NULL OR o.snapshot_digest<>? THEN r.project_configuration_json END,o.snapshot_digest IS NULL,r.project_configuration_revision
FROM runner_identities r LEFT JOIN runner_project_observations o
ON o.organization_id=r.organization_id AND o.runner_id=r.id AND o.project_id=?
WHERE r.organization_id=? AND r.id=?`, prepared.digest, scope.project, scope.organization, scope.credential.Runner.RunnerID).Scan(&document, &legacy, &prepared.mapRevision)
	if err != nil || !document.Valid {
		return prepared, err
	}
	prepared.document = document
	prepared.replacement, err = replaceProjectConfigurationEntry(document.String, string(scope.project), prepared.raw)
	if err != nil || !legacy {
		return prepared, err
	}
	var projects map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document.String), &projects); err != nil {
		return preparedProjectConfiguration{}, err
	}
	entry, ok := projects[string(scope.project)]
	if !ok || string(entry) == "null" {
		return prepared, nil
	}
	prepared.legacyRaw = sql.NullString{String: string(entry), Valid: true}
	var observation runnerauth.ProjectConfiguration
	if err := json.Unmarshal([]byte(prepared.legacyRaw.String), &observation); err != nil {
		return preparedProjectConfiguration{}, err
	}
	prepared.legacyDigest, err = projectConfigurationDigest(observation)
	return prepared, err
}

func storeProjectConfigurationObservation(ctx context.Context, tx *sql.Tx, scope nativeScope, view runnerauth.ProjectConfiguration) error {
	prepared, err := encodeProjectConfiguration(view)
	if err != nil {
		return err
	}
	prepared, err = prepareLegacyProjectConfiguration(ctx, tx, scope, prepared)
	if err != nil {
		return err
	}
	return storePreparedProjectConfiguration(ctx, tx, scope, prepared)
}

func storePreparedProjectConfiguration(ctx context.Context, tx *sql.Tx, scope nativeScope, prepared preparedProjectConfiguration) error {
	view, digest := prepared.view, prepared.digest
	runner := scope.credential.Runner.RunnerID
	var previous string
	var boardVersion int
	err := tx.QueryRowContext(ctx, "SELECT snapshot_digest,board_projection_version FROM runner_project_observations WHERE organization_id = ? AND runner_id = ? AND project_id = ?", scope.organization, runner, scope.project).Scan(&previous, &boardVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) && prepared.legacyRaw.Valid {
		raw, err := readProjectConfigurationEntry(ctx, tx, scope, runner)
		if err != nil {
			return err
		}
		if raw == prepared.legacyRaw {
			previous = prepared.legacyDigest
		}
	}
	if previous != digest {
		var updated int64
		if prepared.document.Valid {
			result, err := tx.ExecContext(ctx, "UPDATE runner_identities SET project_configuration_json=?,project_configuration_revision=project_configuration_revision+1 WHERE organization_id=? AND id=? AND project_configuration_revision=?", prepared.replacement, scope.organization, runner, prepared.mapRevision)
			if err != nil {
				return err
			}
			updated, err = result.RowsAffected()
			if err != nil {
				return err
			}
		}
		if updated == 0 {
			var document string
			if err := tx.QueryRowContext(ctx, "SELECT project_configuration_json FROM runner_identities WHERE organization_id=? AND id=?", scope.organization, runner).Scan(&document); err != nil {
				return err
			}
			raw, err := replaceProjectConfigurationEntry(document, string(scope.project), prepared.raw)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET project_configuration_json=?,project_configuration_revision=project_configuration_revision+1 WHERE organization_id=? AND id=?", raw, scope.organization, runner); err != nil {
				return err
			}
		}
	}
	var diagnostics, runtime any
	if view.Diagnostics != nil {
		diagnostics = formatHubTime(view.Diagnostics.ObservedAt)
		if !view.Diagnostics.RuntimeObservedAt.IsZero() {
			runtime = formatHubTime(view.Diagnostics.RuntimeObservedAt)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runner_project_observations
(organization_id, runner_id, project_id, observed_at, diagnostics_observed_at, runtime_observed_at, snapshot_digest)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (organization_id, runner_id, project_id) DO UPDATE SET
observed_at = excluded.observed_at, diagnostics_observed_at = excluded.diagnostics_observed_at,
runtime_observed_at = excluded.runtime_observed_at, snapshot_digest = excluded.snapshot_digest`,
		scope.organization, runner, scope.project, formatHubTime(view.ObservedAt), diagnostics, runtime, digest)
	if err != nil {
		return err
	}
	if previous != digest || boardVersion != 1 {
		return storeRunnerBoardProjection(ctx, tx, scope, runner, *view)
	}
	return nil
}

func overlayProjectConfigurationObservation(ctx context.Context, q nativeQueryer, scope nativeScope, runner string, view *runnerauth.ProjectConfiguration) error {
	var observed string
	var diagnostics, runtime sql.NullString
	err := q.QueryRowContext(ctx, "SELECT observed_at, diagnostics_observed_at, runtime_observed_at FROM runner_project_observations WHERE organization_id = ? AND runner_id = ? AND project_id = ?", scope.organization, runner, scope.project).Scan(&observed, &diagnostics, &runtime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	view.ObservedAt, err = parseTimeValue(observed)
	if err != nil {
		return err
	}
	if view.Diagnostics != nil && diagnostics.Valid {
		view.Diagnostics.ObservedAt, err = parseTimeValue(diagnostics.String)
		if err != nil {
			return err
		}
		view.Diagnostics.RuntimeObservedAt = time.Time{}
		if runtime.Valid {
			view.Diagnostics.RuntimeObservedAt, err = parseTimeValue(runtime.String)
		}
	}
	return err
}
