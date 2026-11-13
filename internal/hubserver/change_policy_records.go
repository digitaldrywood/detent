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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) changeReviewPolicyRecord(ctx context.Context, scope hubscope.NativeScope) (tracker.ChangeReviewPolicy, error) {
	var rows currentChangePolicyRows
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return tracker.ChangeReviewPolicy{}, hubapi.ProjectRecordError(err)
	}
	approved, err := hydrateChangeSummaryPolicy(ctx, eventstore.NewContent(s.runDetailStore()), scope, rows)
	return approved.ReviewPolicy, hubapi.ProjectRecordError(err)
}

func (s *Service) currentChangePolicyRows(ctx context.Context, scope hubscope.NativeScope) (currentChangePolicyRows, error) {
	var rows currentChangePolicyRows
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readCurrentChangePolicyRows(ctx, r, scope)
		return err
	})
	return rows, err
}

func (s *Service) approveChangeReviewPolicyRecord(c echo.Context, request tracker.ApproveChangeReviewPolicy) error {
	response, err := s.approveChangeReviewPolicyRecordCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Request().Method+" "+c.Request().URL.EscapedPath(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Service) approveChangeReviewPolicyRecordCommand(ctx context.Context, scope hubscope.NativeScope, operation string, request tracker.ApproveChangeReviewPolicy) (json.RawMessage, error) {
	if request.Policy.PolicyID == "" {
		return nil, hubapi.NativePublicInvalid("An approved repository policy identity is required")
	}
	ctx = hubcontent.IsolateContentContext(s.runDetailContext(ctx))
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return replay, nil
	}
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	var rows currentChangePolicyRows
	var current eventstore.ReviewPolicy
	var oldRules tracker.ChangeReviewPolicy
	currentStored := false
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	approval, err := hydrateChangeSummaryPolicy(ctx, content, scope, rows)
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if request.Policy.PolicyID != approval.Policy.ID {
		return nil, hubapi.PolicyMismatch("Review policy must name the current repository policy")
	}
	current, currentStored = rows.Review, rows.hasReview
	oldRules = approval.ReviewPolicy
	if oldRules.ID != request.ExpectedID && (request.ExpectedID != "" || !followsRepositoryGate(oldRules)) {
		return nil, hubapi.PolicyMismatch("Review policy changed; supply its current identity")
	}
	rules := request.Policy
	rules.ID = changerequest.PolicyID(rules)
	if err := changerequest.ValidatePolicy(rules, approval.Policy); err != nil {
		return nil, hubapi.NativeInvalid(err.Error())
	}
	if err := s.validateReviewPolicyPrincipals(ctx, scope, rules, now); err != nil {
		return nil, err
	}
	record, err := content.PrepareReviewPolicy(ctx, eventstore.ReviewPolicy{OrganizationID: scope.Organization, ProjectID: scope.Project, PolicyRevisionID: approval.Policy.ID, Revision: current.Revision + 1, UpdatedBy: scope.Actor().PrincipalID, UpdatedAt: now}, rules)
	if err != nil {
		return nil, err
	}
	var expected string
	if currentStored {
		expected = current.ReviewPolicyID
	}
	response, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		projectPolicy, err := w.ProjectPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		if projectPolicy.OrganizationID != rows.Project.OrganizationID || projectPolicy.ProjectID != rows.Project.ProjectID || projectPolicy.PolicyID != rows.Project.PolicyID || projectPolicy.ApprovedBy != rows.Project.ApprovedBy || !projectPolicy.ApprovedAt.Equal(rows.Project.ApprovedAt) || projectPolicy.PolicyID != request.Policy.PolicyID {
			return hubapi.PolicyMismatch("Approved repository policy changed during review-policy update")
		}
		currentReview, currentErr := w.ReviewPolicy(ctx, scope.Project)
		if rows.hasReview {
			if currentErr != nil || currentReview.ReviewPolicyID != rows.Review.ReviewPolicyID || currentReview.Revision != rows.Review.Revision || currentReview.PolicyRevisionID != rows.Review.PolicyRevisionID {
				return hubapi.PolicyMismatch("Review policy changed during update")
			}
		} else if currentErr == nil || !errors.Is(currentErr, corestore.ErrNotFound) {
			return hubapi.PolicyMismatch("Review policy changed during update")
		}
		if err := s.validateReviewPolicyPrincipalsIn(ctx, w, scope, rules, now); err != nil {
			return err
		}
		if prior, err := w.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey); err == nil {
			if prior.RequestHash != hash {
				return hubapi.NativeConflict(0)
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
			return nil, readErr
		}
		if found {
			return replay, nil
		}
		return nil, err
	}
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
	return response, nil
}

func (s *Service) validateReviewPolicyPrincipals(ctx context.Context, scope hubscope.NativeScope, rules tracker.ChangeReviewPolicy, now time.Time) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.validateReviewPolicyPrincipalsIn(ctx, r, scope, rules, now)
	})
}

func (s *Service) validateReviewPolicyPrincipalsIn(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, rules tracker.ChangeReviewPolicy, now time.Time) error {
	for _, check := range rules.RequiredChecks {
		credential, err := r.AccessCredential(ctx, check.PrincipalID)
		if err != nil || credential.RevokedAt != nil || credential.CreatedAt.After(now) || credential.ExpiresAt != nil && !now.Before(*credential.ExpiresAt) {
			return hubapi.NativePublicInvalid("Expected CI principals must have an active project grant")
		}
		if check.Source == "independent" && credential.Scope == string(hubapi.APIScopeWorker) {
			return hubapi.NativePublicInvalid("Independent validation requires an operator credential")
		}
		if _, err := r.AccessTokenGrant(ctx, check.PrincipalID, scope.Project); err != nil {
			return hubapi.NativePublicInvalid("Expected CI principals must have an active project grant")
		}
	}
	return nil
}
