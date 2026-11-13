package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
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

const nativeBase = "/api/v2/organizations/:organization/projects/:project"
const nativeOrganizationIssuePath = "/api/v2/organizations/:organization/work-items/:item"

type githubIntakeRevision struct {
	requireAdminKey bool
	issuer          *operatorChatIdentity
	authority       *nativeScope
	reporter        *nativeScope
	fallbackRunner  string
	batchID         string
	importID        string
	revision        int64
}

type nativeScope struct {
	integrationOwned      bool
	githubIntakeRevision  *githubIntakeRevision
	integrationRepository string
	duplicateSearch       *nativeDuplicateSearch
	action                string
	actionItem            string
	actionTarget          string
	actionComment         string
	scheduledRepair       *nativeScheduledRepair
	sourceActor           *tracker.Actor
	progressOrigin        progressDefectOrigin
	organization          tracker.OrganizationID
	project               tracker.ProjectID
	credential            apiCredential
	requireHostedAdmin    bool
	importedContent       bool
	managedRunner         string
	managedRunnerScope    string
	managedRunnerProjects []tracker.ProjectID
}

type progressDefectOrigin interface {
	AuthorizeProgressDefect(ctx context.Context, w tenantstore.Writer, owner any, scope nativeScope, leaseID tracker.LeaseID, fencingToken tracker.FencingToken) error
}

type nativeError struct {
	Code            string           `json:"code"`
	Message         string           `json:"message"`
	CurrentRevision tracker.Revision `json:"current_revision,string,omitempty"`
	Details         map[string]any   `json:"details,omitempty"`
	status          int
	publicMessage   bool
	logAttributes   []any
}

func (e *nativeError) Error() string {
	if e.publicMessage {
		return e.Message
	}
	return e.Code
}

func nativeInvalid(message string) error {
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity}
}

func nativePublicInvalid(message string) error {
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, publicMessage: true}
}

func nativeRejection(err error, category, field string) error {
	var failure *nativeError
	rejection := runnerauth.PublicationRejection{Category: category, Field: field}
	if !errors.As(err, &failure) || failure.Code != "invalid_request" || !rejection.Valid() {
		return err
	}
	result := &nativeError{Code: failure.Code, Message: rejection.DiagnosticError(), status: failure.status, Details: map[string]any{"rejection": rejection}}
	switch category {
	case "event_validation", "runtime_validation", "execution_validation", "usage_validation", "owner_validation", "body_decode":
		result.Message, result.publicMessage = failure.Message, true
	}
	return result
}

func nativeNotFound() error {
	_, file, line, _ := runtime.Caller(1)
	return &nativeError{Code: "not_found", Message: "Resource was not found", status: http.StatusNotFound, publicMessage: true, logAttributes: []any{"not_found_source", filepath.Base(file) + ":" + strconv.Itoa(line)}}
}

func nativeNotFoundAt(step string) error {
	return &nativeError{Code: "not_found", Message: "Resource was not found", status: http.StatusNotFound, publicMessage: true, logAttributes: []any{"not_found_step", step}}
}

func nativeConflict(revision tracker.Revision) error {
	return &nativeError{Code: "revision_conflict", Message: "Resource has changed", CurrentRevision: revision, status: http.StatusConflict, publicMessage: true}
}

// isNativeNotFound reports whether err is the opaque native not-found error.
func isNativeNotFound(err error) bool {
	err = fromFeatureError(err)
	var failure *nativeError
	return errors.As(err, &failure) && failure != nil && failure.Code == "not_found"
}

