package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeOrganizationIssuePath = "/api/v2/organizations/:organization/work-items/:item"

func nativeRejection(err error, category, field string) error {
	var failure *hubapi.NativeError
	rejection := runnerauth.PublicationRejection{Category: category, Field: field}
	if !errors.As(err, &failure) || failure.Code != "invalid_request" || !rejection.Valid() {
		return err
	}
	result := &hubapi.NativeError{Code: failure.Code, Message: rejection.DiagnosticError(), Status: failure.Status, Details: map[string]any{"rejection": rejection}}
	switch category {
	case "event_validation", "runtime_validation", "execution_validation", "usage_validation", "owner_validation", "body_decode":
		result.Message, result.PublicMessage = failure.Message, true
	}
	return result
}

func operatorAuthorizationCause(err error) string {
	for _, cause := range []struct {
		err  error
		code string
	}{
		{context.DeadlineExceeded, "deadline_exceeded"},
		{context.Canceled, "cancelled"},
		{pgtransport.ErrOpenResults, "open_results"},
		{pgtransport.ErrUnbound, "unbound_transaction"},
		{pgx.ErrTxClosed, "transaction_closed"},
		{corestore.ErrSerializationFailure, "serialization_failure"},
		{sql.ErrConnDone, "connection_closed"},
		{driver.ErrBadConn, "bad_connection"},
		{accessstore.ErrDenied, "access_denied"},
		{auth.ErrInvalidSession, "invalid_session"},
		{auth.ErrHostedIdentity, "hosted_identity"},
		{operatortool.ErrAccessDenied, "operator_access_denied"},
		{sql.ErrNoRows, "not_found"},
	} {
		if errors.Is(err, cause.err) {
			return cause.code
		}
	}
	return "unclassified"
}

