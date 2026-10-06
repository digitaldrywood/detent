package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errProjectServiceUnavailable = errors.New("project operation is unavailable")

type hubProjectExecutor struct{ service *Service }

func (e hubProjectExecutor) OpenConnection(ctx context.Context) error {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return err
	}
	return e.service.operatorChat.AttachConnection(ctx)
}
func projectToolScope(name string, read bool) apikey.Scope {
	if read {
		return apikey.ScopeRead
	}
	switch name {
	case "apply_local_project_policy", "drain_local_project", "detach_local_project", "command_git_hub_batch", "create_native_project", "create_hosted_project", "update_project_integration", "bind_native_repository", "cutover_project", "approve_project_policy", "revoke_project_policy", "remove_project_secret", "import_sprite_usage":
		return apikey.ScopeAdmin
	}
	return apikey.ScopeWrite
}
func (e hubProjectExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := e.service.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	defs := []operatortool.Definition{}
	for _, d := range append(operatortool.ProjectCatalog(), operatortool.LocalProjectCatalog()...) {
		if operatortool.IsModelSelection(d.Name) && !e.modelSelectionCatalogAllowed(ctx, d) {
			continue
		}
		if d.Name == "project_settings" || d.Name == "demo_setup_scenarios" {
			continue
		}
		if e.service.config.Hosted == nil && d.Name == "create_hosted_project" || e.service.config.Hosted != nil && d.Name == "create_native_project" {
			continue
		}
		requirement := operatortool.Requirement{Scope: projectToolScope(d.Name, d.Annotations.ReadOnly)}
		if requirement.Scope == apikey.ScopeWrite {
			requirement.ResourceKind = "project"
		}
		if _, err := e.service.authorizeCatalog(ctx, requirement); err == nil {
			defs = append(defs, d)
		}
	}
	for _, d := range operatortool.CommandCatalog() {
		if d.Name == operatortool.ActionResult || d.Name == operatortool.ConnectionInfo {
			defs = append(defs, d)
		}
	}
	return defs, nil
}
func (e hubProjectExecutor) Execute(ctx context.Context, call operatortool.Call) (result operatortool.Result, resultErr error) {
	d, known := operatortool.Lookup(call.Name)
	if !known {
		return result, operatortool.ErrUnknownTool
	}
	if call.Name == operatortool.ConnectionInfo || call.Name == operatortool.ActionResult {
		return e.connectionRead(ctx, call)
	}
	if operatortool.IsLocalProjectTool(call.Name) {
		r, err := operatortool.DecodeLocalProjectArguments(call.Name, call.Arguments, true)
		if err != nil {
			return result, err
		}
		scope := apikey.ScopeAdmin
		if d.Annotations.ReadOnly {
			scope = apikey.ScopeRead
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope, ProjectID: r.ProjectID}); err != nil {
			return result, err
		}
		if !d.Annotations.ReadOnly {
			connection := operatortool.CurrentConnection(ctx)
			metadata, err := (mutation.Metadata{PrincipalID: connection.Identity.PrincipalID, OrganizationID: connection.Identity.OrganizationID, ProjectID: r.ProjectID, Action: call.Name, Source: "mcp", CorrelationID: newNativeID("mcp"), Confirmation: "none"}).Bind(r.RequestID, call.Arguments)
			if err != nil {
				return result, operatortool.ErrInvalidArguments
			}
			defer func() {
				outcome := "submitted"
				if resultErr != nil {
					outcome = "failed"
				}
				e.AuditAction(ctx, chatpkg.Action{Mutation: metadata}, outcome)
			}()
		}
		return e.localProjectConfiguration(ctx, call.Name, r)
	}
	if d.Meta.Toolset != "projects" {
		return operatortool.NewAuthorizedExecutor(nil).Execute(ctx, call)
	}
	if call.Name == "monthly_usage_costs" {
		return e.monthlyUsageCosts(ctx, call.Arguments)
	}
	if d.Annotations.ReadOnly {
		return e.read(ctx, call)
	}
	connection := operatortool.CurrentConnection(ctx)
	m := mutation.Metadata{PrincipalID: connection.Identity.PrincipalID, OrganizationID: connection.Identity.OrganizationID, Action: call.Name, Source: "mcp", CorrelationID: newNativeID("mcp"), Confirmation: "none"}
	defer func() {
		outcome := "submitted"
		if resultErr != nil {
			outcome = "failed"
		}
		if errors.Is(resultErr, operatortool.ErrAccessDenied) {
			outcome = "denied"
		}
		e.AuditAction(ctx, chatpkg.Action{Mutation: m}, outcome)
	}()
	var selector struct {
		ProjectID string          `json:"project_id"`
		RequestID string          `json:"request_id"`
		Input     json.RawMessage `json:"input"`
	}
	if err := operatortool.DecodeProjectArguments(call.Arguments, &selector); err != nil {
		return result, err
	}
	if strings.TrimSpace(selector.RequestID) == "" {
		return result, &operatortool.RequestError{Code: "invalid_request", Message: "request_id: a nonempty business retry key is required"}
	}
	if len(selector.Input) == 0 || selector.Input[0] != '{' {
		return result, &operatortool.RequestError{Code: "invalid_request", Message: "input: must be an object"}
	}
	if strings.TrimSpace(selector.ProjectID) == "" && call.Name != "create_native_project" && call.Name != "create_hosted_project" && !operatortool.IsOrganizationModelSelection(call.Name) {
		return result, &operatortool.RequestError{Code: "invalid_request", Message: "project_id: is required"}
	}
	if (call.Name == "create_native_project" || call.Name == "create_hosted_project") && selector.ProjectID != "" {
		return result, &operatortool.RequestError{Code: "invalid_request", Message: "project_id: must be omitted when creating a project"}
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: projectToolScope(call.Name, false), ProjectID: selector.ProjectID})
	if err != nil {
		return result, err
	}
	// Decode the concrete command before it can appear in an approval preview.
	if _, err := e.command(ctx, call, projectCommandPreview); err != nil {
		return result, projectToolError(err)
	}
	if err := e.service.operatorChat.CheckConnection(ctx); err != nil {
		return result, err
	}
	if previous, found, err := e.service.operatorChat.RetryResult(ctx, chatpkg.ActionKind(call.Name), selector.RequestID, call.Arguments); err != nil {
		return result, err
	} else if found {
		if err := e.authorizeCreatedProjectResult(ctx, string(previous.Kind), json.RawMessage(previous.Result), previous.Status); err != nil {
			return result, err
		}
		return e.actionResult(previous)
	}
	m.ProjectID = selector.ProjectID
	m, err = m.Bind(selector.RequestID, call.Arguments)
	if err != nil {
		return result, operatortool.ErrInvalidArguments
	}
	if replay, err := e.command(ctx, call, projectCommandReplay); err != nil {
		return result, projectToolError(err)
	} else if replay != nil {
		if err := e.authorizeCreatedProjectResult(ctx, call.Name, replay, chatpkg.ActionSucceeded); err != nil {
			return result, err
		}
		replay, err = safeProjectCommandResult(call.Name, replay)
		if err != nil {
			return result, err
		}
		return e.actionResult(chatpkg.Action{Kind: chatpkg.ActionKind(call.Name), OrganizationID: connection.Identity.OrganizationID, ProjectID: selector.ProjectID, RequestID: selector.RequestID, Status: chatpkg.ActionSucceeded, Result: string(replay)})
	}
	action, err := e.service.operatorChat.Submit(ctx, chatpkg.Action{Kind: chatpkg.ActionKind(call.Name), ProjectID: selector.ProjectID, RequestID: selector.RequestID, Arguments: call.Arguments, Title: strings.ReplaceAll(call.Name, "_", " "), Mutation: m})
	if err != nil {
		return result, projectToolError(err)
	}
	return e.actionResult(action)
}
func (e hubProjectExecutor) actionResult(action chatpkg.Action) (operatortool.Result, error) {
	if action.Kind == "approve_project_policy" {
		action.Arguments = nil
		if action.Status == chatpkg.ActionSucceeded {
			raw, err := safeProjectCommandResult(string(action.Kind), json.RawMessage(action.Result))
			if err != nil {
				return operatortool.Result{}, err
			}
			action.Result = string(raw)
		}
	}
	return hubProjectResult(struct {
		chatpkg.Action
		ResultTool string `json:"result_tool"`
	}{action, operatortool.ActionResult})
}

