package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeVerificationReopenState = "Todo"

func (s *Service) verifyNativeIssue(c echo.Context) error {
	var request tracker.VerifyFix
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.verifyNativeIssueCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func nativeVerificationInvalid(message string) error {
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}

func nativeVerificationAnswerer(scope nativeScope, issue tracker.NativeIssue) bool {
	actor := scope.actor()
	if scope.credential.Scope == apiScopeWorker || actor.Kind != "human" || actor.PrincipalID == "" {
		return false
	}
	if actor.PrincipalID == issue.Actor.PrincipalID {
		return true
	}
	switch scope.credential.HostedRole {
	case "", "member", "admin", "owner":
		return true
	}
	return false
}

func (s *Service) verifyNativeIssueCommand(ctx context.Context, scope nativeScope, item string, request tracker.VerifyFix) (json.RawMessage, error) {
	scope.action = operatortool.VerifyFix
	scope.actionItem = item
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
		return nil, nativePublicInvalid("Expected revision must be positive")
	}
	options := nativeCommandOptions{OperationID: nativeOperation(scope, "POST", "/work-items/"+item+"/verification"), Item: item, Feature: "collaboration"}
	result, err := s.executeNativeIssueMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		issue, _, err := readNativeIssue(ctx, tx, scope, item)
		if err != nil {
			return nil, err
		}
		if issue.Profile != "native" {
			return nil, nativePublicInvalid("Compatibility project content is externally owned")
		}
		if !nativeVerificationAnswerer(scope, issue) {
			return nil, operatortool.ErrAccessDenied
		}
		if request.ExpectedRevision > 0 && request.ExpectedRevision != issue.Revision {
			return nil, nativeConflict(issue.Revision)
		}
		if !nativeDoneState(issue.State) || issue.Verification == nil || issue.Verification.State != tracker.VerificationPending {
			return nil, nativeVerificationInvalid("Issue is not awaiting verification")
		}
		if request.Result == tracker.VerifyResultVerified {
			if strings.TrimSpace(request.Comment) != "" {
				if _, err := insertNativeComment(ctx, tx, scope, issue, request.Comment, nil, now); err != nil {
					return nil, err
				}
			}
			actor := scope.actor()
			issue.Verification = &tracker.NativeVerification{State: tracker.VerificationVerified, VerifiedBy: &actor, VerifiedAt: &now}
			return persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Fields: []string{"verification"}, Verification: tracker.VerificationVerified}, now)
		}
		project, err := readNativeProject(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if err := validateNativeWorkflowTransition(project.States, issue.State, nativeVerificationReopenState, scope.credential.Scope); err != nil {
			return nil, err
		}
		if _, err := insertNativeComment(ctx, tx, scope, issue, request.Comment, nil, now); err != nil {
			return nil, err
		}
		from := issue.State
		issue.State = nativeVerificationReopenState
		issue.Archived = false
		return persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: issue.State, Reason: "user_requested", ReasonDetail: "Filer verification: not fixed"}, now)
	})
	if err == nil && request.Result == tracker.VerifyResultNotFixed {
		s.wakeSpriteRunnersAfter(scope, result)
	}
	return result, err
}