func (s *Service) nativeAPIError(c echo.Context, err error, attributes ...any) error {
	err = hubapi.FromFeatureError(err)
	var deniedAuthorization *operatortool.AuthorizationError
	authorizationFailure := errors.As(err, &deniedAuthorization)
	authorizationUnavailable := errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid
	if authorizationUnavailable || authorizationFailure || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || errors.Is(err, operatortool.ErrProviderUnavailable) {
		if classified := hubscope.OperatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
			err = classified
		}
	}
	if errors.Is(err, corestore.ErrSerializationFailure) {
		message := "Database transaction was aborted; retry the request"
		s.config.Logger.Error(message, append([]any{"error", err}, postgresErrorAttributes(err)...)...)
		return c.JSON(http.StatusServiceUnavailable, hubapi.APIErrorResponse{Code: "service_unavailable", Message: message})
	}
	if errors.Is(err, receiptstore.ErrExpired) {
		return s.nativeAPIError(c, commandResponseExpired(), attributes...)
	}
	if errors.Is(err, mutation.ErrUncertain) {
		return s.refusedAPIError(c, http.StatusConflict, "idempotency_in_progress", mutation.ErrUncertain.Error(), hubapi.APIErrorResponse{Code: "idempotency_in_progress", Message: mutation.ErrUncertain.Error()})
	}
	if errors.Is(err, searchstore.ErrIndexPending) {
		return c.JSON(http.StatusServiceUnavailable, hubapi.APIErrorResponse{Code: "not_ready", Message: "Search index refresh is pending"})
	}
	var archive *hubapi.NativeArchiveError
	if errors.As(err, &archive) {
		return c.JSON(http.StatusServiceUnavailable, struct {
			Code    string                     `json:"code"`
			Archive *tracker.ArchiveDescriptor `json:"archive"`
		}{"archive_" + archive.Archive.Availability, archive.Archive})
	}
	var authorization *hubscope.OperatorAuthorizationFailure
	if errors.As(err, &authorization) {
		code := "service_unavailable"
		message := authorization.Error()
		if errors.Is(authorization, operatortool.ErrProviderUnavailable) {
			code = auth.HostedReasonProviderUnavailable
		} else if authorizationUnavailable {
			message = "Authorization is temporarily unavailable"
		}
		cause := authorization.Cause
		if deniedAuthorization != nil && deniedAuthorization.Cause != nil {
			cause = deniedAuthorization.Cause
		}
		step := "native_authorization"
		var lookup *accessstore.LookupError
		if errors.As(cause, &lookup) {
			step, cause = lookup.Step, lookup.Cause
		}
		causeClass := cause
		for errors.Unwrap(causeClass) != nil {
			causeClass = errors.Unwrap(causeClass)
		}
		scope := hubscope.NativeRequestScope(c)
		attrs := []any{"error", authorization, "authorization_cause", operatorAuthorizationCause(cause), "authorization_step", step, "cause_class", fmt.Sprintf("%T", causeClass),
			"method", c.Request().Method, "route", c.Path(), "organization_id", string(scope.Organization), "project_id", string(scope.Project)}
		if deniedAuthorization != nil {
			attrs = append(attrs, "authorization_stage", deniedAuthorization.Stage)
		}
		attrs = append(attrs, attributes...)
		attrs = append(attrs, postgresErrorAttributes(cause)...)
		s.config.Logger.Error("operator authorization unavailable", attrs...)
		return c.JSON(http.StatusServiceUnavailable, hubapi.APIErrorResponse{Code: code, Message: message})
	}
	var export *operatortool.WorkBulkExportItemError
	if errors.As(err, &export) {
		s.config.Logger.Error(export.Message, append([]any{"error", err}, s.database.readTimeoutAttributes(err)...)...)
		return c.JSON(http.StatusInternalServerError, export)
	}
	var refusal *apikey.Refusal
	if errors.As(err, &refusal) {
		return s.refusedAPIError(c, http.StatusForbidden, refusal.Code, refusal.Message, refusal)
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		message := operatortool.ErrAccessDenied.Error()
		var denied *operatortool.AuthorizationError
		if errors.As(err, &denied) {
			attributes = append(attributes, "authorization_stage", denied.Stage, "authorization_cause", operatorAuthorizationCause(denied.Cause), "cause_class", fmt.Sprintf("%T", denied.Cause), "principal_id", denied.PrincipalID, "authorization_organization_id", denied.OrganizationID, "authorization_project_id", denied.ProjectID)
		}
		return s.refusedAPIError(c, http.StatusForbidden, "access_denied", message, hubapi.APIErrorResponse{Code: "access_denied", Message: message}, attributes...)
	}
	if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, auth.ErrInvalidSession) {
		return s.refusedAPIError(c, http.StatusForbidden, "access_denied", "Access is no longer available", hubapi.APIErrorResponse{Code: "access_denied", Message: "Access is no longer available"}, append(attributes, "authorization_cause", operatorAuthorizationCause(err), "hosted_reason", auth.HostedIdentityReason(err))...)
	}
	var limit *hostedplans.LimitError
	if errors.As(err, &limit) {
		return s.refusedAPIError(c, http.StatusTooManyRequests, "allowance_exhausted", limit.Error(), struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			*hostedplans.LimitError
		}{"allowance_exhausted", limit.Error(), limit})
	}
	var failure *hubapi.NativeError
	if errors.Is(err, artifact.ErrStorage) {
		message := "Immutable content storage is unavailable; retry when storage is restored"
		if errors.Is(err, hubcontent.ErrContentStorageUnconfigured) {
			message = "Immutable content storage is not configured; configure object storage to persist payloads"
		}
		failure = &hubapi.NativeError{Code: "storage_unreachable", Message: message, Status: http.StatusServiceUnavailable, PublicMessage: true}
		return s.refusedAPIError(c, failure.Status, failure.Code, failure.Message, failure, attributes...)
	}
	if errors.As(err, &failure) {
		attributes = append(attributes, failure.LogAttributes...)
		message := failure.Message
		if failure.Status >= http.StatusBadRequest && failure.Status < http.StatusInternalServerError && !failure.PublicMessage && failure.Code != "checkout_unavailable" {
			message = http.StatusText(failure.Status) + ": " + strings.ReplaceAll(failure.Code, "_", " ")
			if rejection, ok := failure.Details["rejection"].(runnerauth.PublicationRejection); ok && rejection.Valid() {
				message = rejection.DiagnosticError()
			}
		}
		if s.config.Hosted != nil && !failure.PublicMessage {
			if failure.Status >= http.StatusInternalServerError && failure.Code != "checkout_unavailable" {
				message = "The requested operation is unavailable"
			}
			return s.refusedAPIError(c, failure.Status, failure.Code, message, &hubapi.NativeError{Code: failure.Code, Message: message, CurrentRevision: failure.CurrentRevision, Details: failure.Details, Status: failure.Status}, attributes...)
		}
		return s.refusedAPIError(c, failure.Status, failure.Code, message, failure, attributes...)
	}
	var conflict *operatortool.ConflictError
	if errors.As(err, &conflict) {
		return s.refusedAPIError(c, http.StatusConflict, conflict.Code, conflict.Message, conflict)
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, hubapi.NativeNotFound(), "not_found_cause", err.Error())
	}
	if errors.Is(err, tracker.ErrInvalidClaimRequest) || errors.Is(err, tracker.ErrInvalidLeaseRequest) || errors.Is(err, tracker.ErrInvalidWorkEvent) || errors.Is(err, tracker.ErrInvalidCandidateQuery) {
		return s.trackerAPIError(c, err)
	}
	if errors.Is(err, tracker.ErrLeaseConflict) || errors.Is(err, tracker.ErrStaleFencingToken) || errors.Is(err, tracker.ErrLeaseNotFound) || errors.Is(err, tracker.ErrMachineNotFound) || errors.Is(err, tracker.ErrWorkItemNotFound) || errors.Is(err, ErrNoClaimableWork) {
		return s.trackerAPIError(c, err)
	}
	var request *operatortool.RequestError
	if errors.As(err, &request) {
		return s.refusedAPIError(c, http.StatusUnprocessableEntity, request.Code, request.Message, request)
	}
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, leasestore.ErrAttemptConflict) {
		return s.nativeAPIError(c, hubapi.NativeConflict(0), attributes...)
	}
	if errors.Is(err, mutation.ErrConflict) {
		return s.nativeAPIError(c, &hubapi.NativeError{Code: "idempotency_conflict", Message: "Request was already used with different content", Status: http.StatusConflict, PublicMessage: true}, attributes...)
	}
	var mismatch *hubrecords.NativeAttemptIdentityMismatch
	if errors.As(err, &mismatch) {
		attrs := []any{"error", err, "attempt_id", mismatch.AttemptID, "work_item_id", mismatch.WorkItemID, "identity_field", mismatch.Field}
		if mismatch.Values != nil {
			attrs = append(attrs, "expected_value", mismatch.Values.Expected, "actual_value", mismatch.Values.Actual)
		}
		if mismatch.Field == "updated_at" {
			attrs = append(attrs, "timestamp_unit", "microseconds")
		}
		s.config.Logger.Error("Native operation could not be completed", attrs...)
		return c.JSON(http.StatusInternalServerError, hubapi.APIErrorResponse{Code: "native_operation_failed", Message: "Native operation could not be completed"})
	}
	return s.internalAPIError(c, "native_operation_failed", "Native operation could not be completed", err)
}

