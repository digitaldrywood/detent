package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeVerificationReopenState = "Todo"

func (s *Service) verifyNativeIssue(c echo.Context) error {
	var request tracker.VerifyFix
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.verifyNativeIssueCommand(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func nativeVerificationInvalid(message string) error {
	return &hubapi.NativeError{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

func nativeVerificationAnswerer(scope hubscope.NativeScope, issue tracker.NativeIssue) bool {
	actor := scope.Actor()
	if scope.Credential.Scope == hubapi.APIScopeWorker || actor.Kind != "human" || actor.PrincipalID == "" {
		return false
	}
	if actor.PrincipalID == issue.Actor.PrincipalID {
		return true
	}
	switch scope.Credential.HostedRole {
	case "", "member", "admin", "owner":
		return true
	}
	return false
}

func (s *Service) verifyNativeIssueCommand(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.VerifyFix) (json.RawMessage, error) {
	scope.Action = operatortool.VerifyFix
	scope.ActionItem = item
	switch request.Result {
	case tracker.VerifyResultVerified, tracker.VerifyResultNotFixed:
	default:
		return nil, nativeVerificationInvalid("result must be verified or not_fixed")
	}
	if request.Result == tracker.VerifyResultNotFixed && strings.TrimSpace(request.Comment) == "" {
		return nil, nativeVerificationInvalid("not_fixed requires a comment describing what is still broken")
	}
	if len(request.Comment) > 64<<10 {
		return nil, nativeVerificationInvalid("Comment must be at most 64 KiB")
	}
	if request.ExpectedRevision < 0 {
		return nil, hubapi.NativePublicInvalid("Expected revision must be positive")
	}
	options := hubapi.NativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+item+"/verification"), Item: item, Feature: "collaboration"}
	result, err := s.executeWorkItemRecordMutation(ctx, scope, options, request.Mutation, request, "")
	if err == nil && request.Result == tracker.VerifyResultNotFixed && s.wakeSpriteRunnersAfter != nil {
		s.wakeSpriteRunnersAfter(scope, result)
	}
	return result, err
}