func hubProjectResult(value any) (operatortool.Result, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return operatortool.Result{}, errProjectServiceUnavailable
	}
	return operatortool.Result{Content: raw}, nil
}
func projectToolError(err error) error {
	if errors.Is(err, operatortool.ErrAccessDenied) || errors.Is(err, operatortool.ErrInvalidArguments) || errors.Is(err, mutation.ErrConflict) {
		return err
	}
	var failure *nativeError
	if errors.As(err, &failure) && (failure.status == 401 || failure.status == 403 || failure.status == 404) {
		return operatortool.ErrAccessDenied
	}
	if errors.As(err, &failure) && failure.Code == "invalid_request" && failure.publicMessage {
		return &operatortool.RequestError{Code: failure.Code, Message: failure.Message}
	}
	if errors.As(err, &failure) && failure.Code == "policy_mismatch" {
		return &operatortool.ConflictError{Code: failure.Code, Message: failure.Message}
	}
	if errors.As(err, &failure) && (failure.Code == "revision_conflict" || failure.Code == "idempotency_conflict") {
		return mutation.ErrConflict
	}
	return err
}
func (e hubProjectExecutor) ExecuteAction(ctx context.Context, action chatpkg.Action) (chatpkg.ActionExecution, error) {
	ctx, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: projectToolScope(string(action.Kind), false), OrganizationID: action.OrganizationID, ProjectID: action.ProjectID})
	if err != nil {
		return chatpkg.ActionExecution{}, err
	}
	ctx = mutation.WithContext(ctx, action.Mutation)
	result, err := e.command(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments}, projectCommandExecute)
	if err != nil {
		return chatpkg.ActionExecution{}, projectToolError(err)
	}
	if err := e.authorizeCreatedProjectResult(ctx, string(action.Kind), result, chatpkg.ActionSucceeded); err != nil {
		return chatpkg.ActionExecution{}, err
	}
	result, err = safeProjectCommandResult(string(action.Kind), result)
	if err != nil {
		return chatpkg.ActionExecution{}, err
	}
	return chatpkg.ActionExecution{Message: string(result)}, nil
}
func (e hubProjectExecutor) AuditAction(ctx context.Context, action chatpkg.Action, outcome string) {
	m := action.Mutation
	m.RetryIdentity, m.InputHash = "", ""
	if outcome == "approved" || outcome == "rejected" {
		m.Confirmation = outcome
	}
	raw, _ := json.Marshal(struct { //nolint:errcheck // Audit metadata and outcome contain only strings.
		mutation.Metadata
		Outcome string `json:"outcome"`
	}{m, outcome})
	e.service.config.Logger.InfoContext(ctx, "operator mutation", "audit", string(raw))
}
func (e hubProjectExecutor) connectionRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var request struct {
		ActionID string `json:"action_id,omitempty"`
	}
	if operatortool.DecodeProjectArguments(call.Arguments, &request) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return operatortool.Result{}, err
	}
	if err := e.service.operatorChat.CheckConnection(ctx); err != nil {
		return operatortool.Result{}, err
	}
	c := operatortool.CurrentConnection(ctx)
	if call.Name == operatortool.ConnectionInfo {
		if request.ActionID != "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		return hubProjectResult(struct {
			ConnectionID string `json:"connection_id"`
		}{c.ID})
	}
	if request.ActionID == "" {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	action, ok := e.service.operatorChat.Action(c.ID, request.ActionID)
	if !ok {
		return operatortool.Result{}, errProjectServiceUnavailable
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: projectToolScope(string(action.Kind), false), ProjectID: action.ProjectID}); err != nil {
		return operatortool.Result{}, err
	}
	if operatortool.IsModelSelection(string(action.Kind)) {
		if _, err := e.modelSelectionCommand(ctx, operatortool.Call{Name: string(action.Kind), Arguments: action.Arguments}, projectCommandPreview); err != nil {
			return operatortool.Result{}, err
		}
	}
	if err := e.authorizeCreatedProjectResult(ctx, string(action.Kind), json.RawMessage(action.Result), action.Status); err != nil {
		return operatortool.Result{}, err
	}
	return e.actionResult(action)
}