func (s *Service) registerNativeRoutes(e *echo.Echo) {
	s.registerOnboardingRoutes(e)
	e.POST(hubapi.NativeBase+"/local-board-import", s.importLocalBoard, s.requireNativeScope(hubapi.APIScopeOperator))
	s.registerArtifactRoutes(e)
	s.registerReleaseAuditRoutes(e)
	s.registerCloudAttachmentRoutes(e)
	s.registerAttemptDiffBodyRoutes(e)
	s.registerIntegrationRoutes(e)
	s.registerChangeRoutes(e)
	s.registerBudgetOverrideRoutes(e)
	s.registerConfigurationRankRoutes(e)
	read := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	e.GET(hubapi.NativeBase+"/runner-spend", s.getRunnerSpend, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/attempts/:attempt/token-accounting", s.accountRunnerUsage, s.requireNativeScope(hubapi.APIScopeOperator))
	e.GET(hubapi.NativeBase+"/security-audits/evidence", s.getAuditEvidence, read)
	e.GET(hubapi.NativeBase+"/security-audits/:session/dispositions", s.getAuditDispositions, read)
	e.GET(hubapi.NativeBase+"/runner-model-quantiles", s.getRunnerModelQuantiles, read)
	e.GET(hubapi.NativeBase+"/health/findings", s.getHealthFindings, read)
	e.GET(hubapi.NativeBase+"/runner-diagnostics", s.getRunnerProjectDiagnostics, read)
	write := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	e.POST(hubapi.NativeBase+"/security-audits/dispositions", s.postAuditDisposition, write)
	admin := s.requireInstanceAdmin()
	policyAdmin := admin
	if s.config.Hosted != nil {
		policyAdmin = s.requireOnboardingAdmin()
	}
	worker := s.requireNativeScope(hubapi.APIScopeWorker)
	e.GET(hubapi.NativeBase+"/runner-sessions/:session", s.getRunnerSession, read)
	e.GET(hubapi.NativeBase+"/runner-sessions/:session/history/:revision", s.getRunnerSessionHistory, read)
	e.GET(hubapi.NativeBase+"/runner-processes", s.getRunnerSessionProcesses, worker)
	e.POST(hubapi.NativeBase+"/runner-sessions/find", s.findRunnerSession, read)
	e.POST(hubapi.NativeBase+"/runner-sessions", s.mutateRunnerSession, worker)
	e.GET(hubapi.NativeBase+"/policy", s.getProjectPolicy, s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	e.PUT(hubapi.NativeBase+"/policy", s.approveProjectPolicy, policyAdmin)
	e.DELETE(hubapi.NativeBase+"/policy", s.revokeProjectPolicy, policyAdmin)
	e.POST(hubapi.NativeBase+"/policy/observed", s.observeProjectPolicy, worker)
	e.GET("/api/v2/capabilities", s.nativeCapabilities, s.requireAPIScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	organizationRead := s.requireAPIScope(hubapi.APIScopeAdmin)
	if s.config.Hosted == nil {
		organizationRead = admin
	}
	e.GET("/api/v2/organizations", s.nativeOrganizations, organizationRead)
	e.POST("/api/v2/organizations", s.createNativeOrganization, admin)
	if s.config.Hosted != nil {
		e.POST("/api/v2/organizations/:organization/projects", s.createHostedProjectJSON, admin)
		e.DELETE(hubapi.NativeBase, s.deleteCloudProject, s.requireOnboardingAdmin())
	} else {
		e.POST("/api/v2/organizations/:organization/projects", s.createNativeProject, admin)
	}
	e.POST("/api/v2/tokens/:id/grants", s.grantNativeToken, admin)
	e.GET(hubapi.NativeBase, s.getNativeProject, read)
	e.PATCH(hubapi.NativeBase, s.renameProject, s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	e.PUT(hubapi.NativeBase+"/icon", s.setProjectIcon, s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	if s.config.Hosted == nil {
		organization := s.requireConversationOrganization()
		e.GET(hostedOrganizationBase+"/logo", s.getOrganizationLogo, organization)
		e.PUT(hostedOrganizationBase+"/logo", s.setOrganizationLogo, organization)
		e.GET(hostedOrganizationBase+"/attachment-metadata/:attachment", s.getOrganizationLogoAttachment, organization)
		e.GET(hostedOrganizationBase+"/logo-metadata", s.getCurrentOrganizationLogoAttachment, organization)
	}
	s.registerNativeLabelRoutes(e, read)
	e.GET(hubapi.NativeBase+"/work-items", s.listNativeIssues, read)
	e.GET(hubapi.NativeBase+"/board/counts", s.boardModelCounts, read)
	e.GET(hubapi.NativeBase+"/board/status", s.boardModelStatus, read)
	e.GET(hubapi.NativeBase+"/work-items/export", s.exportNativeWorkItems, read)
	e.POST(hubapi.NativeBase+"/work-items", s.createNativeIssue, write)
	e.POST(hubapi.NativeBase+"/scheduled-ci-decisions", s.classifyScheduledFailure, s.requireNativeScope(hubapi.APIScopeOperator))
	e.GET(nativeOrganizationIssuePath, s.getNativeIssue, read)
	e.GET(hubapi.NativeBase+"/work-items/:item", s.getNativeIssue, read)
	e.PATCH(hubapi.NativeBase+"/work-items/:item", s.updateNativeIssue, write)
	e.POST(hubapi.NativeBase+"/work-items/:item/archive", s.archiveNativeIssue, write)
	e.POST(hubapi.NativeBase+"/work-items/:item/restore", s.restoreNativeIssue, write)
	e.POST(hubapi.NativeBase+"/work-items/:item/source-intake", s.intakeLinkedIssue, write)
	e.POST(hubapi.NativeBase+"/work-items/:item/workflow", s.transitionNativeIssue, write)
	e.POST(hubapi.NativeBase+"/work-items/:item/verification", s.verifyNativeIssue, write)
	e.POST(hubapi.NativeBase+"/work-items/:item/dependencies", s.changeNativeDependency, write)
	e.POST(hubapi.NativeBase+"/work-items/:item/dependency-reasons", s.updateNativeDependencyReason, write)
	e.GET(hubapi.NativeBase+"/work-items/:item/comments", s.listNativeComments, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/comments", s.createNativeComment, write)
	e.PATCH(hubapi.NativeBase+"/work-items/:item/comments/:comment", s.updateNativeComment, write)
	e.GET(hubapi.NativeBase+"/work-items/:item/history", s.listNativeHistory, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/runtime", s.getNativeRuntime, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/progress", s.getNativeProgress, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/progress", s.publishNativeProgress, worker)
	e.GET(hubapi.NativeBase+"/work-items/:item/explanation", s.getNativeExplanation, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/source-recovery", s.getNativeSourceRecovery, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/source-recovery", s.transferNativeSource, write)
	e.GET(hubapi.NativeBase+"/work-items/:item/runtime/github-timings", s.getNativeGitHubTimings, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/attempts", s.listNativeAttempts, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/attempts/:attempt", s.getNativeAttempt, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/versions/:revision", s.getNativeVersion, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/comments/:comment/versions/:revision", s.getNativeVersion, read)
	e.POST(hubapi.NativeBase+"/claims", s.claimNativeIssue, worker)
	e.POST(hubapi.NativeBase+"/claims/preview", s.previewProviderCandidates, worker)
	e.GET(hubapi.NativeBase+"/claims/wait", s.waitNativeCandidates, worker)
	e.POST(hubapi.NativeBase+"/leases/:lease/renew", s.renewNativeLease, worker)
	e.POST(hubapi.NativeBase+"/leases/:lease/release", s.releaseNativeLease, worker)
	e.POST(hubapi.NativeBase+"/machines/register", s.registerNativeMachine, worker)
	e.POST(hubapi.NativeBase+"/work-items/:item/events", s.appendNativeRunEvent, worker)
	e.POST(hubapi.NativeBase+"/go-test-receipts/attestations", s.attestGoTestReceipts, worker)
	e.GET(hubapi.NativeBase+"/go-test-receipts", s.lookupGoTestReceipts, worker)
	e.POST(hubapi.NativeBase+"/go-test-receipts", s.recordGoTestReceipt, worker)
	// Stored attempt diffs and the pull request panel.
	// The diff write is a worker endpoint fenced by the
	// producer's lease; every read follows the issue's read rule.
	e.POST(hubapi.NativeBase+"/attempts/:attempt/diff", s.postAttemptDiff, worker)
	e.POST(hubapi.NativeBase+"/attempts/:attempt/diff/check", s.checkAttemptDiff, worker)
	e.GET(hubapi.NativeBase+"/attempts/:attempt/diff", s.getAttemptDiff, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/diff", s.getWorkItemDiff, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/pull-requests", s.listWorkItemPullRequests, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/release-pick", s.associateReleasePick, write)
	e.GET(hubapi.NativeBase+"/work-items/:item/release-pick", s.getReleaseDeliveryStatus, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/release-pick/publication", s.publishReleasePick, write)
	e.GET(hubapi.NativeBase+"/workspaces", s.listWorkspaces, read)
	e.GET(hubapi.NativeBase+"/workspaces/:workspace", s.getWorkspace, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/workspace", s.workspaceForWorkItem, worker)
	e.POST(hubapi.NativeBase+"/workspaces/:workspace/worker/bind", s.bindWorkspaceWorker, worker)
	e.POST(hubapi.NativeBase+"/workspaces/:workspace/worker/heartbeat", s.heartbeatWorkspaceWorker, worker)
	e.POST(hubapi.NativeBase+"/workspaces/:workspace/worker/unbind", s.unbindWorkspaceWorker, worker)
	e.POST(hubapi.NativeBase+"/workspaces", s.createWorkspace, write)
	e.DELETE(hubapi.NativeBase+"/workspaces/:workspace", s.deleteWorkspace, write)
	e.POST(hubapi.NativeBase+"/workspaces/:workspace/relay-tickets", s.mintWorkspaceRelayTicket, read)
	e.GET(hubapi.NativeBase+"/workspaces/:workspace/relay", s.openWorkspaceRelay, read)
	e.GET(hubapi.NativeBase+"/workspaces/:workspace/worker/relay", s.openWorkspaceWorkerRelay, worker)
	e.POST(hubapi.NativeBase+"/workspaces/:workspace/worker/action-runs", s.reportWorkspaceActionRun, worker)
	e.GET(hubapi.NativeBase+"/actions", s.listProjectActions, read)
	e.POST(hubapi.NativeBase+"/actions", s.createProjectAction, write)
	e.PATCH(hubapi.NativeBase+"/actions/:action", s.patchProjectAction, write)
	e.DELETE(hubapi.NativeBase+"/actions/:action", s.deleteProjectAction, write)
	e.GET(hubapi.NativeBase+"/actions/:action/runs", s.listProjectActionRuns, read)
	e.POST(hubapi.NativeBase+"/actions/:action/runs", s.createProjectActionRun, write)
	e.GET(hubapi.NativeBase+"/actions/:action/runs/:run", s.getProjectActionRun, read)
	e.GET(hubapi.NativeBase+"/actions/:action/runs/:run/output", s.getProjectActionRunOutput, read)
	// Terminal recordings. They are mounted under the
	// project's read scope like everything else, and then narrowed again by the
	// handler: a recording's audience is the person who ran it plus owners and
	// admins, and a user-isolation recording's is owners alone, which is
	// narrower than any scope the router can express.
	e.GET(hubapi.NativeBase+"/workspaces/:workspace/terminal-recordings", s.listWorkspaceTerminalRecordings, read)
	e.GET(hubapi.NativeBase+"/workspaces/:workspace/terminal-recordings/:recording", s.getWorkspaceTerminalRecording, read)
}

func (s *Service) requireInstanceAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if s.config.Hosted != nil {
			return s.requireHostedAdministration(next)
		}
		return s.requireAPIScope(hubapi.APIScopeAdmin)(func(c echo.Context) error {
			credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
			if !ok || credential.NativeOnly {
				return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "Instance administrator is required", hubapi.APIErrorResponse{Code: "insufficient_scope", Message: "Instance administrator is required"})
			}
			return next(c)
		})
	}
}

func (s *Service) requireNativeScope(roles ...hubapi.APIScope) echo.MiddlewareFunc {
	return s.requireHubauthNative(roles...)
}

func (s *Service) resolveNativeRequestScope(readCtx context.Context, c echo.Context) error {
	return s.resolveHubauthNativeScope(readCtx, c)
}

func (d *database) authorizeNativeProject(ctx context.Context, scope hubscope.NativeScope) error {
	if d.postgres == nil {
		return hubapi.NativeNotFound()
	}
	service := &Service{config: Config{Clock: d.now}}
	return d.postgres.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if scope.Credential.Runner.RunnerID != "" {
			if scope.Credential.Runner.OrganizationID != scope.Organization {
				return hubapi.NativeNotFound()
			}
			identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
			if err != nil {
				return hubapi.ProjectRecordError(err)
			}
			if identity.RunnerID != scope.Credential.Runner.RunnerID {
				return hubapi.NativeNotFound()
			}
			allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, d.now())
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
			}
		} else if err := service.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return hubapi.ProjectRecordError(err)
		}
		if project.DeletedAt != nil {
			return hubapi.NativeNotFound()
		}
		return nil
	})
}

func authorizeNativeProject(ctx context.Context, db hubapi.NativeQueryer, scope hubscope.NativeScope) error {
	if scope.Credential.Runner.RunnerID != "" && scope.Credential.Runner.OrganizationID != scope.Organization {
		return hubapi.NativeNotFound()
	}
	var count int
	query := "SELECT count(*) FROM projects WHERE organization_id = ? AND id = ?"
	args := []any{scope.Organization, scope.Project}
	condition, grantArgs := scope.Credential.ProjectGrantSQL("projects.organization_id", "projects.id")
	query += " AND (" + condition + ")"
	args = append(args, grantArgs...)
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return hubapi.NativeNotFound()
	}
	return nil
}

func nativeIntegrationActor(principalID string) *tracker.Actor {
	return &tracker.Actor{Kind: "integration", PrincipalID: principalID}
}

func (s *Service) nativeMutation(c echo.Context, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, hubscope.NativeScope, time.Time) (any, error)) error {
	return s.nativeMutationStatus(c, http.StatusOK, command, input, operation)
}

