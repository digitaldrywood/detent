package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
)

func ScreenReadNames() []string {
	return []string{"get_native_project", "get_onboarding", "project_setup", "get_project_policy", operatortool.GetProjectModelSelection, operatortool.GetOrganizationModelSelection, operatortool.BoardActivity, "project_secret_metadata", "monthly_usage_costs", operatortool.WorkList, operatortool.HealthFindings, operatortool.BoardState, operatortool.TimeSeries, operatortool.AnalyticsDashboard, operatortool.Reports, "get_project_usage", "get_organization_plan", "get_billing_status", "get_billing_usage", "get_project_members", operatortool.MembershipList, operatortool.GetRunnerCapacity, operatortool.GetRunnerUpdate}
}

func ScreenTools() []runner.AgentTool {
	empty := `{"type":"object","properties":{},"additionalProperties":false}`
	tools := []runner.AgentTool{
		Tool("get_slack_integration", "Read organization Slack integration metadata from Settings: masked webhook presence, channel and delivery status. Requires owner/admin access; secret values are never returned.", empty),
		Tool("get_project_budget", "Read this project's monthly budget policy from Settings, including revision and configured spending limits.", empty),
		Tool("get_project_fleet", "Read the Providers & runners screen summary for attached self-hosted and Sprite runners in this project. Returns only fields the current viewer can see. Management details are available through get_project_runners and get_runner_capacity with runner permission.", `{"type":"object","properties":{},"additionalProperties":false}`),
		Tool("get_project_runners", "Read attached self-hosted and Sprite runners for this project from the Providers & runners screen: heartbeat, health, host and provider capacity, routing, update state and readiness problems. Requires runner details permission. Read this before answering worker health; Sprites alone do not describe the project's runners.", `{"type":"object","properties":{},"additionalProperties":false}`),
		Tool("get_project_diagnostics", "Read the Diagnostics screen for this project, including health findings, capacity, claim refusals and configuration.", `{"type":"object","properties":{"range":{"type":"string","enum":["24h","7d","30d","90d"]}},"additionalProperties":false}`),
		Tool("get_project_reports", "Read the Reports screen for this project, including completion, stages, throughput, spend and source coverage.", `{"type":"object","properties":{"range":{"type":"string","enum":["24h","48h","7d","30d"]}},"additionalProperties":false}`),
	}
	for _, name := range ScreenReadNames() {
		definition, ok := operatortool.Lookup(ScreenTarget(name))
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
		tools = append(tools, Tool(name, description+" Read only within this conversation's project and the current human viewer's permissions.", string(raw)))
	}
	return tools
}

func ScreenRead(name string) bool {
	return name == "get_slack_integration" || name == "get_project_budget" || slices.Contains(ScreenReadNames(), name) || (name == "get_project_runners" || name == "get_project_fleet") || name == "get_project_diagnostics" || name == "get_project_reports"
}

func ReadPermission(name string) string {
	switch ScreenTarget(name) {
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

func (d ScreenReadDeps) Read(ctx context.Context, record conversations.ConversationRecord, call runner.AgentToolCall) (any, error) {
	ctx, err := d.ActionContext(ctx, record)
	if err != nil {
		return nil, fmt.Errorf("you don't have access to %s: %w", ReadPermission(call.Name), operatortool.ErrAccessDenied)
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: string(record.ProjectID)})
	if err != nil {
		return nil, fmt.Errorf("you don't have access to %s: %w", ReadPermission(call.Name), operatortool.ErrAccessDenied)
	}
	credential, err := d.Operator(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.AuthorizeRecord(ctx, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential}, record, false); err != nil {
		return nil, fmt.Errorf("you don't have access to this conversation's stored answers: equal screen access is required: %w", operatortool.ErrAccessDenied)
	}
	var fields map[string]json.RawMessage
	if len(call.Arguments) > hubapi.CoordinatorToolArgumentBytes || json.Unmarshal(call.Arguments, &fields) != nil || fields == nil {
		return nil, ErrToolArguments
	}
	if _, ok := fields["project_id"]; ok {
		return nil, fmt.Errorf("%w: project_id is fixed by this conversation", ErrToolArguments)
	}
	if _, ok := fields["scope"]; ok {
		return nil, ErrToolArguments
	}
	if call.Name == operatortool.MembershipList && credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return nil, fmt.Errorf("you don't have access to %s: %w", ReadPermission(call.Name), operatortool.ErrAccessDenied)
	}
	var value any
	target := ScreenTarget(call.Name)
	switch target {
	case "get_slack_integration":
		if err = DecodeArguments(call.Arguments, &struct{}{}); err == nil {
			if !hubscope.CanManageProjectSecrets(credential) {
				err = operatortool.ErrAccessDenied
			} else {
				value, err = d.ReadSlack(ctx, record.OrganizationID)
			}
		}
	case "get_project_budget":
		if err = DecodeArguments(call.Arguments, &struct{}{}); err == nil {
			value, err = d.ReadBudget(ctx, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential})
		}
	case "get_project_runners", "get_project_fleet":
		if err = DecodeArguments(call.Arguments, &struct{}{}); err == nil && call.Name == "get_project_runners" {
			ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, ResourceKind: "runners"})
			if err != nil {
				if errors.Is(err, operatortool.ErrAccessDenied) {
					return nil, fmt.Errorf("you don't have access to %s: %w", ReadPermission(call.Name), operatortool.ErrAccessDenied)
				}
				return nil, err
			}
		}
		if err == nil {
			value, err = d.ReadRunners(ctx, record, credential, call.Name == "get_project_runners" || d.AllRunnerGrants(ctx, credential))
		}
	case "get_project_diagnostics", "get_project_reports":
		value, err = d.ReadReport(ctx, record, credential, call)
	default:
		if ProjectReadScoped(call.Name) {
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
				return nil, ErrToolArguments
			}
			runnerCtx, authErr := operatortool.AuthorizeCurrent(ctx, d.FleetRequirement(call.Name))
			if authErr != nil {
				return nil, fmt.Errorf("you don't have access to %s: %w", ReadPermission(call.Name), authErr)
			}
			attached, attachErr := d.RunnerAttached(runnerCtx, record, id)
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
			result, err = d.ExecuteOperator(ctx, operatortool.Call{Name: target, Arguments: raw})
			value = result.Content
			if err == nil && target == "get_project_policy" {
				value, err = PolicyEvidence(result.Content)
			}
			if err == nil && target == operatortool.MembershipList {
				value, err = d.ProjectMembers(result.Content, record.ProjectID)
			}
		}
	}
	if err != nil {
		if errors.Is(err, operatortool.ErrAccessDenied) {
			return nil, fmt.Errorf("you don't have access to %s: %w", ReadPermission(call.Name), operatortool.ErrAccessDenied)
		}
		return nil, err
	}
	if err := d.RestrictAudience(ctx, record, credential); err != nil {
		return nil, err
	}
	return value, nil
}

