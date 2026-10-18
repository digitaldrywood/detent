package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coordinatorScreenReadNames() []string {
	return []string{"get_native_project", "get_onboarding", "project_setup", "get_project_policy", operatortool.GetProjectModelSelection, operatortool.GetOrganizationModelSelection, operatortool.BoardActivity, "project_secret_metadata", "repository_freshness", "monthly_usage_costs", operatortool.WorkList, operatortool.HealthFindings, operatortool.BoardState, operatortool.TimeSeries, operatortool.AnalyticsDashboard, operatortool.Reports, "get_project_usage", "get_organization_plan", "get_billing_status", "get_billing_usage", "get_project_members", operatortool.MembershipList, operatortool.GetRunnerCapacity, operatortool.GetRunnerUpdate}
}

func coordinatorScreenTools() []runner.AgentTool {
	empty := `{"type":"object","properties":{},"additionalProperties":false}`
	tools := []runner.AgentTool{
		coordinatorTool("get_slack_integration", "Read organization Slack integration metadata from Settings: masked webhook presence, channel and delivery status. Requires owner/admin access; secret values are never returned.", empty),
		coordinatorTool("get_project_budget", "Read this project's monthly budget policy from Settings, including revision and configured spending limits.", empty),
		coordinatorTool("get_project_fleet", "Read the Providers & runners screen summary for attached self-hosted and Sprite runners in this project. Returns only fields the current viewer can see. Management details are available through get_project_runners and get_runner_capacity with runner permission.", `{"type":"object","properties":{},"additionalProperties":false}`),
		coordinatorTool("get_project_runners", "Read attached self-hosted and Sprite runners for this project from the Providers & runners screen: heartbeat, health, host and provider capacity, routing, update state and readiness problems. Requires runner details permission. Read this before answering worker health; Sprites alone do not describe the project's runners.", `{"type":"object","properties":{},"additionalProperties":false}`),
		coordinatorTool("get_project_diagnostics", "Read the Diagnostics screen for this project, including health findings, capacity, claim refusals and configuration.", `{"type":"object","properties":{"range":{"type":"string","enum":["24h","7d","30d","90d"]}},"additionalProperties":false}`),
		coordinatorTool("get_project_reports", "Read the Reports screen for this project, including completion, stages, throughput, spend and source coverage.", `{"type":"object","properties":{"range":{"type":"string","enum":["24h","48h","7d","30d"]}},"additionalProperties":false}`),
	}
	for _, name := range coordinatorScreenReadNames() {
		definition, ok := operatortool.Lookup(coordinatorScreenTarget(name))
		if !ok || !definition.Annotations.ReadOnly {
			continue
		}
		var schema map[string]any
		if json.Unmarshal(definition.InputSchema, &schema) != nil || schema == nil {
			continue
		}
		if properties, ok := schema["properties"].(map[string]any); ok {
			delete(properties, "project_id")
			delete(properties, "scope")
		}
		if required, ok := schema["required"].([]any); ok {
			schema["required"] = slices.DeleteFunc(required, func(value any) bool { return value == "project_id" || value == "scope" })
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		description := definition.Description
		if name == "get_project_members" {
			description = "Read the Members screen's safe member roster, omitting management fields for viewers without member-management access."
		}
		tools = append(tools, coordinatorTool(name, description+" Read only within this conversation's project and the current human viewer's permissions.", string(raw)))
	}
	return tools
}

func coordinatorScreenRead(name string) bool {
	return name == "get_slack_integration" || name == "get_project_budget" || slices.Contains(coordinatorScreenReadNames(), name) || (name == "get_project_runners" || name == "get_project_fleet") || name == "get_project_diagnostics" || name == "get_project_reports"
}

func coordinatorReadPermission(name string) string {
	switch coordinatorScreenTarget(name) {
	case "get_project_runners", operatortool.GetRunnerCapacity, operatortool.GetRunnerUpdate:
		return "runner details (runner administration or manage_runner grants)"
	case operatortool.BillingStatus, operatortool.BillingUsage:
		return "billing (organization owner)"
	case "get_slack_integration":
		return "Slack integration settings (organization owner or admin)"
	case operatortool.HostedPlan:
		return "plan details (organization owner or admin)"
	case operatortool.MembershipList:
		return "member management (organization owner or admin)"
	default:
		return "project screen read access"
	}
}

func (t *coordinatorToolset) screenRead(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	s := t.coordinator.service.server
	ctx, err := t.actionContext(ctx, record)
	if err != nil {
		return nil, fmt.Errorf("you don't have access to %s: %w", coordinatorReadPermission(call.Name), operatortool.ErrAccessDenied)
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, fmt.Errorf("you don't have access to %s: %w", coordinatorReadPermission(call.Name), operatortool.ErrAccessDenied)
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return nil, err
	}
	if err := t.coordinator.service.authorizeRead(ctx, s.database.reader, nativeScope{organization: record.OrganizationID, project: record.ProjectID, credential: credential}, record); err != nil {
		return nil, fmt.Errorf("you don't have access to this conversation's stored answers: equal screen access is required: %w", operatortool.ErrAccessDenied)
	}
	var fields map[string]json.RawMessage
	if len(call.Arguments) > coordinatorToolArgumentBytes || json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return nil, errCoordinatorToolArguments
	}
	if _, ok := fields["project_id"]; ok {
		return nil, fmt.Errorf("%w: project_id is fixed by this conversation", errCoordinatorToolArguments)
	}
	if _, ok := fields["scope"]; ok {
		return nil, errCoordinatorToolArguments
	}
	if call.Name == operatortool.MembershipList && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return nil, fmt.Errorf("you don't have access to %s: %w", coordinatorReadPermission(call.Name), operatortool.ErrAccessDenied)
	}
	var value any
	target := coordinatorScreenTarget(call.Name)
	switch target {
	case "get_slack_integration":
		if err = decodeCoordinatorArguments(call.Arguments, &struct{}{}); err == nil {
			if !canManageProjectSecrets(credential) {
				err = operatortool.ErrAccessDenied
			} else {
				value, err = readSlackIntegration(ctx, s.database.reader, record.OrganizationID)
			}
		}
	case "get_project_budget":
		if err = decodeCoordinatorArguments(call.Arguments, &struct{}{}); err == nil {
			value, err = readMonthlyBudgetSettings(ctx, s.database.reader, nativeScope{organization: record.OrganizationID, project: record.ProjectID, credential: credential})
		}
	case "get_project_runners", "get_project_fleet":
		if err = decodeCoordinatorArguments(call.Arguments, &struct{}{}); err == nil && call.Name == "get_project_runners" {
			ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, ResourceKind: "runners"})
		}
		if err == nil {
			value, err = s.coordinatorProjectRunners(ctx, record, credential, call.Name == "get_project_runners" || s.hostedAllRunnerGrants(ctx, credential))
		}
	case "get_project_diagnostics", "get_project_reports":
		value, err = s.coordinatorScreenReport(ctx, record, credential, call)
	default:
		if coordinatorProjectReadScoped(call.Name) {
			fields["project_id"], err = json.Marshal(record.ProjectID)
		}
		if err == nil && call.Name == "monthly_usage_costs" {
			fields["scope"] = json.RawMessage(`"project"`)
		}
		if err == nil && target == operatortool.HostedUsage {
			if _, present := fields["range"]; !present {
				fields["range"] = json.RawMessage(`"24h"`)
			}
		}
		var raw json.RawMessage
		if err == nil {
			raw, err = json.Marshal(fields)
		}
		if err == nil && (call.Name == operatortool.GetRunnerCapacity || call.Name == operatortool.GetRunnerUpdate) {
			var id string
			if json.Unmarshal(fields["runner_id"], &id) != nil || strings.TrimSpace(id) == "" {
				return nil, errCoordinatorToolArguments
			}
			runnerCtx, authErr := operatortool.AuthorizeCurrent(ctx, hubFleetRequirement(call.Name))
			if authErr != nil {
				return nil, fmt.Errorf("you don't have access to %s: %w", coordinatorReadPermission(call.Name), authErr)
			}
			attached, attachErr := s.coordinatorRunnerAttached(runnerCtx, record, id)
			if attachErr != nil {
				return nil, attachErr
			}
			if !attached {
				return nil, errors.New("runner is not attached to this project")
			}
			ctx = runnerCtx
		}
		if err == nil {
			var result operatortool.Result
			result, err = (hostedOperatorExecutor{service: s}).Execute(ctx, operatortool.Call{Name: target, Arguments: raw})
			value = result.Content
			if err == nil && target == "get_project_policy" {
				value, err = coordinatorPolicyEvidence(result.Content)
			}
			if err == nil && target == operatortool.MembershipList {
				value, err = coordinatorProjectMembers(result.Content, record.ProjectID)
			}
		}
	}
	if err != nil {
		if errors.Is(err, operatortool.ErrAccessDenied) {
			return nil, fmt.Errorf("you don't have access to %s: %w", coordinatorReadPermission(call.Name), operatortool.ErrAccessDenied)
		}
		return nil, err
	}
	if err := t.restrictReadAudience(ctx, record, credential); err != nil {
		return nil, err
	}
	return value, nil
}

