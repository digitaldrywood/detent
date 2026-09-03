package tracker

import (
	"context"
	"errors"
	"time"
)

const DefaultCandidateLimit = 100

const (
	QueuePriorityUrgent = iota
	QueuePriorityHigh
	QueuePriorityNormal
	QueuePriorityLow
)

var (
	ErrInvalidCandidateQuery = errors.New("invalid tracker candidate query")
	ErrInvalidWorkItemID     = errors.New("invalid tracker work item ID")
	ErrNotImplemented        = errors.New("tracker operation not implemented")
	ErrStoreRequired         = errors.New("tracker store is required")
)

type WorkItemID int64

type RepositoryID int64

type MachineID string

type LeaseID string

type FencingToken int64

type SourceState string

const (
	SourceStateUnknown SourceState = "unknown"
	SourceStateOpen    SourceState = "open"
	SourceStateClosed  SourceState = "closed"
)

type SyncStatus string

const (
	SyncStatusSynced   SyncStatus = "synced"
	SyncStatusPending  SyncStatus = "pending"
	SyncStatusRetrying SyncStatus = "retrying"
	SyncStatusError    SyncStatus = "error"
	SyncStatusStale    SyncStatus = "stale"
)

type DispatchReasonCode string

const (
	DispatchReasonSourceStateUnknown           DispatchReasonCode = "source_state_unknown"
	DispatchReasonIssueClosed                  DispatchReasonCode = "issue_closed"
	DispatchReasonWorkflowStateMissing         DispatchReasonCode = "workflow_state_missing"
	DispatchReasonWorkflowStateTerminal        DispatchReasonCode = "workflow_state_terminal"
	DispatchReasonWorkflowStateNotDispatchable DispatchReasonCode = "workflow_state_not_dispatchable"
	DispatchReasonBlockerUnresolved            DispatchReasonCode = "blocker_unresolved"
	DispatchReasonLeaseActive                  DispatchReasonCode = "lease_active"
	DispatchReasonRepositoryConcurrencyLimit   DispatchReasonCode = "repository_concurrency_limit"
	DispatchReasonProjectConcurrencyLimit      DispatchReasonCode = "project_concurrency_limit"
	DispatchReasonNoCompatibleMachine          DispatchReasonCode = "no_compatible_machine"
	DispatchReasonMachineUnhealthy             DispatchReasonCode = "machine_unhealthy"
	DispatchReasonMachineCapacityFull          DispatchReasonCode = "machine_capacity_full"
	DispatchReasonOperatorPaused               DispatchReasonCode = "operator_paused"
	DispatchReasonSyncUnsafe                   DispatchReasonCode = "sync_unsafe"
	DispatchReasonSyncError                    DispatchReasonCode = "sync_error"
	DispatchReasonSyncStale                    DispatchReasonCode = "sync_stale"
)

type RepositoryReference struct {
	ID           RepositoryID `json:"id"`
	GitHubNodeID string       `json:"github_node_id"`
	Owner        string       `json:"owner"`
	Name         string       `json:"name"`
}

type GitHubIssueReference struct {
	NodeID     string `json:"node_id"`
	DatabaseID *int64 `json:"database_id,omitempty"`
	Number     int    `json:"number"`
}

type WorkflowState struct {
	ID           int64  `json:"id"`
	SourceName   string `json:"source_name"`
	Name         string `json:"name"`
	Terminal     bool   `json:"terminal"`
	Dispatchable bool   `json:"dispatchable"`
}

type QueueSummary struct {
	Scope        string `json:"scope"`
	State        string `json:"state"`
	Rank         string `json:"rank"`
	PriorityRank *int   `json:"priority_rank,omitempty"`
}

type WorkItemReference struct {
	ID            WorkItemID     `json:"id"`
	Repository    string         `json:"repository"`
	IssueNumber   int            `json:"issue_number"`
	Title         string         `json:"title"`
	URL           string         `json:"url"`
	SourceState   SourceState    `json:"source_state"`
	WorkflowState *WorkflowState `json:"workflow_state,omitempty"`
}

type MachineSummary struct {
	ID          MachineID `json:"id"`
	Hostname    string    `json:"hostname"`
	DisplayName string    `json:"display_name"`
}

type LeaseSummary struct {
	ID           LeaseID        `json:"id"`
	FencingToken FencingToken   `json:"fencing_token"`
	Machine      MachineSummary `json:"machine"`
	SessionID    string         `json:"session_id"`
	AcquiredAt   time.Time      `json:"acquired_at"`
	RenewedAt    time.Time      `json:"renewed_at"`
	ExpiresAt    time.Time      `json:"expires_at"`
}