// nativeMutationStatus is nativeMutation with the success status the route
// reports. A replay answers with the same status as the first call, so a
// created resource stays 201 on every retry of its key.
func (s *Service) nativeMutationStatus(c echo.Context, status int, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, hubscope.NativeScope, time.Time) (any, error)) error {
	options := hubapi.NativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Item: c.Param("item"), ChangeID: c.Param("change"), VersionID: c.Param("version"), CheckPrincipal: changeCheckRequest(c), RequireLease: c.Param("item") != "" && !strings.HasSuffix(c.Path(), "/events") && c.Path() != changeBase+"/:change/versions/:version/checks", Completion: hostedCompletionMutation(c, input), Feature: hostedMutationFeature(c)}
	result, err := s.executeNativeMutation(c.Request().Context(), hubscope.NativeRequestScope(c), options, command, input, operation)
	if err != nil {
		if request, ok := input.(tracker.NativeRunEvent); ok {
			return s.nativeRunEventError(c, request, err)
		}
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(status, result)
}

func (s *Service) executeNativeMutation(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, hubscope.NativeScope, time.Time) (any, error)) (result json.RawMessage, resultErr error) {
	if request, ok := input.(projectSpriteAllocationRequest); ok {
		return s.setProjectSpriteAllocationRecord(ctx, scope, options.OperationID, request)
	}
	if request, ok := input.(CutoverRequest); ok {
		return s.cutoverProjectRecordCommand(ctx, scope, options, request)
	}
	if request, ok := input.(projectNativeSummaryRequest); ok {
		return s.projectNativeSummaryRecordCommand(ctx, scope, options, request)
	}
	if request, ok := input.(tracker.GitHubBatchCommand); ok {
		return s.commandGitHubBatchRecord(ctx, scope, options.OperationID, request)
	}
	if request, ok := input.(startGitHubImportRequest); ok {
		return s.startGitHubImportRecord(ctx, scope, options.OperationID, request)
	}
	if s.config.TenantRepository != nil && scope.Project == "" {
		if request, ok := input.(cloudModelSelectionRequest); ok {
			scope.RequireHostedAdmin = true
			return s.updateOrganizationModelRecord(ctx, scope, options.OperationID, request)
		}
	}
	if s.config.TenantRepository != nil && (scope.Project != "" || isProjectCreation(input)) && isProjectRecordCommand(input) {
		return s.executeProjectRecordMutation(ctx, scope, options, command, input)
	}
	return nil, hubapi.NativePublicInvalid("Unsupported project command")
}

