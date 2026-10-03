package tracker

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const NativeExecutionCapability = "fenced_run_history"

const NativeRuntimeEvidenceCapability = "native_runtime_evidence"
const NativeAdmissionEvidenceCapability = "native_admission_evidence"
const NativeAdmissionObservationCapability = "native_admission_observation"

type NativeExecutionIdentity struct {
	Role    string `json:"role"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

type NativeCheckpoint struct {
	Resume          string                 `json:"resume"`
	Availability    string                 `json:"availability"`
	Storage         string                 `json:"storage"`
	WorktreeState   string                 `json:"worktree_state"`
	HeadSHA         string                 `json:"head_sha,omitempty"`
	WorkspaceDigest string                 `json:"workspace_digest,omitempty"`
	ExpectedHeadSHA string                 `json:"expected_head_sha,omitempty"`
	ExternalEffect  string                 `json:"external_effect"`
	EffectState     string                 `json:"effect_state"`
	EffectID        string                 `json:"effect_id,omitempty"`
	Change          *NativeChangeReference `json:"change,omitempty"`
}

type NativeChangeReference struct {
	ChangeID  string `json:"change_id"`
	VersionID string `json:"version_id"`
	HeadSHA   string `json:"head_sha"`
}

type NativeAttempt struct {
	NativeRunData
	Status             string            `json:"status"`
	StartedAt          time.Time         `json:"started_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	Checkpoint         *NativeCheckpoint `json:"checkpoint,omitempty"`
	WorkItemRevision   Revision          `json:"work_item_revision,string"`
	DispatchGeneration int64             `json:"dispatch_generation,string"`
	LeaseRenewedAt     time.Time         `json:"lease_renewed_at"`
	LeaseExpiresAt     time.Time         `json:"lease_expires_at"`
	Current            bool              `json:"current"`
	RuntimeFreshness   string            `json:"runtime_freshness"`
}