func projectCommandInput[T any](raw json.RawMessage) (operatortool.ProjectRequest[T], error) {
	var r operatortool.ProjectRequest[T]
	err := operatortool.DecodeProjectArguments(raw, &r)
	return r, err
}

type projectCommandMode int

const (
	projectCommandPreview projectCommandMode = iota
	projectCommandReplay
	projectCommandExecute
)

func (e hubProjectExecutor) command(ctx context.Context, call operatortool.Call, mode projectCommandMode) (json.RawMessage, error) {
	if operatortool.IsModelSelection(call.Name) {
		return e.modelSelectionCommand(ctx, call, mode)
	}
	s := e.service
	scope, err := e.projectScope(ctx)
	if err != nil {
		return nil, err
	}
	var operation func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)
	var input any
	var projectID, requestID, resourceID string
	feature := "collaboration"
	switch call.Name {
	case "import_sprite_usage":
		r, err := projectCommandInput[operatortool.SpriteUsageInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		if !canManageProjectSecrets(scope.credential) {
			return nil, operatortool.ErrAccessDenied
		}
		request := spriteUsageRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, Observations: r.Input.Observations}
		if err := validateSpriteUsageRequest(&request, s.config.now()); err != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		if mode == projectCommandPreview {
			return nil, nil
		}
		scope.project, scope.requireHostedAdmin = tracker.ProjectID(r.ProjectID), true
		if mode == projectCommandReplay {
			raw, _, err := s.nativeCommandReplay(ctx, scope, "sprite_usage.import", r.RequestID, request)
			return raw, err
		}
		return s.importSpriteUsageCommand(ctx, scope, request)
	case "create_native_project":
		if s.config.Hosted != nil {
			return nil, errProjectServiceUnavailable
		}
		r, err := projectCommandInput[operatortool.ProjectCreateInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		if strings.TrimSpace(r.Input.Name) == "" || len(r.Input.Name) > 200 || validateNativeStates(r.Input.States) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		request := createNativeProjectRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, Name: r.Input.Name, States: r.Input.States, RequireDependencies: r.Input.RequireDependencies}
		input = request
		operation = s.createNativeProjectOperation(request)
	case "create_hosted_project":
		if s.config.Hosted == nil || scope.credential.Hosted == nil {
			return nil, errProjectServiceUnavailable
		}
		r, err := projectCommandInput[operatortool.HostedProjectCreateInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		if !r.Input.GrantAccess || strings.TrimSpace(r.Input.Name) == "" || len(r.Input.Name) > 120 {
			return nil, operatortool.ErrInvalidArguments
		}
		input = r.Input
		operation = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			var states []tracker.NativeState
			if r.Input.States != nil {
				states = *r.Input.States
			}
			id, err := s.createHostedProjectInTx(ctx, tx, scope.credential, strings.TrimSpace(r.Input.Name), states)
			if err != nil {
				return nil, err
			}
			scope.project = tracker.ProjectID(id)
			return readNativeProject(ctx, tx, scope)
		}
	case "save_onboarding":
		r, err := projectCommandInput[operatortool.OnboardingInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		request := saveOnboardingRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, Progress: r.Input.Progress}
		if request.Progress.Validate() != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		input = request
		operation = s.saveOnboardingOperation(request)
	case "update_project_integration":
		r, err := projectCommandInput[operatortool.IntegrationInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		feature = "github_integration"
		request := updateProjectIntegrationRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, ExpectedRevision: r.Input.ExpectedRevision, Intake: r.Input.Intake, Projection: r.Input.Projection, RepositoryEnabled: r.Input.RepositoryEnabled, States: r.Input.States, WorkflowMarkdown: r.Input.WorkflowMarkdown}
		input = request
		operation = s.updateProjectIntegrationOperation(request)
	case "start_git_hub_import":
		r, err := projectCommandInput[operatortool.ImportStartInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		feature = "github_integration"
		request := startGitHubImportRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, IssueNumber: r.Input.IssueNumber, Restart: r.Input.Restart, ExpectedRevision: r.Input.ExpectedRevision}
		input = request
		operation = s.startGitHubImportOperation(request)
	case "cutover_project":
		r, err := projectCommandInput[operatortool.CutoverInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		feature = "github_integration"
		request := CutoverRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, ClosedState: r.Input.ClosedState, DryRun: r.Input.DryRun, Checkpoint: r.Input.Checkpoint, AcceptPartial: r.Input.AcceptPartial, CloseSource: r.Input.CloseSource, DestinationURL: r.Input.DestinationURL, States: r.Input.States, InitialState: r.Input.InitialState}
		input = request
		operation = s.cutoverProjectOperation(request)
	case "command_git_hub_batch":
		r, err := projectCommandInput[operatortool.GitHubBatchInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		if r.Input.Action != "discover" && r.Input.Action != "more" && r.Input.Action != "apply" && r.Input.Action != "retry" {
			return nil, operatortool.ErrInvalidArguments
		}
		projectID, requestID = r.ProjectID, r.RequestID
		request := tracker.GitHubBatchCommand{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, Revision: r.Input.Revision, Action: r.Input.Action, RunnerID: r.Input.RunnerID, Labels: r.Input.Labels, IncludeClosed: r.Input.IncludeClosed, Numbers: r.Input.Numbers, Destination: r.Input.Destination, AllowDispatch: r.Input.AllowDispatch}
		input = request
		operation = s.commandGitHubBatchOperation(request)
	case "approve_project_policy":
		r, err := projectCommandInput[operatortool.PolicyApprovalInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		if r.Input.Policy.Validate() != nil {
			return nil, &operatortool.RequestError{Code: "invalid_request", Message: "Policy descriptor is invalid or its identity digest does not match"}
		}
		if workflowconfig.ValidateSharedPolicy(r.Input.Policy) != nil {
			return nil, &operatortool.RequestError{Code: "invalid_request", Message: "Policy configuration is invalid or its digest does not match the descriptor"}
		}
		input = r.Input
		operation = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			policyScope, err := operatorPolicyScope(ctx, tx, scope, r.Input.RepositoryPolicy)
			if err != nil {
				return nil, err
			}
			return s.database.approvePolicyInTx(ctx, tx, policyScope, scope.credential.ID, policy.Change{ExpectedID: r.Input.ExpectedID, Policy: r.Input.Policy})
		}
	case "revoke_project_policy":
		r, err := projectCommandInput[operatortool.PolicyRevokeInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		input = r.Input
		operation = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			policyScope, err := operatorPolicyScope(ctx, tx, scope, r.Input.RepositoryPolicy)
			if err != nil {
				return nil, err
			}
			return revokeProjectPolicyInTx(ctx, tx, policyScope, r.Input.ExpectedID)
		}
	case "remove_project_secret":
		r, err := projectCommandInput[struct{}](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID = r.ProjectID, r.RequestID
		input = r.Input
		if !canManageProjectSecrets(scope.credential) {
			return nil, operatortool.ErrAccessDenied
		}
		operation = func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
			err := removeProjectSecretInTx(ctx, tx, scope, now)
			return projectSecretStatus{Kind: flySpritesToken}, err
		}
	case "project_native_summary":
		r, err := projectCommandInput[operatortool.SummaryInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		projectID, requestID, resourceID = r.ProjectID, r.RequestID, r.Input.ItemID
		request := projectNativeSummaryRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, Body: r.Input.Body}
		input = request
		operation = s.projectNativeSummaryOperation(request, resourceID)
	case "bind_native_repository":
		r, err := projectCommandInput[operatortool.RepositoryInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		if !validCheckoutRepository(r.Input.Repository) {
			return nil, operatortool.ErrInvalidArguments
		}
		if r.Input.Source != "" && r.Input.Source != "runner_checkout" {
			return nil, operatortool.ErrInvalidArguments
		}
		if mode == projectCommandPreview {
			return nil, nil
		}
		scope.project = tracker.ProjectID(r.ProjectID)
		scope.requireHostedAdmin = true
		request := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: r.RequestID}, ExpectedRevision: r.Input.ExpectedRevision, Repository: r.Input.Repository, Source: r.Input.Source}
		suffix, feature := "/integration/repository", "github_integration"
		if r.Input.Source == "runner_checkout" {
			suffix, feature = "/onboarding/repository", "collaboration"
		}
		options := nativeCommandOptions{OperationID: projectOperationID(scope, "POST", suffix), Feature: feature}
		if mode == projectCommandReplay {
			raw, _, err := s.nativeCommandReplay(ctx, scope, options.OperationID, r.RequestID, request)
			return raw, err
		}
		return s.bindNativeRepositoryCommand(ctx, scope, options, request)
	case "advance_git_hub_import":
		r, err := projectCommandInput[operatortool.ImportAdvanceInput](call.Arguments)
		if err != nil {
			return nil, err
		}
		if r.Input.ImportID == "" {
			return nil, operatortool.ErrInvalidArguments
		}
		if mode == projectCommandPreview {
			return nil, nil
		}
		scope.project = tracker.ProjectID(r.ProjectID)
		if mode == projectCommandReplay {
			input := struct {
				ExpectedRevision tracker.Revision `json:"expected_revision,string"`
			}{r.Input.ExpectedRevision}
			raw, _, err := s.nativeCommandReplay(ctx, scope, projectOperationID(scope, "POST", "/imports/"+r.Input.ImportID+"/advance"), r.RequestID, input)
			return raw, err
		}
		return s.advanceGitHubImportCommand(ctx, scope, r.Input.ImportID, r.Input.ExpectedRevision, r.RequestID)
	default:
		return nil, errProjectServiceUnavailable
	}
	if mode == projectCommandPreview {
		return nil, nil
	}
	scope.project = tracker.ProjectID(projectID)
	scope.requireHostedAdmin = projectToolScope(call.Name, false) == apikey.ScopeAdmin
	suffix := map[string]string{"command_git_hub_batch": "/onboarding/issue-intake", "create_native_project": "", "create_hosted_project": "", "save_onboarding": "/onboarding", "update_project_integration": "/integration", "start_git_hub_import": "/imports", "cutover_project": "/integration/cutover", "approve_project_policy": "/policy", "revoke_project_policy": "/policy", "remove_project_secret": "/secrets/" + flySpritesToken, "project_native_summary": "/work-items/" + resourceID + "/projection"}[call.Name]
	method := "POST"
	switch call.Name {
	case "save_onboarding", "update_project_integration", "approve_project_policy":
		method = "PUT"
	case "revoke_project_policy", "remove_project_secret":
		method = "DELETE"
	}
	if mode == projectCommandReplay {
		raw, _, err := s.nativeCommandReplay(ctx, scope, projectOperationID(scope, method, suffix), requestID, input)
		return raw, err
	}
	return s.executeNativeMutation(ctx, scope, nativeCommandOptions{OperationID: projectOperationID(scope, method, suffix), Feature: feature}, tracker.Mutation{IdempotencyKey: requestID}, input, operation)
}
func projectOperationID(scope nativeScope, method, suffix string) string {
	path := "/api/v2/organizations/" + string(scope.organization) + "/projects"
	if scope.project != "" {
		path += "/" + string(scope.project)
	}
	return method + " " + path + suffix
}

