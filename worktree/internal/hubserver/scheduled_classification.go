package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
)

type ciTextBackend struct{ config *ConversationConfig }

func (b ciTextBackend) Generate(ctx context.Context, instructions, input string) (string, error) {
	if b.config == nil || b.config.Backend == nil {
		return "", errors.New("CI AI backend unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var text strings.Builder
	request := runner.AgentTurnRequest{Workspace: b.config.Workspace, Prompt: input, ToolInstructions: instructions, Model: b.config.Model, ReasoningEffort: b.config.ReasoningEffort,
		ReadOnly: true, TextOnly: true, MaxTurns: 1, TurnTimeout: 20 * time.Second, MaxDuration: 20 * time.Second}
	_, err := b.config.Backend.RunTurn(ctx, request, func(update runner.AgentUpdate) error {
		if update.Type == runner.AgentUpdateMessageDelta {
			if text.Len()+len(update.Delta) > progresscheck.MaxOutputBytes {
				return errors.New("CI AI output exceeds byte bound")
			}
			text.WriteString(update.Delta)
		}
		return nil
	})
	return text.String(), err
}

func (s *Service) readCIClassification(ctx context.Context, scope nativeScope, input progresscheck.CIFailure) (progresscheck.CIDecision, bool, error) {
	var result progresscheck.CIDecision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(policystore.ScheduledReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		result, err = owner.ReadScheduledDecision(ctx, scope.project, input)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return result, false, nil
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return result, false, nativePublicInvalid("CI classification commit differs from retained failure")
	}
	return result, err == nil, err
}

func (s *Service) scheduledRepository(ctx context.Context, scope nativeScope) (string, error) {
	if s.config.TenantRepository == nil {
		return "", workItemStorageUnavailable()
	}
	var repository string
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if record.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		repository = record.Settings.CheckoutRepository
		if repository == "" {
			repository = record.Settings.Repository
		}
		return nil
	})
	return repository, projectRecordError(err)
}

func (s *Service) classifyScheduledFailure(c echo.Context) error {
	scope := nativeRequestScope(c)
	if scope.credential.HostedRole == "reporter" {
		return s.nativeAPIError(c, &nativeError{Code: "insufficient_scope", status: http.StatusForbidden})
	}
	var input progresscheck.CIFailure
	if err := decodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := input.Validate(); err != nil {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid or oversized CI classification evidence"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Minute)
	defer cancel()
	repository, err := s.scheduledRepository(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if !strings.EqualFold(repository, input.Repository) {
		return s.nativeAPIError(c, nativePublicInvalid("CI classification repository differs from project"))
	}
	input.Repository = repository
	result, found, err := s.readCIClassification(ctx, scope, input)
	if found || err != nil {
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err = (progresscheck.AI{Backend: ciTextBackend{config: s.config.Conversation}}).ClassifyCI(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return s.nativeAPIError(c, ctx.Err())
		}
		result = progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "AI classification unavailable or invalid; diagnose the retained job evidence through the source repair path."}
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(policystore.ScheduledWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		return owner.StoreScheduledDecision(ctx, scope.project, input, result, s.config.now().UTC())
	})
	if err != nil {
		return s.nativeAPIError(c, fmt.Errorf("store CI classification: %w", err))
	}
	result, _, err = s.readCIClassification(ctx, scope, input)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
