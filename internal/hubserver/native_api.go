package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeBase = "/api/v2/organizations/:organization/projects/:project"
const nativeOrganizationIssuePath = "/api/v2/organizations/:organization/work-items/:item"

type nativeScope struct {
	sourceActor        *tracker.Actor
	organization       tracker.OrganizationID
	project            tracker.ProjectID
	credential         apiCredential
	requireHostedAdmin bool
}

type nativeError struct {
	Code            string           `json:"code"`
	Message         string           `json:"message"`
	CurrentRevision tracker.Revision `json:"current_revision,string,omitempty"`
	Details         map[string]any   `json:"details,omitempty"`
	status          int
	publicMessage   bool
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

func nativeNotFound() error {
	return &nativeError{Code: "not_found", Message: "Resource was not found", status: http.StatusNotFound}
}

func nativeConflict(revision tracker.Revision) error {
	return &nativeError{Code: "revision_conflict", Message: "Resource has changed", CurrentRevision: revision, status: http.StatusConflict}
}

// isNativeNotFound reports whether err is the opaque native not-found error.
func isNativeNotFound(err error) bool {
	var failure *nativeError
	return errors.As(err, &failure) && failure != nil && failure.Code == "not_found"
}

func isNativeConflict(err error) bool {
	var failure *nativeError
	return errors.As(err, &failure) && failure != nil && failure.Code == "revision_conflict"
}

func (s *Service) nativeAPIError(c echo.Context, err error) error {
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "access_denied", Message: err.Error()})
	}
	if errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, auth.ErrInvalidSession) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "access_denied", Message: "Access is no longer available"})
	}
	var limit *hostedLimitError
	if errors.As(err, &limit) {
		return c.JSON(http.StatusTooManyRequests, struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			*hostedLimitError
		}{"allowance_exhausted", limit.Error(), limit})
	}
	var failure *nativeError
	if errors.As(err, &failure) {
		if s.config.Hosted != nil {
			if failure.Code == "checkout_unavailable" || failure.publicMessage {
				return c.JSON(failure.status, apiErrorResponse{Code: failure.Code, Message: failure.Message})
			}
			if len(failure.Details) > 0 {
				return c.JSON(failure.status, &nativeError{Code: failure.Code, Message: "The requested operation is unavailable", Details: failure.Details, status: failure.status})
			}
			return c.JSON(failure.status, apiErrorResponse{Code: failure.Code, Message: "The requested operation is unavailable"})
		}
		return c.JSON(failure.status, failure)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if errors.Is(err, tracker.ErrInvalidClaimRequest) || errors.Is(err, tracker.ErrInvalidLeaseRequest) || errors.Is(err, tracker.ErrInvalidWorkEvent) || errors.Is(err, tracker.ErrInvalidCandidateQuery) {
		return trackerAPIError(c, err)
	}
	if errors.Is(err, tracker.ErrLeaseConflict) || errors.Is(err, tracker.ErrStaleFencingToken) || errors.Is(err, tracker.ErrLeaseNotFound) || errors.Is(err, tracker.ErrMachineNotFound) || errors.Is(err, tracker.ErrWorkItemNotFound) || errors.Is(err, ErrNoClaimableWork) {
		return trackerAPIError(c, err)
	}
	return s.internalAPIError(c, "native_operation_failed", "Native operation could not be completed", err)
}