type NativePhase struct {
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

type NativeRuntimeObservation struct {
	PhasesDropped  int                              `json:"phases_dropped"`
	LocalAttemptID int64                            `json:"local_attempt_id,omitempty"`
	Generation     uint64                           `json:"generation,omitempty"`
	HeartbeatAt    time.Time                        `json:"heartbeat_at"`
	Phase          string                           `json:"phase"`
	Phases         []NativePhase                    `json:"phases"`
	Identity       agentidentity.Identity           `json:"identity,omitzero"`
	Activity       *workflowmetrics.ActivityProfile `json:"activity,omitempty"`
	Landing        *NativeLandingReceipt            `json:"landing,omitempty"`
	REST           *NativeRESTEvidence              `json:"rest,omitempty"`
	GitHub         *NativeGitHubScope               `json:"github,omitempty"`
}

func (r *NativeRuntimeObservation) WithoutActivitySpans() *NativeRuntimeObservation {
	if r == nil {
		return nil
	}
	summary := *r
	if r.Activity != nil {
		profile := *r.Activity
		profile.Spans = nil
		profile.CoverageNotes = slices.Clone(profile.CoverageNotes)
		const note = "activity_spans_omitted_read_with_board_session_history"
		if !slices.Contains(profile.CoverageNotes, note) {
			profile.CoverageNotes = append(profile.CoverageNotes, note)
		}
		summary.Activity = &profile
	}
	return &summary
}

type NativeLandingReceipt struct {
	ChangeID    string    `json:"change_id,omitempty"`
	VersionID   string    `json:"version_id,omitempty"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	Landed      bool      `json:"landed"`
	MergeSHA    string    `json:"merge_sha,omitempty"`
	BaseRef     string    `json:"base_ref,omitempty"`
	Method      string    `json:"method,omitempty"`
	RefusalKind string    `json:"refusal_kind,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
}

type NativeRESTEvidence struct {
	DivergencesDropped int                    `json:"divergences_dropped"`
	WindowsDropped     int                    `json:"windows_dropped"`
	Divergences        []NativeRESTDivergence `json:"divergences"`
	Source             string                 `json:"source"`
	Coverage           string                 `json:"coverage"`
	ObservedAt         time.Time              `json:"observed_at"`
	Requests           int64                  `json:"instrumented_requests"`
	Windows            []NativeRESTWindow     `json:"windows"`
}

type NativeRESTDivergence struct {
	CredentialIdentity   string    `json:"credential_identity"`
	Resource             string    `json:"resource"`
	Attribution          string    `json:"attribution"`
	ObservedRequests     int64     `json:"observed_requests"`
	DetentRequests       int64     `json:"detent_requests"`
	AttributedRequests   int64     `json:"attributed_requests"`
	UnattributedRequests int64     `json:"unattributed_requests"`
	WindowStartedAt      time.Time `json:"window_started_at"`
	LastObservedAt       time.Time `json:"last_observed_at"`
	ResetAt              time.Time `json:"reset_at"`
}

type NativeRESTWindow struct {
	RateLimited        bool      `json:"rate_limited"`
	RetryAfterSeconds  float64   `json:"retry_after_seconds"`
	UsedObserved       bool      `json:"used_observed"`
	CredentialIdentity string    `json:"credential_identity"`
	Resource           string    `json:"resource"`
	EndpointFamily     string    `json:"endpoint_family"`
	BudgetScope        string    `json:"budget_scope,omitempty"`
	Requests           int64     `json:"requests"`
	Limit              int64     `json:"limit"`
	Used               int64     `json:"used"`
	Remaining          int64     `json:"remaining"`
	ResetAt            time.Time `json:"reset_at"`
	ObservedAt         time.Time `json:"observed_at"`
	Status             int       `json:"status"`
}

type NativeSchedulerDecision struct {
	RunnerID         string    `json:"runner_id,omitempty"`
	Source           string    `json:"source"`
	Outcome          string    `json:"outcome"`
	Reason           string    `json:"reason"`
	At               time.Time `json:"at"`
	WorkItemRevision Revision  `json:"work_item_revision,string"`
}

type NativeAdmissionObservation struct {
	Context        NativeAdmissionContext `json:"context"`
	RunnerRevision int64                  `json:"runner_revision"`
	ReceivedAt     time.Time              `json:"received_at,omitempty"`
}

type NativeAdmissionContext struct {
	ObservedAt     time.Time `json:"observed_at"`
	PolicyID       string    `json:"policy_id"`
	WorkflowStates []string  `json:"workflow_states,omitempty"`
	Authors        []string  `json:"authors,omitempty"`
	Assignees      []string  `json:"assignees,omitempty"`
	LabelInclude   []string  `json:"label_include,omitempty"`
	LabelExclude   []string  `json:"label_exclude,omitempty"`
}

func (c NativeAdmissionContext) Validate() error {
	if c.ObservedAt.IsZero() || c.PolicyID == "" || len(c.PolicyID) > 128 {
		return errors.New("Admission context requires a bounded policy identity")
	}
	for _, values := range [][]string{c.WorkflowStates, c.Authors, c.Assignees, c.LabelInclude, c.LabelExclude} {
		if len(values) > 32 {
			return errors.New("Admission selectors exceed their bound")
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 128 {
				return errors.New("Invalid admission selector")
			}
		}
	}
	return nil
}

type NativeRuntimeAdmission struct {
	SelectorSource     string     `json:"selector_source,omitempty"`
	SelectorObservedAt *time.Time `json:"selector_observed_at,omitempty"`
	RunnerID           string     `json:"runner_id"`
	RunnerRevision     int64      `json:"runner_revision"`
	PolicyID           string     `json:"policy_id"`
	Source             string     `json:"source"`
	ObservedAt         time.Time  `json:"observed_at"`
	Outcome            string     `json:"outcome"`
	ReasonCode         string     `json:"reason_code,omitempty"`
	Reason             string     `json:"reason"`
	Unavailable        []string   `json:"unavailable"`
}

type NativeRuntimeEvidence struct {
	Admission        []NativeRuntimeAdmission `json:"admission,omitempty"`
	CurrentLease     *NativeLease             `json:"current_lease,omitempty"`
	Issue            NativeIssue              `json:"issue"`
	Attempt          *NativeAttempt           `json:"attempt,omitempty"`
	Selection        string                   `json:"selection"`
	LatestTransition *CollaborationEvent      `json:"latest_transition,omitempty"`
	LatestDecision   *CollaborationEvent      `json:"latest_decision,omitempty"`
	Scheduling       NativeSchedulerDecision  `json:"scheduling"`
	Capacity         []NativeRuntimeCapacity  `json:"capacity"`
	Change           *ChangeDetail            `json:"change,omitempty"`
	ObservedAt       time.Time                `json:"observed_at"`
	Unavailable      []string                 `json:"unavailable"`
}

func (i NativeIssue) RuntimeReference() NativeIssue {
	i.Body = ""
	if i.LinkedSource != nil {
		linked := *i.LinkedSource
		linked.Snapshot = nil
		i.LinkedSource = &linked
	}
	return i
}

type NativeRuntimeCapacity struct {
	ProviderCapacity []providercapacity.View `json:"provider_capacity,omitempty"`
	RunnerID         string                  `json:"runner_id"`
	ObservedAt       time.Time               `json:"observed_at"`
	Health           string                  `json:"health"`
	Available        int                     `json:"available"`
	Exclusions       []string                `json:"exclusions"`
}

type NativeRecovery struct {
	Lease        NativeLease            `json:"lease"`
	Issue        NativeIssue            `json:"issue"`
	Discussion   []NativeComment        `json:"discussion"`
	History      []CollaborationEvent   `json:"history"`
	Attempts     []NativeAttempt        `json:"attempts"`
	Change       *NativeChangeReference `json:"change,omitempty"`
	ChangeDetail *ChangeDetail          `json:"change_detail,omitempty"`
}
