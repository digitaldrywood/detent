package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrNoClaimableWork = errors.New("no compatible work item is claimable")

type claimAPIRequest struct {
	PolicyID      string                 `json:"policy_id"`
	WorkItemID    tracker.WorkItemID     `json:"work_item_id,omitempty"`
	MachineID     tracker.MachineID      `json:"machine_id"`
	SessionID     string                 `json:"session_id"`
	TTLSeconds    int64                  `json:"ttl_seconds"`
	RepositoryIDs []tracker.RepositoryID `json:"repository_ids,omitempty"`
	Repositories  []string               `json:"repositories,omitempty"`
	WorkflowState []string               `json:"workflow_states,omitempty"`
	Authors       []string               `json:"authors,omitempty"`
	Assignees     []string               `json:"assignees,omitempty"`
	LabelInclude  []string               `json:"label_include,omitempty"`
	LabelExclude  []string               `json:"label_exclude,omitempty"`
	Scope         string                 `json:"scope,omitempty"`
}

type claimCandidateQuery struct {
	PolicyID       string
	RequirePolicy  bool
	NativeScope    *nativeScope
	RepositoryIDs  []tracker.RepositoryID
	Repositories   []string
	WorkflowStates []string
	Authors        []string
	Assignees      []string
	LabelInclude   []string
	LabelExclude   []string
	Scope          string
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
	Payload      *legacyProgressData  `json:"payload,omitempty"`
	OccurredAt   time.Time            `json:"occurred_at,omitempty"`
}