func (s *Service) registerNativeRoutes(e *echo.Echo) {
	s.registerOnboardingRoutes(e)
	s.registerArtifactRoutes(e)
	s.registerCloudAttachmentRoutes(e)
	s.registerIntegrationRoutes(e)
	s.registerChangeRoutes(e)
	read := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	e.GET(nativeBase+"/health/findings", s.getHealthFindings, read)
	write := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	admin := s.requireInstanceAdmin()
	policyAdmin := admin
	if s.config.Hosted != nil {
		policyAdmin = s.requireOnboardingAdmin()
	}
	worker := s.requireNativeScope(apiScopeWorker)
	e.GET(nativeBase+"/policy", s.getProjectPolicy, s.requireNativeScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin))
	e.PUT(nativeBase+"/policy", s.approveProjectPolicy, policyAdmin)
	e.DELETE(nativeBase+"/policy", s.revokeProjectPolicy, policyAdmin)
	e.POST(nativeBase+"/policy/observed", s.observeProjectPolicy, worker)
	e.GET("/api/v2/capabilities", s.nativeCapabilities, s.requireAPIScope(apiScopeWorker, apiScopeOperator))
	e.GET("/api/v2/organizations", s.nativeOrganizations, admin)
	e.POST("/api/v2/organizations", s.createNativeOrganization, admin)
	if s.config.Hosted != nil {
		e.POST("/api/v2/organizations/:organization/projects", s.createHostedProjectJSON, admin)
	} else {
		e.POST("/api/v2/organizations/:organization/projects", s.createNativeProject, admin)
	}
	e.POST("/api/v2/tokens/:id/grants", s.grantNativeToken, admin)
	e.GET(nativeBase, s.getNativeProject, read)
	e.GET(nativeBase+"/labels", s.listNativeLabels, read)
	e.GET(nativeBase+"/work-items", s.listNativeIssues, read)
	e.POST(nativeBase+"/work-items", s.createNativeIssue, write)
	e.GET(nativeOrganizationIssuePath, s.getNativeIssue, read)
	e.GET(nativeBase+"/work-items/:item", s.getNativeIssue, read)
	e.PATCH(nativeBase+"/work-items/:item", s.updateNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/archive", s.archiveNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/restore", s.restoreNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/source-intake", s.intakeLinkedIssue, worker)
	e.POST(nativeBase+"/work-items/:item/workflow", s.transitionNativeIssue, write)
	e.POST(nativeBase+"/work-items/:item/dependencies", s.changeNativeDependency, write)
	e.GET(nativeBase+"/work-items/:item/comments", s.listNativeComments, read)
	e.POST(nativeBase+"/work-items/:item/comments", s.createNativeComment, write)
	e.PATCH(nativeBase+"/work-items/:item/comments/:comment", s.updateNativeComment, write)
	e.GET(nativeBase+"/work-items/:item/history", s.listNativeHistory, read)
	e.GET(nativeBase+"/work-items/:item/runtime", s.getNativeRuntime, read)
	e.GET(nativeBase+"/work-items/:item/explanation", s.getNativeExplanation, read)
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
	// Stored attempt diffs and the pull request panel (decisions section
	// 18.5 and 18.6). The diff write is a worker endpoint fenced by the
	// producer's lease; every read follows the issue's read rule.
	e.POST(nativeBase+"/attempts/:attempt/diff", s.postAttemptDiff, worker)
	e.GET(nativeBase+"/attempts/:attempt/diff", s.getAttemptDiff, read)
	e.GET(nativeBase+"/work-items/:item/diff", s.getWorkItemDiff, read)
	e.GET(nativeBase+"/work-items/:item/pull-requests", s.listWorkItemPullRequests, read)
	// Workspace sessions (decisions section 18.1). The reads follow the
	// issue's read rule; the writes need write on the project, and a
	// terminal in requires needs the grant's runners flag on top.
	e.POST(nativeBase+"/workspaces", s.createWorkspace, write)
	e.GET(nativeBase+"/workspaces", s.listWorkspaces, read)
	e.GET(nativeBase+"/workspaces/:workspace", s.getWorkspace, read)
	e.DELETE(nativeBase+"/workspaces/:workspace", s.deleteWorkspace, write)
	e.GET(nativeBase+"/work-items/:item/workspace", s.workspaceForWorkItem, worker)
	e.POST(nativeBase+"/workspaces/:workspace/relay-tickets", s.mintWorkspaceRelayTicket, read)
	e.GET(nativeBase+"/workspaces/:workspace/relay", s.openWorkspaceRelay, read)
	e.GET(nativeBase+"/workspaces/:workspace/worker/relay", s.openWorkspaceWorkerRelay, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/bind", s.bindWorkspaceWorker, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/heartbeat", s.heartbeatWorkspaceWorker, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/unbind", s.unbindWorkspaceWorker, worker)
	e.POST(nativeBase+"/workspaces/:workspace/worker/action-runs", s.reportWorkspaceActionRun, worker)
	e.GET(nativeBase+"/actions", s.listProjectActions, read)
	e.POST(nativeBase+"/actions", s.createProjectAction, write)
	e.PATCH(nativeBase+"/actions/:action", s.patchProjectAction, write)
	e.DELETE(nativeBase+"/actions/:action", s.deleteProjectAction, write)
	e.GET(nativeBase+"/actions/:action/runs", s.listProjectActionRuns, read)
	e.POST(nativeBase+"/actions/:action/runs", s.createProjectActionRun, write)
	e.GET(nativeBase+"/actions/:action/runs/:run", s.getProjectActionRun, read)
	e.GET(nativeBase+"/actions/:action/runs/:run/output", s.getProjectActionRunOutput, read)
	// Terminal recordings (decisions section 18.3). They are mounted under the
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
				return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "insufficient_scope", Message: "Instance administrator is required"})
			}
			return next(c)
		})
	}
}

