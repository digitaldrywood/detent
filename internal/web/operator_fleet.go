package web

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/aidebug"
	"github.com/digitaldrywood/detent/internal/apikey"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type fleetRequest struct {
	ProjectID   string          `json:"project_id,omitempty"`
	RequestID   string          `json:"request_id,omitempty"`
	Reference   string          `json:"reference,omitempty"`
	RunnerID    string          `json:"runner_id,omitempty"`
	MachineID   string          `json:"machine_id,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	Recovery    string          `json:"recovery,omitempty"`
	Host        string          `json:"host,omitempty"`
	Since       string          `json:"since,omitempty"`
	Limit       int             `json:"limit,omitempty"`
	Offset      int             `json:"offset,omitempty"`
	Release     bool            `json:"release,omitempty"`
	FromRelease bool            `json:"from_release,omitempty"`
	WarningIDs  []string        `json:"warning_ids,omitempty"`
	AttemptID   int64           `json:"attempt_id,omitempty"`
	Action      string          `json:"action,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Change      json.RawMessage `json:"change,omitempty"`
}

func dashboardFleetTool(name string) bool {
	return slices.Contains([]string{operatortool.InstanceHealth, operatortool.AIDebugPrompt, operatortool.Dashboard, operatortool.HealthDashboard, operatortool.DiagnosticsDashboard, operatortool.OperationsReport, operatortool.RunnerFleet, operatortool.Refresh, operatortool.CapacityClear, operatortool.TrackerAvailabilityClear, operatortool.ForgeAvailabilityClear, operatortool.FailureBreakerCanary, operatortool.UpdateApply, operatortool.ProgressCredit, operatortool.AcknowledgeWarnings, operatortool.RecoverAttempt, operatortool.UpdateFleetRunner, operatortool.UpdateFleetHost}, name)
}

func dashboardFleetRequirement(name, projectID string) operatortool.Requirement {
	d, _ := operatortool.FleetDefinition(name)
	scope := apikey.ScopeRead
	if !d.Annotations.ReadOnly {
		scope = apikey.ScopeAdmin
	}
	if name == operatortool.Refresh || name == operatortool.ProgressCredit || name == operatortool.AcknowledgeWarnings || name == operatortool.RecoverAttempt {
		scope = apikey.ScopeWrite
	}
	r := operatortool.Requirement{Scope: scope, ProjectID: projectID}
	switch name {
	case operatortool.InstanceHealth, operatortool.OperationsReport, operatortool.RunnerFleet, operatortool.UpdateFleetRunner, operatortool.UpdateFleetHost, operatortool.Refresh, operatortool.UpdateApply:
		r.ResourceKind = "instance"
	case operatortool.CapacityClear, operatortool.TrackerAvailabilityClear, operatortool.ForgeAvailabilityClear, operatortool.FailureBreakerCanary:
		if projectID == "" {
			r.ResourceKind = "instance"
		}
	}
	return r
}

func (s *Server) fleetToolAvailable(name string) bool {
	switch name {
	case operatortool.RunnerFleet, operatortool.UpdateFleetRunner, operatortool.UpdateFleetHost:
		return s.runnerFleet != nil
	case operatortool.Refresh:
		return s.refresher != nil
	case operatortool.UpdateApply:
		return s.updateApplier != nil
	case operatortool.RecoverAttempt:
		return s.recovery != nil
	case operatortool.ProgressCredit:
		return s.store != nil && s.issueExplainer != nil
	case operatortool.AcknowledgeWarnings:
		return s.stalenessWarnings != nil
	case operatortool.OperationsReport:
		return s.store != nil
	default:
		return dashboardFleetTool(name)
	}
}

func decodeFleetRequest(name string, raw json.RawMessage) (fleetRequest, error) {
	if err := operatortool.ValidateFleetArguments(name, raw); err != nil {
		return fleetRequest{}, err
	}
	var r fleetRequest
	if err := operatortool.DecodeArguments(raw, &r); err != nil {
		return r, err
	}
	if name == operatortool.UpdateApply && (r.RunnerID != "" || len(r.Change) != 0) {
		return r, operatortool.ErrInvalidArguments
	}
	if r.Limit == 0 {
		r.Limit = 100
	}
	return r, nil
}