func isNativeConflict(err error) bool {
	err = fromFeatureError(err)
	var failure *nativeError
	return errors.As(err, &failure) && failure != nil && failure.Code == "revision_conflict"
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
	err = fromFeatureError(err)
	var deniedAuthorization *operatortool.AuthorizationError
	authorizationFailure := errors.As(err, &deniedAuthorization)
	authorizationUnavailable := errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid
	if authorizationUnavailable || authorizationFailure || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || errors.Is(err, operatortool.ErrProviderUnavailable) {
		if classified := operatorAuthorizationError(err); !errors.Is(classified, operatortool.ErrAccessDenied) {
			err = classified
		}
	}
	if errors.Is(err, corestore.ErrSerializationFailure) {
		message := "Database transaction was aborted; retry the request"
		s.config.Logger.Error(message, append([]any{"error", err}, postgresErrorAttributes(err)...)...)
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "service_unavailable", Message: message})
	}
	if errors.Is(err, receiptstore.ErrExpired) {
		return s.nativeAPIError(c, commandResponseExpired(), attributes...)
	}
	if errors.Is(err, mutation.ErrUncertain) {
		return s.refusedAPIError(c, http.StatusConflict, "idempotency_in_progress", mutation.ErrUncertain.Error(), apiErrorResponse{Code: "idempotency_in_progress", Message: mutation.ErrUncertain.Error()})
	}
	if errors.Is(err, searchstore.ErrIndexPending) {
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "not_ready", Message: "Search index refresh is pending"})
	}
	var archive *nativeArchiveError
	if errors.As(err, &archive) {
		return c.JSON(http.StatusServiceUnavailable, struct {
			Code    string                     `json:"code"`
			Archive *tracker.ArchiveDescriptor `json:"archive"`
		}{"archive_" + archive.Archive.Availability, archive.Archive})
	}
	var authorization *operatorAuthorizationFailure
	if errors.As(err, &authorization) {
		code := "service_unavailable"
		message := authorization.Error()
		if errors.Is(authorization, operatortool.ErrProviderUnavailable) {
			code = auth.HostedReasonProviderUnavailable
		} else if authorizationUnavailable {
			message = "Authorization is temporarily unavailable"
		}
		cause := authorization.cause
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
		scope := nativeRequestScope(c)
		attrs := []any{"error", authorization, "authorization_cause", operatorAuthorizationCause(cause), "authorization_step", step, "cause_class", fmt.Sprintf("%T", causeClass),
			"method", c.Request().Method, "route", c.Path(), "organization_id", string(scope.organization), "project_id", string(scope.project)}
		if deniedAuthorization != nil {
			attrs = append(attrs, "authorization_stage", deniedAuthorization.Stage)
		}
		attrs = append(attrs, attributes...)
		attrs = append(attrs, postgresErrorAttributes(cause)...)
		s.config.Logger.Error("operator authorization unavailable", attrs...)
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: code, Message: message})
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
		return s.refusedAPIError(c, http.StatusForbidden, "access_denied", message, apiErrorResponse{Code: "access_denied", Message: message}, attributes...)
	}
	if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, auth.ErrInvalidSession) {
		return s.refusedAPIError(c, http.StatusForbidden, "access_denied", "Access is no longer available", apiErrorResponse{Code: "access_denied", Message: "Access is no longer available"}, append(attributes, "authorization_cause", operatorAuthorizationCause(err), "hosted_reason", auth.HostedIdentityReason(err))...)
	}
	var limit *hostedLimitError
	if errors.As(err, &limit) {
		return s.refusedAPIError(c, http.StatusTooManyRequests, "allowance_exhausted", limit.Error(), struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			*hostedLimitError
		}{"allowance_exhausted", limit.Error(), limit})
	}
	var failure *nativeError
	if errors.Is(err, artifact.ErrStorage) {
		message := "Immutable content storage is unavailable; retry when storage is restored"
		if errors.Is(err, errContentStorageUnconfigured) {
			message = "Immutable content storage is not configured; configure object storage to persist payloads"
		}
		failure = &nativeError{Code: "storage_unreachable", Message: message, status: http.StatusServiceUnavailable, publicMessage: true}
		return s.refusedAPIError(c, failure.status, failure.Code, failure.Message, failure, attributes...)
	}
	if errors.As(err, &failure) {
		attributes = append(attributes, failure.logAttributes...)
		message := failure.Message
		if failure.status >= http.StatusBadRequest && failure.status < http.StatusInternalServerError && !failure.publicMessage && failure.Code != "checkout_unavailable" {
			message = http.StatusText(failure.status) + ": " + strings.ReplaceAll(failure.Code, "_", " ")
			if rejection, ok := failure.Details["rejection"].(runnerauth.PublicationRejection); ok && rejection.Valid() {
				message = rejection.DiagnosticError()
			}
		}
		if s.config.Hosted != nil && !failure.publicMessage {
			if failure.status >= http.StatusInternalServerError && failure.Code != "checkout_unavailable" {
				message = "The requested operation is unavailable"
			}
			return s.refusedAPIError(c, failure.status, failure.Code, message, &nativeError{Code: failure.Code, Message: message, CurrentRevision: failure.CurrentRevision, Details: failure.Details, status: failure.status}, attributes...)
		}
		return s.refusedAPIError(c, failure.status, failure.Code, message, failure, attributes...)
	}
	var conflict *operatortool.ConflictError
	if errors.As(err, &conflict) {
		return s.refusedAPIError(c, http.StatusConflict, conflict.Code, conflict.Message, conflict)
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound(), "not_found_cause", err.Error())
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
		return s.nativeAPIError(c, nativeConflict(0), attributes...)
	}
	if errors.Is(err, mutation.ErrConflict) {
		return s.nativeAPIError(c, &nativeError{Code: "idempotency_conflict", Message: "Request was already used with different content", status: http.StatusConflict, publicMessage: true}, attributes...)
	}
	var mismatch *nativeAttemptIdentityMismatch
	if errors.As(err, &mismatch) {
		attrs := []any{"error", err, "attempt_id", mismatch.AttemptID, "work_item_id", mismatch.WorkItemID, "identity_field", mismatch.Field}
		if mismatch.Values != nil {
			attrs = append(attrs, "expected_value", mismatch.Values.Expected, "actual_value", mismatch.Values.Actual)
		}
		if mismatch.Field == "updated_at" {
			attrs = append(attrs, "timestamp_unit", "microseconds")
		}
		s.config.Logger.Error("Native operation could not be completed", attrs...)
		return c.JSON(http.StatusInternalServerError, apiErrorResponse{Code: "native_operation_failed", Message: "Native operation could not be completed"})
	}
	return s.internalAPIError(c, "native_operation_failed", "Native operation could not be completed", err)
}