func safeProjectCommandResult(name string, raw json.RawMessage) (json.RawMessage, error) {
	if name == "approve_project_policy" {
		var approval policy.Approval
		if json.Unmarshal(raw, &approval) != nil {
			return nil, errProjectServiceUnavailable
		}
		approval.History, approval.HistoryNext = nil, ""
		return json.Marshal(approval)
	}
	if name == "command_git_hub_batch" {
		var view githubBatchView
		if json.Unmarshal(raw, &view) != nil {
			return nil, errProjectServiceUnavailable
		}
		page, err := boundedProjectBatch(view, operatortool.ProjectReadRequest{Limit: 200})
		if err != nil {
			return nil, err
		}
		return json.Marshal(page)
	}
	if name != "advance_git_hub_import" && name != "start_git_hub_import" {
		return raw, nil
	}
	var job GitHubImport
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, errProjectServiceUnavailable
	}
	if job.LastError != "" {
		job.LastError = "Import transport is unavailable"
	}
	return json.Marshal(job)
}

func (e hubProjectExecutor) authorizeCreatedProjectResult(ctx context.Context, name string, raw json.RawMessage, status chatpkg.ActionStatus) error {
	if status != chatpkg.ActionSucceeded || name != "create_native_project" && name != "create_hosted_project" {
		return nil
	}
	var project tracker.NativeProject
	if json.Unmarshal(raw, &project) != nil || project.ID == "" {
		return errProjectServiceUnavailable
	}
	_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(project.ID)})
	return err
}

func (e hubProjectExecutor) projectScope(ctx context.Context) (nativeScope, error) {
	resolve, ok := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
	if !ok {
		return nativeScope{}, operatortool.ErrAccessDenied
	}
	scope, err := resolve(ctx)
	if err != nil {
		return nativeScope{}, operatortool.ErrAccessDenied
	}
	return scope, nil
}