type legacyProgressData struct {
	Step string `json:"step"`
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
	lease, err := s.database.claimNext(c.Request().Context(), claim, claimCandidateQuery{
		PolicyID:       request.PolicyID,
		RequirePolicy:  true,
		RepositoryIDs:  request.RepositoryIDs,
		Repositories:   request.Repositories,
		WorkflowStates: request.WorkflowState,
		Authors:        request.Authors,
		Assignees:      request.Assignees,
		LabelInclude:   request.LabelInclude,
		LabelExclude:   request.LabelExclude,
		Scope:          request.Scope,
	}, s.config.ReconcileInterval)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	lease.PolicyID, err = s.database.leasePolicyID(c.Request().Context(), lease.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
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
	lease, err := s.database.renew(c.Request().Context(), tracker.RenewRequest{LeaseID: leaseID, FencingToken: request.FencingToken, TTL: ttl}, true)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	lease.PolicyID, err = s.database.leasePolicyID(c.Request().Context(), lease.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
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
	if request.Kind != "progress" || request.Payload != nil && !slices.Contains([]string{"plan", "implement", "test", "review", "complete"}, request.Payload.Step) {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_event", Message: "Legacy progress requires an allowlisted step; use v2 for native events"})
	}
	var payload map[string]any
	if request.Payload != nil {
		payload = map[string]any{"step": request.Payload.Step}
	}
	event := tracker.WorkEvent{
		WorkItemID: id, FencingToken: request.FencingToken, MachineID: request.MachineID,
		SessionID: request.SessionID, RunID: request.RunID, Kind: request.Kind,
		Payload: payload, OccurredAt: request.OccurredAt,
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.config.now().UTC()
	}
	if err := s.database.appendEvent(c.Request().Context(), event, true); err != nil {
		return s.nativeAPIError(c, err)
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

func (d *database) claimNext(ctx context.Context, request tracker.ClaimRequest, query claimCandidateQuery, reconcileInterval time.Duration) (lease tracker.Lease, resultErr error) {
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
	repositories := normalizedQueryStrings(query.Repositories)
	if len(query.Repositories) > 0 && len(repositories) == 0 {
		return tracker.Lease{}, tracker.ErrInvalidCandidateQuery
	}
	workflowStates := normalizedQueryStrings(query.WorkflowStates)
	if len(query.WorkflowStates) > 0 && len(workflowStates) == 0 {
		return tracker.Lease{}, tracker.ErrInvalidCandidateQuery
	}
	authors := normalizedQueryStrings(query.Authors)
	if len(query.Authors) > 0 && len(authors) == 0 {
		return tracker.Lease{}, tracker.ErrInvalidCandidateQuery
	}
	assignees := normalizedQueryStrings(query.Assignees)
	if len(query.Assignees) > 0 && len(assignees) == 0 {
		return tracker.Lease{}, tracker.ErrInvalidCandidateQuery
	}
	labelInclude := normalizedQueryStrings(query.LabelInclude)
	if len(query.LabelInclude) > 0 && len(labelInclude) == 0 {
		return tracker.Lease{}, tracker.ErrInvalidCandidateQuery
	}
	labelExclude := normalizedQueryStrings(query.LabelExclude)
	if len(query.LabelExclude) > 0 && len(labelExclude) == 0 {
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
	if err := authorizeClaimScope(ctx, tx, request, query.NativeScope); err != nil {
		return tracker.Lease{}, err
	}
	var policyScope string
	if query.RequirePolicy {
		policyScope, err = validateClaimPolicy(ctx, tx, query, request.MachineID)
		if err != nil {
			return tracker.Lease{}, err
		}
	}
	if existing, found, err := readLeaseBySession(ctx, tx, request.SessionID); err != nil {
		return tracker.Lease{}, err
	} else if found {
		if err := authorizeClaimItem(ctx, tx, existing.issueID, query.NativeScope); err != nil {
			return tracker.Lease{}, err
		}
		if existing.session.Machine.ID != request.MachineID || !existing.session.ExpiresAt.After(now) {
			return tracker.Lease{}, fmt.Errorf("%w: session is no longer claimable", tracker.ErrLeaseConflict)
		}
		if request.WorkItemID > 0 && existing.issueID != request.WorkItemID {
			return tracker.Lease{}, fmt.Errorf("%w: session is already assigned to work item %d", tracker.ErrLeaseConflict, existing.issueID)
		}
		if query.RequirePolicy {
			var pinned string
			if err := tx.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id = ? AND scope = ?", existing.session.ID, policyScope).Scan(&pinned); err != nil || pinned != query.PolicyID {
				return tracker.Lease{}, policyMismatch("Existing claim is pinned to a different policy; release it before requesting a new attempt")
			}
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
	claimableRepositories := make(map[tracker.RepositoryID]struct{})
	if query.NativeScope == nil {
		freshness, err := queryRepositoryFreshness(ctx, tx, now, reconcileInterval)
		if err != nil {
			return tracker.Lease{}, err
		}
		for _, repository := range freshness.Repositories {
			if repository.Status == "fresh" {
				claimableRepositories[tracker.RepositoryID(repository.ID)] = struct{}{}
			}
		}
	}
	ids, err := claimCandidateIDs(ctx, tx, query, repositoryIDs, repositories, workflowStates, authors, assignees, labelInclude, labelExclude, claimableRepositories)
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
		if query.RequirePolicy {
			if _, err := tx.ExecContext(ctx, "INSERT INTO lease_policies (lease_id, scope, policy_id) VALUES (?, ?, ?)", lease.ID, policyScope, query.PolicyID); err != nil {
				return tracker.Lease{}, err
			}
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

func claimCandidateIDs(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, repositoryIDs []tracker.RepositoryID, repositories []string, workflowStates []string, authors []string, assignees []string, labelInclude []string, labelExclude []string, claimableRepositories map[tracker.RepositoryID]struct{}) ([]tracker.WorkItemID, error) {
	scope := query.Scope
	organization, project := "", ""
	if query.NativeScope != nil {
		organization, project = string(query.NativeScope.organization), string(query.NativeScope.project)
	}
	repositoryFilter := make(map[tracker.RepositoryID]struct{}, len(repositoryIDs))
	for _, id := range repositoryIDs {
		repositoryFilter[id] = struct{}{}
	}
	repositoryNameFilter := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		repositoryNameFilter[strings.ToLower(repository)] = struct{}{}
	}
	workflowFilter := stringSet(workflowStates)
	authorFilter := stringSet(authors)
	assigneeFilter := stringSet(assignees)
	labelIncludeFilter := stringSet(labelInclude)
	labelExcludeFilter := stringSet(labelExclude)
	rows, err := tx.QueryContext(ctx, `
SELECT i.id, COALESCE(r.id, 0), COALESCE(r.github_owner, ''), COALESCE(r.github_name, ''), lower(trim(ws.detent_state)),
       lower(trim(i.author_login)), i.labels_json, i.assignees_json
FROM issues i
LEFT JOIN repositories r ON r.id = i.repository_id
JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
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
  AND ((? = '' AND p.profile = 'github_compatible') OR (i.organization_id = ? AND i.project_id = ? AND p.profile = 'native'))
  AND ws.id IS NOT NULL
  AND ws.terminal = 0
  AND lower(trim(ws.detent_state)) <> 'cancelled'
  AND ws.dispatchable = 1
  AND (? = '' OR q.id IS NOT NULL)
  AND (p.require_dependencies = 0 OR NOT EXISTS (
    SELECT 1
    FROM issue_dependencies dependency
    JOIN issues blocker ON blocker.id = dependency.blocker_issue_id
    LEFT JOIN workflow_states blocker_state ON blocker_state.id = blocker.workflow_state_id
    WHERE dependency.dependent_issue_id = i.id
      AND (blocker_state.id IS NULL OR blocker_state.terminal = 0)
  ))
ORDER BY
  CASE q.priority_override WHEN 0 THEN 0 WHEN 1 THEN 1 WHEN 2 THEN 2 WHEN 3 THEN 3 ELSE 4 END,
  CASE WHEN q.rank IS NULL OR trim(q.rank) = '' THEN 1 ELSE 0 END,
  trim(q.rank), i.created_at, lower(trim(r.github_owner)), lower(trim(r.github_name)), i.github_number, i.id`, scope, scope, scope, organization, organization, project, scope)
	if err != nil {
		return nil, fmt.Errorf("query hub claim candidates: %w", err)
	}
	defer rows.Close()
	var ids []tracker.WorkItemID
	for rows.Next() {
		var id tracker.WorkItemID
		var repositoryID tracker.RepositoryID
		var repositoryOwner string
		var repositoryName string
		var workflowState string
		var authorID string
		var labelsJSON string
		var assigneesJSON string
		if err := rows.Scan(&id, &repositoryID, &repositoryOwner, &repositoryName, &workflowState, &authorID, &labelsJSON, &assigneesJSON); err != nil {
			return nil, fmt.Errorf("scan hub claim candidate: %w", err)
		}
		if _, ok := claimableRepositories[repositoryID]; !ok && query.NativeScope == nil {
			continue
		}
		if len(repositoryFilter) > 0 {
			if _, ok := repositoryFilter[repositoryID]; !ok {
				continue
			}
		}
		if len(repositoryNameFilter) > 0 {
			if _, ok := repositoryNameFilter[strings.ToLower(strings.TrimSpace(repositoryOwner)+"/"+strings.TrimSpace(repositoryName))]; !ok {
				continue
			}
		}
		if len(workflowFilter) > 0 {
			if _, ok := workflowFilter[workflowState]; !ok {
				continue
			}
		}
		if len(authorFilter) > 0 {
			if _, ok := authorFilter[authorID]; !ok {
				continue
			}
		}
		labels, err := normalizedJSONStringSet(labelsJSON)
		if err != nil {
			return nil, fmt.Errorf("decode hub claim candidate labels: %w", err)
		}
		candidateAssignees, err := normalizedJSONStringSet(assigneesJSON)
		if err != nil {
			return nil, fmt.Errorf("decode hub claim candidate assignees: %w", err)
		}
		if len(assigneeFilter) > 0 && !setsIntersect(candidateAssignees, assigneeFilter) {
			continue
		}
		if !setContainsAll(labels, labelIncludeFilter) || setsIntersect(labels, labelExcludeFilter) {
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hub claim candidates: %w", err)
	}
	return ids, nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func normalizedJSONStringSet(value string) (map[string]struct{}, error) {
	var values []string
	if err := json.Unmarshal([]byte(value), &values); err != nil {
		return nil, err
	}
	return stringSet(values), nil
}

func setsIntersect(left map[string]struct{}, right map[string]struct{}) bool {
	for value := range left {
		if _, ok := right[value]; ok {
			return true
		}
	}
	return false
}

func setContainsAll(haystack map[string]struct{}, needles map[string]struct{}) bool {
	for value := range needles {
		if _, ok := haystack[value]; !ok {
			return false
		}
	}
	return true
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
	result, err := s.database.db.ExecContext(c.Request().Context(), `
INSERT INTO machines (id, hostname, display_name, capabilities_json, capacity, version, last_heartbeat_at, registered_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  hostname = excluded.hostname,
  display_name = excluded.display_name,
  capabilities_json = excluded.capabilities_json,
  capacity = excluded.capacity,
  version = excluded.version,
  last_heartbeat_at = excluded.last_heartbeat_at,
  updated_at = excluded.updated_at
WHERE machines.organization_id IS NULL`,
		request.ID, request.Hostname, request.DisplayName, string(capabilities), request.Capacity, request.Version,
		formatHubTime(now), formatHubTime(now), formatHubTime(now),
	)
	if err != nil {
		return s.internalAPIError(c, "machine_register_failed", "Machine could not be registered", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "machine_not_found", Message: "Machine was not found"})
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