func (s *Service) registerNativeRoutes(e *echo.Echo) {
	s.registerOnboardingRoutes(e)
	e.POST(nativeBase+"/local-board-import", s.importLocalBoard, s.requireNativeScope(apiScopeOperator))
	s.registerArtifactRoutes(e)
	s.registerReleaseAuditRoutes(e)
	s.registerCloudAttachmentRoutes(e)
	s.registerAttemptDiffBodyRoutes(e)
	s.registerIntegrationRoutes(e)
	s.registerChangeRoutes(e)
	s.registerBudgetOverrideRoutes(e)
	s.registerConfigurationRankRoutes(e)
	read := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	e.GET(nativeBase+"/runner-spend", s.getRunnerSpend, read)
	e.POST(nativeBase+"/work-items/:item/attempts/:attempt/token-accounting", s.accountRunnerUsage, s.requireNativeScope(apiScopeOperator))
	e.GET(nativeBase+"/security-audits/evidence", s.getAuditEvidence, read)
	e.GET(nativeBase+"/security-audits/:session/dispositions", s.getAuditDispositions, read)
	e.GET(nativeBase+"/runner-model-quantiles", s.getRunnerModelQuantiles, read)
	e.GET(nativeBase+"/health/findings", s.getHealthFindings, read)
	e.GET(nativeBase+"/runner-diagnostics", s.getRunnerProjectDiagnostics, read)
	write := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	e.POST(nativeBase+"/security-audits/dispositions", s.postAuditDisposition, write)
	admin := s.requireInstanceAdmin()
	policyAdmin := admin
	if s.config.Hosted != nil {
		policyAdmin = s.requireOnboardingAdmin()
	}
	worker := s.requireNativeScope(apiScopeWorker)
	e.GET(nativeBase+"/runner-sessions/:session", s.getRunnerSession, read)
	e.GET(nativeBase+"/runner-sessions/:session/history/:revision", s.getRunnerSessionHistory, read)
	e.GET(nativeBase+"/runner-processes", s.getRunnerSessionProcesses, worker)
	e.POST(nativeBase+"/runner-sessions/find", s.findRunnerSession, read)
	e.POST(nativeBase+"/runner-sessions", s.mutateRunnerSession, worker)
	e.GET(nativeBase+"/policy", s.getProjectPolicy, s.requireNativeScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin))
	e.PUT(nativeBase+"/policy", s.approveProjectPolicy, policyAdmin)
	e.DELETE(nativeBase+"/policy", s.revokeProjectPolicy, policyAdmin)
	e.POST(nativeBase+"/policy/observed", s.observeProjectPolicy, worker)
	e.GET("/api/v2/capabilities", s.nativeCapabilities, s.requireAPIScope(apiScopeWorker, apiScopeOperator))
	organizationRead := s.requireAPIScope(apiScopeAdmin)
	if s.config.Hosted == nil {
		organizationRead = admin
	}
	e.GET("/api/v2/organizations", s.nativeOrganizations, organizationRead)
	e.POST("/api/v2/organizations", s.createNativeOrganization, admin)
	if s.config.Hosted != nil {
		e.POST("/api/v2/organizations/:organization/projects", s.createHostedProjectJSON, admin)
		e.DELETE(nativeBase, s.deleteCloudProject, s.requireOnboardingAdmin())
	} else {
		e.POST("/api/v2/organizations/:organization/projects", s.createNativeProject, admin)
	}
	e.POST("/api/v2/tokens/:id/grants", s.grantNativeToken, admin)
	e.GET(nativeBase, s.getNativeProject, read)
	e.PATCH(nativeBase, s.renameProject, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	e.PUT(nativeBase+"/icon", s.setProjectIcon, s.requireNativeScope(apiScopeOperator, apiScopeAdmin))
	if s.config.Hosted == nil {
		organization := s.requireConversationOrganization()
		e.GET(hostedOrganizationBase+"/logo", s.getOrganizationLogo, organization)
		e.PUT(hostedOrganizationBase+"/logo", s.setOrganizationLogo, organization)
		e.GET(hostedOrganizationBase+"/attachment-metadata/:attachment", s.getOrganizationLogoAttachment, organization)
		e.GET(hostedOrganizationBase+"/logo-metadata", s.getCurrentOrganizationLogoAttachment, organization)
	}
	s.registerNativeLabelRoutes(e, read)
	e.GET(nativeBase+"/work-items", s.listNativeIssues, read)
	e.GET(nativeBase+"/board/counts", s.boardModelCounts, read)
	e.GET(nativeBase+"/board/status", s.boardModelStatus, read)
	e.GET(nativeBase+"/work-items/export", s.exportNativeWorkItems, read)
	e.POST(nativeBase+"/work-items", s.createNativeIssue, write)
	e.POST(nativeBase+"/scheduled-ci-decisions", s.classifyScheduledFailure, s.requireNativeScope(apiScopeOperator))
	e.GET(nativeOrganizationIssuePath, s.getNativeIssue, read)
	e.GET(nativeBase+"/work-items/:item", s.getNativeIssue, read)
	e.PATCH(nativeBase+"/work-items/:item", s.updateNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/archive", s.archiveNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/restore", s.restoreNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/source-intake", s.intakeLinkedIssue, write)
	e.POST(nativeBase+"/work-items/:item/workflow", s.transitionNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/verification", s.verifyNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/dependencies", s.changeNativeDependency, write)
	e.POST(nativeBase+"/work-items/:item/dependency-reasons", s.updateNativeDependencyReason, write)
	e.GET(nativeBase+"/work-items/:item/comments", s.listNativeComments, read)
	e.POST(nativeBase+"/work-items/:item/comments", s.createNativeComment, write)
	e.PATCH(nativeBase+"/work-items/:item/comments/:comment", s.updateNativeComment, write)
	e.GET(nativeBase+"/work-items/:item/history", s.listNativeHistory, read)
	e.GET(nativeBase+"/work-items/:item/runtime", s.getNativeRuntime, read)
	e.GET(nativeBase+"/work-items/:item/progress", s.getNativeProgress, read)
	e.POST(nativeBase+"/work-items/:item/progress", s.publishNativeProgress, worker)
	e.GET(nativeBase+"/work-items/:item/explanation", s.getNativeExplanation, read)
	e.GET(nativeBase+"/work-items/:item/source-recovery", s.getNativeSourceRecovery, read)
	e.POST(nativeBase+"/work-items/:item/source-recovery", s.transferNativeSource, write)
	e.GET(nativeBase+"/work-items/:item/runtime/github-timings", s.getNativeGitHubTimings, read)
	e.GET(nativeBase+"/work-items/:item/attempts", s.listNativeAttempts, read)
	e.GET(nativeBase+"/work-items/:item/attempts/:attempt", s.getNativeAttempt, read)
	e.GET(nativeBase+"/work-items/:item/versions/:revision", s.getNativeVersion, read)
	e.GET(nativeBase+"/work-items/:item/comments/:comment/versions/:revision", s.getNativeVersion, read)
	e.POST(nativeBase+"/claims", s.claimNativeIssue, worker)
	e.POST(nativeBase+"/claims/preview", s.previewProviderCandidates, worker)
	e.GET(nativeBase+"/claims/wait", s.waitNativeCandidates, worker)
	e.POST(nativeBase+"/leases/:lease/renew", s.renewNativeLease, worker)
	e.POST(nativeBase+"/leases/:lease/release", s.releaseNativeLease, worker)
	e.POST(nativeBase+"/machines/register", s.registerNativeMachine, worker)
	e.POST(nativeBase+"/work-items/:item/events", s.appendNativeRunEvent, worker)
	e.POST(nativeBase+"/go-test-receipts/attestations", s.attestGoTestReceipts, worker)
	e.GET(nativeBase+"/go-test-receipts", s.lookupGoTestReceipts, worker)
	e.POST(nativeBase+"/go-test-receipts", s.recordGoTestReceipt, worker)
	// Stored attempt diffs and the pull request panel.
	// The diff write is a worker endpoint fenced by the
	// producer's lease; every read follows the issue's read rule.
	e.POST(nativeBase+"/attempts/:attempt/diff", s.postAttemptDiff, worker)
	e.POST(nativeBase+"/attempts/:attempt/diff/check", s.checkAttemptDiff, worker)
	e.GET(nativeBase+"/attempts/:attempt/diff", s.getAttemptDiff, read)
	e.GET(nativeBase+"/work-items/:item/diff", s.getWorkItemDiff, read)
	e.GET(nativeBase+"/work-items/:item/pull-requests", s.listWorkItemPullRequests, read)
	e.POST(nativeBase+"/work-items/:item/release-pick", s.associateReleasePick, write)
	e.GET(nativeBase+"/work-items/:item/release-pick", s.getReleaseDeliveryStatus, read)
	e.POST(nativeBase+"/work-items/:item/release-pick/publication", s.publishReleasePick, write)
	e.GET(nativeBase+"/workspaces", s.listWorkspaces, read)
	e.GET(nativeBase+"/workspaces/:workspace", s.getWorkspace, read)
	e.GET(nativeBase+"/work-items/:item/workspace", s.workspaceForWorkItem, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/bind", s.bindWorkspaceWorker, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/heartbeat", s.heartbeatWorkspaceWorker, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/unbind", s.unbindWorkspaceWorker, worker)
	e.POST(nativeBase+"/workspaces", s.createWorkspace, write)
	e.DELETE(nativeBase+"/workspaces/:workspace", s.deleteWorkspace, write)
	e.POST(nativeBase+"/workspaces/:workspace/relay-tickets", s.mintWorkspaceRelayTicket, read)
	e.GET(nativeBase+"/workspaces/:workspace/relay", s.openWorkspaceRelay, read)
	e.GET(nativeBase+"/workspaces/:workspace/worker/relay", s.openWorkspaceWorkerRelay, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/action-runs", s.reportWorkspaceActionRun, worker)
	e.GET(nativeBase+"/actions", s.listProjectActions, read)
	e.POST(nativeBase+"/actions", s.createProjectAction, write)
	e.PATCH(nativeBase+"/actions/:action", s.patchProjectAction, write)
	e.DELETE(nativeBase+"/actions/:action", s.deleteProjectAction, write)
	e.GET(nativeBase+"/actions/:action/runs", s.listProjectActionRuns, read)
	e.POST(nativeBase+"/actions/:action/runs", s.createProjectActionRun, write)
	e.GET(nativeBase+"/actions/:action/runs/:run", s.getProjectActionRun, read)
	e.GET(nativeBase+"/actions/:action/runs/:run/output", s.getProjectActionRunOutput, read)
	// Terminal recordings. They are mounted under the
	// project's read scope like everything else, and then narrowed again by the
	// handler: a recording's audience is the person who ran it plus owners and
	// admins, and a user-isolation recording's is owners alone, which is
	// narrower than any scope the router can express.
	e.GET(nativeBase+"/workspaces/:workspace/terminal-recordings", s.listWorkspaceTerminalRecordings, read)
	e.GET(nativeBase+"/workspaces/:workspace/terminal-recordings/:recording", s.getWorkspaceTerminalRecording, read)
}

func (s *Service) requireInstanceAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if s.config.Hosted != nil {
			return s.requireHostedAdministration(next)
		}
		return s.requireAPIScope(apiScopeAdmin)(func(c echo.Context) error {
			credential, ok := c.Get("hub_api_credential").(apiCredential)
			if !ok || credential.NativeOnly {
				return s.refusedAPIError(c, http.StatusForbidden, "insufficient_scope", "Instance administrator is required", apiErrorResponse{Code: "insufficient_scope", Message: "Instance administrator is required"})
			}
			return next(c)
		})
	}
}

func (s *Service) requireNativeScope(roles ...apiScope) echo.MiddlewareFunc {
	return s.requireHubauthNative(roles...)
}

func (s *Service) resolveNativeRequestScope(readCtx context.Context, c echo.Context) error {
	return s.resolveHubauthNativeScope(readCtx, c)
}

func (d *database) authorizeNativeProject(ctx context.Context, scope nativeScope) error {
	if d.postgres == nil {
		return nativeNotFound()
	}
	service := &Service{config: Config{now: d.now}}
	return d.postgres.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if scope.credential.Runner.RunnerID != "" {
			if scope.credential.Runner.OrganizationID != scope.organization {
				return nativeNotFound()
			}
			identity, err := r.RunnerIdentityByCredential(ctx, scope.credential.ID)
			if err != nil {
				return projectRecordError(err)
			}
			if identity.RunnerID != scope.credential.Runner.RunnerID {
				return nativeNotFound()
			}
			allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, d.now())
			if err != nil {
				return err
			}
			if !allowed {
				return nativeNotFound()
			}
		} else if err := service.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return projectRecordError(err)
		}
		if project.DeletedAt != nil {
			return nativeNotFound()
		}
		return nil
	})
}

