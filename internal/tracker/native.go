package tracker

import (
	"bytes"
	"encoding/json"
	"errors"
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
	// Change is the item's change review surface, present only on a resource
	// the caller asked for it with (include=change). It is null everywhere
	// else, so the default work item resource is what it always was.
	Change *NativeIssueChange `json:"change,omitempty"`
}

// Change review connectors. The value is stated on every change review
// surface, because "no pull request" and "no connector that could ever
// produce one" are different facts and they decide differently.
const (
	// NativeChangeConnectorNone is a project with no GitHub connector, which
	// is decisions section 18.6's "projects without a GitHub connector show
	// the change request alone". No pull request can ever mirror its changes.
	NativeChangeConnectorNone = "none"
	// NativeChangeConnectorGitHub is a project with a GitHub repository bound
	// and enabled, whose changes a pull request can mirror even when none
	// does yet.
	NativeChangeConnectorGitHub = "github"
)

// NativeIssueChange is a work item's change review surface: what the hub
// itself holds for the item, and whether a pull request can ever mirror it.
//
// Decisions section 18.6 joins the hub's own change request with the GitHub
// connector's projection of its pull request, and a project without a
// connector shows the change request alone. A promotion decision needs the
// same two halves: whether the item's change is reviewed on a pull request at
// all, and whether an attempt has recorded a change for the item as it stands.
type NativeIssueChange struct {
	// Connector is NativeChangeConnectorGitHub or NativeChangeConnectorNone.
	// It is the stated fact the promotion rule turns on, not the absence of a
	// pull request: a project that has no connector can never produce one,
	// while a project that has one may simply not have opened it yet.
	Connector string `json:"connector"`
	// ChangeID names the item's latest change request, empty when it has none.
	ChangeID string `json:"change_id,omitempty"`
	// Number, State, Draft and URL are the change as a pull request: the
	// connector's projection when it mirrors one, and the hub's own change
	// request otherwise, which is open until the change merges. Number is
	// zero when nothing mirrors the change.
	Number int    `json:"number,omitempty"`
	State  string `json:"state,omitempty"`
	Draft  bool   `json:"draft,omitempty"`
	URL    string `json:"url,omitempty"`
	// HeadSHA is the head the change currently names.
	HeadSHA string `json:"head_sha,omitempty"`
	// Revision is the work item revision the recorded change covers: the
	// revision the succeeded attempt that produced it was dispatched for
	// (decisions section 9.2.1's native_attempts.work_item_revision). Zero
	// means no attempt has recorded a change or an attempt diff for the item.
	Revision Revision `json:"revision,string"`
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

// PriorityPatch is the priority member of UpdateIssue.
//
// A patch has three things to say about a priority and a plain `*int` can
// only say two of them: an omitted member means "leave it alone", so there
// was no way to ask for the priority to be removed. This type carries the
// third. On the wire an absent member still leaves the priority alone, a
// number sets it, and either `null` or the word `"none"` clears it — the word
// because a JSON `null` is easy for a client to send by accident and hard for
// a reader of a request log to tell apart from an omission.
type PriorityPatch struct {
	present bool
	level   *int
}

// LeavePriority is the zero patch: the priority is not part of this edit.
func LeavePriority() PriorityPatch { return PriorityPatch{} }

// ClearPriority removes whatever priority the issue has.
func ClearPriority() PriorityPatch { return PriorityPatch{present: true} }

// SetPriority sets the level. A nil level leaves the priority alone, which is
// what a caller with nothing to say about it passes.
func SetPriority(level *int) PriorityPatch {
	if level == nil {
		return PriorityPatch{}
	}
	value := *level
	return PriorityPatch{present: true, level: &value}
}

// Present reports whether the patch says anything about the priority at all.
func (p PriorityPatch) Present() bool { return p.present }

func (p PriorityPatch) IsZero() bool { return !p.present }

// Level is the level the patch asks for, or nil where it asks for a clear.
func (p PriorityPatch) Level() *int {
	if p.level == nil {
		return nil
	}
	value := *p.level
	return &value
}

// ErrInvalidPriorityPatch is returned for a priority member that is neither a
// number, nor null, nor the word "none".
var ErrInvalidPriorityPatch = errors.New(`priority must be a number, null, or "none"`)

func (p *PriorityPatch) UnmarshalJSON(data []byte) error {
	p.present = true
	p.level = nil
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var word string
		if err := json.Unmarshal(trimmed, &word); err != nil {
			return err
		}
		if word != "none" {
			return ErrInvalidPriorityPatch
		}
		return nil
	}
	var level int
	if err := json.Unmarshal(trimmed, &level); err != nil {
		return ErrInvalidPriorityPatch
	}
	p.level = &level
	return nil
}

// MarshalJSON writes the level, or null for both the absent and the cleared
// patch. The idempotency fingerprint is taken over this encoding, so the two
// only have to be encoded consistently, not distinguishably; a request that
// clears a priority and one that omits it carry different keys in practice
// because they are different intents.
func (p PriorityPatch) MarshalJSON() ([]byte, error) {
	if p.level == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*p.level)
}

type UpdateIssue struct {
	Mutation
	ExpectedRevision Revision      `json:"expected_revision,string"`
	Title            *string       `json:"title,omitempty"`
	Body             *string       `json:"body,omitempty"`
	Priority         PriorityPatch `json:"priority,omitzero"`
	Labels           *[]string     `json:"labels,omitempty"`
	Assignees        *[]string     `json:"assignees,omitempty"`
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
	Change            *NativeChangeReference `json:"change,omitempty"`
	Run               *NativeRunData         `json:"run,omitempty"`
	Revision          Revision               `json:"revision,string,omitempty"`
	Fields            []string               `json:"fields,omitempty"`
	CommentID         string                 `json:"comment_id,omitempty"`
	RelatedWorkItemID NativeWorkItemID       `json:"related_work_item_id,omitempty"`
	Operation         string                 `json:"operation,omitempty"`
	FromState         string                 `json:"from_state,omitempty"`
	ToState           string                 `json:"to_state,omitempty"`
	Reason            string                 `json:"reason,omitempty"`
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
	// Usage is what the attempt has spent so far, one entry per provider and
	// model (decisions section 17.5). A runner that reports none leaves the
	// field out, so the event is byte-identical to what it was before.
	Usage []NativeUsage `json:"usage,omitempty"`
}

type NativeRunEvent struct {
	Mutation
	Type          string        `json:"type"`
	SchemaVersion int           `json:"schema_version"`
	OccurredAt    time.Time     `json:"occurred_at,omitempty"`
	Data          NativeRunData `json:"data"`
}
