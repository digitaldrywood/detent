package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type budgetOverrideValue struct {
	ProjectID      tracker.ProjectID `json:"project_id"`
	PerDayMaxUSD   *float64          `json:"per_day_max_usd"`
	PerIssueMaxUSD *float64          `json:"per_issue_max_usd"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Reason         string            `json:"reason"`
}

type budgetOverrideCommand struct {
	tracker.Mutation
	PerDayMaxUSD   *float64  `json:"per_day_max_usd"`
	PerIssueMaxUSD *float64  `json:"per_issue_max_usd"`
	ExpiresAt      time.Time `json:"expires_at"`
	Reason         string    `json:"reason"`
}

func (s *Service) registerBudgetOverrideRoutes(e *echo.Echo) {
	e.GET(nativeBase+"/budget-override", s.getBudgetOverride, s.requireNativeScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin))
	write := s.requireNativeScope(apiScopeOperator, apiScopeAdmin)
	e.PUT(nativeBase+"/budget-override", s.putBudgetOverride, write)
	e.DELETE(nativeBase+"/budget-override", s.deleteBudgetOverride, write)
}

func budgetOverrideResponse(value postgres.BudgetOverride) budgetOverrideValue {
	return budgetOverrideValue{ProjectID: value.ProjectID, PerDayMaxUSD: value.PerDayMaxUSD, PerIssueMaxUSD: value.PerIssueMaxUSD, ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt, Reason: value.Reason}
}

func (s *Service) getBudgetOverride(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	var value postgres.BudgetOverride
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		owner, ok := r.(postgres.BudgetOverrideReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		value, err = owner.ActiveBudgetOverride(ctx, scope.project, s.config.now())
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, budgetOverrideResponse(value))
}

func (s *Service) putBudgetOverride(c echo.Context) error {
	return s.writeBudgetOverride(c, false)
}

func (s *Service) deleteBudgetOverride(c echo.Context) error {
	return s.writeBudgetOverride(c, true)
}

func (s *Service) writeBudgetOverride(c echo.Context, clear bool) error {
	var command budgetOverrideCommand
	if err := c.Bind(&command); err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid budget override request"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	result, err := s.executeBudgetOverride(ctx, nativeRequestScope(c), command, clear)
	if errors.Is(err, postgres.ErrBudgetOverride) {
		err = nativePublicInvalid("Budget override exceeds the current project policy")
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) budgetOverridePolicy(ctx context.Context, record projectstore.Record, now time.Time) (postgres.BudgetOverridePolicy, error) {
	markdown, err := s.projectRecordWorkflow(ctx, record)
	if err != nil {
		return postgres.BudgetOverridePolicy{}, err
	}
	workflow, err := config.ParseWorkflow([]byte(markdown))
	if err != nil {
		return postgres.BudgetOverridePolicy{}, nativePublicInvalid("Project workflow is invalid")
	}
	value := workflow.Config.Budget
	duration := time.Duration(value.OverrideMaxDurationSeconds) * time.Second
	if duration <= 0 {
		duration = budget.DefaultOverrideMaxDuration
	}
	multiplier := value.OverrideMaxMultiplier
	if multiplier <= 0 {
		multiplier = budget.DefaultOverrideMaxMultiplier
	}
	return postgres.BudgetOverridePolicy{ProjectID: record.Core.ID, SettingsRevision: record.Settings.Revision, Enabled: value.Enabled, BasePerDayMaxUSD: value.PerDayMaxUSD, BasePerIssueMaxUSD: value.PerIssueMaxUSD, MaxDuration: duration, MaxMultiplier: multiplier, Now: now}, nil
}

func (s *Service) executeBudgetOverride(ctx context.Context, scope nativeScope, command budgetOverrideCommand, clear bool) (json.RawMessage, error) {
	operation := "set_budget_override"
	if clear {
		operation = "clear_budget_override"
	}
	input := struct {
		Project tracker.ProjectID
		Command budgetOverrideCommand
	}{scope.project, command}
	if raw, found, err := s.nativeCommandReplay(ctx, scope, operation, command.IdempotencyKey, input); err != nil || found {
		return raw, err
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	var record projectstore.Record
	var previous postgres.BudgetOverride
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		var err error
		record, err = r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		owner, ok := r.(postgres.BudgetOverrideReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		previous, err = owner.ActiveBudgetOverride(ctx, scope.project, now)
		if !clear && errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	value := postgres.BudgetOverride{OrganizationID: scope.organization, ProjectID: scope.project, PerDayMaxUSD: command.PerDayMaxUSD, PerIssueMaxUSD: command.PerIssueMaxUSD, ExpiresAt: command.ExpiresAt.UTC().Truncate(time.Microsecond), CreatedAt: now, Reason: strings.TrimSpace(command.Reason)}
	if previous.Revision != 0 {
		value.CreatedAt = previous.CreatedAt
	}
	var policy postgres.BudgetOverridePolicy
	var response any = struct {
		Status string `json:"status"`
	}{"ok"}
	if !clear {
		policy, err = s.budgetOverridePolicy(ctx, record, now)
		if err != nil {
			return nil, err
		}
		response = budgetOverrideResponse(value)
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: command.IdempotencyKey, RequestHash: hash}, raw)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	var replay *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		existing, err := w.ReplayReceipt(ctx, scope.credential.ID, operation, command.IdempotencyKey)
		if err == nil {
			if existing.RequestHash != hash {
				return nativeConflict(0)
			}
			replay = &existing
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		owner, ok := w.(postgres.BudgetOverrideCommandWriter)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		if clear {
			err = owner.ClearBudgetOverride(ctx, scope.project)
		} else if !value.ExpiresAt.After(s.config.now()) {
			return postgres.ErrBudgetOverride
		} else {
			_, err = owner.CompareAndSetBudgetOverride(ctx, value, policy, previous.Revision)
		}
		if err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		if committed, found, replayErr := s.nativeCommandReplay(ctx, scope, operation, command.IdempotencyKey, input); replayErr == nil && found {
			return committed, nil
		}
		return nil, err
	}
	if replay != nil {
		return content.Receipt(ctx, *replay)
	}
	return raw, nil
}
