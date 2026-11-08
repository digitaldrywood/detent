package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) updateMonthlyBudgetRecord(c echo.Context, scope nativeScope, request monthlyBudgetRequest) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return s.nativeAPIError(c, err)
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := "PUT " + c.Request().URL.EscapedPath()
	content := eventstore.NewContent(s.runDetailStore())
	readReceipt := func() (eventstore.Receipt, error) {
		var receipt eventstore.Receipt
		err := s.database.billingRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Writer) error {
			if err := r.LockAccessOrganization(ctx); err != nil {
				return err
			}
			if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
				return err
			}
			var err error
			receipt, err = r.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
			return err
		})
		return receipt, err
	}
	respond := func(receipt eventstore.Receipt) error {
		if receipt.ResponseExpired || receipt.ResponseReplayUntilNs <= s.config.now().UnixNano() {
			return s.nativeAPIError(c, receiptstore.ErrExpired)
		}
		if receipt.RequestHash != hash {
			return s.nativeAPIError(c, nativeConflict(0))
		}
		raw, err := content.Receipt(ctx, receipt)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSONBlob(http.StatusOK, raw)
	}
	if receipt, err := readReceipt(); err == nil {
		return respond(receipt)
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return s.nativeAPIError(c, err)
	}
	if err := request.Policy.Validate(); err != nil {
		return s.nativeAPIError(c, nativePublicInvalid(err.Error()))
	}
	if request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return s.nativeAPIError(c, nativePublicInvalid("Expected revision is invalid"))
	}
	policy := request.Policy
	policy.Mode = policy.EffectiveMode()
	raw, err := json.Marshal(monthlyBudgetSettings{Revision: request.ExpectedRevision + 1, Policy: policy})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.database.billingRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if previous, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
			if previous.ResponseExpired || previous.ResponseReplayUntilNs <= s.config.now().UnixNano() {
				return receiptstore.ErrExpired
			}
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			receipt = previous
			return nil
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
			return err
		}
		if scope.project != "" {
			if _, err := w.Project(ctx, scope.project); err != nil {
				return err
			}
		}
		current, err := w.BillingMonthlyPolicy(ctx, scope.project)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if current.Revision != request.ExpectedRevision {
			return nativeConflict(current.Revision)
		}
		if err := w.SetBillingMonthlyPolicy(ctx, billingstore.MonthlyPolicy{OrganizationID: scope.organization, ProjectID: scope.project, Revision: request.ExpectedRevision + 1, Policy: policy, Actor: scope.credential.ID, UpdatedAt: now}, request.ExpectedRevision); err != nil {
			return err
		}
		before, err := w.ArchiveBytes(ctx)
		if err != nil {
			return err
		}
		if _, err := w.PutReceipt(ctx, receipt); err != nil {
			return err
		}
		after, err := w.ArchiveBytes(ctx)
		if err != nil {
			return err
		}
		limit, err := s.attachmentQuotaLimit(ctx, w)
		if err != nil {
			return err
		}
		usage, err := w.AdjustCollaborationBytes(ctx, after-before, limit)
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
		if s.database.plans() == nil {
			return nil
		}
		entitlement, err := s.database.hostedEntitlementRecord(ctx, w, now)
		if err != nil {
			return err
		}
		if limit, limited := entitlement.Allowances["api_mutations"]; limited {
			plans := s.database.plans()
			window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
			usageID := artifact.Digest([]byte(scope.credential.ID + "\x00" + operation + "\x00" + request.IdempotencyKey))
			usage, err := w.ApplyBillingUsage(ctx, billingstore.UsageReceipt{OrganizationID: scope.organization, ID: "monthly-budget/" + usageID, WindowStart: window, Metric: "api_mutations", Amount: 1}, limit)
			if err != nil {
				return err
			}
			if !usage.Allowed {
				return &hostedLimitError{Resource: "api_mutations", Allowance: limit, Consumption: usage.Used}
			}
		}
		return nil
	})
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
		if committed, replayErr := readReceipt(); replayErr == nil {
			return respond(committed)
		} else if !errors.Is(replayErr, corestore.ErrNotFound) {
			return s.nativeAPIError(c, replayErr)
		}
		return s.nativeAPIError(c, nativeConflict(0))
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.notifyNativeDispatch(ctx, scope, request)
	return respond(receipt)
}
