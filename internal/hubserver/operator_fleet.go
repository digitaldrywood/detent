package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errHubOperatorUnavailable = errors.New("operator command is unavailable")

type hubOperatorResolverKey struct{}
type hubFleetExecutor struct{ service *Service }
type hubFleetRequest = operatortool.HubFleetArguments

func hubFleetTool(name string) bool {
	if name == operatortool.AnalyticsDashboard || name == operatortool.TimeSeries || name == operatortool.Reports || name == operatortool.GetUrgentRunnerUpdate || name == operatortool.MarkUrgentRunnerUpdate {
		return true
	}
	return slices.Contains([]string{operatortool.UpdateApply, operatortool.GetRunnerUpdate, operatortool.InstanceHealth, operatortool.NativeCapabilities, operatortool.OutboxHealth, operatortool.CreateRunnerEnrollment, operatortool.RevokeRunnerEnrollment, operatortool.RevokeRunnerIdentity, operatortool.GetRunnerRouting, operatortool.ListRunnerRouting, operatortool.UpdateRunnerRouting, operatortool.UpdateRunnerHost, operatortool.GetRunnerCapacity, operatortool.UpdateRunnerCapacity, operatortool.HostedFleet, operatortool.GitHubRequestCounts}, name)
}
func hubFleetRequirement(name string) operatortool.Requirement {
	if name == operatortool.AnalyticsDashboard || name == operatortool.TimeSeries || name == operatortool.Reports || name == operatortool.HostedFleet || name == operatortool.InstanceHealth || name == operatortool.NativeCapabilities || name == operatortool.OutboxHealth {
		return operatortool.Requirement{Scope: apikey.ScopeRead}
	}
	return operatortool.Requirement{Scope: apikey.ScopeAdmin, ResourceKind: "runners"}
}
func currentHubOperator(ctx context.Context) (apiCredential, error) {
	resolve, ok := ctx.Value(hubOperatorResolverKey{}).(func(context.Context) (apiCredential, error))
	if !ok {
		return apiCredential{}, operatortool.ErrAccessDenied
	}
	return resolve(ctx)
}
func (e hubFleetExecutor) OpenConnection(ctx context.Context) error {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return err
	}
	return e.service.operatorChat.AttachConnection(ctx)
}
func (e hubFleetExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := e.service.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	result := []operatortool.Definition{}
	catalog, err := operatorCatalog(ctx)
	if err != nil {
		return nil, operatortool.ErrAccessDenied
	}
	credential := catalog.credential
	for _, d := range operatortool.CommandCatalog() {
		if !e.service.hubFleetReadPermitted(d.Name, credential) {
			continue
		}

		if d.Name == operatortool.ConnectionInfo || d.Name == operatortool.ActionResult {
			if e.service.config.Hosted != nil {
				result = append(result, d)
			}
			continue
		}
		if !hubFleetTool(d.Name) || d.Name == operatortool.GetRunnerRouting && e.service.config.Hosted != nil || d.Name == operatortool.HostedFleet && (credential.SessionHash == "" || e.service.config.Hosted == nil) || d.Name == operatortool.GitHubRequestCounts && e.service.config.Hosted != nil || !d.Annotations.ReadOnly && e.service.config.CredentialMaintenance {
			continue
		}
		if _, err := e.service.authorizeCatalog(ctx, hubFleetRequirement(d.Name)); err == nil {
			result = append(result, d)
		}
	}
	return result, nil
}
func hubOperatorResult(value any) (operatortool.Result, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	return operatortool.Result{Content: raw}, nil
}

type hubRunnerRouting struct {
	runnerauth.Runner
	Leases []hubRunnerLease `json:"leases"`
}

type hubRunnerLease struct {
	runnerauth.RunnerLease
	Policy hubRoutingPolicy `json:"policy"`
}