func ProjectReadScoped(name string) bool {
	name = ScreenTarget(name)
	return name != operatortool.GetOrganizationModelSelection && name != operatortool.MembershipList && name != operatortool.HostedPlan && name != operatortool.BillingStatus && name != operatortool.BillingUsage
}

func RunnerAttached(ctx context.Context, record conversations.ConversationRecord, id string, operator func(context.Context) (hubapi.APICredential, error), runner func(context.Context, hubscope.NativeScope, string) (runnerauth.Runner, error)) (bool, error) {
	credential, err := operator(ctx)
	if err != nil {
		return false, err
	}
	r, err := runner(ctx, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential}, id)
	if err != nil {
		return false, err
	}
	return r.AllowsProject(record.ProjectID), nil
}

func (s ScreenReportDeps) Report(ctx context.Context, record conversations.ConversationRecord, credential hubapi.APICredential, call runner.AgentToolCall) (any, error) {
	if !s.FleetPermitted(operatortool.Reports, credential) {
		return nil, operatortool.ErrAccessDenied
	}
	var args struct {
		Range string `json:"range"`
	}
	if err := DecodeArguments(call.Arguments, &args); err != nil {
		return nil, err
	}
	if args.Range == "" {
		args.Range = "24h"
	}
	var w operatortool.AnalyticsWindow
	if call.Name == "get_project_reports" {
		hours := map[string]int{"24h": 24, "48h": 48, "7d": 168, "30d": 720}[args.Range]
		if hours == 0 {
			return nil, ErrToolArguments
		}
		to := s.Now().UTC().Truncate(time.Hour)
		bucket := time.Hour
		if hours > 240 {
			bucket = 4 * time.Hour
		}
		w = operatortool.AnalyticsWindow{From: to.Add(-time.Duration(hours) * time.Hour), To: to, Bucket: bucket}
	} else {
		window, err := s.UsageWindow(args.Range, s.Now())
		if err != nil {
			return nil, err
		}
		w = operatortool.AnalyticsWindow{From: window.From, To: window.To, Bucket: time.Hour}
	}
	if call.Name == "get_project_reports" {
		report, readable, err := s.ReadProjectReport(ctx, credential, string(record.ProjectID), w)
		if err != nil {
			return nil, err
		}
		if !readable {
			return nil, operatortool.ErrAccessDenied
		}
		return report, nil
	}
	return s.ReadDiagnostics(ctx, credential, w, record.ProjectID)
}

func ScreenTarget(name string) string {
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

func (d ReadAudienceDeps) Restrict(ctx context.Context, record conversations.ConversationRecord, credential hubapi.APICredential) error {
	var audience string
	err := d.ReadTenant(ctx, record.OrganizationID, func(r tenantstore.Reader) error {
		var err error
		audience, err = conversations.ConversationReadAudienceRecord(ctx, r, record.OrganizationID, hubscope.AuthCredential(credential))
		return err
	})
	if err != nil {
		return operatortool.ErrAccessDenied
	}
	return d.Write(ctx, record.ID, func(ctx context.Context, w tenantstore.Writer, fresh *conversations.ConversationRecord, now time.Time) error {
		if fresh.ReadAudience != "" && fresh.ReadAudience != audience {
			return fmt.Errorf("equal screen access is required: %w", operatortool.ErrAccessDenied)
		}
		if fresh.ReadAudience == audience {
			return nil
		}
		fresh.ReadAudience = audience
		return SaveConversation(ctx, w, fresh, now)
	}, func(ctx context.Context, p *conversations.ConversationPublicationPlan) error {
		if p.Record.ReadAudience == audience {
			return nil
		}
		p.Record.ReadAudience = audience
		return p.SaveConversation(ctx)
	})
}