func authorizeNativeProject(ctx context.Context, db nativeQueryer, scope nativeScope) error {
	if scope.credential.Runner.RunnerID != "" && scope.credential.Runner.OrganizationID != scope.organization {
		return nativeNotFound()
	}
	var count int
	query := "SELECT count(*) FROM projects WHERE organization_id = ? AND id = ?"
	args := []any{scope.organization, scope.project}
	condition, grantArgs := scope.credential.projectGrantSQL("projects.organization_id", "projects.id")
	query += " AND (" + condition + ")"
	args = append(args, grantArgs...)
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return nativeNotFound()
	}
	return nil
}

func nativeRequestScope(c echo.Context) nativeScope {
	scope, ok := c.Get("native_scope").(nativeScope)
	if !ok {
		return nativeScope{}
	}
	return scope
}

func nativeIntegrationActor(principalID string) *tracker.Actor {
	return &tracker.Actor{Kind: "integration", PrincipalID: principalID}
}

func (scope nativeScope) actor() tracker.Actor {
	if scope.sourceActor != nil {
		return *scope.sourceActor
	}
	kind := "human"
	if scope.credential.Scope == apiScopeWorker || scope.credential.Runner.RunnerID != "" {
		kind = "runner"
	} else if scope.credential.EntryKey != nil && scope.credential.EntryKey.Kind == "service" || scope.credential.HostedKeyScope != "" && scope.credential.HostedKeyKind == "service" {
		kind = "integration"
	}
	principal := operatorIdentity(scope.credential, string(scope.organization)).PrincipalID
	if kind == "integration" && scope.credential.HostedKeyKind == "service" {
		principal = scope.credential.ID
	}
	return tracker.Actor{Kind: kind, PrincipalID: principal, ViaKey: scope.credential.viaKey(), Agent: scope.credential.Agent}
}

