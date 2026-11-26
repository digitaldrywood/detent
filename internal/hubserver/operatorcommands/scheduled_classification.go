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
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
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

func (s ScheduledCIDeps) readCIClassification(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure, jobKey string) (progresscheck.CIDecision, bool, error) {
	var result progresscheck.CIDecision
	var pending bool
	err := s.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(policystore.ScheduledReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		result, pending, err = owner.ReadScheduledDecision(ctx, scope.Project, input, jobKey, s.Now().UTC())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return result, false, nil
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return result, false, hubapi.NativePublicInvalid("CI classification commit differs from retained failure")
	}
	if err == nil && pending {
		return progresscheck.CIDecision{}, false, &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
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

const scheduledClassificationTimeout = time.Minute

func (s ScheduledCIDeps) reserveClassification(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure, jobKey, receiptID string) error {
	return s.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.Authorize(ctx, w, scope, true); err != nil {
			return err
		}
		owner, ok := w.(policystore.ScheduledWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		now := s.Now().UTC()
		claimed, err := owner.ClaimScheduledClassification(ctx, scope.Project, jobKey, receiptID, now, now.Add(2*scheduledClassificationTimeout))
		if err != nil {
			return err
		}
		if !claimed {
			return &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
		}
		switch _, _, readErr := owner.ReadScheduledDecision(ctx, scope.Project, input, jobKey, now); {
		case readErr == nil:
			return &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
		case errors.Is(readErr, corestore.ErrRevisionConflict):
			return hubapi.NativePublicInvalid("CI classification commit differs from retained failure")
		case !errors.Is(readErr, corestore.ErrNotFound):
			return readErr
		}
		return s.Plans.ChargeScheduledClassification(ctx, w, scope.Organization, scope.Project, receiptID, now)
	})
}

func (s ScheduledCIDeps) writeClassification(ctx context.Context, scope hubscope.NativeScope, at time.Time, write func(policystore.ScheduledWriter, int64) (billingstore.UsageResult, error)) error {
	return s.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.Authorize(ctx, w, scope, true); err != nil {
			return err
		}
		owner, ok := w.(policystore.ScheduledWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		limit := int64(math.MaxInt64)
		if s.Plans.Plans() != nil {
			entitlement, err := hubapi.FeatureResult(s.Plans.EntitlementRecord(ctx, w, at))
			if err != nil {
				return err
			}
			if allowance, limited := entitlement.Allowances["collaboration_bytes"]; limited {
				limit = allowance
			}
		}
		usage, err := write(owner, limit)
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
		return nil
	})
}

func (s ScheduledCIDeps) classifyReserved(ctx context.Context, scope hubscope.NativeScope, input progresscheck.CIFailure, jobKey, receiptID string) (result progresscheck.CIDecision, err error) {
	claimReleased := false
	defer func() {
		if claimReleased {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cleanupErr := s.Repository.WriteTenant(cleanup, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(policystore.ScheduledWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			_, releaseErr := owner.ReleaseScheduledClassification(cleanup, scope.Project, jobKey, receiptID, nil)
			return releaseErr
		})
		err = errors.Join(err, cleanupErr)
	}()
	if err := s.Verify(ctx, scope, input); err != nil {
		return result, err
	}
	fallback := progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "AI classification unavailable or invalid; diagnose the retained job evidence through the source repair path."}
	at := s.Now().UTC()
	err = s.writeClassification(ctx, scope, at, func(owner policystore.ScheduledWriter, limit int64) (billingstore.UsageResult, error) {
		locked, err := owner.LockScheduledClassification(ctx, scope.Project, jobKey, receiptID, at)
		if err != nil {
			return billingstore.UsageResult{}, err
		}
		if !locked {
			return billingstore.UsageResult{}, &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
		}
		usage, err := owner.StoreScheduledDecision(ctx, scope.Project, input, fallback, at, limit)
		if err == nil && usage.Allowed && !usage.Applied {
			return usage, &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
		}
		return usage, err
	})
	if err != nil {
		return result, fmt.Errorf("record CI classification invocation: %w", err)
	}
	result, err = (progresscheck.AI{Backend: s.Text}).ClassifyCI(ctx, input)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return fallback, nil
	}
	at = s.Now().UTC()
	err = s.writeClassification(ctx, scope, at, func(owner policystore.ScheduledWriter, limit int64) (billingstore.UsageResult, error) {
		released, err := owner.ReleaseScheduledClassification(ctx, scope.Project, jobKey, receiptID, &at)
		if err != nil {
			return billingstore.UsageResult{}, err
		}
		if !released {
			return billingstore.UsageResult{}, &hubapi.NativeError{Code: "conflict", Status: http.StatusConflict}
		}
		return owner.UpdateScheduledDecision(ctx, scope.Project, input, result, limit)
	})
	if err != nil {
		return result, fmt.Errorf("store CI classification: %w", err)
	}
	claimReleased = true
	return result, nil
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
	ctx, cancel := context.WithTimeout(c.Request().Context(), scheduledClassificationTimeout)
	defer cancel()
	repository, err := ScheduledRepository(ctx, s.Repository, scope)
	if err != nil {
		return s.Error(c, err)
	}
	if !strings.EqualFold(repository, input.Repository) {
		return s.Error(c, hubapi.NativePublicInvalid("CI classification repository differs from project"))
	}
	input.Repository = repository
	identity, err := json.Marshal([]string{input.Repository, input.RunID, input.RunAttempt, input.JobID})
	if err != nil {
		return s.Error(c, err)
	}
	jobKey := policy.Digest(identity)
	result, found, err := s.readCIClassification(ctx, scope, input, jobKey)
	if found || err != nil {
		if err != nil {
			return s.Error(c, err)
		}
		return c.JSON(http.StatusOK, result)
	}
	receiptID := hubapi.NewID("scheduled_ci")
	if err := s.reserveClassification(ctx, scope, input, jobKey, receiptID); err != nil {
		return s.Error(c, err)
	}
	result, err = s.classifyReserved(ctx, scope, input, jobKey, receiptID)
	if err != nil {
		return s.Error(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
