package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) changeReviewPolicyRecord(ctx context.Context, scope nativeScope) (tracker.ChangeReviewPolicy, error) {
	var rows currentChangePolicyRows
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeReviewPolicy{}, projectRecordError(err)
	}
	approved, err := hydrateChangeSummaryPolicy(ctx, eventstore.NewContent(s.runDetailStore()), scope, rows)
	return approved.ReviewPolicy, projectRecordError(err)
}

func (s *Service) currentChangePolicyRows(ctx context.Context, scope nativeScope) (currentChangePolicyRows, error) {
	var rows currentChangePolicyRows
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readCurrentChangePolicyRows(ctx, r, scope)
		return err
	})
	return rows, err
}

func (s *Service) approveChangeReviewPolicyRecord(c echo.Context, request tracker.ApproveChangeReviewPolicy) error {
	scope := nativeRequestScope(c)
	if request.Policy.PolicyID == "" {
		return s.nativeAPIError(c, nativePublicInvalid("An approved repository policy identity is required"))
	}
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if found {
		return c.JSONBlob(http.StatusOK, replay)
	}
	now, err := s.database.currentTime()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	var rows currentChangePolicyRows
	var current eventstore.ReviewPolicy
	var oldRules tracker.ChangeReviewPolicy
	currentStored := false
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	approval, err := hydrateChangeSummaryPolicy(ctx, content, scope, rows)
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	if request.Policy.PolicyID != approval.Policy.ID {
		return s.nativeAPIError(c, policyMismatch("Review policy must name the current repository policy"))
	}
	current, currentStored = rows.Review, rows.hasReview
	oldRules = approval.ReviewPolicy
	if oldRules.ID != request.ExpectedID && (request.ExpectedID != "" || !followsRepositoryGate(oldRules)) {
		return s.nativeAPIError(c, policyMismatch("Review policy changed; supply its current identity"))
	}
	rules := request.Policy
	rules.ID = changerequest.PolicyID(rules)
	if err := changerequest.ValidatePolicy(rules, approval.Policy); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if err := s.validateReviewPolicyPrincipals(ctx, scope, rules, now); err != nil {
		return s.nativeAPIError(c, err)
	}
	record, err := content.PrepareReviewPolicy(ctx, eventstore.ReviewPolicy{OrganizationID: scope.organization, ProjectID: scope.project, PolicyRevisionID: approval.Policy.ID, Revision: current.Revision + 1, UpdatedBy: scope.actor().PrincipalID, UpdatedAt: now}, rules)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var expected string
	if currentStored {
		expected = current.ReviewPolicyID
	}
	response, err := json.Marshal(rules)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		projectPolicy, err := w.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if projectPolicy.OrganizationID != rows.Project.OrganizationID || projectPolicy.ProjectID != rows.Project.ProjectID || projectPolicy.PolicyID != rows.Project.PolicyID || projectPolicy.ApprovedBy != rows.Project.ApprovedBy || !projectPolicy.ApprovedAt.Equal(rows.Project.ApprovedAt) || projectPolicy.PolicyID != request.Policy.PolicyID {
			return policyMismatch("Approved repository policy changed during review-policy update")
		}
		currentReview, currentErr := w.ReviewPolicy(ctx, scope.project)
		if rows.hasReview {
			if currentErr != nil || currentReview.ReviewPolicyID != rows.Review.ReviewPolicyID || currentReview.Revision != rows.Review.Revision || currentReview.PolicyRevisionID != rows.Review.PolicyRevisionID {
				return policyMismatch("Review policy changed during update")
			}
		} else if currentErr == nil || !errors.Is(currentErr, corestore.ErrNotFound) {
			return policyMismatch("Review policy changed during update")
		}
		if err := s.validateReviewPolicyPrincipalsIn(ctx, w, scope, rules, now); err != nil {
			return err
		}
		if prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
			if prior.RequestHash != hash {
				return nativeConflict(0)
			}
			return corestore.ErrSerializationFailure
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if !currentStored || current.ReviewPolicyID != rules.ID {
			if err := w.SetReviewPolicy(ctx, record, expected); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
		replay, found, readErr := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
		if readErr != nil {
			return s.nativeAPIError(c, readErr)
		}
		if found {
			return c.JSONBlob(http.StatusOK, replay)
		}
		return s.nativeAPIError(c, err)
	}
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return c.JSON(http.StatusOK, rules)
}

func (s *Service) validateReviewPolicyPrincipals(ctx context.Context, scope nativeScope, rules tracker.ChangeReviewPolicy, now time.Time) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.validateReviewPolicyPrincipalsIn(ctx, r, scope, rules, now)
	})
}

func (s *Service) validateReviewPolicyPrincipalsIn(ctx context.Context, r tenantstore.Reader, scope nativeScope, rules tracker.ChangeReviewPolicy, now time.Time) error {
	for _, check := range rules.RequiredChecks {
		credential, err := r.AccessCredential(ctx, check.PrincipalID)
		if err != nil || credential.RevokedAt != nil || credential.CreatedAt.After(now) || credential.ExpiresAt != nil && !now.Before(*credential.ExpiresAt) {
			return nativePublicInvalid("Expected CI principals must have an active project grant")
		}
		if check.Source == "independent" && credential.Scope == string(apiScopeWorker) {
			return nativePublicInvalid("Independent validation requires an operator credential")
		}
		if _, err := r.AccessTokenGrant(ctx, check.PrincipalID, scope.project); err != nil {
			return nativePublicInvalid("Expected CI principals must have an active project grant")
		}
	}
	return nil
}
