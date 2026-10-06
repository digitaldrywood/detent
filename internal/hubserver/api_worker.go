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

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
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
	Limit                   int
	After                   tracker.WorkItemID
	WorkItemID              tracker.WorkItemID
	OnlyIDs                 []tracker.WorkItemID
	AvailableAt             time.Time
	MinimumRunnerVersion    string
	DispatchPriorityByState []string
	DispatchPriorityByLabel []string
	PrioritizeUnblockers    bool
	ProviderCandidates      []tracker.NativeCapacityCandidate
	PolicyID                string
	RequirePolicy           bool
	NativeScope             *nativeScope
	RepositoryIDs           []tracker.RepositoryID
	Repositories            []string
	WorkflowStates          []string
	Authors                 []string
	Assignees               []string
	LabelInclude            []string
	LabelExclude            []string
	Scope                   string
	// WorkspaceLane reports that the claim came from a runner's workspace
	// lane, which asked for workspace items by declaring the workspace
	// capability. Every other claim, and the provider candidate preview, is
	// an ordinary issue claim and never sees a workspace item: a workspace
	// is its own work item kind and not project work (decisions section
	// 18.1).
	WorkspaceLane bool
	HomeProjects  []tracker.ProjectID
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
		MinimumRunnerVersion: minimumRunnerVersion(s.config.Version),
		PolicyID:             request.PolicyID,
		RequirePolicy:        true,
		RepositoryIDs:        request.RepositoryIDs,
		Repositories:         request.Repositories,
		WorkflowStates:       request.WorkflowState,
		Authors:              request.Authors,
		Assignees:            request.Assignees,
		LabelInclude:         request.LabelInclude,
		LabelExclude:         request.LabelExclude,
		Scope:                request.Scope,
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
			if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
				resultErr = errors.Join(resultErr, rollbackErr)
			}
		}
	}()
	now, err := d.currentTime()
	if err != nil {
		return tracker.Lease{}, err
	}
	if err := authorizeClaimScope(ctx, tx, request, query.NativeScope); err != nil {
		return tracker.Lease{}, err
	}
	if query.NativeScope != nil {
		if err := requireRunnerAuthority(ctx, tx, *query.NativeScope, now); err != nil {
			return tracker.Lease{}, err
		}
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
		if query.NativeScope != nil {
			if err := requireLeaseRunner(ctx, tx, existing.session.ID, *query.NativeScope); err != nil {
				return tracker.Lease{}, err
			}
		}
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
	var version string
	if err := tx.QueryRowContext(ctx, "SELECT version FROM machines WHERE id = ?", request.MachineID).Scan(&version); err != nil {
		return tracker.Lease{}, err
	}
	if err := runnerVersionError(query.MinimumRunnerVersion, version); err != nil {
		return tracker.Lease{}, err
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
		if query.NativeScope != nil && request.WorkItemID > 0 {
			if err := recordNativeSchedulingOutcome(ctx, tx, query.NativeScope, request.WorkItemID, tracker.NativeSchedulerDecision{Source: "native_host_capacity", Outcome: "skipped", Reason: "Shared host capacity is full or paused"}, now); err != nil {
				return tracker.Lease{}, err
			}
			if err := tx.Commit(); err != nil {
				return tracker.Lease{}, err
			}
		}
		if query.NativeScope != nil && query.NativeScope.credential.Runner.RunnerID != "" {
			return tracker.Lease{}, &nativeError{Code: "host_capacity", Message: "Shared host capacity is full or paused", status: http.StatusConflict}
		}
		return tracker.Lease{}, ErrNoClaimableWork
	}
	if query.NativeScope != nil && query.NativeScope.credential.Runner.RunnerID != "" {
		if err := validateRunnerDispatch(ctx, tx, *query.NativeScope, now); err != nil {
			var refusal *nativeError
			if request.WorkItemID > 0 && errors.As(err, &refusal) {
				if recordErr := recordNativeSchedulingOutcome(ctx, tx, query.NativeScope, request.WorkItemID, tracker.NativeSchedulerDecision{Source: "native_runner_routing", Outcome: "skipped", Reason: refusal.Code}, now); recordErr != nil {
					return tracker.Lease{}, recordErr
				}
				if commitErr := tx.Commit(); commitErr != nil {
					return tracker.Lease{}, commitErr
				}
			}
			return tracker.Lease{}, err
		}
		if !query.WorkspaceLane {
			if err := validateRunnerIsolation(ctx, tx, *query.NativeScope, now); err != nil {
				return tracker.Lease{}, err
			}
		}
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
	homeID, homeRestricted, err := d.runnerHomeSelection(ctx, tx, query, request.WorkItemID, now)
	if err != nil {
		return tracker.Lease{}, err
	}
	query.WorkItemID = request.WorkItemID
	query.AvailableAt = now
	if homeRestricted && homeID > 0 {
		query.OnlyIDs = []tracker.WorkItemID{homeID}
	}
	if query.NativeScope != nil {
		query.Limit = 100
		if request.WorkItemID > 0 || len(query.ProviderCandidates) == 1 || homeRestricted && homeID > 0 {
			query.Limit = 1
		}
	}
	ids, err := claimCandidateIDs(ctx, tx, query, repositoryIDs, repositories, workflowStates, authors, assignees, labelInclude, labelExclude, claimableRepositories)
	if err != nil {
		return tracker.Lease{}, err
	}
	// Workspace items are gated per candidate rather than per credential
	// (decisions section 18.1): eligibility depends on the surfaces the
	// workspace requires and on whether its attempt's worktree is still
	// retained on one particular runner, so the answer differs between two
	// workspaces the same runner is looking at.
	workspaceGate, err := gateWorkspaceClaim(ctx, tx, query.NativeScope, query.WorkspaceLane, d.workspaceRetainAfterRun, d.workspaceTerminalIsolation, now)
	if err != nil {
		return tracker.Lease{}, err
	}
	var providerWait error
	for _, id := range ids {
		if homeRestricted && id != homeID {
			continue
		}
		if request.WorkItemID > 0 && id != request.WorkItemID {
			continue
		}
		if _, ineligible := workspaceGate.skip[id]; ineligible {
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
		ready, evaluated, err := nativeLandingCandidateReady(ctx, tx, query.NativeScope, id, request.MachineID, now, true)
		if err != nil {
			return tracker.Lease{}, err
		}
		if evaluated {
			if err := recordNativeSchedulingDecision(ctx, tx, query.NativeScope, id, ready, now); err != nil {
				return tracker.Lease{}, err
			}
		}
		if !ready {
			continue
		}
		// A workspace claim reserves one slot of the runner's capacity and
		// nothing else (decisions section 18.1): the session serves files
		// over the relay and runs no model, so it never goes through provider
		// requirement matching and never carries a provider reservation.
		var reservation providercapacity.Reservation
		reserved := false
		if !workspaceGate.skipsProviderReservation(id) {
			reservation, reserved, err = selectProviderCapacity(ctx, tx, query, id, now)
			if err != nil {
				if errors.Is(err, ErrNoClaimableWork) || isProviderWait(err) {
					if recordErr := recordNativeSchedulingOutcome(ctx, tx, query.NativeScope, id, tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: "Current provider requirement has no available reservation"}, now); recordErr != nil {
						return tracker.Lease{}, recordErr
					}
				}
				if errors.Is(err, ErrNoClaimableWork) {
					continue
				}
				if isProviderWait(err) {
					providerWait = err
					continue
				}
				return tracker.Lease{}, err
			}
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
		if query.NativeScope != nil && query.NativeScope.credential.Runner.RunnerID != "" {
			snapshot, err := readRunnerRoutingSnapshot(ctx, tx, query.NativeScope.organization, query.NativeScope.credential.Runner.RunnerID, now)
			if err != nil {
				return tracker.Lease{}, err
			}
			policy, err := json.Marshal(isolation.Policy{Tier: snapshot.Routing.IsolationTier, HostServices: snapshot.Routing.HostServices})
			if err != nil {
				return tracker.Lease{}, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO lease_runners (lease_id, runner_id, isolation_policy_json) VALUES (?, ?, ?)", lease.ID, query.NativeScope.credential.Runner.RunnerID, string(policy)); err != nil {
				return tracker.Lease{}, err
			}
		}
		if reserved && query.NativeScope != nil {
			if err := writeProviderReservation(ctx, tx, lease.ID, query.NativeScope.organization, reservation); err != nil {
				return tracker.Lease{}, err
			}
		}
		if err := recordNativeSchedulingOutcome(ctx, tx, query.NativeScope, id, tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", Reason: "The scheduler granted a fenced lease"}, now); err != nil {
			return tracker.Lease{}, err
		}
		if err := tx.Commit(); err != nil {
			return tracker.Lease{}, fmt.Errorf("commit hub claim next: %w", err)
		}
		return lease, nil
	}
	if err := tx.Commit(); err != nil {
		return tracker.Lease{}, fmt.Errorf("commit idle hub claim: %w", err)
	}
	if providerWait != nil {
		return tracker.Lease{}, providerWait
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

// notAlreadyAnsweredClause excludes an item whose most recent attempt already
// succeeded against the item as it stands, for an issue query that aliases the
// issues table as i.
//
// The latest attempt is the one with the highest fencing token. It applies to
// native projects only: on a github_compatible project the issues row is a
// projection whose writes never bump revision, so the clause would strand an
// item whose lane moved on GitHub.
const notAlreadyAnsweredClause = `(p.profile <> 'native' OR lower(trim(ws.detent_state)) = 'merging' OR NOT EXISTS (SELECT 1 FROM native_attempts answered
 WHERE answered.organization_id = i.organization_id
   AND answered.project_id = i.project_id
   AND answered.work_item_id = i.native_id
   AND answered.status = 'succeeded'
   AND NOT (COALESCE(json_extract(answered.data_json, '$.disposition.status'), '') = 'in_progress'
     AND COALESCE(json_extract(answered.data_json, '$.disposition.blockers'), 1) = 0
     AND COALESCE(json_extract(answered.data_json, '$.disposition.human_action'), 1) = 0
     AND COALESCE(json_extract(answered.data_json, '$.disposition.reason_code'), '') = '')
   AND answered.work_item_revision >= i.revision
   AND answered.dispatch_generation >= i.dispatch_generation
   AND answered.fencing_token = (SELECT max(latest.fencing_token) FROM native_attempts latest
     WHERE latest.organization_id = i.organization_id
       AND latest.project_id = i.project_id
       AND latest.work_item_id = i.native_id)))`

// claimWorkspaceExclusionArg binds the candidate query's workspace exclusion.
// A workspace session's dispatch issue is not project work and never becomes
// project work: closing the workspace closes the association, it does not hand
// the issue to the issue lane (decisions section 18.1). Only a claim that
// declared the workspace capability is offered one, and the exclusion lives in
// the candidate query rather than in a skip set so that the provider candidate
// preview, which shares this query, does not offer one either.
//
// It is a bound argument over a constant clause rather than a clause spliced
// into the statement, so the query the driver prepares stays one constant
// string.
func claimWorkspaceExclusionArg(query claimCandidateQuery) int {
	if query.WorkspaceLane {
		return 1
	}
	return 0
}

func claimCandidateIDs(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, repositoryIDs []tracker.RepositoryID, repositories []string, workflowStates []string, authors []string, assignees []string, labelInclude []string, labelExclude []string, claimableRepositories map[tracker.RepositoryID]struct{}) ([]tracker.WorkItemID, error) {
	if query.NativeScope != nil {
		return nativeCandidateIDs(ctx, tx, query, repositoryIDs, repositories, workflowStates, authors, assignees, labelInclude, labelExclude)
	}
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
	homeProjects, err := marshalNative(query.HomeProjects)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT i.id, COALESCE(r.id, 0), COALESCE(r.github_owner, ''), COALESCE(r.github_name, ''), lower(trim(ws.detent_state)),
       lower(trim(i.author_login)), i.labels_json, i.assignees_json,
       q.priority_override, COALESCE(q.rank, ''), COALESCE(i.native_created_at, i.created_at), i.project_id, i.number,
       CASE WHEN ? THEN (SELECT count(DISTINCT d.dependent_issue_id) FROM issue_dependencies d
         JOIN issues dependent ON dependent.id = d.dependent_issue_id
         JOIN projects dp ON dp.id = dependent.project_id AND dp.require_dependencies = 1
         LEFT JOIN workflow_states ds ON ds.id = dependent.workflow_state_id
         WHERE d.blocker_issue_id = i.id AND dependent.archived = 0 AND COALESCE(ds.terminal, 0) = 0) ELSE 0 END
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
WHERE (p.profile = 'native' OR lower(trim(i.github_state)) = 'open')
	AND NOT EXISTS (SELECT 1 FROM github_imports g WHERE g.work_item_id = i.native_id AND g.intake_pending = 1)
  AND ((? = '' AND p.profile = 'github_compatible') OR (i.organization_id = ? AND (i.project_id = ? AND ? = 0 OR i.project_id IN (SELECT value FROM json_each(?))) AND p.profile = 'native'))
  AND ws.id IS NOT NULL
  AND ws.terminal = 0
  AND i.archived = 0
  AND lower(trim(ws.detent_state)) <> 'cancelled'
  AND ws.dispatchable = 1
  AND (? = 1 OR `+notWorkspaceItemClause+`)
  AND `+notAlreadyAnsweredClause+`
  AND ((? = '' AND ? = 0) OR q.id IS NOT NULL)
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
  trim(q.rank), i.created_at, lower(trim(r.github_owner)), lower(trim(r.github_name)), i.github_number, i.id`, query.PrioritizeUnblockers, scope, scope, scope, organization, organization, project, len(query.HomeProjects), homeProjects, claimWorkspaceExclusionArg(query), scope, len(query.HomeProjects))
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
		var priority sql.NullInt64
		var rank, created, projectID string
		var number, unblockerCount int
		if err := rows.Scan(&id, &repositoryID, &repositoryOwner, &repositoryName, &workflowState, &authorID, &labelsJSON, &assigneesJSON, &priority, &rank, &created, &projectID, &number, &unblockerCount); err != nil {
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