type CheckSummary struct {
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	Total      int    `json:"total,omitempty"`
	Pending    int    `json:"pending,omitempty"`
	Passed     int    `json:"passed,omitempty"`
	Failed     int    `json:"failed,omitempty"`
}

type ReviewSummary struct {
	State            string `json:"state,omitempty"`
	Decision         string `json:"decision,omitempty"`
	Approvals        int    `json:"approvals,omitempty"`
	ChangesRequested int    `json:"changes_requested,omitempty"`
	Comments         int    `json:"comments,omitempty"`
}

type MergeSummary struct {
	State       string     `json:"state,omitempty"`
	Ready       bool       `json:"ready"`
	RefreshedAt *time.Time `json:"refreshed_at,omitempty"`
}

type PullRequestSummary struct {
	ID           int64         `json:"id"`
	GitHubNodeID string        `json:"github_node_id"`
	Number       int           `json:"number"`
	Title        string        `json:"title"`
	URL          string        `json:"url"`
	State        string        `json:"state"`
	Draft        bool          `json:"draft"`
	HeadRef      string        `json:"head_ref"`
	HeadSHA      string        `json:"head_sha"`
	BaseRef      string        `json:"base_ref"`
	BaseSHA      string        `json:"base_sha"`
	Checks       CheckSummary  `json:"checks"`
	Reviews      ReviewSummary `json:"reviews"`
	Merge        MergeSummary  `json:"merge"`
}

type DispatchReason struct {
	Code       DispatchReasonCode `json:"code"`
	Message    string             `json:"message"`
	WorkItemID *WorkItemID        `json:"work_item_id,omitempty"`
	LeaseID    LeaseID            `json:"lease_id,omitempty"`
	Repository RepositoryID       `json:"repository_id,omitempty"`
	Scope      string             `json:"scope,omitempty"`
	MachineID  MachineID          `json:"machine_id,omitempty"`
	SessionID  string             `json:"session_id,omitempty"`
	Active     int                `json:"active,omitempty"`
	Limit      int                `json:"limit,omitempty"`
}

type Dispatchability struct {
	Dispatchable bool             `json:"dispatchable"`
	Reasons      []DispatchReason `json:"reasons"`
}

type ConcurrencyUsage struct {
	Active int `json:"active"`
	Limit  int `json:"limit"`
}

type MachineAvailability struct {
	ID            MachineID      `json:"id"`
	Healthy       bool           `json:"healthy"`
	Capacity      int            `json:"capacity"`
	ActiveLeases  int            `json:"active_leases"`
	RepositoryIDs []RepositoryID `json:"repository_ids,omitempty"`
	ProjectScopes []string       `json:"project_scopes,omitempty"`
}

type DispatchSnapshot struct {
	EvaluatedAt            time.Time                         `json:"evaluated_at"`
	TargetMachineID        MachineID                         `json:"target_machine_id,omitempty"`
	TargetSessionID        string                            `json:"target_session_id,omitempty"`
	RepositoryConcurrency  map[RepositoryID]ConcurrencyUsage `json:"repository_concurrency,omitempty"`
	ProjectConcurrency     map[string]ConcurrencyUsage       `json:"project_concurrency,omitempty"`
	Machines               []MachineAvailability             `json:"machines"`
	OperatorPaused         bool                              `json:"operator_paused"`
	PausedRepositories     []RepositoryID                    `json:"paused_repositories,omitempty"`
	PausedProjects         []string                          `json:"paused_projects,omitempty"`
	SyncSafetyBlocked      bool                              `json:"sync_safety_blocked"`
	SyncUnsafeRepositories []RepositoryID                    `json:"sync_unsafe_repositories,omitempty"`
}

type DispatchQueue struct {
	Dispatchable    []WorkItem `json:"dispatchable"`
	NonDispatchable []WorkItem `json:"non_dispatchable"`
}

type WorkItem struct {
	ID              WorkItemID           `json:"id"`
	Repository      RepositoryReference  `json:"repository"`
	GitHub          GitHubIssueReference `json:"github"`
	Title           string               `json:"title"`
	BodyExcerpt     string               `json:"body_excerpt"`
	URL             string               `json:"url"`
	SourceState     SourceState          `json:"source_state"`
	WorkflowState   *WorkflowState       `json:"workflow_state,omitempty"`
	Queue           *QueueSummary        `json:"queue,omitempty"`
	Labels          []string             `json:"labels"`
	Assignees       []string             `json:"assignees"`
	CreatedAt       *time.Time           `json:"created_at,omitempty"`
	UpdatedAt       *time.Time           `json:"updated_at,omitempty"`
	SourceUpdatedAt *time.Time           `json:"source_updated_at,omitempty"`
	SourceSyncedAt  *time.Time           `json:"source_synced_at,omitempty"`
	Blockers        []WorkItemReference  `json:"blockers"`
	Dependents      []WorkItemReference  `json:"dependents"`
	Dispatchability Dispatchability      `json:"dispatchability"`
	ActiveLease     *LeaseSummary        `json:"active_lease,omitempty"`
	PullRequests    []PullRequestSummary `json:"pull_requests"`
	SyncStatus      SyncStatus           `json:"github_sync_status"`
}

