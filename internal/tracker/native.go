package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/workpad"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

const NativeProtocolMajor = 2

const NativeDispatchPriorityCapability = "dispatch_priority"

const NativeLandingCapacityCapability = "project_landing_capacity"

const NativeDispatchWaitCapability = "native_dispatch_wait"
const NativeHostMetricsCapability = "runner_host_metrics"

const NativeHeartbeatChangesCapability = "native_heartbeat_changes"

const NativeProviderCapacityCapability = "provider_capacity_reservations"
const NativeRunnerSkillsCapability = "runner_project_skills"

const NativeCheckoutRepositoryCapability = "runner_checkout_repository"
const NativeRunnerUpdateCapability = "runner_installed_update"

const NativeProjectDiagnosticsCapability = "runner_project_diagnostics"

const NativeProjectConfigurationCapability = "runner_project_configuration"

const NativeRunnerCapacityCapability = "runner_capacity_configuration"
const NativeLocalChecksCapability = "runner_local_checks"
const NativeProjectCheckoutCapability = "runner_project_checkout"
const NativeRunnerCommitPolicyCapability = "runner_commit_policy"

const NativeRunnerSetupCapability = "runner_project_setup"
const NativeRunnerSetupDeclarationCapability = "runner_setup_declaration"

// NativeWorkspaceCapability is declared by a runner's workspace lane on its
// claim. It is what separates the lane that holds a workspace session open
// from the lane that runs issues: a workspace is its own work item kind and
// the ordinary claim never sees one. The signal is a
// declared capability rather than a label filter, because a label filter is a
// preference an operator can set on any lane and this is an authority.
const NativeWorkspaceCapability = "workspace_sessions"

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
	ViaKey      string `json:"via_key,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	// Agent names the assistant that acted on the principal's behalf, such
	// as "Luna (gpt-6.1-sol)"; empty when the principal acted directly.
	Agent string `json:"agent,omitempty"`
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
	ChangeSequence         int64                `json:"change_sequence,omitempty"`
	LatestTransitionReason string               `json:"-"`
	ReleaseDelivery        *ReleaseDelivery     `json:"release_delivery,omitempty"`
	IssueContract          *issuecontract.State `json:"issue_contract,omitempty" yaml:"issue_contract,omitempty"`
	OmittedFields          []string             `json:"omitted_fields,omitempty"`
	LinkedSource           *LinkedIssueSource   `json:"linked_source,omitempty"`
	IgnoreDependencies     bool                 `json:"ignore_dependencies,omitempty"`
	NativeReference
	WebURL             string              `json:"web_url"`
	PublicationReused  bool                `json:"publication_reused,omitempty"`
	Type               string              `json:"type,omitempty"`
	ReportContext      *ReportContext      `json:"report_context,omitempty"`
	Verification       *NativeVerification `json:"verification,omitempty"`
	Title              string              `json:"title"`
	Body               string              `json:"body"`
	Snippet            string              `json:"snippet,omitempty"`
	SearchScore        float64             `json:"search_score,omitempty"`
	State              string              `json:"state"`
	ReporterStatus     string              `json:"reporter_status,omitempty"`
	Archived           bool                `json:"archived"`
	Terminal           bool                `json:"terminal"`
	SchedulingEligible *bool               `json:"scheduling_eligible,omitempty"`
	Priority           *int                `json:"priority,omitempty"`
	Labels             []string            `json:"labels"`
	Assignees          []string            `json:"assignees"`
	Actor              Actor               `json:"actor"`
	Provenance         *Provenance         `json:"provenance,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	UpdatedAt          time.Time           `json:"updated_at"`
	ClosedAt           *time.Time          `json:"closed_at,omitempty"`
	ListChange         *NativeListChange   `json:"list_change,omitempty"`
	PullRequest        *NativePullRequest  `json:"pull_request,omitempty"`
	LastActivityAt     time.Time           `json:"last_activity_at"`
	Dependencies       []NativeWorkItemID  `json:"dependencies"`
	Blockers           []NativeDependency  `json:"blockers"`
	Dependents         []NativeDependency  `json:"dependents"`
	Related            []NativeDependency  `json:"related,omitempty"`
	DependencyReady    bool                `json:"dependency_ready"`
	BlockedReason      DependencyReason    `json:"blocked_reason"`
	UnblocksReason     DependencyReason    `json:"unblocks_reason"`
	ExternalReferences []ExternalReference `json:"external_references"`
	// Change is the item's change review surface, present only on a resource
	// the caller asked for it with (include=change). It is null everywhere
	// else, so the default work item resource is what it always was.
	Change *NativeIssueChange `json:"change,omitempty"`
}

