package operatorcommands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
)

type ScheduledCIDeps struct {
	Repository tenantstore.Repository
	Plans      *hostedplans.Service
	Authorize  func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	Verify     func(context.Context, hubscope.NativeScope, progresscheck.CIFailure) error
	Now        func() time.Time
	Text       CITextBackend
	Error      func(echo.Context, error) error
	Invalid    func(echo.Context, error) error
}

type CITextBackend struct {
	Backend         runner.AgentBackend
	Workspace       string
	Model           string
	ReasoningEffort string
}

func (b CITextBackend) Generate(ctx context.Context, instructions, input string) (string, error) {
	if b.Backend == nil {
		return "", errors.New("CI AI backend unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var text strings.Builder
	request := runner.AgentTurnRequest{Workspace: b.Workspace, Prompt: input, ToolInstructions: instructions, Model: b.Model, ReasoningEffort: b.ReasoningEffort,
		ReadOnly: true, TextOnly: true, MaxTurns: 1, TurnTimeout: 20 * time.Second, MaxDuration: 20 * time.Second}
	_, err := b.Backend.RunTurn(ctx, request, func(update runner.AgentUpdate) error {
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

func (s ScheduledCIDeps) readCIClassification(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure) (progresscheck.CIDecision, bool, error) {
	var result progresscheck.CIDecision
	err := s.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(policystore.ScheduledReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		result, err = owner.ReadScheduledDecision(ctx, scope.Project, input)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return result, false, nil
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return result, false, hubapi.NativePublicInvalid("CI classification commit differs from retained failure")
	}
	return result, err == nil, err
}

func ScheduledRepository(ctx context.Context, reader tenantstore.Repository, scope hubscope.NativeScope) (string, error) {
	if reader == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	var repository string
	err := reader.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := r.ProjectMetadata(ctx, scope.Project)
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
	return repository, hubapi.ProjectRecordError(err)
}

func (s ScheduledCIDeps) reserveClassification(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure) error {
	identity, err := json.Marshal([]string{string(scope.Project), input.Repository, input.RunID, input.RunAttempt, input.JobID})
	if err != nil {
		return fmt.Errorf("encode CI classification identity: %w", err)
	}
	return s.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.Authorize(ctx, w, scope, true); err != nil {
			return err
		}
		return s.Plans.ReserveScheduledClassification(ctx, w, scope.Organization, "scheduled-ci/"+policy.Digest(identity), s.Now().UTC())
	})
}

func (s ScheduledCIDeps) storeClassification(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure, result progresscheck.CIDecision) error {
	return s.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.Authorize(ctx, w, scope, true); err != nil {
			return err
		}
		owner, ok := w.(policystore.ScheduledWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		now := s.Now().UTC()
		limit := int64(math.MaxInt64)
		if s.Plans.Plans() != nil {
			entitlement, err := hubapi.FeatureResult(s.Plans.EntitlementRecord(ctx, w, now))
			if err != nil {
				return err
			}
			if allowance, limited := entitlement.Allowances["collaboration_bytes"]; limited {
				limit = allowance
			}
		}
		usage, err := owner.StoreScheduledDecision(ctx, scope.Project, input, result, now, limit)
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
		return nil
	})
}

func (s ScheduledCIDeps) Classify(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.HostedRole == "reporter" {
		return s.Error(c, &hubapi.NativeError{Code: "insufficient_scope", Status: http.StatusForbidden})
	}
	var input progresscheck.CIFailure
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.Invalid(c, err)
	}
	if err := input.Validate(); err != nil {
		return s.Error(c, hubapi.NativePublicInvalid("Invalid or oversized CI classification evidence"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Minute)
	defer cancel()
	repository, err := ScheduledRepository(ctx, s.Repository, scope)
	if err != nil {
		return s.Error(c, err)
	}
	if !strings.EqualFold(repository, input.Repository) {
		return s.Error(c, hubapi.NativePublicInvalid("CI classification repository differs from project"))
	}
	input.Repository = repository
	result, found, err := s.readCIClassification(ctx, scope, input)
	if found || err != nil {
		if err != nil {
			return s.Error(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	if err := s.Verify(ctx, scope, input); err != nil {
		return s.Error(c, err)
	}
	if err := s.reserveClassification(ctx, scope, input); err != nil {
		return s.Error(c, err)
	}
	result, err = (progresscheck.AI{Backend: s.Text}).ClassifyCI(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return s.Error(c, ctx.Err())
		}
		result = progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "AI classification unavailable or invalid; diagnose the retained job evidence through the source repair path."}
	}
	if err := s.storeClassification(ctx, scope, input, result); err != nil {
		return s.Error(c, fmt.Errorf("store CI classification: %w", err))
	}
	result, _, err = s.readCIClassification(ctx, scope, input)
	if err != nil {
		return s.Error(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
