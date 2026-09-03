package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrNoClaimableWork = errors.New("no compatible work item is claimable")

type claimAPIRequest struct {
	WorkItemID    tracker.WorkItemID     `json:"work_item_id,omitempty"`
	MachineID     tracker.MachineID      `json:"machine_id"`
	SessionID     string                 `json:"session_id"`
	TTLSeconds    int64                  `json:"ttl_seconds"`
	RepositoryIDs []tracker.RepositoryID `json:"repository_ids,omitempty"`
	WorkflowState []string               `json:"workflow_states,omitempty"`
	Scope         string                 `json:"scope,omitempty"`
}

type renewLeaseAPIRequest struct {
	FencingToken tracker.FencingToken `json:"fencing_token"`
	TTLSeconds   int64                `json:"ttl_seconds"`
}

type releaseLeaseAPIRequest struct {
	FencingToken tracker.FencingToken `json:"fencing_token"`
	Reason       string               `json:"reason,omitempty"`
}

type workEventAPIRequest struct {
	FencingToken tracker.FencingToken `json:"fencing_token"`
	MachineID    tracker.MachineID    `json:"machine_id,omitempty"`
	SessionID    string               `json:"session_id,omitempty"`
	RunID        string               `json:"run_id,omitempty"`
	Kind         string               `json:"kind"`
	Payload      map[string]any       `json:"payload,omitempty"`
	OccurredAt   time.Time            `json:"occurred_at,omitempty"`
}

type machineRequest struct {
	ID           tracker.MachineID `json:"id"`
	Hostname     string            `json:"hostname"`
	DisplayName  string            `json:"display_name,omitempty"`
	Capabilities map[string]any    `json:"capabilities,omitempty"`
	Capacity     int               `json:"capacity"`
	Version      string            `json:"version"`
}

type machineHeartbeatRequest struct {
	DisplayName  *string         `json:"display_name,omitempty"`
	Capabilities *map[string]any `json:"capabilities,omitempty"`
	Capacity     *int            `json:"capacity,omitempty"`
	Version      *string         `json:"version,omitempty"`
}