func (s *Server) executeFleetRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	r, err := decodeFleetRequest(call.Name, call.Arguments)
	if err != nil {
		return operatortool.Result{}, err
	}
	requirement := dashboardFleetRequirement(call.Name, r.ProjectID)
	if call.Name == operatortool.AIDebugPrompt {
		if r.Scope == "" {
			r.Scope = "issue"
		}
		if r.Scope == "fleet" {
			requirement.ResourceKind = "instance"
		} else if r.ProjectID == "" || r.Scope == "issue" && r.Reference == "" {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return operatortool.Result{}, err
	}
	if !s.fleetToolAvailable(call.Name) {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	switch call.Name {
	case operatortool.InstanceHealth:
		value, _ := s.readInstanceHealth(ctx)
		return operatorResult(struct {
			Health     healthResponse `json:"health"`
			ObservedAt time.Time      `json:"observed_at"`
			URL        string         `json:"url"`
		}{value, s.now().UTC(), "/health"})
	case operatortool.AIDebugPrompt:
		snapshot, err := operatortool.ProjectSnapshot(ctx, s.latestSnapshot(ctx))
		if err != nil {
			return operatortool.Result{}, err
		}
		projection, err := s.aiDebugProjectionFromSnapshot(ctx, aidebug.Scope(r.Scope), r.ProjectID, r.Reference, snapshot)
		if err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		prompt, err := projection.Prompt()
		if err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		return operatorResult(struct {
			Projection aidebug.Projection `json:"projection"`
			Prompt     string             `json:"prompt"`
			ObservedAt time.Time          `json:"observed_at"`
			URL        string             `json:"url"`
		}{projection, prompt, s.now().UTC(), "/api/v1/ai-debug?" + url.Values{"scope": {r.Scope}, "project": {r.ProjectID}, "issue": {r.Reference}}.Encode()})
	case operatortool.RunnerFleet:
		fleet, err := s.runnerFleet.Fleet(ctx)
		if err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		var eligibility *runnerauth.ProjectEligibility
		if r.ProjectID != "" {
			value, err := s.runnerFleet.ProjectEligibility(ctx, r.ProjectID)
			if err != nil {
				return operatortool.Result{}, errOperatorCommandUnavailable
			}
			eligibility = &value
		}
		if r.RunnerID != "" {
			fleet.Runners = slices.DeleteFunc(slices.Clone(fleet.Runners), func(v runnerauth.Runner) bool { return v.RunnerID != r.RunnerID })
		}
		end := min(len(fleet.Runners), r.Offset+r.Limit)
		start := min(r.Offset, len(fleet.Runners))
		more := end < len(fleet.Runners)
		fleet.Runners = fleet.Runners[start:end]
		return operatorResult(struct {
			Fleet       runnerauth.Fleet               `json:"fleet"`
			Eligibility *runnerauth.ProjectEligibility `json:"eligibility,omitempty"`
			ObservedAt  time.Time                      `json:"observed_at"`
			HasMore     bool                           `json:"has_more"`
		}{fleet, eligibility, s.now().UTC(), more})
	case operatortool.OperationsReport:
		now := s.now().UTC()
		since := now.Add(-24 * time.Hour)
		if r.Since != "" {
			since, err = time.Parse(time.RFC3339Nano, r.Since)
			if err != nil || since.After(now) || since.Before(now.Add(-31*24*time.Hour)) {
				return operatortool.Result{}, operatortool.ErrInvalidArguments
			}
		}
		report, err := s.readOperationsReport(ctx, now, since)
		if err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		return operatorResult(report)
	default:
		var snapshot telemetry.Snapshot
		pendingEnrichment := false
		if call.Name == operatortool.Dashboard {
			var enriched bool
			snapshot, enriched = s.latestBoardSnapshot()
			pendingEnrichment = !enriched
		} else {
			snapshot = s.latestSnapshot(ctx)
		}
		// Return the application read model, never HTML, dashboard cookies or tokens.
		resourceURL := "/"
		switch call.Name {
		case operatortool.HealthDashboard:
			snapshot = s.healthDashboardData(ctx, snapshot).Snapshot
			resourceURL = "/health/ui"
		case operatortool.DiagnosticsDashboard:
			snapshot = s.diagnosticsDashboardData(ctx, snapshot).Snapshot
			resourceURL = "/diagnostics"
		default:
			snapshot = s.dashboardFirstPaintData(ctx, snapshot, pendingEnrichment).Snapshot
		}
		snapshot, err = operatortool.ProjectSnapshot(ctx, snapshot)
		if err != nil {
			return operatortool.Result{}, err
		}
		if r.ProjectID != "" {
			snapshot = projectScopedSnapshotForProject(snapshot, telemetry.Project{ID: r.ProjectID})
		}
		if len(snapshot.Running) > r.Limit {
			snapshot.Running = snapshot.Running[:r.Limit]
		}
		if len(snapshot.BoardIssues) > r.Limit {
			snapshot.BoardIssues = snapshot.BoardIssues[:r.Limit]
		}
		return operatorResult(struct {
			Snapshot          telemetry.Snapshot `json:"snapshot"`
			URL               string             `json:"url"`
			ObservedAt        time.Time          `json:"observed_at"`
			PendingEnrichment bool               `json:"pending_enrichment,omitempty"`
		}{snapshot, resourceURL, s.now().UTC(), pendingEnrichment})
	}
}

func (s *Server) fleetActionProposal(ctx context.Context, name string, raw json.RawMessage) (chatpkg.Action, error) {
	// request_id has been stripped by the shared mutation adapter.
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return chatpkg.Action{}, operatortool.ErrInvalidArguments
	}
	fields["request_id"] = json.RawMessage(`"preview"`)
	bounded, err := json.Marshal(fields)
	if err != nil {
		return chatpkg.Action{}, operatortool.ErrInvalidArguments
	}
	r, err := decodeFleetRequest(name, bounded)
	if err != nil {
		return chatpkg.Action{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, dashboardFleetRequirement(name, r.ProjectID))
	if err != nil {
		return chatpkg.Action{}, err
	}
	if !s.fleetToolAvailable(name) {
		return chatpkg.Action{}, errOperatorCommandUnavailable
	}
	a := chatpkg.Action{Kind: chatpkg.ActionKind(name), ProjectID: r.ProjectID, Title: name, Description: string(raw), ResourceURL: "/"}
	switch name {
	case operatortool.UpdateFleetRunner:
		var change runnerauth.RoutingChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return a, operatortool.ErrInvalidArguments
		}
		change.Routing = change.Normalized()
		if change.Validate() != nil {
			return a, operatortool.ErrInvalidArguments
		}
		fleet, err := s.runnerFleet.Fleet(ctx)
		if err != nil || !fleet.Editable {
			return a, errOperatorCommandUnavailable
		}
		found := false
		for _, runner := range fleet.Runners {
			if runner.RunnerID == r.RunnerID && runner.Revision == change.ExpectedRevision {
				found = true
				a.CurrentState = strconv.FormatInt(runner.Revision, 10)
				a.MaterialChange = operatortool.RoutingRequiresApproval(runner.Routing, change.Routing) || runner.CapacityRequiresApplication(change.CapacityLimit)
			}
		}
		if !found {
			return a, errOperatorCommandUnavailable
		}
		a.IssueID = r.RunnerID
		a.Identifier = r.RunnerID
		a.ResourceURL = "/fleet/runners?runner=" + url.QueryEscape(r.RunnerID)
	case operatortool.UpdateFleetHost:
		var change runnerauth.HostChange
		if operatortool.DecodeArguments(r.Change, &change) != nil || change.DisplayName == "" || len(change.DisplayName) > 200 || strings.ContainsAny(change.DisplayName, "\r\n\x00") {
			return a, operatortool.ErrInvalidArguments
		}
		fleet, err := s.runnerFleet.Fleet(ctx)
		if err != nil || !fleet.Editable {
			return a, errOperatorCommandUnavailable
		}
		found := false
		for _, runner := range fleet.Runners {
			if string(runner.MachineID) == r.MachineID && runner.HostRevision == change.ExpectedRevision {
				found = true
				a.CurrentState = strconv.FormatInt(runner.HostRevision, 10)
				a.MaterialChange = runner.HostCapacity != change.Capacity
			}
		}
		if !found {
			return a, errOperatorCommandUnavailable
		}
		a.IssueID = r.MachineID
		a.Identifier = r.MachineID
		a.ResourceURL = "/fleet/runners"
	case operatortool.ProgressCredit:
		explanation, err := s.issueExplainer.Explain(ctx, explain.Query{ProjectID: r.ProjectID, Reference: r.Reference})
		if err != nil || explanation.Identity.ProjectID != r.ProjectID || explanation.Identity.IssueID == "" {
			return a, errOperatorCommandUnavailable
		}
		a.IssueID = explanation.Identity.IssueID
		a.Identifier = explanation.Identity.Identifier
		a.ResourceURL = explanation.Identity.IssueURL
	case operatortool.RecoverAttempt:
		receipt, err := s.recovery.WorkAttemptReceipt(ctx, r.ProjectID, r.AttemptID)
		if err != nil || receipt.Attempt.ProjectID != r.ProjectID || receipt.Attempt.AttemptID != r.AttemptID {
			return a, errOperatorCommandUnavailable
		}
		available := r.Action == "inspect"
		for _, action := range receipt.Available {
			if string(action.Action) == r.Action {
				available = true
			}
		}
		if !available {
			return a, errOperatorCommandUnavailable
		}
		a.IssueID = receipt.Attempt.IssueID
		a.Identifier = receipt.Attempt.Identifier
		a.WorkAttemptID = r.AttemptID
		a.Destination = r.Action
		a.CurrentState = receipt.Attempt.Status + ":" + receipt.Attempt.TerminalState
		a.ProviderSessionID = receipt.Attempt.CurrentCommand
		a.ResourceURL = workAttemptReceiptURL(receipt.Attempt)
	}
	return a, nil
}