func (s *Service) requireNativeScope(roles ...apiScope) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireAPIScope(roles...)(func(c echo.Context) error {
			credential, ok := c.Get("hub_api_credential").(apiCredential)
			if !ok {
				return s.nativeAPIError(c, nativeNotFound())
			}
			scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project")), credential: credential}
			if c.Path() == nativeOrganizationIssuePath {
				err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT project_id FROM issues WHERE organization_id = ? AND native_id = ?", scope.organization, c.Param("item")).Scan(&scope.project)
				if err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			runnerHeartbeat := c.Path() == nativeBase+"/machines/:machine/heartbeat" && credential.Runner.RunnerID != ""
			if runnerHeartbeat {
				if credential.Runner.OrganizationID != scope.organization {
					return s.nativeAPIError(c, nativeNotFound())
				}
			} else {
				write := !hostedReadRequest(c) && !artifactReadGrantRequest(c) && !relayTicketRequest(c)
				if err := s.requireHostedProject(c.Request().Context(), s.database.db, scope, write); err != nil {
					return s.nativeAPIError(c, err)
				}
				if err := s.database.authorizeNativeProject(c.Request().Context(), scope); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			c.Set("native_scope", scope)
			c.Response().Header().Set("Cache-Control", "no-store")
			if credential.Hosted != nil {
				if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.project), 0); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
			return next(c)
		})
	}
}

func (d *database) authorizeNativeProject(ctx context.Context, scope nativeScope) error {
	return authorizeNativeProject(ctx, d.db, scope)
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
	if scope.credential.Scope == apiScopeWorker {
		kind = "runner"
	}
	return tracker.Actor{Kind: kind, PrincipalID: operatorIdentity(scope.credential, string(scope.organization)).PrincipalID}
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
	options := nativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Item: c.Param("item"), CheckPrincipal: changeCheckRequest(c), RequireLease: c.Param("item") != "" && !strings.HasSuffix(c.Path(), "/events") && c.Path() != changeBase+"/:change/versions/:version/checks", Completion: hostedCompletionMutation(c, input), Feature: hostedMutationFeature(c)}
	result, err := s.executeNativeMutation(c.Request().Context(), nativeRequestScope(c), options, command, input, operation)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(status, result)
}

type nativeCommandOptions struct {
	OperationID, Item, Feature               string
	CheckPrincipal, RequireLease, Completion bool
	ArtifactRead                             bool
}