func (s *Service) requireCompatibilityResource(c echo.Context) error {
	path := c.Path()
	if strings.HasPrefix(path, "/api/v1/work-items/:id") || strings.HasPrefix(path, "/api/v1/leases/:id") || strings.HasPrefix(path, "/api/v1/machines/:id") {
		return echo.NewHTTPError(http.StatusNotFound, hubapi.APIErrorResponse{Code: "not_found", Message: "Resource was not found"})
	}
	return nil
}

func nativeCommandReceiptReference(ctx context.Context, query hubapi.NativeQueryer, scope hubscope.NativeScope, operation, key, hash string) (string, bool, error) {
	var storedHash, response string
	var expired bool
	err := query.QueryRowContext(ctx, `SELECT request_hash, COALESCE(NULLIF(response_object_key,''),response_json),response_expired FROM native_commands WHERE organization_id = ? AND actor_id = ? AND command_key = ? AND operation>=? AND operation<? AND (operation = ? OR substr(operation, 1, length(?) + 1) = ? || ' ') ORDER BY created_at, operation LIMIT 1`, scope.Organization, scope.Credential.ID, key, operation, operation+"!", operation, operation, operation).Scan(&storedHash, &response, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if storedHash != hash {
		return "", false, &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
	}
	if expired {
		return "", true, commandResponseExpired()
	}
	return response, true, nil
}

// External-read commands consult the same receipt before fetching a provider.
// Current application authority is rechecked even when returning a replay.
func (s *Service) nativeCommandReplay(ctx context.Context, scope hubscope.NativeScope, operation, key string, input any) (json.RawMessage, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if strings.TrimSpace(key) == "" || len(key) > 128 {
		return nil, false, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := hubapi.CommandHash(input)
	if err != nil {
		return nil, false, err
	}
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return nil, false, hubapi.NativeNotFound()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, false, err
	}
	var receipt eventstore.Receipt
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = r.ReplayReceipt(ctx, scope.Credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, hubapi.ProjectRecordError(err)
	}
	if receipt.RequestHash != hash {
		return nil, false, &hubapi.NativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, PublicMessage: true}
	}
	resolved, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return resolved, true, err
}
