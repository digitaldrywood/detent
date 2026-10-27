package tracker

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const NativeExecutionCapability = "fenced_run_history"

const NativeRuntimeEvidenceCapability = "native_runtime_evidence"
const NativePreProviderFailureCapability = "native_pre_provider_failure"
const NativeAdmissionEvidenceCapability = "native_admission_evidence"
const NativeAdmissionObservationCapability = "native_admission_observation"

type NativeExecutionIdentity struct {
	Role            string `json:"role"`
	Backend         string `json:"backend"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type NativeCheckpoint struct {
	Publication     *NativePRPublication   `json:"publication,omitempty"`
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
	Ref             string                 `json:"ref,omitempty"`
	CommitSHA       string                 `json:"commit_sha,omitempty"`
	BaseSHA         string                 `json:"base_sha,omitempty"`
	TreeSHA         string                 `json:"tree_sha,omitempty"`
}

const CheckpointRefPrefix = "refs/detent/checkpoints/"

func (c *NativeCheckpoint) GitRef() bool {
	return c != nil && c.Storage == "git_ref" && c.Ref != "" && c.CommitSHA != "" && c.TreeSHA != ""
}

type NativeChangeReference struct {
	ChangeID  string `json:"change_id"`
	VersionID string `json:"version_id"`
	HeadSHA   string `json:"head_sha"`
}

type NativeAttempt struct {
	NativeRunData
	Unavailable                 []string          `json:"unavailable,omitempty"`
	TerminalFailureAvailability string            `json:"terminal_failure_availability"`
	FinalizationAvailability    string            `json:"finalization_availability"`
	ClaimReleasedAt             *time.Time        `json:"claim_released_at,omitempty"`
	Status                      string            `json:"status"`
	StartedAt                   time.Time         `json:"started_at"`
	UpdatedAt                   time.Time         `json:"updated_at"`
	Checkpoint                  *NativeCheckpoint `json:"checkpoint,omitempty"`
	WorkItemRevision            Revision          `json:"work_item_revision,string"`
	DispatchGeneration          int64             `json:"dispatch_generation,string"`
	LeaseRenewedAt              time.Time         `json:"lease_renewed_at"`
	LeaseExpiresAt              time.Time         `json:"lease_expires_at"`
	Current                     bool              `json:"current"`
	RuntimeFreshness            string            `json:"runtime_freshness"`
}

type NativePhase struct {
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

type NativeRuntimeObservation struct {
	WorkerLaunch    *NativeWorkerLaunch              `json:"worker_launch,omitempty"`
	Pipeline        []gate.PipelineTiming            `json:"pipeline,omitempty"`
	PipelineDropped int                              `json:"pipeline_dropped,omitempty"`
	Validation      *gate.CommandResult              `json:"validation,omitempty"`
	Recovery        *NativeRecoveryDecision          `json:"recovery,omitempty"`
	Completion      *NativeCompletionObservation     `json:"completion,omitempty"`
	PhasesDropped   int                              `json:"phases_dropped"`
	LocalAttemptID  int64                            `json:"local_attempt_id,omitempty"`
	Generation      uint64                           `json:"generation,omitempty"`
	HeartbeatAt     time.Time                        `json:"heartbeat_at"`
	Phase           string                           `json:"phase"`
	Phases          []NativePhase                    `json:"phases"`
	Identity        agentidentity.Identity           `json:"identity,omitzero"`
	Activity        *workflowmetrics.ActivityProfile `json:"activity,omitempty"`
	Landing         *NativeLandingReceipt            `json:"landing,omitempty"`
	REST            *NativeRESTEvidence              `json:"rest,omitempty"`
	GitHub          *NativeGitHubScope               `json:"github,omitempty"`
}

type NativeWorkerLaunch struct {
	PID              int       `json:"pid"`
	GroupID          int       `json:"pgid"`
	ProcessStart     time.Time `json:"process_start"`
	ExecutionVersion string    `json:"execution_version"`
	ExecutionCommit  string    `json:"execution_commit"`
	ExecutionSHA256  string    `json:"execution_sha256"`
	ControlVersion   string    `json:"control_version"`
	PolicyID         string    `json:"policy_id"`
	PolicyRevision   string    `json:"policy_revision"`
	Protocol         int       `json:"protocol"`
	SelectedAt       time.Time `json:"selected_at"`
}

type NativeRecoveryDecision struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

type NativeCompletionObservation struct {
	Source             string    `json:"source"`
	Coverage           string    `json:"coverage"`
	AcceptanceRecorded bool      `json:"acceptance_recorded"`
	ObservedAt         time.Time `json:"observed_at"`
}

type NativeFinalization struct {
	Publication     *NativePRPublication   `json:"publication,omitempty"`
	Unavailable     []string               `json:"unavailable"`
	ObservedAt      time.Time              `json:"observed_at"`
	Source          string                 `json:"source"`
	Coverage        string                 `json:"coverage"`
	Settled         bool                   `json:"settled"`
	Changed         bool                   `json:"changed"`
	Files           int                    `json:"files"`
	ChangeID        string                 `json:"change_id,omitempty"`
	VersionID       string                 `json:"version_id,omitempty"`
	BaseSHA         string                 `json:"base_sha,omitempty"`
	HeadSHA         string                 `json:"head_sha,omitempty"`
	Reviewed        bool                   `json:"reviewed"`
	VersionError    string                 `json:"version_error,omitempty"`
	VersionCode     string                 `json:"version_code,omitempty"`
	Error           string                 `json:"error,omitempty"`
	TextTruncated   bool                   `json:"text_truncated"`
	TextRedacted    bool                   `json:"text_redacted"`
	SourceVersion   *NativeChangeReference `json:"source_version,omitempty"`
	SourceAttemptID string                 `json:"source_attempt_id,omitempty"`
}

type NativePRPublication struct {
	ChangeID        string                  `json:"change_id"`
	VersionID       string                  `json:"version_id"`
	Repository      string                  `json:"repository"`
	BaseRef         string                  `json:"base_ref"`
	Branch          string                  `json:"branch"`
	HeadSHA         string                  `json:"head_sha"`
	PolicyID        string                  `json:"policy_id"`
	SourceVersion   NativeChangeReference   `json:"source_version"`
	SourceAttemptID string                  `json:"source_attempt_id"`
	External        ChangeExternalReference `json:"external"`
}

const NativeFinalizationTextLimit = 4096

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

type NativeLandingCIReceipt struct {
	HeadSHA        string   `json:"head_sha"`
	PullRequest    int      `json:"pull_request"`
	State          string   `json:"state"`
	RequiredChecks []string `json:"required_checks,omitempty"`
	PendingChecks  []string `json:"pending_checks,omitempty"`
	MissingChecks  []string `json:"missing_checks,omitempty"`
	FailedChecks   []string `json:"failed_checks,omitempty"`
	TriggerLabel   string   `json:"trigger_label,omitempty"`
	Triggered      bool     `json:"triggered,omitempty"`
}

type NativeLandingReceipt struct {
	SecurityAudit   *SecurityAuditReview    `json:"security_audit,omitempty"`
	Pipeline        []gate.PipelineTiming   `json:"pipeline,omitempty"`
	PipelineDropped int                     `json:"pipeline_dropped,omitempty"`
	Path            string                  `json:"path,omitempty"`
	Packages        []string                `json:"packages,omitempty"`
	Waiting         bool                    `json:"waiting,omitempty"`
	CI              *NativeLandingCIReceipt `json:"ci,omitempty"`
	Gate            *gate.CommandResult     `json:"gate,omitempty"`
	FromState       string                  `json:"from_state,omitempty"`
	TargetState     string                  `json:"target_state,omitempty"`
	Refusal         string                  `json:"refusal,omitempty"`
	GateFailed      bool                    `json:"gate_failed,omitempty"`
	Rebased         bool                    `json:"rebased,omitempty"`
	ChangeID        string                  `json:"change_id,omitempty"`
	VersionID       string                  `json:"version_id,omitempty"`
	HeadSHA         string                  `json:"head_sha,omitempty"`
	Landed          bool                    `json:"landed"`
	MergeSHA        string                  `json:"merge_sha,omitempty"`
	BaseRef         string                  `json:"base_ref,omitempty"`
	BaseSHA         string                  `json:"base_sha,omitempty"`
	Method          string                  `json:"method,omitempty"`
	RefusalKind     string                  `json:"refusal_kind,omitempty"`
	ObservedAt      time.Time               `json:"observed_at"`
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
	LastRefreshAt   time.Time     `json:"last_refresh_at,omitzero"`
	RefreshDuration time.Duration `json:"refresh_duration,omitempty"`
	ObservedAt      time.Time     `json:"observed_at"`
	PolicyID        string        `json:"policy_id"`
	WorkflowStates  []string      `json:"workflow_states,omitempty"`
	Authors         []string      `json:"authors,omitempty"`
	Assignees       []string      `json:"assignees,omitempty"`
	LabelInclude    []string      `json:"label_include,omitempty"`
	LabelExclude    []string      `json:"label_exclude,omitempty"`
}

func (c NativeAdmissionContext) Validate() error {
	if c.RefreshDuration < 0 {
		return errors.New("refresh duration must not be negative")
	}
	if c.ObservedAt.IsZero() || c.PolicyID == "" || len(c.PolicyID) > 128 {
		return errors.New("admission context requires a bounded policy identity")
	}
	for _, values := range [][]string{c.WorkflowStates, c.Authors, c.Assignees, c.LabelInclude, c.LabelExclude} {
		if len(values) > 32 {
			return errors.New("admission selectors exceed their bound")
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 128 {
				return errors.New("invalid admission selector")
			}
		}
	}
	return nil
}

type NativeAnsweredExclusion struct {
	Kind            string     `json:"kind"`
	AttemptID       string     `json:"attempt_id"`
	RetryEligibleAt *time.Time `json:"retry_eligible_at,omitempty"`
}

type NativeRuntimeAdmission struct {
	AnsweredExclusion      *NativeAnsweredExclusion `json:"answered_exclusion,omitempty"`
	UnresolvedDependencies []NativeDependency       `json:"unresolved_dependencies,omitempty"`
	SelectorSource         string                   `json:"selector_source,omitempty"`
	SelectorObservedAt     *time.Time               `json:"selector_observed_at,omitempty"`
	RunnerID               string                   `json:"runner_id"`
	RunnerRevision         int64                    `json:"runner_revision"`
	PolicyID               string                   `json:"policy_id"`
	Source                 string                   `json:"source"`
	ObservedAt             time.Time                `json:"observed_at"`
	Outcome                string                   `json:"outcome"`
	ReasonCode             string                   `json:"reason_code,omitempty"`
	Reason                 string                   `json:"reason"`
	Unavailable            []string                 `json:"unavailable"`
}

type NativeRuntimeEvidence struct {
	CurrentVersion           *NativeChangeVersionEvidence `json:"current_version,omitempty"`
	SourceRecovery           *NativeSourceRecovery        `json:"source_recovery,omitempty"`
	ValidationAudit          *ValidationAudit             `json:"validation_audit,omitempty"`
	Admission                []NativeRuntimeAdmission     `json:"admission,omitempty"`
	CurrentLease             *NativeLease                 `json:"current_lease,omitempty"`
	Issue                    NativeIssue                  `json:"issue"`
	Attempt                  *NativeAttempt               `json:"attempt,omitempty"`
	Selection                string                       `json:"selection"`
	LatestTransition         *CollaborationEvent          `json:"latest_transition,omitempty"`
	LatestDecision           *CollaborationEvent          `json:"latest_decision,omitempty"`
	NativeCandidateExclusion *CollaborationEvent          `json:"native_candidate_exclusion,omitempty"`
	Scheduling               NativeSchedulerDecision      `json:"scheduling"`
	Capacity                 []NativeRuntimeCapacity      `json:"capacity"`
	Change                   *ChangeDetail                `json:"change,omitempty"`
	ObservedAt               time.Time                    `json:"observed_at"`
	Unavailable              []string                     `json:"unavailable"`
}

type NativeChangeVersionEvidence struct {
	NativeChangeReference
	PublishedAt time.Time             `json:"published_at"`
	Validation  *gate.CommandResult   `json:"validation,omitempty"`
	Landing     *NativeLandingReceipt `json:"landing,omitempty"`
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

type NativeRecoveryPage struct {
	NativeReference
	Section    string `json:"section"`
	RecordID   string `json:"record_id,omitempty"`
	VersionID  string `json:"version_id,omitempty"`
	Data       []byte `json:"data"`
	SHA256     string `json:"sha256"`
	NextCursor string `json:"next_cursor,omitempty"`
	Offset     int64  `json:"offset"`
	TotalBytes int64  `json:"total_bytes"`
}

type NativeRecoveryRead struct {
	DetailPath string                  `json:"detail_path"`
	Omitted    []string                `json:"omitted"`
	IssueBody  *NativeRecoveryBodyRead `json:"issue_body,omitempty"`
}

type NativeRecoveryBodyRead struct {
	Cursor     string `json:"cursor"`
	SHA256     string `json:"sha256"`
	TotalBytes int64  `json:"total_bytes"`
}

type NativeRecovery struct {
	Read            *NativeRecoveryRead    `json:"read,omitempty"`
	Continuation    *NativeRecoveryPage    `json:"continuation,omitempty"`
	SourceAttemptID string                 `json:"source_attempt_id,omitempty"`
	SourceVersionID string                 `json:"source_version_id,omitempty"`
	Lease           NativeLease            `json:"lease"`
	Issue           NativeIssue            `json:"issue"`
	Discussion      []NativeComment        `json:"discussion"`
	History         []CollaborationEvent   `json:"history"`
	Attempts        []NativeAttempt        `json:"attempts"`
	Change          *NativeChangeReference `json:"change,omitempty"`
	ChangeDetail    *ChangeDetail          `json:"change_detail,omitempty"`
}

func (c *NativeCheckpoint) PreservesSource() bool {
	return c != nil && (c.WorktreeState != "clean" || c.Resume != "fresh_checkout" || c.UncertainForgeEffect())
}

func (c *NativeCheckpoint) UncertainForgeEffect() bool {
	return c != nil && (c.ExternalEffect == "git_push" || c.ExternalEffect == "pr_create") && (c.EffectState == "pending" || c.EffectState == "ambiguous")
}

func (r NativeRecovery) SourceAttempt() *NativeAttempt {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		attempt := &r.Attempts[i]
		if r.SourceAttemptID != "" {
			if attempt.AttemptID == r.SourceAttemptID {
				return attempt
			}
		} else if attempt.Checkpoint.PreservesSource() {
			return attempt
		}
	}
	if r.SourceAttemptID == "" && len(r.Attempts) > 0 {
		return &r.Attempts[len(r.Attempts)-1]
	}
	return nil
}

type NativeSourceRecovery struct {
	Destinations        []NativeSourceRecoveryRunner `json:"destinations"`
	WorkItemID          NativeWorkItemID             `json:"work_item_id"`
	Revision            Revision                     `json:"revision,string"`
	SourceMachineID     MachineID                    `json:"source_machine_id"`
	SourceRunnerName    string                       `json:"source_runner_name"`
	SourceRunnerID      string                       `json:"source_runner_id"`
	AttemptID           string                       `json:"attempt_id"`
	VersionID           string                       `json:"version_id"`
	HeadSHA             string                       `json:"head_sha"`
	BaseSHA             string                       `json:"base_sha"`
	DestinationRunnerID string                       `json:"destination_runner_id"`
	Available           bool                         `json:"available"`
	Quiesced            bool                         `json:"quiesced"`
	Reason              string                       `json:"reason"`
}

type NativeSourceRecoveryRunner struct {
	ID   string `json:"runner_id"`
	Name string `json:"name"`
}