func (credential apiCredential) viaKey() string {
	if credential.EntryKey != nil {
		return credential.EntryKey.ID
	}
	if credential.HostedKeyScope != "" {
		return credential.ID
	}
	return ""
}

func newNativeID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func marshalNative(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

func (s *Service) nativeMutation(c echo.Context, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)) error {
	return s.nativeMutationStatus(c, http.StatusOK, command, input, operation)
}

// nativeMutationStatus is nativeMutation with the success status the route
// reports. A replay answers with the same status as the first call, so a
// created resource stays 201 on every retry of its key.
func (s *Service) nativeMutationStatus(c echo.Context, status int, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)) error {
	options := nativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Item: c.Param("item"), ChangeID: c.Param("change"), VersionID: c.Param("version"), CheckPrincipal: changeCheckRequest(c), RequireLease: c.Param("item") != "" && !strings.HasSuffix(c.Path(), "/events") && c.Path() != changeBase+"/:change/versions/:version/checks", Completion: hostedCompletionMutation(c, input), Feature: hostedMutationFeature(c)}
	result, err := s.executeNativeMutation(c.Request().Context(), nativeRequestScope(c), options, command, input, operation)
	if err != nil {
		if request, ok := input.(tracker.NativeRunEvent); ok {
			return s.nativeRunEventError(c, request, err)
		}
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(status, result)
}

