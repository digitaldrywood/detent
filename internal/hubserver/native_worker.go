package hubserver

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func authorizeClaimScope(ctx context.Context, tx *sql.Tx, request tracker.ClaimRequest, scope *nativeScope) error {
	var count int
	query := "SELECT count(*) FROM machines WHERE id = ? AND organization_id IS NULL"
	args := []any{request.MachineID}
	if scope != nil {
		query = "SELECT count(*) FROM machines WHERE id = ? AND organization_id = ? AND token_id = ?"
		args = append(args, scope.organization, scope.credential.ID)
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return tracker.ErrMachineNotFound
	}
	if request.WorkItemID != 0 {
		return authorizeClaimItem(ctx, tx, request.WorkItemID, scope)
	}
	return nil
}

func authorizeClaimItem(ctx context.Context, tx *sql.Tx, id tracker.WorkItemID, scope *nativeScope) error {
	var count int
	query := "SELECT count(*) FROM issues WHERE id = ? AND github_node_id IS NOT NULL"
	args := []any{id}
	if scope != nil {
		query = "SELECT count(*) FROM issues WHERE id = ? AND organization_id = ? AND project_id = ?"
		args = append(args, scope.organization, scope.project)
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return tracker.ErrWorkItemNotFound
	}
	return nil
}

func (s *Service) claimNativeIssue(c echo.Context) error {
	var request tracker.NativeClaim
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if request.ProtocolMajor != tracker.NativeProtocolMajor || !slices.Contains(request.Capabilities, "scoped_collaboration") || !slices.Contains(request.Capabilities, "native_issues") {
		return s.nativeAPIError(c, nativeInvalid("Native protocol and required collaboration capabilities must be negotiated"))
	}
	for _, capability := range request.Capabilities {
		if !slices.Contains([]string{"native_issues", "scoped_collaboration", "revision_conflicts", "idempotent_mutations"}, capability) {
			return s.nativeAPIError(c, nativeInvalid("Unknown required capability"))
		}
	}
	if len(request.SessionID) > 128 {
		return s.nativeAPIError(c, nativeInvalid("Session ID is too long"))
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid("Lease TTL is invalid"))
	}
	scope := nativeRequestScope(c)
	var id tracker.WorkItemID
	if request.WorkItemID != "" {
		_, id, err = readNativeIssue(c.Request().Context(), s.database.db, scope, string(request.WorkItemID))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	lease, err := s.database.claimNext(c.Request().Context(), tracker.ClaimRequest{WorkItemID: id, MachineID: request.MachineID, SessionID: request.SessionID, TTL: ttl}, claimCandidateQuery{
		NativeScope: &scope, Scope: string(scope.project), WorkflowStates: request.WorkflowStates, Authors: request.Authors, Assignees: request.Assignees, LabelInclude: request.LabelInclude, LabelExclude: request.LabelExclude,
	}, s.config.ReconcileInterval)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.respondNativeLease(c, scope, lease)
}

func (s *Service) respondNativeLease(c echo.Context, scope nativeScope, lease tracker.Lease) error {
	var id tracker.NativeWorkItemID
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT native_id FROM issues WHERE id = ? AND organization_id = ? AND project_id = ?", lease.WorkItemID, scope.organization, scope.project).Scan(&id); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, tracker.NativeLease{ID: lease.ID, WorkItemID: id, MachineID: lease.Machine.ID, SessionID: lease.SessionID, FencingToken: lease.FencingToken, AcquiredAt: lease.AcquiredAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt})
}

func (s *Service) requireNativeLease(c echo.Context) error {
	scope := nativeRequestScope(c)
	var count int
	err := s.database.db.QueryRowContext(c.Request().Context(), `SELECT count(*) FROM leases l JOIN issues i ON i.id = l.issue_id JOIN machines m ON m.id = l.machine_id
WHERE l.lease_id = ? AND i.organization_id = ? AND i.project_id = ? AND m.organization_id = ? AND m.token_id = ?`, c.Param("lease"), scope.organization, scope.project, scope.organization, scope.credential.ID).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return nativeNotFound()
	}
	return nil
}

func (s *Service) renewNativeLease(c echo.Context) error {
	if err := s.requireNativeLease(c); err != nil {
		return s.nativeAPIError(c, err)
	}
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid("Lease TTL is invalid"))
	}
	lease, err := s.database.Renew(c.Request().Context(), tracker.RenewRequest{LeaseID: tracker.LeaseID(c.Param("lease")), FencingToken: request.FencingToken, TTL: ttl})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.respondNativeLease(c, nativeRequestScope(c), lease)
}

func (s *Service) releaseNativeLease(c echo.Context) error {
	if err := s.requireNativeLease(c); err != nil {
		return s.nativeAPIError(c, err)
	}
	var request tracker.NativeLeaseMutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if !slices.Contains([]string{"completed", "cancelled", "failed", "work_item_hydration_failed", "work_item_identity_missing", "released"}, request.Reason) {
		return s.nativeAPIError(c, nativeInvalid("Release reason is invalid"))
	}
	if err := s.database.Release(c.Request().Context(), tracker.ReleaseRequest{LeaseID: tracker.LeaseID(c.Param("lease")), FencingToken: request.FencingToken, Reason: request.Reason}); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) registerNativeMachine(c echo.Context) error {
	var request struct {
		ID          tracker.MachineID `json:"id"`
		Hostname    string            `json:"hostname"`
		DisplayName string            `json:"display_name"`
		Capacity    int               `json:"capacity"`
		Version     string            `json:"version"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if strings.TrimSpace(string(request.ID)) == "" || len(request.ID) > 128 || strings.TrimSpace(request.Hostname) == "" || len(request.Hostname) > 200 || len(request.DisplayName) > 200 || strings.TrimSpace(request.Version) == "" || len(request.Version) > 100 || request.Capacity < 0 {
		return s.nativeAPIError(c, nativeInvalid("Machine identity, hostname, version, and nonnegative capacity are required"))
	}
	scope := nativeRequestScope(c)
	now := formatHubTime(s.config.now())
	result, err := s.database.db.ExecContext(c.Request().Context(), `INSERT INTO machines (id, hostname, display_name, capacity, version, last_heartbeat_at, registered_at, updated_at, organization_id, token_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET hostname = excluded.hostname, display_name = excluded.display_name, capacity = excluded.capacity, version = excluded.version, last_heartbeat_at = excluded.last_heartbeat_at, updated_at = excluded.updated_at
WHERE machines.organization_id = excluded.organization_id AND machines.token_id = excluded.token_id`, request.ID, request.Hostname, request.DisplayName, request.Capacity, request.Version, now, now, now, scope.organization, scope.credential.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if count != 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return c.JSON(http.StatusOK, request)
}
