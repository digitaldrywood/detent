package tracker

import (
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

const NativeProtocolMajor = 2

const NativeProviderCapacityCapability = "provider_capacity_reservations"

type OrganizationID string
type ProjectID string
type NativeWorkItemID string
type Revision int64

type NativeReference struct {
	OrganizationID OrganizationID   `json:"organization_id"`
	ProjectID      ProjectID        `json:"project_id"`
	WorkItemID     NativeWorkItemID `json:"work_item_id"`
	Number         int              `json:"number"`
	Revision       Revision         `json:"revision,string"`
	Profile        string           `json:"profile"`
}

type Actor struct {
	Kind        string `json:"kind"`
	PrincipalID string `json:"principal_id"`
}

type Provenance struct {
	Provider          string    `json:"provider"`
	ExternalID        string    `json:"external_id"`
	AuthorID          string    `json:"author_id"`
	AuthorDisplayName string    `json:"author_display_name,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitzero"`
	UpdatedAt         time.Time `json:"updated_at,omitzero"`
	ObservedAt        time.Time `json:"observed_at,omitzero"`
}

type NativeIssue struct {
	IgnoreDependencies bool `json:"ignore_dependencies,omitempty"`
	NativeReference
	Title              string              `json:"title"`
	Body               string              `json:"body"`
	State              string              `json:"state"`
	Terminal           bool                `json:"terminal"`
	Priority           *int                `json:"priority,omitempty"`
	Labels             []string            `json:"labels"`
	Assignees          []string            `json:"assignees"`
	Actor              Actor               `json:"actor"`
	Provenance         *Provenance         `json:"provenance,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	Dependencies       []NativeWorkItemID  `json:"dependencies"`
	Blockers           []NativeDependency  `json:"blockers"`
	ExternalReferences []ExternalReference `json:"external_references"`
}

type ExternalReference struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
}

type NativeDependency struct {
	ID        NativeWorkItemID `json:"work_item_id"`
	ProjectID ProjectID        `json:"project_id"`
	State     string           `json:"state"`
	Terminal  bool             `json:"terminal"`
}

type NativeComment struct {
	ID             string           `json:"comment_id"`
	OrganizationID OrganizationID   `json:"organization_id"`
	ProjectID      ProjectID        `json:"project_id"`
	WorkItemID     NativeWorkItemID `json:"work_item_id"`
	Revision       Revision         `json:"revision,string"`
	Sequence       int64            `json:"sequence,string"`
	Body           string           `json:"body"`
	Actor          Actor            `json:"actor"`
	EditedBy       *Actor           `json:"edited_by,omitempty"`
	Provenance     *Provenance      `json:"provenance,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type Mutation struct {
	IdempotencyKey string       `json:"idempotency_key"`
	LeaseID        LeaseID      `json:"lease_id,omitempty"`
	FencingToken   FencingToken `json:"fencing_token,string,omitempty"`
}

type CreateIssue struct {
	Mutation
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	State      string      `json:"state"`
	Priority   *int        `json:"priority,omitempty"`
	Labels     []string    `json:"labels"`
	Assignees  []string    `json:"assignees"`
	Provenance *Provenance `json:"provenance,omitempty"`
}

type UpdateIssue struct {
	Mutation
	ExpectedRevision Revision  `json:"expected_revision,string"`
	Title            *string   `json:"title,omitempty"`
	Body             *string   `json:"body,omitempty"`
	Priority         *int      `json:"priority,omitempty"`
	Labels           *[]string `json:"labels,omitempty"`
	Assignees        *[]string `json:"assignees,omitempty"`
}

type CreateComment struct {
	Mutation
	Body       string      `json:"body"`
	Provenance *Provenance `json:"provenance,omitempty"`
}

type UpdateComment struct {
	Mutation
	ExpectedRevision Revision `json:"expected_revision,string"`
	Body             string   `json:"body"`
}

type Transition struct {
	Mutation
	ExpectedRevision Revision `json:"expected_revision,string"`
	State            string   `json:"state"`
	Reason           string   `json:"reason"`
}

type DependencyMutation struct {
	Mutation
	ExpectedRevision  Revision         `json:"expected_revision,string"`
	RelatedWorkItemID NativeWorkItemID `json:"related_work_item_id"`
	Operation         string           `json:"operation"`
}

type CollaborationEvent struct {
	ID                string            `json:"event_id"`
	OrganizationID    OrganizationID    `json:"organization_id"`
	ProjectID         ProjectID         `json:"project_id"`
	AggregateType     string            `json:"aggregate_type"`
	AggregateID       NativeWorkItemID  `json:"aggregate_id"`
	AggregateSequence int64             `json:"aggregate_sequence,string"`
	Type              string            `json:"type"`
	SchemaVersion     int               `json:"schema_version"`
	RecordedAt        time.Time         `json:"recorded_at"`
	Actor             Actor             `json:"actor"`
	Data              CollaborationData `json:"data"`
}

type CollaborationData struct {
	Run               *NativeRunData   `json:"run,omitempty"`
	Revision          Revision         `json:"revision,string,omitempty"`
	Fields            []string         `json:"fields,omitempty"`
	CommentID         string           `json:"comment_id,omitempty"`
	RelatedWorkItemID NativeWorkItemID `json:"related_work_item_id,omitempty"`
	Operation         string           `json:"operation,omitempty"`
	FromState         string           `json:"from_state,omitempty"`
	ToState           string           `json:"to_state,omitempty"`
	Reason            string           `json:"reason,omitempty"`
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type NativeState struct {
	OperatorOnly bool     `json:"operator_only,omitempty"`
	Name         string   `json:"name"`
	Terminal     bool     `json:"terminal"`
	Dispatchable bool     `json:"dispatchable"`
	Transitions  []string `json:"transitions"`
}

type NativeProject struct {
	ID                  ProjectID      `json:"project_id"`
	OrganizationID      OrganizationID `json:"organization_id"`
	Name                string         `json:"name"`
	Profile             string         `json:"profile"`
	States              []NativeState  `json:"states"`
	RequireDependencies bool           `json:"require_dependencies"`
}

type NativeClaim struct {
	ProviderCandidates []NativeCapacityCandidate `json:"provider_candidates,omitempty"`
	PolicyID           string                    `json:"policy_id"`
	WorkItemID         NativeWorkItemID          `json:"work_item_id,omitempty"`
	MachineID          MachineID                 `json:"machine_id"`
	SessionID          string                    `json:"session_id"`
	TTLSeconds         int64                     `json:"ttl_seconds"`
	ProtocolMajor      int                       `json:"protocol_major"`
	Capabilities       []string                  `json:"capabilities"`
	WorkflowStates     []string                  `json:"workflow_states,omitempty"`
	Authors            []string                  `json:"authors,omitempty"`
	Assignees          []string                  `json:"assignees,omitempty"`
	LabelInclude       []string                  `json:"label_include,omitempty"`
	LabelExclude       []string                  `json:"label_exclude,omitempty"`
}

type NativeLease struct {
	ProviderReservation *providercapacity.Reservation `json:"provider_reservation,omitempty"`
	ServerTime          time.Time                     `json:"server_time"`
	PolicyID            string                        `json:"policy_id"`
	ID                  LeaseID                       `json:"lease_id"`
	WorkItemID          NativeWorkItemID              `json:"work_item_id"`
	MachineID           MachineID                     `json:"machine_id"`
	SessionID           string                        `json:"session_id"`
	FencingToken        FencingToken                  `json:"fencing_token,string"`
	AcquiredAt          time.Time                     `json:"acquired_at"`
	RenewedAt           time.Time                     `json:"renewed_at"`
	ExpiresAt           time.Time                     `json:"expires_at"`
}

type NativeCapacityCandidate struct {
	WorkItemID  NativeWorkItemID             `json:"work_item_id"`
	Revision    Revision                     `json:"revision,string"`
	Requirement providercapacity.Requirement `json:"requirement"`
}

type NativeCapacityPreview struct {
	NativeClaim
	After WorkItemID `json:"after,omitempty"`
}

type NativeCapacityPage struct {
	Items []NativeIssue `json:"items"`
	Next  WorkItemID    `json:"next,omitempty"`
}

type NativeLeaseMutation struct {
	FencingToken FencingToken `json:"fencing_token,string"`
	TTLSeconds   int64        `json:"ttl_seconds,omitempty"`
	Reason       string       `json:"reason,omitempty"`
}

type NativeRunData struct {
	Sequence     int64                    `json:"sequence,string,omitempty"`
	Identity     *NativeExecutionIdentity `json:"identity,omitempty"`
	MachineID    MachineID                `json:"machine_id,omitempty"`
	RunnerID     string                   `json:"runner_id,omitempty"`
	SessionID    string                   `json:"session_id,omitempty"`
	Handoff      *NativeCheckpoint        `json:"handoff,omitempty"`
	LeaseID      LeaseID                  `json:"lease_id"`
	FencingToken FencingToken             `json:"fencing_token,string"`
	RunID        string                   `json:"run_id"`
	AttemptID    string                   `json:"attempt_id"`
	PolicyID     string                   `json:"policy_id"`
	Outcome      string                   `json:"outcome,omitempty"`
	ArtifactIDs  []string                 `json:"artifact_ids,omitempty"`
}

type NativeRunEvent struct {
	Mutation
	Type          string        `json:"type"`
	SchemaVersion int           `json:"schema_version"`
	OccurredAt    time.Time     `json:"occurred_at,omitempty"`
	Data          NativeRunData `json:"data"`
}