func (s *Server) executeFleetAction(ctx context.Context, a chatpkg.Action) (string, error) {
	var r fleetRequest
	if err := operatortool.DecodeArguments(a.Arguments, &r); err != nil {
		return "", err
	}
	var value any
	var err error
	switch string(a.Kind) {
	case operatortool.Refresh:
		value, err = s.requestOperatorRefresh(ctx)
	case operatortool.CapacityClear:
		value, err = s.clearCapacity(ctx, r.ProjectID, r.Scope, r.Recovery)
	case operatortool.TrackerAvailabilityClear:
		value, err = s.clearTrackerAvailability(ctx, r.ProjectID)
	case operatortool.ForgeAvailabilityClear:
		value, err = s.clearForgeAvailability(ctx, r.ProjectID, r.Host)
	case operatortool.FailureBreakerCanary:
		value, err = s.requestBreakerCanary(ctx, r.ProjectID)
	case operatortool.UpdateApply:
		value, err = s.applyOperatorUpdate(ctx, r.Release, r.FromRelease)
	case operatortool.AcknowledgeWarnings:
		value, err = s.acknowledgeOperatorWarnings(ctx, r.ProjectID, r.WarningIDs)
	case operatortool.ProgressCredit:
		value, err = s.creditOperatorProgress(ctx, explain.Identity{ProjectID: a.ProjectID, IssueID: a.IssueID, Identifier: a.Identifier, IssueURL: a.ResourceURL})
	case operatortool.RecoverAttempt:
		value, err = s.recovery.RecoverWorkAttempt(ctx, orchestrator.WorkAttemptRecoveryRequest{ProjectID: r.ProjectID, AttemptID: r.AttemptID, Action: orchestrator.WorkAttemptRecoveryAction(r.Action), Confirm: true, Reason: r.Reason, Operator: operatortool.ConnectionIdentity(ctx).PrincipalID})
	case operatortool.UpdateFleetRunner:
		var change runnerauth.RoutingChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return "", operatortool.ErrInvalidArguments
		}
		change.Routing = change.Normalized()
		err = s.runnerFleet.UpdateRunner(ctx, r.RunnerID, change)
		value = struct {
			RunnerID string `json:"runner_id"`
			Revision int64  `json:"revision"`
		}{r.RunnerID, change.ExpectedRevision + 1}
	case operatortool.UpdateFleetHost:
		var change runnerauth.HostChange
		if operatortool.DecodeArguments(r.Change, &change) != nil {
			return "", operatortool.ErrInvalidArguments
		}
		err = s.runnerFleet.UpdateHost(ctx, tracker.MachineID(r.MachineID), change)
		value = struct {
			MachineID string `json:"machine_id"`
			Revision  int64  `json:"revision"`
		}{r.MachineID, change.ExpectedRevision + 1}
	default:
		return "", operatortool.ErrUnknownTool
	}
	if err != nil {
		return "", errOperatorCommandUnavailable
	}
	result, err := operatorResult(value)
	if err != nil {
		return "", err
	}
	return string(result.Content), nil
}

func (s *Server) fleetMutationRequirement(name, projectID string) operatortool.Requirement {
	if dashboardFleetTool(name) {
		return dashboardFleetRequirement(name, projectID)
	}
	return operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: projectID}
}