type machineResponse struct {
	ID              tracker.MachineID `json:"id"`
	Hostname        string            `json:"hostname"`
	DisplayName     string            `json:"display_name"`
	Capabilities    map[string]any    `json:"capabilities"`
	Capacity        int               `json:"capacity"`
	Version         string            `json:"version"`
	LastHeartbeatAt time.Time         `json:"last_heartbeat_at"`
	RegisteredAt    time.Time         `json:"registered_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

func (s *Service) claimWorkItem(c echo.Context) error {
	var request claimAPIRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if request.WorkItemID < 0 {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_claim", Message: "work_item_id must be positive when supplied"})
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_claim", Message: err.Error()})
	}
	claim := tracker.ClaimRequest{WorkItemID: request.WorkItemID, MachineID: request.MachineID, SessionID: request.SessionID, TTL: ttl}
	lease, err := s.database.claimNext(c.Request().Context(), claim, tracker.CandidateQuery{
		RepositoryIDs:  request.RepositoryIDs,
		WorkflowStates: request.WorkflowState,
		Scope:          request.Scope,
	})
	if err != nil {
		return trackerAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, lease)
}

func (s *Service) renewLease(c echo.Context) error {
	leaseID, err := apiLeaseID(c)
	if err != nil {
		return trackerAPIError(c, err)
	}
	var request renewLeaseAPIRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	ttl, err := apiTTL(request.TTLSeconds)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_lease", Message: err.Error()})
	}
	lease, err := s.tracker.Renew(c.Request().Context(), tracker.RenewRequest{LeaseID: leaseID, FencingToken: request.FencingToken, TTL: ttl})
	if err != nil {
		return trackerAPIError(c, err)
	}
	return c.JSON(http.StatusOK, lease)
}

func (s *Service) releaseLease(c echo.Context) error {
	leaseID, err := apiLeaseID(c)
	if err != nil {
		return trackerAPIError(c, err)
	}
	var request releaseLeaseAPIRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	err = s.tracker.Release(c.Request().Context(), tracker.ReleaseRequest{LeaseID: leaseID, FencingToken: request.FencingToken, Reason: request.Reason})
	if err != nil {
		return trackerAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) appendWorkItemEvent(c echo.Context) error {
	id, err := apiWorkItemID(c)
	if err != nil {
		return trackerAPIError(c, err)
	}
	var request workEventAPIRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	event := tracker.WorkEvent{
		WorkItemID: id, FencingToken: request.FencingToken, MachineID: request.MachineID,
		SessionID: request.SessionID, RunID: request.RunID, Kind: request.Kind,
		Payload: request.Payload, OccurredAt: request.OccurredAt,
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.config.now().UTC()
	}
	if err := s.tracker.AppendEvent(c.Request().Context(), event); err != nil {
		return trackerAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, event)
}

func trackerAPIError(c echo.Context, err error) error {
	status := http.StatusInternalServerError
	code := "hub_operation_failed"
	message := "Hub operation failed"
	switch {
	case errors.Is(err, ErrNoClaimableWork):
		status, code, message = http.StatusConflict, "no_claimable_work", "No compatible work item is claimable"
	case errors.Is(err, tracker.ErrWorkItemNotFound), errors.Is(err, tracker.ErrInvalidWorkItemID):
		status, code, message = http.StatusNotFound, "work_item_not_found", "Work item was not found"
	case errors.Is(err, tracker.ErrMachineNotFound):
		status, code, message = http.StatusNotFound, "machine_not_found", "Machine was not found"
	case errors.Is(err, tracker.ErrLeaseNotFound):
		status, code, message = http.StatusNotFound, "lease_not_found", "Lease was not found"
	case errors.Is(err, tracker.ErrLeaseConflict):
		status, code, message = http.StatusConflict, "lease_conflict", "Work item is already leased"
	case errors.Is(err, tracker.ErrStaleFencingToken):
		status, code, message = http.StatusConflict, "stale_fencing_token", "Lease fencing token is stale"
	case errors.Is(err, tracker.ErrInvalidClaimRequest), errors.Is(err, tracker.ErrInvalidLeaseRequest), errors.Is(err, tracker.ErrInvalidWorkEvent), errors.Is(err, tracker.ErrInvalidCandidateQuery):
		status, code, message = http.StatusUnprocessableEntity, "invalid_request", "Request is invalid"
	}
	return c.JSON(status, apiErrorResponse{Code: code, Message: message})
}

func (d *database) claimNext(ctx context.Context, request tracker.ClaimRequest, query tracker.CandidateQuery) (lease tracker.Lease, resultErr error) {
	request.MachineID = tracker.MachineID(strings.TrimSpace(string(request.MachineID)))
	request.SessionID = strings.TrimSpace(request.SessionID)
	query.Scope = strings.TrimSpace(query.Scope)
	if request.MachineID == "" || request.SessionID == "" || request.TTL <= 0 {
		return tracker.Lease{}, tracker.ErrInvalidClaimRequest
	}
	repositoryIDs, err := normalizedRepositoryIDs(query.RepositoryIDs)
	if err != nil {
		return tracker.Lease{}, err
	}
	workflowStates := normalizedQueryStrings(query.WorkflowStates)
	if len(query.WorkflowStates) > 0 && len(workflowStates) == 0 {
		return tracker.Lease{}, tracker.ErrInvalidCandidateQuery
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return tracker.Lease{}, fmt.Errorf("begin hub claim next: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	now, err := d.currentTime()
	if err != nil {
		return tracker.Lease{}, err
	}
	if existing, found, err := readLeaseBySession(ctx, tx, request.SessionID); err != nil {
		return tracker.Lease{}, err
	} else if found {
		if existing.session.Machine.ID != request.MachineID || !existing.session.ExpiresAt.After(now) {
			return tracker.Lease{}, fmt.Errorf("%w: session is no longer claimable", tracker.ErrLeaseConflict)
		}
		if err := tx.Commit(); err != nil {
			return tracker.Lease{}, fmt.Errorf("commit idempotent hub claim next: %w", err)
		}
		return leaseFromRecord(existing), nil
	}
	if request.WorkItemID > 0 {
		if err := requireWorkItem(ctx, tx, request.WorkItemID); err != nil {
			return tracker.Lease{}, err
		}
	}
	capacity, err := machineClaimCapacity(ctx, tx, request.MachineID, now)
	if err != nil {
		return tracker.Lease{}, err
	}
	if capacity <= 0 {
		return tracker.Lease{}, ErrNoClaimableWork
	}
	ids, err := claimCandidateIDs(ctx, tx, query.Scope, repositoryIDs, workflowStates)
	if err != nil {
		return tracker.Lease{}, err
	}
	for _, id := range ids {
		if request.WorkItemID > 0 && id != request.WorkItemID {
			continue
		}
		current, found, err := readUnreleasedLease(ctx, tx, id)
		if err != nil {
			return tracker.Lease{}, err
		}
		if found && current.session.ExpiresAt.After(now) {
			if request.WorkItemID > 0 {
				return tracker.Lease{}, fmt.Errorf("%w: work item %d is held by lease %s", tracker.ErrLeaseConflict, id, current.session.ID)
			}
			continue
		}
		request.WorkItemID = id
		lease, err = d.claimInTransaction(ctx, tx, request, now)
		if err != nil {
			return tracker.Lease{}, err
		}
		if err := tx.Commit(); err != nil {
			return tracker.Lease{}, fmt.Errorf("commit hub claim next: %w", err)
		}
		return lease, nil
	}
	return tracker.Lease{}, ErrNoClaimableWork
}

func readLeaseBySession(ctx context.Context, tx *sql.Tx, sessionID string) (leaseRecord, bool, error) {
	return scanLeaseRecord(tx.QueryRowContext(ctx, `
SELECT l.issue_id, l.lease_id, l.fencing_token, l.machine_id, m.hostname, m.display_name,
       l.session_id, l.acquired_at, l.renewed_at, l.expires_at, l.released_at
FROM leases l
JOIN machines m ON m.id = l.machine_id
WHERE l.session_id = ?`, sessionID))
}

func machineClaimCapacity(ctx context.Context, tx *sql.Tx, machineID tracker.MachineID, now time.Time) (int, error) {
	var capacity int
	if err := tx.QueryRowContext(ctx, "SELECT capacity FROM machines WHERE id = ?", machineID).Scan(&capacity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: %s", tracker.ErrMachineNotFound, machineID)
		}
		return 0, fmt.Errorf("read hub machine capacity: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT expires_at FROM leases WHERE machine_id = ? AND released_at IS NULL", machineID)
	if err != nil {
		return 0, fmt.Errorf("query hub machine leases: %w", err)
	}
	defer rows.Close()
	active := 0
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return 0, fmt.Errorf("scan hub machine lease expiry: %w", err)
		}
		expiresAt, err := parseTimeValue(value)
		if err != nil {
			return 0, fmt.Errorf("parse hub machine lease expiry: %w", err)
		}
		if expiresAt.After(now) {
			active++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate hub machine leases: %w", err)
	}
	return capacity - active, nil
}

func claimCandidateIDs(ctx context.Context, tx *sql.Tx, scope string, repositoryIDs []tracker.RepositoryID, workflowStates []string) ([]tracker.WorkItemID, error) {
	repositoryFilter := make(map[tracker.RepositoryID]struct{}, len(repositoryIDs))
	for _, id := range repositoryIDs {
		repositoryFilter[id] = struct{}{}
	}
	workflowFilter := make(map[string]struct{}, len(workflowStates))
	for _, state := range workflowStates {
		workflowFilter[state] = struct{}{}
	}
	rows, err := tx.QueryContext(ctx, `
SELECT i.id, r.id, lower(trim(ws.detent_state))
FROM issues i
JOIN repositories r ON r.id = i.repository_id
LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN queue_entries q ON q.id = (
  SELECT candidate.id
  FROM queue_entries candidate
  WHERE candidate.issue_id = i.id
    AND (? = '' OR candidate.scope = ?)
  ORDER BY CASE WHEN candidate.scope = ? THEN 0 ELSE 1 END, candidate.scope, candidate.id
  LIMIT 1
)
WHERE lower(trim(i.github_state)) = 'open'
  AND ws.id IS NOT NULL
  AND ws.terminal = 0
  AND lower(trim(ws.detent_state)) <> 'cancelled'
  AND ws.dispatchable = 1
  AND (? = '' OR q.id IS NOT NULL)
  AND NOT EXISTS (
    SELECT 1
    FROM issue_dependencies dependency
    JOIN issues blocker ON blocker.id = dependency.blocker_issue_id
    LEFT JOIN workflow_states blocker_state ON blocker_state.id = blocker.workflow_state_id
    WHERE dependency.dependent_issue_id = i.id
      AND (blocker_state.id IS NULL OR blocker_state.terminal = 0)
  )
ORDER BY
  CASE q.priority_override WHEN 0 THEN 0 WHEN 1 THEN 1 WHEN 2 THEN 2 WHEN 3 THEN 3 ELSE 4 END,
  CASE WHEN q.rank IS NULL OR trim(q.rank) = '' THEN 1 ELSE 0 END,
  trim(q.rank), i.created_at, lower(trim(r.github_owner)), lower(trim(r.github_name)), i.github_number, i.id`, scope, scope, scope, scope)
	if err != nil {
		return nil, fmt.Errorf("query hub claim candidates: %w", err)
	}
	defer rows.Close()
	var ids []tracker.WorkItemID
	for rows.Next() {
		var id tracker.WorkItemID
		var repositoryID tracker.RepositoryID
		var workflowState string
		if err := rows.Scan(&id, &repositoryID, &workflowState); err != nil {
			return nil, fmt.Errorf("scan hub claim candidate: %w", err)
		}
		if len(repositoryFilter) > 0 {
			if _, ok := repositoryFilter[repositoryID]; !ok {
				continue
			}
		}
		if len(workflowFilter) > 0 {
			if _, ok := workflowFilter[workflowState]; !ok {
				continue
			}
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hub claim candidates: %w", err)
	}
	return ids, nil
}

func (s *Service) registerMachine(c echo.Context) error {
	var request machineRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	request.ID = tracker.MachineID(strings.TrimSpace(string(request.ID)))
	request.Hostname = strings.TrimSpace(request.Hostname)
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	request.Version = strings.TrimSpace(request.Version)
	if request.ID == "" || request.Hostname == "" || request.Version == "" || request.Capacity < 0 {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_machine", Message: "Machine ID, hostname, version, and non-negative capacity are required"})
	}
	capabilities, err := json.Marshal(request.Capabilities)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_machine", Message: "Machine capabilities are invalid"})
	}
	if string(capabilities) == "null" {
		capabilities = []byte("{}")
	}
	now, err := s.database.currentTime()
	if err != nil {
		return s.internalAPIError(c, "machine_register_failed", "Machine could not be registered", err)
	}
	_, err = s.database.db.ExecContext(c.Request().Context(), `
INSERT INTO machines (id, hostname, display_name, capabilities_json, capacity, version, last_heartbeat_at, registered_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  hostname = excluded.hostname,
  display_name = excluded.display_name,
  capabilities_json = excluded.capabilities_json,
  capacity = excluded.capacity,
  version = excluded.version,
  last_heartbeat_at = excluded.last_heartbeat_at,
  updated_at = excluded.updated_at`,
		request.ID, request.Hostname, request.DisplayName, string(capabilities), request.Capacity, request.Version,
		formatHubTime(now), formatHubTime(now), formatHubTime(now),
	)
	if err != nil {
		return s.internalAPIError(c, "machine_register_failed", "Machine could not be registered", err)
	}
	response, err := s.database.machine(c.Request().Context(), request.ID)
	if err != nil {
		return s.internalAPIError(c, "machine_register_failed", "Machine could not be registered", err)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Service) heartbeatMachine(c echo.Context) error {
	id := tracker.MachineID(strings.TrimSpace(c.Param("id")))
	if id == "" {
		return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "machine_not_found", Message: "Machine was not found"})
	}
	var request machineHeartbeatRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if request.Capacity != nil && *request.Capacity < 0 {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_machine", Message: "Machine capacity must be non-negative"})
	}
	current, err := s.database.machine(c.Request().Context(), id)
	if errors.Is(err, tracker.ErrMachineNotFound) {
		return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "machine_not_found", Message: "Machine was not found"})
	}
	if err != nil {
		return s.internalAPIError(c, "machine_heartbeat_failed", "Machine heartbeat could not be recorded", err)
	}
	if request.DisplayName != nil {
		current.DisplayName = strings.TrimSpace(*request.DisplayName)
	}
	if request.Capabilities != nil {
		current.Capabilities = *request.Capabilities
	}
	if request.Capacity != nil {
		current.Capacity = *request.Capacity
	}
	if request.Version != nil {
		current.Version = strings.TrimSpace(*request.Version)
		if current.Version == "" {
			return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_machine", Message: "Machine version must not be empty"})
		}
	}
	capabilities, err := json.Marshal(current.Capabilities)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_machine", Message: "Machine capabilities are invalid"})
	}
	now, err := s.database.currentTime()
	if err != nil {
		return s.internalAPIError(c, "machine_heartbeat_failed", "Machine heartbeat could not be recorded", err)
	}
	result, err := s.database.db.ExecContext(c.Request().Context(), `
UPDATE machines
SET display_name = ?, capabilities_json = ?, capacity = ?, version = ?, last_heartbeat_at = ?, updated_at = ?
WHERE id = ?`, current.DisplayName, string(capabilities), current.Capacity, current.Version, formatHubTime(now), formatHubTime(now), id)
	if err != nil {
		return s.internalAPIError(c, "machine_heartbeat_failed", "Machine heartbeat could not be recorded", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "machine_not_found", Message: "Machine was not found"})
	}
	current.LastHeartbeatAt = now
	current.UpdatedAt = now
	return c.JSON(http.StatusOK, current)
}

func (d *database) machine(ctx context.Context, id tracker.MachineID) (machineResponse, error) {
	var response machineResponse
	var capabilitiesJSON string
	var heartbeatAt string
	var registeredAt string
	var updatedAt string
	err := d.db.QueryRowContext(ctx, `
SELECT id, hostname, display_name, capabilities_json, capacity, version, last_heartbeat_at, registered_at, updated_at
FROM machines WHERE id = ?`, id).Scan(
		&response.ID, &response.Hostname, &response.DisplayName, &capabilitiesJSON, &response.Capacity,
		&response.Version, &heartbeatAt, &registeredAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return machineResponse{}, fmt.Errorf("%w: %s", tracker.ErrMachineNotFound, id)
	}
	if err != nil {
		return machineResponse{}, fmt.Errorf("read hub machine: %w", err)
	}
	if err := json.Unmarshal([]byte(capabilitiesJSON), &response.Capabilities); err != nil {
		return machineResponse{}, fmt.Errorf("decode hub machine capabilities: %w", err)
	}
	var parseErr error
	response.LastHeartbeatAt, parseErr = parseTimeValue(heartbeatAt)
	if parseErr == nil {
		response.RegisteredAt, parseErr = parseTimeValue(registeredAt)
	}
	if parseErr == nil {
		response.UpdatedAt, parseErr = parseTimeValue(updatedAt)
	}
	if parseErr != nil {
		return machineResponse{}, fmt.Errorf("decode hub machine timestamp: %w", parseErr)
	}
	return response, nil
}