func coordinatorProjectReadScoped(name string) bool {
	name = coordinatorScreenTarget(name)
	return name != operatortool.GetOrganizationModelSelection && name != operatortool.MembershipList && name != operatortool.HostedPlan && name != operatortool.BillingStatus && name != operatortool.BillingUsage
}

func (s *Service) coordinatorRunnerAttached(ctx context.Context, record conversationRecord, id string) (bool, error) {
	r, err := readRunnerWithClock(ctx, s.database.reader, record.OrganizationID, id, s.config.now)
	if err != nil {
		return false, err
	}
	return r.AllowsProject(record.ProjectID), nil
}

func (s *Service) coordinatorProjectRunners(ctx context.Context, record conversationRecord, credential apiCredential, editable bool) (any, error) {
	if s.config.Hosted == nil || string(record.OrganizationID) != s.config.Hosted.OrganizationID {
		return nil, operatortool.ErrAccessDenied
	}
	visible := map[tracker.ProjectID]bool{record.ProjectID: true}
	fleet, err := s.hostedFleetRunners(ctx, credential, visible)
	if err != nil {
		return nil, err
	}
	attached := make([]hostedFleetRunner, 0, len(fleet))
	connected := map[string]bool{}
	connectionHealth := map[string]string{}
	for _, r := range fleet {
		source, err := readRunnerWithClock(ctx, s.database.reader, record.OrganizationID, r.ID, s.config.now)
		if err != nil {
			return nil, err
		}
		if !source.AllowsProject(record.ProjectID) {
			continue
		}
		connected[r.ID] = source.State == "active" && source.ConnectionHealth == "online"
		connectionHealth[r.ID] = source.ConnectionHealth
		if r.Routing != nil {
			routing := *r.Routing
			routing.ProjectIDs = slices.DeleteFunc(slices.Clone(routing.ProjectIDs), func(project tracker.ProjectID) bool { return project != record.ProjectID })
			routing.ProjectRanks = map[tracker.ProjectID]int{record.ProjectID: routing.ProjectRanks[record.ProjectID]}
			r.Routing = &routing
		}
		r.Problems = slices.DeleteFunc(slices.Clone(r.Problems), func(p runnerauth.Problem) bool { return p.ProjectID != "" && p.ProjectID != string(record.ProjectID) })
		if r.Capacity != nil {
			capacity := *r.Capacity
			capacity.Projects = maps.Clone(capacity.Projects)
			for project := range capacity.Projects {
				if project != record.ProjectID {
					delete(capacity.Projects, project)
				}
			}
			r.Capacity = &capacity
		}
		attached = append(attached, r)
	}
	scopeHostUsage(attached)
	reservations, err := s.hostedVisibleReservations(ctx, visible)
	if err != nil {
		return nil, err
	}
	scopeProviderUsage(attached, reservations)
	type projectRunner struct {
		hostedFleetRunner
		Connected        bool   `json:"connected"`
		ConnectionHealth string `json:"connection_health"`
	}
	runners := make([]projectRunner, 0, len(attached))
	for _, r := range attached {
		runners = append(runners, projectRunner{r, connected[r.ID], connectionHealth[r.ID]})
	}
	return map[string]any{"project_id": record.ProjectID, "usage_scope": "project", "runners": runners, "observed_at": s.config.now(), "runner_options": []string{"self-hosted", "Sprites"}, "runner_details_access": editable}, nil
}