// executeNativeMutation owns the shared transaction, current native authority,
// hosted allowances, audit history and durable business replay for all callers.
func (s *Service) executeNativeMutation(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any, operation func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error)) (result json.RawMessage, resultErr error) {
	ctx = s.linkedSourceContext(ctx)
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, nativeInvalid("An idempotency key of at most 128 bytes is required")
	}
	requestHash, err := nativeCommandHash(input)
	if err != nil {
		return nil, err
	}
	operationID := options.OperationID
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if options.ArtifactRead {
		if err := s.recheckHostedArtifactReader(ctx, tx, scope); err != nil {
			return nil, err
		}
	} else if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
		return nil, err
	}
	if s.config.Hosted != nil && scope.credential.Hosted == nil && scope.credential.Runner.RunnerID == "" && options.CheckPrincipal {
		if err := s.requireHostedChangeCheckPrincipal(ctx, tx, scope); err != nil {
			return nil, err
		}
	}
	// Older hosted receipts included the originating session in operation.
	// Read those records as the same business operation across reconnects.
	response, found, err := nativeCommandReceipt(ctx, tx, scope, operationID, command.IdempotencyKey, requestHash)
	if err != nil {
		return nil, err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return json.RawMessage(response), nil
	}
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
		return nil, err
	}
	if scope.credential.Scope == apiScopeWorker && options.RequireLease {
		if err := requireNativeMutationLease(ctx, tx, scope, options.Item, command, now); err != nil {
			return nil, err
		}
	}
	metrics := hostedNativeMutationMetrics(input, options.Completion)
	before, err := s.database.hostedConsumption(ctx, tx, now, metrics...)
	if err != nil {
		return nil, err
	}
	completion := options.Completion
	event, runEvent := input.(tracker.NativeRunEvent)
	consolidateRunEvent := runEvent && !completion && s.database.hostedPlans != nil
	var runEventState hostedRunEventState
	if consolidateRunEvent {
		runEventState, err = readHostedRunEventState(ctx, tx, event)
		if err != nil {
			return nil, err
		}
	}
	if !completion && !options.ArtifactRead {
		if err := s.database.requireHostedFeature(ctx, tx, options.Feature, now); err != nil {
			return nil, err
		}
	}
	value, err := operation(ctx, tx, scope, now)
	if err != nil {
		return nil, err
	}
	response, err = marshalNative(value)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_commands (organization_id, actor_id, operation, command_key, request_hash, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, scope.organization, scope.credential.ID, operationID, command.IdempotencyKey, requestHash, response, formatHubTime(now)); err != nil {
		return nil, err
	}
	if consolidateRunEvent {
		after, err := s.database.hostedRunEventConsumption(ctx, tx, now, before, runEventState, event, response)
		if err != nil {
			return nil, err
		}
		if err := s.database.checkHostedConsumptionGrowth(ctx, tx, before, after, now, completion); err != nil {
			return nil, err
		}
	} else if err := s.database.checkHostedGrowth(ctx, tx, before, now, completion, metrics...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.notifyNativeDispatch(scope, input)
	return json.RawMessage(response), nil
}
func (s *Service) requireCompatibilityResource(c echo.Context) error {
	var query string
	path := c.Path()
	switch {
	case strings.HasPrefix(path, "/api/v1/work-items/:id"):
		query = "SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ? AND p.profile = 'native'"
	case strings.HasPrefix(path, "/api/v1/leases/:id"):
		query = "SELECT count(*) FROM leases l JOIN issues i ON i.id = l.issue_id JOIN projects p ON p.id = i.project_id WHERE l.lease_id = ? AND p.profile = 'native'"
	case strings.HasPrefix(path, "/api/v1/machines/:id"):
		query = "SELECT count(*) FROM machines WHERE id = ? AND organization_id IS NOT NULL"
	default:
		return nil
	}
	var count int
	if err := s.database.db.QueryRowContext(c.Request().Context(), query, c.Param("id")).Scan(&count); err != nil {
		return fmt.Errorf("check compatibility resource: %w", err)
	}
	if count != 0 {
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

func nativeCommandReceipt(ctx context.Context, query nativeQueryer, scope nativeScope, operation, key, hash string) (string, bool, error) {
	var storedHash, response string
	err := query.QueryRowContext(ctx, `SELECT request_hash, response_json FROM native_commands WHERE organization_id = ? AND actor_id = ? AND command_key = ? AND (operation = ? OR substr(operation, 1, length(?) + 1) = ? || ' ') ORDER BY created_at, operation LIMIT 1`, scope.organization, scope.credential.ID, key, operation, operation, operation).Scan(&storedHash, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if storedHash != hash {
		return "", false, &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict}
	}
	return response, true, nil
}

// External-read commands consult the same receipt before fetching a provider.
// Current application authority is rechecked even when returning a replay.
func (s *Service) nativeCommandReplay(ctx context.Context, scope nativeScope, operation, key string, input any) (json.RawMessage, bool, error) {
	if strings.TrimSpace(key) == "" || len(key) > 128 {
		return nil, false, nativeInvalid("An idempotency key of at most 128 bytes is required")
	}
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(ctx, tx, scope); err != nil {
		return nil, false, err
	}
	response, found, err := nativeCommandReceipt(ctx, tx, scope, operation, key, hash)
	if !found || err != nil {
		return nil, found, err
	}
	return json.RawMessage(response), true, nil
}