type NativeListChange struct {
	LandingPending bool   `json:"landing_pending,omitempty"`
	Branch         string `json:"branch"`
	HeadSHA        string `json:"head_sha"`
}

type NativePullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// Change review connectors. The value is stated on every change review
// surface, because "no pull request" and "no connector that could ever
// produce one" are different facts and they decide differently.
const (
	// NativeChangeConnectorNone is a project with no GitHub connector, which
	// shows the change request alone. No pull request can ever mirror its changes.
	NativeChangeConnectorNone = "none"
	// NativeChangeConnectorGitHub is a project with a GitHub repository bound
	// and enabled, whose changes a pull request can mirror even when none
	// does yet.
	NativeChangeConnectorGitHub = "github"
)

// NativeIssueChange is a work item's change review surface: what the hub
// itself holds for the item, and whether a pull request can ever mirror it.
//
// The pull request contract joins the hub's own change request with the GitHub
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
	// (native_attempts.work_item_revision). Zero
	// means no attempt has recorded a change or an attempt diff for the item.
	Revision Revision `json:"revision,string"`
}

type ExternalReference struct {
	Provider   string `json:"provider"`
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	URL        string `json:"url,omitempty"`
	Repository string `json:"repository,omitempty"`
	Number     int    `json:"number,omitempty"`
}

type DependencyReason struct {
	Text         string `json:"text"`
	Basis        string `json:"basis"`
	CurrentBasis string `json:"current_basis"`
	Revision     int64  `json:"revision"`
}

type UpdateDependencyReason struct {
	Mutation
	WorkItemID       NativeWorkItemID `json:"work_item_id"`
	Kind             string           `json:"kind"`
	Text             string           `json:"text"`
	Basis            string           `json:"basis"`
	ExpectedRevision int64            `json:"expected_revision"`
}

