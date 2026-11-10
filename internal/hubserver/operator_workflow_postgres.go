package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) resolveWorkflowItem(ctx context.Context, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	return s.resolveOperatorRecordReadItem(ctx, scope, reference)
}

func (s *Service) workflowProjectStates(ctx context.Context, scope nativeScope) ([]tracker.NativeState, error) {
	project, err := s.readTenantProjectRecord(ctx, scope)
	return project.States, err
}

func (s *Service) workflowCommandReplay(ctx context.Context, scope nativeScope, item string, transition tracker.Transition) (json.RawMessage, bool, error) {
	operation := nativeOperation(scope, http.MethodPost, "/work-items/"+item+"/workflow")
	payload := struct {
		tracker.Transition
		ConversationID string `json:"conversation_id,omitempty"`
	}{Transition: transition}
	hash, err := nativeCommandHash(payload)
	if err != nil {
		return nil, false, err
	}
	receipt, found, err := s.humanWorkflowReceipt(ctx, scope, item, operation, transition.IdempotencyKey, hash)
	if err != nil || !found {
		return nil, found, err
	}
	if receipt.ResponseExpired {
		return nil, true, receiptstore.ErrExpired
	}
	raw, err := s.readWorkItemRecordReceipt(ctx, scope, receipt, transition, item)
	return raw, true, err
}

func workflowSafeError(err error) error {
	var denied *operatortool.AuthorizationError
	if errors.As(err, &denied) {
		return err
	}
	if errors.Is(operatorAuthorizationError(err), operatortool.ErrProviderUnavailable) {
		return operatortool.ErrProviderUnavailable
	}
	if errors.Is(err, receiptstore.ErrExpired) {
		return receiptstore.ErrExpired
	}
	var failure *nativeError
	if errors.As(err, &failure) && failure.status == http.StatusGone && failure.Code == "idempotency_expired" {
		return receiptstore.ErrExpired
	}
	if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, operatortool.ErrProjectScopeRequired) {
		return &operatortool.AuthorizationError{Stage: "workflow", Cause: err}
	}
	safe := hubSafeChangeError(err)
	if errors.Is(safe, operatortool.ErrAccessDenied) {
		return &operatortool.AuthorizationError{Stage: "workflow", Cause: err}
	}
	if errors.Is(safe, mutation.ErrConflict) {
		if conflict := operatorNativeConflict(err); conflict != nil {
			return conflict
		}
	}
	return safe
}