type nativeCommandOptions struct {
	RunnerAdministration                     bool
	OperationID, Item, Feature               string
	ChangeID, VersionID                      string
	CheckPrincipal, RequireLease, Completion bool
	ArtifactRead                             bool
}

func (s *Service) executeNativeMutation(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)) (result json.RawMessage, resultErr error) {
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
	if s.config.TenantRepository != nil && scope.project == "" {
		if request, ok := input.(cloudModelSelectionRequest); ok {
			scope.requireHostedAdmin = true
			return s.updateOrganizationModelRecord(ctx, scope, options.OperationID, request)
		}
	}
	if s.config.TenantRepository != nil && (scope.project != "" || isProjectCreation(input)) && isProjectRecordCommand(input) {
		return s.executeProjectRecordMutation(ctx, scope, options, command, input)
	}
	return nil, nativePublicInvalid("Unsupported project command")
}

func (s *Service) requireCompatibilityResource(c echo.Context) error {
	path := c.Path()
	if strings.HasPrefix(path, "/api/v1/work-items/:id") || strings.HasPrefix(path, "/api/v1/leases/:id") || strings.HasPrefix(path, "/api/v1/machines/:id") {
		return echo.NewHTTPError(http.StatusNotFound, apiErrorResponse{Code: "not_found", Message: "Resource was not found"})
	}
	return nil
}