type NativeDependency struct {
	Dispatchable bool       `json:"dispatchable"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
	Number       int        `json:"number"`
	Title        string     `json:"title"`
	WebURL       string     `json:"web_url"`
	StartedAt    *time.Time `json:"started_at,omitempty"`

	Identifier string           `json:"identifier,omitempty"`
	ID         NativeWorkItemID `json:"work_item_id"`
	ProjectID  ProjectID        `json:"project_id"`
	State      string           `json:"state"`
	Terminal   bool             `json:"terminal"`
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
	Deleted        bool             `json:"deleted,omitempty"`
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
	ChallengeToken      string                          `json:"challenge_token,omitempty"`
	NotDuplicateOf      []int                           `json:"not_duplicate_of,omitempty"`
	ScheduledValidation *gate.ScheduledValidationReport `json:"scheduled_validation,omitempty"`
	GitHubIssueURL      string                          `json:"github_issue_url,omitempty"`
	Type                string                          `json:"type,omitempty"`
	ReportContext       *ReportContext                  `json:"report_context,omitempty"`
	Title               string                          `json:"title"`
	Body                string                          `json:"body"`
	State               string                          `json:"state"`
	Priority            *int                            `json:"priority,omitempty"`
	Labels              []string                        `json:"labels"`
	Assignees           []string                        `json:"assignees"`
	Provenance          *Provenance                     `json:"provenance,omitempty"`
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
	Type             *string       `json:"type,omitempty"`
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
	Deleted          bool     `json:"deleted,omitempty"`
}

const (
	VerificationPending  = "pending"
	VerificationVerified = "verified"
	VerifyResultVerified = "verified"
	VerifyResultNotFixed = "not_fixed"
)

type NativeVerification struct {
	State      string     `json:"state"`
	VerifiedBy *Actor     `json:"verified_by,omitempty"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

type VerifyFix struct {
	Mutation
	ExpectedRevision Revision `json:"expected_revision,string,omitempty"`
	Result           string   `json:"result"`
	Comment          string   `json:"comment,omitempty"`
}

type Transition struct {
	Mutation
	PolicyID         string   `json:"policy_id,omitempty"`
	BlockerAttemptID string   `json:"blocker_attempt_id,omitempty"`
	ExpectedRevision Revision `json:"expected_revision,string"`
	State            string   `json:"state"`
	Reason           string   `json:"reason"`
	ReasonDetail     string   `json:"reason_detail,omitempty"`
}

type DependencyMutation struct {
	Mutation
	RelationshipType  string            `json:"relationship_type,omitempty"`
	ExpectedRevision  Revision          `json:"expected_revision,string"`
	RelatedIssue      *NativeDependency `json:"related_issue,omitempty"`
	RelatedWorkItemID NativeWorkItemID  `json:"related_work_item_id"`
	Operation         string            `json:"operation"`
}

type CollaborationEvent struct {
	ID                string               `json:"event_id"`
	OrganizationID    OrganizationID       `json:"organization_id"`
	ProjectID         ProjectID            `json:"project_id"`
	AggregateType     string               `json:"aggregate_type"`
	AggregateID       NativeWorkItemID     `json:"aggregate_id"`
	AggregateSequence int64                `json:"aggregate_sequence,string"`
	Type              string               `json:"type"`
	SchemaVersion     int                  `json:"schema_version"`
	RecordedAt        time.Time            `json:"recorded_at"`
	Actor             Actor                `json:"actor"`
	Data              CollaborationData    `json:"data"`
	DataOmission      *HistoryDataOmission `json:"data_omission,omitempty"`
}

type HistoryDataOmission struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type CollaborationData struct {
	BlockerAttemptID  string                   `json:"blocker_attempt_id,omitempty"`
	Decision          *NativeSchedulerDecision `json:"decision,omitempty"`
	Change            *NativeChangeReference   `json:"change,omitempty"`
	Run               *NativeRunData           `json:"run,omitempty"`
	Revision          Revision                 `json:"revision,string,omitempty"`
	Fields            []string                 `json:"fields,omitempty"`
	CommentID         string                   `json:"comment_id,omitempty"`
	RelatedIssue      *NativeDependency        `json:"related_issue,omitempty"`
	RelatedWorkItemID NativeWorkItemID         `json:"related_work_item_id,omitempty"`
	Operation         string                   `json:"operation,omitempty"`
	FromState         string                   `json:"from_state,omitempty"`
	ToState           string                   `json:"to_state,omitempty"`
	Reason            string                   `json:"reason,omitempty"`
	ReasonDetail      string                   `json:"reason_detail,omitempty"`
	Verification      string                   `json:"verification,omitempty"`
}

type Page[T any] struct {
	Items      []T              `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Compaction *EventCompaction `json:"compaction,omitempty"`
}

type EventCompaction struct {
	ThroughSequence int64            `json:"through_sequence"`
	RemovedEvents   int64            `json:"removed_events"`
	RetainedEvents  int64            `json:"retained_events"`
	RemovedBytes    int64            `json:"removed_bytes"`
	RemovedByType   map[string]int64 `json:"removed_by_type"`
	CompactedAt     time.Time        `json:"compacted_at"`

	CompactedActivityEvents int64 `json:"compacted_activity_events,omitempty"`
}

type NativeIssuePage struct {
	ChangeSequence int64 `json:"change_sequence"`
	Page[NativeIssue]
	Total        int                `json:"total"`
	Work         *NativeWorkSummary `json:"work,omitempty"`
	Cards        []NativeBoardCard  `json:"cards,omitempty"`
	Sequence     int64              `json:"sequence,omitempty"`
	GrantVersion string             `json:"grant_version,omitempty"`
}

type NativeWorkSummary struct {
	Completed      int                   `json:"completed"`
	Cancelled      int                   `json:"cancelled"`
	ShippedWindows []NativeShippedWindow `json:"shipped_windows,omitempty"`
	Items          []NativeIssue         `json:"items"`
	Lanes          []NativeWorkLane      `json:"lanes"`
	Truncated      bool                  `json:"truncated"`
	AsOf           time.Time             `json:"as_of"`
}

type NativeShippedWindow struct {
	Window    string `json:"window"`
	Shipped   int    `json:"shipped"`
	Cancelled int    `json:"cancelled"`
}

type NativeWorkLane struct {
	State   string `json:"state"`
	Total   int    `json:"total"`
	Running int    `json:"running"`
}

type NativeState = policy.State

type NativeProject struct {
	Icon                *projecticon.Icon `json:"project_icon"`
	CloneURL            string            `json:"clone_url,omitempty"`
	Repository          string            `json:"repository,omitempty"`
	Slug                string            `json:"slug"`
	ModelSelection      json.RawMessage   `json:"model_selection,omitempty"`
	WorkflowMarkdown    string            `json:"workflow_markdown,omitempty"`
	ID                  ProjectID         `json:"project_id"`
	OrganizationID      OrganizationID    `json:"organization_id"`
	Name                string            `json:"name"`
	Profile             string            `json:"profile"`
	States              []NativeState     `json:"states"`
	RequireDependencies bool              `json:"require_dependencies"`
}

type NativeClaim struct {
	LandingCapacity         int                       `json:"landing_capacity,omitempty"`
	DispatchPriorityByState []string                  `json:"dispatch_priority_by_state,omitempty"`
	DispatchPriorityByLabel []string                  `json:"dispatch_priority_by_label,omitempty"`
	PrioritizeUnblockers    bool                      `json:"prioritize_unblockers,omitempty"`
	ProviderCandidates      []NativeCapacityCandidate `json:"provider_candidates,omitempty"`
	PolicyID                string                    `json:"policy_id"`
	WorkItemID              NativeWorkItemID          `json:"work_item_id,omitempty"`
	MachineID               MachineID                 `json:"machine_id"`
	SessionID               string                    `json:"session_id"`
	TTLSeconds              int64                     `json:"ttl_seconds"`
	ProtocolMajor           int                       `json:"protocol_major"`
	Capabilities            []string                  `json:"capabilities"`
	WorkflowStates          []string                  `json:"workflow_states,omitempty"`
	Authors                 []string                  `json:"authors,omitempty"`
	Assignees               []string                  `json:"assignees,omitempty"`
	LabelInclude            []string                  `json:"label_include,omitempty"`
	LabelExclude            []string                  `json:"label_exclude,omitempty"`
}

type NativeLease struct {
	RunDetailBatching   bool                          `json:"run_detail_batching,omitempty"`
	IsolationPolicy     *isolation.Policy             `json:"isolation_policy,omitempty"`
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
	Limit int        `json:"limit,omitempty"`
}

type NativeCapacityPage struct {
	Items []NativeIssue `json:"items"`
	Next  WorkItemID    `json:"next,omitempty"`
}

type NativeLeaseMutation struct {
	CancelAttempt   bool         `json:"cancel_attempt,omitempty"`
	LandingCapacity int          `json:"landing_capacity,omitempty"`
	FencingToken    FencingToken `json:"fencing_token,string"`
	TTLSeconds      int64        `json:"ttl_seconds,omitempty"`
	Reason          string       `json:"reason,omitempty"`
}

type NativeDisposition struct {
	Status          string            `json:"status"`
	Blockers        bool              `json:"blockers"`
	HumanAction     bool              `json:"human_action"`
	ReasonCode      string            `json:"reason_code,omitempty"`
	FinalSummary    string            `json:"final_summary,omitempty"`
	BlockerEvidence []workpad.Blocker `json:"blocker_evidence,omitempty"`
}

type NativeRunData struct {
	Steps           []NativeStepReceipt       `json:"steps,omitempty"`
	ProcessUsage    *procgroup.Usage          `json:"process_usage,omitempty"`
	TerminalFailure *NativeTerminalFailure    `json:"terminal_failure,omitempty"`
	Finalization    *NativeFinalization       `json:"finalization,omitempty"`
	Evidence        []NativeEvidence          `json:"evidence,omitempty"`
	CompletionBody  string                    `json:"completion_body,omitempty"`
	Disposition     *NativeDisposition        `json:"disposition,omitempty"`
	Runtime         *NativeRuntimeObservation `json:"runtime,omitempty"`
	Sequence        int64                     `json:"sequence,string,omitempty"`
	Identity        *NativeExecutionIdentity  `json:"identity,omitempty"`
	MachineID       MachineID                 `json:"machine_id,omitempty"`
	RunnerID        string                    `json:"runner_id,omitempty"`
	SessionID       string                    `json:"session_id,omitempty"`
	Handoff         *NativeCheckpoint         `json:"handoff,omitempty"`
	LeaseID         LeaseID                   `json:"lease_id"`
	FencingToken    FencingToken              `json:"fencing_token,string"`
	RunID           string                    `json:"run_id"`
	AttemptID       string                    `json:"attempt_id"`
	PolicyID        string                    `json:"policy_id"`
	Outcome         string                    `json:"outcome,omitempty"`
	ArtifactIDs     []string                  `json:"artifact_ids,omitempty"`
	// Usage is what the attempt has spent so far, one entry per provider and
	// model. A runner that reports none leaves the
	// field out, so the event is byte-identical to what it was before.
	Usage []NativeUsage `json:"usage,omitempty"`
}

type NativeEvidence struct {
	AttachmentID string `json:"attachment_id"`
	Caption      string `json:"caption"`
	Reference    string `json:"reference"`
}

type NativeRunEvent struct {
	PriorEvents []NativeRunEvent `json:"prior_events,omitempty"`
	Mutation
	Type          string        `json:"type"`
	SchemaVersion int           `json:"schema_version"`
	OccurredAt    time.Time     `json:"occurred_at,omitempty"`
	Data          NativeRunData `json:"data"`
}

// MutationForContext forwards the application retry identity without deriving
// business identity from an HTTP/JSON-RPC request or including sensitive input.
func MutationForContext(ctx context.Context, fallback string) Mutation {
	if m, ok := mutation.FromContext(ctx); ok && m.RetryIdentity != "" {
		return Mutation{IdempotencyKey: m.RetryIdentity}
	}
	return Mutation{IdempotencyKey: fallback}
}

// NativeLabel is the existing project's label-picker read model.
type NativeLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	// Count orders the existing project label-picker suggestions by usage.
	Count int `json:"count"`
}

// WithExpectedRevision carries an operator command's optimistic precondition
// to the native connector while the orchestrator owns the lane write.
type expectedRevisionKey struct{}

func WithExpectedRevision(ctx context.Context, revision Revision) context.Context {
	return context.WithValue(ctx, expectedRevisionKey{}, revision)
}
func ExpectedRevision(ctx context.Context) Revision {
	revision, ok := ctx.Value(expectedRevisionKey{}).(Revision)
	if !ok {
		return 0
	}
	return revision
}