type hubRoutingPolicy struct {
	Schema         int                 `json:"schema"`
	ID             string              `json:"policy_id"`
	SourceRevision string              `json:"source_revision"`
	SourceDigest   string              `json:"source_digest"`
	ConfigDigest   string              `json:"config_digest"`
	Profile        string              `json:"profile,omitempty"`
	Requirements   policy.Requirements `json:"requirements"`
	Gates          policy.Gates        `json:"gates"`
}

func hubRunnerRoutingProjection(runner runnerauth.Runner) hubRunnerRouting {
	result := hubRunnerRouting{Runner: runner, Leases: make([]hubRunnerLease, len(runner.Leases))}
	for i, lease := range runner.Leases {
		pinned := lease.Policy
		result.Leases[i] = hubRunnerLease{RunnerLease: lease, Policy: hubRoutingPolicy{
			Schema: pinned.Schema, ID: pinned.ID, SourceRevision: pinned.SourceRevision,
			SourceDigest: pinned.SourceDigest, ConfigDigest: pinned.ConfigDigest, Profile: pinned.Profile,
			Requirements: pinned.Requirements, Gates: pinned.Gates,
		}}
	}
	return result
}

func hubFleetReceiptProjection(name string, raw json.RawMessage) (json.RawMessage, error) {
	if name != operatortool.UpdateRunnerRouting {
		return raw, nil
	}
	var runner runnerauth.Runner
	if err := json.Unmarshal(raw, &runner); err != nil {
		return nil, errHubOperatorUnavailable
	}
	projected, err := json.Marshal(hubRunnerRoutingProjection(runner))
	if err != nil {
		return nil, errHubOperatorUnavailable
	}
	return projected, nil
}

