package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
)

type operatorBudgetRequest = operatortool.BudgetArguments

func decodeOperatorBudget(name string, raw json.RawMessage) (operatorBudgetRequest, error) {
	var request operatorBudgetRequest
	if operatortool.DecodeArguments(raw, &request) != nil || strings.TrimSpace(request.ProjectID) == "" || len(request.ProjectID) > 256 || len(request.Duration) > 128 || len(request.Reason) > 280 {
		return request, operatortool.ErrInvalidArguments
	}
	if name == operatortool.BudgetOverrideClear {
		if request.Duration != "" || request.Reason != "" || request.PerDayMaxUSD != nil || request.PerIssueMaxUSD != nil {
			return request, operatortool.ErrInvalidArguments
		}
		return request, nil
	}
	for _, value := range []*float64{request.PerDayMaxUSD, request.PerIssueMaxUSD} {
		if value != nil && (*value <= 0 || math.IsInf(*value, 0) || math.IsNaN(*value)) {
			return request, operatortool.ErrInvalidArguments
		}
	}
	duration, err := time.ParseDuration(request.Duration)
	if err != nil || duration <= 0 || strings.TrimSpace(request.Reason) == "" || request.PerDayMaxUSD == nil && request.PerIssueMaxUSD == nil {
		return request, operatortool.ErrInvalidArguments
	}
	return request, nil
}

// Set and clear are the same application operations the HTTP dashboard uses.
func (s *Server) setProjectBudget(ctx context.Context, request operatorBudgetRequest) (store.BudgetOverride, error) {
	tracked, ok := s.registry.Get(project.ID(request.ProjectID))
	if !ok {
		return store.BudgetOverride{}, errOperatorCommandUnavailable
	}
	writer, ok := s.store.(budget.OverrideWriter)
	if !ok {
		return store.BudgetOverride{}, errOperatorCommandUnavailable
	}
	duration, err := time.ParseDuration(request.Duration)
	if err != nil {
		return store.BudgetOverride{}, err
	}
	cfg := tracked.Workflow().Config.Budget
	return budget.SetOverride(ctx, writer, budget.Config{Enabled: cfg.Enabled, ProjectID: request.ProjectID, PerDayMaxUSD: cfg.PerDayMaxUSD, PerIssueMaxUSD: cfg.PerIssueMaxUSD, Overrides: writer}, budget.OverrideLimits{MaxDuration: time.Duration(cfg.OverrideMaxDurationSeconds) * time.Second, MaxMultiplier: cfg.OverrideMaxMultiplier}, budget.OverrideRequest{ProjectID: request.ProjectID, PerDayMaxUSD: request.PerDayMaxUSD, PerIssueMaxUSD: request.PerIssueMaxUSD, Duration: duration, Reason: request.Reason, Now: s.now().UTC()})
}

func (s *Server) clearProjectBudget(ctx context.Context, projectID string) error {
	if _, ok := s.registry.Get(project.ID(projectID)); !ok {
		return errOperatorCommandUnavailable
	}
	writer, ok := s.store.(budget.OverrideWriter)
	if !ok {
		return errOperatorCommandUnavailable
	}
	if err := writer.ClearBudgetOverride(ctx, projectID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

func (s *Server) budgetActionProposal(ctx context.Context, name string, raw json.RawMessage) (chat.Action, error) {
	request, err := decodeOperatorBudget(name, raw)
	if err != nil {
		return chat.Action{}, err
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: request.ProjectID}); err != nil {
		return chat.Action{}, err
	}
	tracked, ok := s.registry.Get(project.ID(request.ProjectID))
	if !ok {
		return chat.Action{}, errOperatorCommandUnavailable
	}
	writer, ok := s.store.(budget.OverrideWriter)
	if !ok {
		return chat.Action{}, errOperatorCommandUnavailable
	}
	active, err := writer.ActiveBudgetOverride(ctx, request.ProjectID, s.now())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return chat.Action{}, err
	}
	rawBinding, err := json.Marshal(struct {
		Config   config.Budget
		Override store.BudgetOverride
	}{tracked.Workflow().Config.Budget, active})
	if err != nil {
		return chat.Action{}, err
	}
	sum := sha256.Sum256(rawBinding)
	return chat.Action{Kind: chat.ActionKind(name), ProjectID: request.ProjectID, IssueID: request.ProjectID, Identifier: request.ProjectID, Title: name, Reason: request.Reason, CurrentState: hex.EncodeToString(sum[:])}, nil
}

func (s *Server) executeBudgetAction(ctx context.Context, action chat.Action) error {
	request, err := decodeOperatorBudget(string(action.Kind), action.Arguments)
	if err != nil {
		return err
	}
	if action.Kind == chat.ActionKind(operatortool.BudgetOverrideClear) {
		return s.clearProjectBudget(ctx, request.ProjectID)
	}
	_, err = s.setProjectBudget(ctx, request)
	return err
}

func (s *Server) readUsageReport(ctx context.Context, query store.UsageReportQuery) (store.UsageReport, error) {
	if s.store == nil {
		return store.UsageReport{}, errOperatorCommandUnavailable
	}
	// HTTP dashboards use the same credential project grants. The MCP adapter
	// has already selected freshly authorized projects through its connection.
	if query.ProjectIDs == nil {
		if credential, ok := apiCredentialFromContext(ctx); ok && len(credential.ProjectIDs) > 0 {
			query.ProjectIDs = append([]string{}, credential.ProjectIDs...)
		}
	}
	return s.store.UsageReport(ctx, query)
}

func (s *Server) operatorUsageReport(ctx context.Context, raw json.RawMessage) (operatortool.Result, error) {
	var request operatortool.UsageArguments
	if operatortool.DecodeArguments(raw, &request) != nil || len(request.ProjectID) > 256 || len(request.From) > 10 || len(request.To) > 10 {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID}); err != nil {
		return operatortool.Result{}, err
	}
	now := s.now().UTC()
	if request.To == "" {
		request.To = now.Format(time.DateOnly)
	}
	if request.From == "" {
		request.From = now.AddDate(0, 0, -7).Format(time.DateOnly)
	}
	query, problem, _ := usageReportQueryValues(request.By, request.From, request.To)
	if problem != nil || query.To.Sub(query.From) > 90*24*time.Hour {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	query.ProjectIDs = []string{}
	if request.ProjectID != "" {
		query.ProjectIDs = append(query.ProjectIDs, request.ProjectID)
	} else {
		for _, id := range s.configuredProjectIDs() {
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id}); err == nil {
				query.ProjectIDs = append(query.ProjectIDs, id)
			}
		}
	}
	report, err := s.readUsageReport(ctx, query)
	if err != nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	truncated := len(report.Rows) > operatortool.MaxItemLimit
	report.Rows = report.Rows[:min(len(report.Rows), operatortool.MaxItemLimit)]
	return operatorResult(struct {
		GeneratedAt time.Time              `json:"generated_at"`
		Truncated   bool                   `json:"truncated"`
		Report      usageReportAPIResponse `json:"report"`
	}{now, truncated, usageReportResponse(report, s.pricing)})
}
