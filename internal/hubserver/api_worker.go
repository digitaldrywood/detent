package hubserver

import (
	"errors"
	"net/http"
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
	LandingCapacity         int
	LandingCapacityKnown    bool
	LandingBatchGroup       string
	Limit                   int
	After                   tracker.WorkItemID
	SelectionCursor         *runnerSelectionCursor
	WorkItemID              tracker.WorkItemID
	OnlyIDs                 []tracker.WorkItemID
	AvailableAt             time.Time
	MinimumRunnerVersion    string
	DispatchPriorityByState []string
	DispatchPriorityByLabel []string
	PrioritizeUnblockers    bool
	ProviderCandidates      []tracker.NativeCapacityCandidate
	RequireProviderCapacity bool
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
	// is its own work item kind and not project work.
	WorkspaceLane bool
	PageSize      int
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

func (s *Service) trackerAPIError(c echo.Context, err error) error {
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
	return s.refusedAPIError(c, status, code, message, apiErrorResponse{Code: code, Message: message})
}

// claimPreflightAcquire bounds how long a preflight waits for a claim reader;
// a busy pool sends the claim down the full write pass instead of queueing.

// claimMode selects how one claim pass runs: a preflight on the reader that
// stops at the first claimable candidate, a write pass restricted to that
// candidate, or the full write pass.