func (s *Service) coordinatorScreenReport(ctx context.Context, record conversationRecord, credential apiCredential, call runner.AgentToolCall) (any, error) {
	if !s.hubFleetReadPermitted(operatortool.Reports, credential) {
		return nil, operatortool.ErrAccessDenied
	}
	var args struct {
		Range string `json:"range"`
	}
	if err := decodeCoordinatorArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	if args.Range == "" {
		args.Range = "24h"
	}
	var w operatortool.AnalyticsWindow
	if call.Name == "get_project_reports" {
		hours := map[string]int{"24h": 24, "48h": 48, "7d": 168, "30d": 720}[args.Range]
		if hours == 0 {
			return nil, errCoordinatorToolArguments
		}
		to := s.config.now().UTC().Truncate(time.Hour)
		bucket := time.Hour
		if hours > 240 {
			bucket = 4 * time.Hour
		}
		w = operatortool.AnalyticsWindow{From: to.Add(-time.Duration(hours) * time.Hour), To: to, Bucket: bucket}
	} else {
		window, err := usageRangeWindow(args.Range, s.config.now())
		if err != nil {
			return nil, err
		}
		w = operatortool.AnalyticsWindow{From: window.From, To: window.To, Bucket: time.Hour}
	}
	analytics, err := s.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{ProjectID: string(record.ProjectID), Limit: maxAnalyticsPopulation}, w)
	if err != nil {
		return nil, err
	}
	if len(analytics.Projects) != 1 {
		return nil, operatortool.ErrAccessDenied
	}
	if call.Name == "get_project_reports" {
		return s.readHostedReports(ctx, analytics)
	}
	return s.readHostedDiagnostics(ctx, analytics)
}

func coordinatorScreenTarget(name string) string {
	switch name {
	case "get_project_usage":
		return operatortool.HostedUsage
	case "get_organization_plan":
		return operatortool.HostedPlan
	case "get_billing_status":
		return operatortool.BillingStatus
	case "get_billing_usage":
		return operatortool.BillingUsage
	case "get_project_members":
		return operatortool.MembershipList
	default:
		return name
	}
}

func coordinatorProjectMembers(raw json.RawMessage, project tracker.ProjectID) (any, error) {
	var page struct {
		Members     operatoradmin.PageResult[hostedMemberView]     `json:"members"`
		Invitations operatoradmin.PageResult[hostedInvitationView] `json:"invitations"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	for index := range page.Members.Items {
		page.Members.Items[index].Grants = slices.DeleteFunc(page.Members.Items[index].Grants, func(grant hostedMemberGrant) bool { return grant.ProjectID != string(project) })
	}
	for index := range page.Invitations.Items {
		page.Invitations.Items[index].Grants = slices.DeleteFunc(page.Invitations.Items[index].Grants, func(grant hostedMemberGrant) bool { return grant.ProjectID != string(project) })
	}
	return page, nil
}