func nativeCommandHash(input any) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func nativeCommandReceiptReference(ctx context.Context, query nativeQueryer, scope nativeScope, operation, key, hash string) (string, bool, error) {
	var storedHash, response string
	var expired bool
	err := query.QueryRowContext(ctx, `SELECT request_hash, COALESCE(NULLIF(response_object_key,''),response_json),response_expired FROM native_commands WHERE organization_id = ? AND actor_id = ? AND command_key = ? AND operation>=? AND operation<? AND (operation = ? OR substr(operation, 1, length(?) + 1) = ? || ' ') ORDER BY created_at, operation LIMIT 1`, scope.organization, scope.credential.ID, key, operation, operation+"!", operation, operation, operation).Scan(&storedHash, &response, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if storedHash != hash {
		return "", false, &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
	}
	if expired {
		return "", true, commandResponseExpired()
	}
	return response, true, nil
}

// External-read commands consult the same receipt before fetching a provider.
// Current application authority is rechecked even when returning a replay.
func (s *Service) nativeCommandReplay(ctx context.Context, scope nativeScope, operation, key string, input any) (json.RawMessage, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if strings.TrimSpace(key) == "" || len(key) > 128 {
		return nil, false, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, false, err
	}
	if s.config.TenantRepository == nil || s.config.AccessRepository == nil {
		return nil, false, nativeNotFound()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, false, err
	}
	var receipt eventstore.Receipt
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = r.ReplayReceipt(ctx, scope.credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, projectRecordError(err)
	}
	if receipt.RequestHash != hash {
		return nil, false, &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
	}
	resolved, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, receipt)
	return resolved, true, err
}