type CandidateQuery struct {
	RepositoryIDs  []RepositoryID `json:"repository_ids,omitempty"`
	WorkflowStates []string       `json:"workflow_states,omitempty"`
	Scope          string         `json:"scope,omitempty"`
	Limit          int            `json:"limit,omitempty"`
}

type ClaimRequest struct {
	WorkItemID WorkItemID    `json:"work_item_id"`
	MachineID  MachineID     `json:"machine_id"`
	SessionID  string        `json:"session_id"`
	TTL        time.Duration `json:"ttl"`
}

type RenewRequest struct {
	LeaseID      LeaseID       `json:"lease_id"`
	FencingToken FencingToken  `json:"fencing_token"`
	TTL          time.Duration `json:"ttl"`
}

type ReleaseRequest struct {
	LeaseID      LeaseID      `json:"lease_id"`
	FencingToken FencingToken `json:"fencing_token"`
	Reason       string       `json:"reason,omitempty"`
}

type Lease struct {
	LeaseSummary
	WorkItemID WorkItemID `json:"work_item_id"`
}

type WorkEvent struct {
	WorkItemID   WorkItemID     `json:"work_item_id"`
	FencingToken *FencingToken  `json:"fencing_token,omitempty"`
	MachineID    MachineID      `json:"machine_id,omitempty"`
	SessionID    string         `json:"session_id,omitempty"`
	RunID        string         `json:"run_id,omitempty"`
	Kind         string         `json:"kind"`
	Payload      map[string]any `json:"payload,omitempty"`
	OccurredAt   time.Time      `json:"occurred_at"`
}

type Tracker interface {
	ListCandidates(context.Context, CandidateQuery) ([]WorkItem, error)
	ListDispatchQueue(context.Context, CandidateQuery, DispatchSnapshot) (DispatchQueue, error)
	GetWorkItems(context.Context, []WorkItemID) ([]WorkItem, error)
	Claim(context.Context, ClaimRequest) (Lease, error)
	Renew(context.Context, RenewRequest) (Lease, error)
	Release(context.Context, ReleaseRequest) error
	AppendEvent(context.Context, WorkEvent) error
}

type Store interface {
	ListCandidateRecords(context.Context, CandidateQuery) ([]Record, error)
	GetWorkItemRecords(context.Context, []WorkItemID) ([]Record, error)
}

func NewStore(store Store) (Tracker, error) {
	if store == nil {
		return nil, ErrStoreRequired
	}
	return &storeTracker{store: store}, nil
}

type storeTracker struct {
	store Store
}

var _ Tracker = (*storeTracker)(nil)

func (t *storeTracker) ListCandidates(ctx context.Context, query CandidateQuery) ([]WorkItem, error) {
	records, err := t.store.ListCandidateRecords(ctx, query)
	if err != nil {
		return nil, err
	}
	items := normalizeRecords(records)
	sortWorkItemsForDispatch(items)
	return items, nil
}

func (t *storeTracker) ListDispatchQueue(ctx context.Context, query CandidateQuery, snapshot DispatchSnapshot) (DispatchQueue, error) {
	items, err := t.ListCandidates(ctx, query)
	if err != nil {
		return DispatchQueue{}, err
	}
	return DeriveDispatchQueue(items, snapshot), nil
}

func (t *storeTracker) GetWorkItems(ctx context.Context, ids []WorkItemID) ([]WorkItem, error) {
	records, err := t.store.GetWorkItemRecords(ctx, ids)
	if err != nil {
		return nil, err
	}
	return normalizeRecords(records), nil
}

func (*storeTracker) Claim(context.Context, ClaimRequest) (Lease, error) {
	return Lease{}, ErrNotImplemented
}

func (*storeTracker) Renew(context.Context, RenewRequest) (Lease, error) {
	return Lease{}, ErrNotImplemented
}

func (*storeTracker) Release(context.Context, ReleaseRequest) error {
	return ErrNotImplemented
}

func (*storeTracker) AppendEvent(context.Context, WorkEvent) error {
	return ErrNotImplemented
}