func (e hubFleetExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	s := e.service
	if call.Name == operatortool.ConnectionInfo {
		if operatortool.DecodeArguments(call.Arguments, &struct{}{}) != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if err := s.operatorChat.CheckConnection(ctx); err != nil {
			return operatortool.Result{}, err
		}
		c := s.operatorChat.Conversation(operatortool.CurrentConnection(ctx).ID)
		return hubOperatorResult(struct {
			ID           string `json:"connection_id"`
			Organization string `json:"organization_id"`
		}{c.ConnectionID, c.OrganizationID})
	}
	if call.Name == operatortool.ActionResult {
		var r operatortool.ActionResultArguments
		if operatortool.DecodeArguments(call.Arguments, &r) != nil || r.ActionID == "" || len(r.ActionID) > 256 {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
		if err := s.operatorChat.CheckConnection(ctx); err != nil {
			return operatortool.Result{}, err
		}
		a, ok := s.operatorChat.Action(operatortool.CurrentConnection(ctx).ID, r.ActionID)
		if !ok {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		if _, err := operatortool.AuthorizeCurrent(ctx, hubFleetRequirement(string(a.Kind))); err != nil {
			return operatortool.Result{}, err
		}
		return e.actionResult(ctx, a)
	}
	if !hubFleetTool(call.Name) {
		return operatortool.NewAuthorizedExecutor(nil).Execute(ctx, call)
	}
	if call.Name == operatortool.AnalyticsDashboard || call.Name == operatortool.TimeSeries || call.Name == operatortool.Reports {
		return s.executeAnalyticsRead(ctx, call)
	}
	if err := operatortool.ValidateFleetArguments(call.Name, call.Arguments); err != nil {
		return operatortool.Result{}, err
	}
	var r hubFleetRequest
	if operatortool.DecodeArguments(call.Arguments, &r) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, hubFleetRequirement(call.Name))
	if err != nil {
		return operatortool.Result{}, err
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if !s.hubFleetReadPermitted(call.Name, credential) {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if r.ProjectID != "" {
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID}); err != nil {
			return operatortool.Result{}, err
		}
	}

	d, _ := operatortool.FleetDefinition(call.Name)
	if d.Annotations.ReadOnly {
		if r.Limit == 0 {
			r.Limit = 100
		}
		var value any
		switch call.Name {
		case operatortool.InstanceHealth:
			value, _ = s.readInstanceHealth(ctx)
		case operatortool.NativeCapabilities:
			value, err = s.readNativeCapabilities(ctx)
		case operatortool.OutboxHealth:
			cursor, decodeErr := decodeTimelineCursor(r.Cursor)
			if decodeErr != nil {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
			value, err = s.readOutboxHealthPage(ctx, r.Limit, cursor)
		case operatortool.HostedFleet:
			if s.config.Hosted == nil || credential.SessionHash == "" {
				return operatortool.Result{}, operatortool.ErrAccessDenied
			}
			fleet, readErr := s.readHostedFleet(ctx, credential)
			err = readErr
			end := min(len(fleet.Runners), r.Offset+r.Limit)
			start := min(r.Offset, len(fleet.Runners))
			more := end < len(fleet.Runners)
			fleet.Runners = fleet.Runners[start:end]
			value = struct {
				Fleet      hostedFleetResponse `json:"fleet"`
				HasMore    bool                `json:"has_more"`
				ObservedAt time.Time           `json:"observed_at"`
			}{fleet, more, s.config.now()}
		case operatortool.GetRunnerUpdate:
			value, err = s.readRunnerUpdate(ctx, nativeScope{organization: tracker.OrganizationID(operatortool.ConnectionIdentity(ctx).OrganizationID), credential: credential}, r.RunnerID)
		case operatortool.GetUrgentRunnerUpdate:
			value, err = s.readUrgentRunnerUpdateCommand(ctx, nativeScope{organization: tracker.OrganizationID(operatortool.ConnectionIdentity(ctx).OrganizationID), credential: credential})
		case operatortool.GetRunnerCapacity:
			value, err = s.readRunnerCapacity(ctx, nativeScope{organization: tracker.OrganizationID(operatortool.ConnectionIdentity(ctx).OrganizationID), credential: credential}, r.RunnerID, r.Backend)
		case operatortool.GetRunnerRouting:
			var runner runnerauth.Runner
			runner, err = s.readRunnerRouting(ctx, nativeScope{organization: tracker.OrganizationID(operatortool.ConnectionIdentity(ctx).OrganizationID), credential: credential}, r.RunnerID)
			value = hubRunnerRoutingProjection(runner)
		case operatortool.ListRunnerRouting:
			runners, readErr := s.listRunnerRoutingData(ctx, tracker.OrganizationID(operatortool.ConnectionIdentity(ctx).OrganizationID), r.Limit+1, r.Offset)
			if readErr != nil {
				return operatortool.Result{}, errHubOperatorUnavailable
			}
			more := len(runners) > r.Limit
			if more {
				runners = runners[:r.Limit]
			}
			projected := make([]hubRunnerRouting, len(runners))
			for i, runner := range runners {
				projected[i] = hubRunnerRoutingProjection(runner)
			}
			value = struct {
				Runners    []hubRunnerRouting `json:"runners"`
				HasMore    bool               `json:"has_more"`
				ObservedAt time.Time          `json:"observed_at"`
			}{projected, more, s.config.now()}
		case operatortool.GitHubRequestCounts:
			if s.config.Hosted != nil || credential.NativeOnly || credential.Scope != apiScopeAdmin {
				return operatortool.Result{}, operatortool.ErrAccessDenied
			}
			counts := []GitHubRequestCount{}
			if s.config.GitHubRequestCounts != nil {
				counts = s.config.GitHubRequestCounts()
			}
			value = counts
		}
		if err != nil {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		resourceURL := "/fleet"
		switch call.Name {
		case operatortool.InstanceHealth:
			resourceURL = "/health"
		case operatortool.OutboxHealth:
			resourceURL = "/api/v1/outbox/health"
		case operatortool.NativeCapabilities:
			resourceURL = "/api/v2/capabilities"
		}
		return hubOperatorResult(struct {
			Data       any       `json:"data"`
			ObservedAt time.Time `json:"observed_at"`
			URL        string    `json:"url"`
		}{value, s.config.now(), resourceURL})
	}
	if s.config.CredentialMaintenance {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &fields) != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	delete(fields, "request_id")
	encoded, err := json.Marshal(fields)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	arguments := json.RawMessage(encoded)
	if prior, found, err := s.operatorChat.RetryResult(ctx, chatpkg.ActionKind(call.Name), r.RequestID, arguments); found || err != nil {
		if err != nil {
			return operatortool.Result{}, err
		}
		return e.actionResult(ctx, prior)
	}
	identity := operatortool.ConnectionIdentity(ctx)
	m := mutation.Metadata{PrincipalID: identity.PrincipalID, OrganizationID: identity.OrganizationID, ProjectID: r.ProjectID, Action: call.Name, Source: "mcp", Confirmation: "none", CorrelationID: newNativeID("mcp")}
	defer func() {
		s.config.Logger.InfoContext(ctx, "operator mutation request", "principal_id", m.PrincipalID, "organization_id", m.OrganizationID, "action", m.Action, "correlation_id", m.CorrelationID)
	}()
	m, err = m.Bind(r.RequestID, arguments)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	replayCtx := mutation.WithContext(ctx, m)
	if raw, found, replayErr := s.readHostedOperation(replayCtx, hostedCommand{actor: identity.PrincipalID, operation: "mcp " + call.Name, key: r.RequestID, input: arguments}); found || replayErr != nil {
		if replayErr != nil {
			return operatortool.Result{}, safeHubOperatorError(replayErr)
		}
		var outcome struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &outcome); err != nil {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		status := "succeeded"
		if outcome.Status == "rejected" {
			status = "rejected"
		} else {
			raw, err = hubFleetReceiptProjection(call.Name, raw)
			if err != nil {
				return operatortool.Result{}, err
			}
		}
		return hubOperatorResult(struct {
			Status        string          `json:"status"`
			CorrelationID string          `json:"correlation_id"`
			Receipt       json.RawMessage `json:"receipt"`
		}{status, m.CorrelationID, raw})
	}
	action, err := e.proposal(ctx, call.Name, arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	m.ResourceID = action.IssueID
	action.RequestID, action.Arguments, action.Mutation = r.RequestID, arguments, m
	action, err = s.operatorChat.Submit(ctx, action)
	if err != nil {
		return operatortool.Result{}, safeHubOperatorError(err)
	}
	return e.actionResult(ctx, action)
}
func safeHubOperatorError(err error) error {
	var problem *nativeError
	if errors.As(err, &problem) && problem.Code == "revision_conflict" {
		return mutation.ErrConflict
	}
	if errors.Is(err, mutation.ErrConflict) {
		return mutation.ErrConflict
	}
	if errors.Is(err, mutation.ErrUncertain) {
		return mutation.ErrUncertain
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return err
	}
	return errHubOperatorUnavailable
}
func (e hubFleetExecutor) proposal(ctx context.Context, name string, arguments json.RawMessage) (chatpkg.Action, error) {
	var r hubFleetRequest
	if operatortool.DecodeArguments(arguments, &r) != nil {
		return chatpkg.Action{}, operatortool.ErrInvalidArguments
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return chatpkg.Action{}, operatortool.ErrAccessDenied
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, hubFleetRequirement(name))
	if err != nil {
		return chatpkg.Action{}, err
	}
	org := operatortool.ConnectionIdentity(ctx).OrganizationID
	a := chatpkg.Action{Kind: chatpkg.ActionKind(name), ProjectID: r.ProjectID, Title: name, Description: string(arguments), ResourceURL: "/fleet"}
	switch name {
	case operatortool.MarkUrgentRunnerUpdate:
		var change urgentRunnerUpdateChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return a, operatortool.ErrInvalidArguments
		}
		current, err := e.service.readUrgentRunnerUpdateCommand(ctx, nativeScope{organization: tracker.OrganizationID(org), credential: credential})
		if err != nil {
			return a, safeHubOperatorError(err)
		}
		urgent, ok := current.(urgentRunnerUpdate)
		if !ok {
			return a, errHubOperatorUnavailable
		}
		if change.ExpectedRevision != urgent.Revision {
			return a, errHubOperatorUnavailable
		}
		a.IssueID = "urgent_runner_update"
		a.Identifier = "urgent_runner_update"
		a.CurrentState = strconv.FormatInt(urgent.Revision, 10)
		a.MaterialChange = true
	case operatortool.UpdateApply, operatortool.UpdateRunnerRouting, operatortool.UpdateRunnerCapacity, operatortool.RevokeRunnerIdentity:
		runner, err := readRunnerWithClock(ctx, e.service.database.db, tracker.OrganizationID(org), r.RunnerID, e.service.config.now)
		if err != nil {
			return a, errHubOperatorUnavailable
		}
		a.IssueID = r.RunnerID
		a.Identifier = r.RunnerID
		a.CurrentState = strconv.FormatInt(runner.Revision, 10)
		switch runner.ConnectionHealth {
		case "revoked", "expired":
			a.CurrentState += ":" + runner.ConnectionHealth
		}

		if name == operatortool.UpdateApply {
			var change runnerUpdateChange
			if r.RunnerID == "" || r.Release || r.FromRelease || operatortool.DecodeArguments(r.Change, &change) != nil || change.delivery("validate", e.service.config.now()).Validate() != nil {
				return a, operatortool.ErrInvalidArguments
			}
			if err := runnerUpdateReady(runner, change, e.service.config.now()); err != nil {
				return a, safeHubOperatorError(err)
			}
			a.MaterialChange = true
		}
		if name == operatortool.UpdateRunnerCapacity {
			var change runnerCapacityChange
			if operatortool.DecodeArguments(r.Change, &change) != nil || change.Validate() != nil || change.ExpectedRevision != runner.Revision {
				return a, errHubOperatorUnavailable
			}
			a.MaterialChange = true
		}
		if name == operatortool.UpdateRunnerRouting {
			var change runnerRoutingRequest
			if operatortool.DecodeArguments(r.Change, &change) != nil || change.ExpectedRevision != runner.Revision || change.effective(runner.Routing).Validate() != nil {
				return a, errHubOperatorUnavailable
			}
			a.MaterialChange = operatortool.RoutingRequiresApproval(runner.Routing, change.effective(runner.Routing).Routing) || runner.CapacityRequiresApplication(change.CapacityLimit)
		}
	case operatortool.UpdateRunnerHost:
		var change runnerauth.HostChange
		if operatortool.DecodeArguments(r.Change, &change) != nil || strings.TrimSpace(change.DisplayName) == "" || len(change.DisplayName) > 200 || strings.ContainsAny(change.DisplayName, "\r\n\x00") {
			return a, operatortool.ErrInvalidArguments
		}
		// Ownership and exact revision use the same host read backing runner views.
		var revision int64
		var capacity int
		if e.service.database.db.QueryRowContext(ctx, "SELECT routing_revision, capacity FROM machines WHERE organization_id=? AND id=?", org, r.MachineID).Scan(&revision, &capacity) != nil || revision != change.ExpectedRevision {
			return a, errHubOperatorUnavailable
		}
		a.IssueID = r.MachineID
		a.Identifier = r.MachineID
		a.CurrentState = strconv.FormatInt(revision, 10)
		a.MaterialChange = capacity != change.Capacity
	case operatortool.RevokeRunnerEnrollment:
		var expires string
		if e.service.database.db.QueryRowContext(ctx, "SELECT expires_at FROM runner_enrollments WHERE organization_id=? AND id=? AND revoked_at IS NULL AND redeemed_at IS NULL", org, r.EnrollmentID).Scan(&expires) != nil {
			return a, errHubOperatorUnavailable
		}
		a.IssueID = r.EnrollmentID
		a.Identifier = r.EnrollmentID
		a.CurrentState = expires
	case operatortool.CreateRunnerEnrollment:
		if !r.Enrollment.Valid() && !r.Enrollment.Unbound() || !runnerauth.ValidOperations(r.Enrollment.Operations) {
			return a, operatortool.ErrInvalidArguments
		}
	}
	return a, nil
}
func (e hubFleetExecutor) ExecuteAction(ctx context.Context, a chatpkg.Action) (chatpkg.ActionExecution, error) {
	ctx, err := operatortool.AuthorizeCurrent(ctx, hubFleetRequirement(string(a.Kind)))
	if err != nil {
		return chatpkg.ActionExecution{}, err
	}
	identity := operatortool.ConnectionIdentity(ctx)
	bound, err := a.Mutation.Bind(a.RequestID, a.Arguments)
	if err != nil || a.Mutation.PrincipalID != identity.PrincipalID || a.Mutation.OrganizationID != identity.OrganizationID || a.Mutation.Action != string(a.Kind) || bound.InputHash != a.Mutation.InputHash || bound.RetryIdentity != a.Mutation.RetryIdentity {
		return chatpkg.ActionExecution{}, operatortool.ErrAccessDenied
	}
	ctx = mutation.WithContext(ctx, a.Mutation)
	command := hostedCommand{actor: identity.PrincipalID, operation: "mcp " + string(a.Kind), key: a.RequestID, input: a.Arguments}
	claimed, replay, err := e.service.claimHostedOperation(ctx, command)
	if err != nil {
		return chatpkg.ActionExecution{}, safeHubOperatorError(err)
	}
	if claimed {
		current, err := e.proposal(ctx, string(a.Kind), a.Arguments)
		if err != nil {
			return chatpkg.ActionExecution{}, err
		}
		if current.IssueID != a.IssueID || current.CurrentState != a.CurrentState || current.MaterialChange != a.MaterialChange {
			return chatpkg.ActionExecution{}, errHubOperatorUnavailable
		}
		value, err := e.executeCommand(ctx, a)
		if err != nil {
			return chatpkg.ActionExecution{}, safeHubOperatorError(err)
		}
		completed, err := e.service.completeHostedOperation(ctx, command, value)
		if err != nil {
			return chatpkg.ActionExecution{}, errHubOperatorUnavailable
		}
		replay = completed
	}
	if string(a.Kind) == operatortool.CreateRunnerEnrollment {
		var receipt runnerauth.Enrollment
		if json.Unmarshal(replay, &receipt) != nil || receipt.ID == "" {
			return chatpkg.ActionExecution{}, errHubOperatorUnavailable
		}
		a.IssueID, a.Identifier = receipt.ID, receipt.ID
	}
	return chatpkg.ActionExecution{Message: "Action completed.", ResourceID: a.IssueID, Identifier: a.Identifier, URL: a.ResourceURL}, nil
}
func (e hubFleetExecutor) executeCommand(ctx context.Context, a chatpkg.Action) (any, error) {
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return nil, err
	}
	scope := nativeScope{organization: tracker.OrganizationID(a.OrganizationID), credential: credential}
	var r hubFleetRequest
	if operatortool.DecodeArguments(a.Arguments, &r) != nil {
		return nil, operatortool.ErrInvalidArguments
	}
	s := e.service
	switch string(a.Kind) {
	case operatortool.MarkUrgentRunnerUpdate:
		var change urgentRunnerUpdateChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		change.Confirm = true
		change.IdempotencyKey = a.RequestID
		return s.markUrgentRunnerUpdateCommand(ctx, scope, change)
	case operatortool.CreateRunnerEnrollment:
		return s.createRunnerEnrollmentCommand(ctx, scope, r.Enrollment)
	case operatortool.RevokeRunnerEnrollment:
		return s.revokeRunnerEnrollmentCommand(ctx, scope, r.EnrollmentID)
	case operatortool.RevokeRunnerIdentity:
		return s.revokeRunnerIdentityCommand(ctx, scope, r.RunnerID)
	case operatortool.UpdateApply:
		var change runnerUpdateChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		change.Confirm = true
		change.IdempotencyKey = a.RequestID
		return s.applyRunnerUpdateCommand(ctx, scope, r.RunnerID, change)
	case operatortool.UpdateRunnerCapacity:
		var change runnerCapacityChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		change.IdempotencyKey = a.RequestID
		return s.updateRunnerCapacityCommand(ctx, scope, r.RunnerID, change)
	case operatortool.UpdateRunnerRouting:
		var change runnerRoutingRequest
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		return s.updateRunnerRoutingCommand(ctx, scope, r.RunnerID, change)
	case operatortool.UpdateRunnerHost:
		var change runnerauth.HostChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return nil, operatortool.ErrInvalidArguments
		}
		return s.updateRunnerHostCommand(ctx, scope, r.MachineID, change)
	}
	return nil, operatortool.ErrUnknownTool
}
func (e hubFleetExecutor) actionResult(ctx context.Context, a chatpkg.Action) (operatortool.Result, error) {
	var receipt json.RawMessage
	if a.Status == chatpkg.ActionSucceeded {
		ctx = mutation.WithContext(ctx, a.Mutation)
		// Read/replay through the existing application receipt contract. Tokens are
		// returned only here to the original authenticated connection, never in the
		// browser preview, audit log, or another operator's conversation.
		raw, found, err := e.service.readHostedOperation(ctx, hostedCommand{actor: a.Mutation.PrincipalID, operation: "mcp " + string(a.Kind), key: a.RequestID, input: a.Arguments})
		if err != nil {
			return operatortool.Result{}, safeHubOperatorError(err)
		}
		if !found {
			return operatortool.Result{}, errHubOperatorUnavailable
		}
		receipt, err = hubFleetReceiptProjection(string(a.Kind), raw)
		if err != nil {
			return operatortool.Result{}, err
		}
	}
	return hubOperatorResult(struct {
		Action     chatpkg.Action       `json:"preview"`
		ID         string               `json:"action_id"`
		Status     chatpkg.ActionStatus `json:"status"`
		ResultTool string               `json:"result_tool"`
		Receipt    json.RawMessage      `json:"receipt,omitempty"`
	}{a, a.ID, a.Status, operatortool.ActionResult, receipt})
}
func (e hubFleetExecutor) AuditAction(ctx context.Context, a chatpkg.Action, outcome string) {
	m := a.Mutation
	m.ResourceID = a.IssueID
	m.RetryIdentity = ""
	m.InputHash = ""
	if outcome == "approved" || outcome == "rejected" {
		m.Confirmation = outcome
	}
	e.service.config.Logger.InfoContext(ctx, "operator mutation", "audit", m, "outcome", outcome)
	if outcome == "rejected" {
		persistCtx, cancel := context.WithTimeout(mutation.WithContext(context.WithoutCancel(ctx), a.Mutation), 2*time.Second)
		defer cancel()
		command := hostedCommand{actor: a.Mutation.PrincipalID, operation: "mcp " + string(a.Kind), key: a.RequestID, input: a.Arguments}
		claimed, _, err := e.service.claimHostedOperation(persistCtx, command)
		if err == nil && claimed {
			_, err = e.service.completeHostedOperation(persistCtx, command, struct {
				Status string `json:"status"`
			}{"rejected"})
		}
		if err != nil {
			e.service.config.Logger.WarnContext(ctx, "operator rejection receipt unavailable")
		}
	}
	if e.service.config.Hosted != nil {
		credential, err := currentHubOperator(ctx)
		if err != nil {
			return
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := e.service.hostedAudit(mutation.WithContext(auditCtx, m), credential.Hosted, "action", "mcp "+string(a.Kind)+" "+outcome, a.ProjectID, 0); err != nil {
			e.service.config.Logger.WarnContext(ctx, "operator mutation audit unavailable")
		}
	}
}

// Keep these reads on the same deployment and credential boundary as the
// dashboard routes. Hosted browsers cannot access the legacy instance APIs.
func (s *Service) hubFleetReadPermitted(name string, credential apiCredential) bool {
	switch name {
	case operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports:
		return s.config.Hosted != nil && s.database != nil
	case operatortool.InstanceHealth, operatortool.NativeCapabilities:
		return s.config.Hosted == nil
	case operatortool.OutboxHealth:
		return s.config.Hosted == nil && !credential.NativeOnly
	default:
		return true
	}
}
