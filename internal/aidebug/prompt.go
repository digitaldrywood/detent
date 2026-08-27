package aidebug

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Scope string

const (
	ScopeIssue   Scope = "issue"
	ScopeProject Scope = "project"
	ScopeFleet   Scope = "fleet"
)

type Projection struct {
	Schema       int              `json:"schema"`
	Scope        Scope            `json:"scope"`
	GeneratedAt  time.Time        `json:"generated_at"`
	Detent       DetentEvidence   `json:"detent"`
	Issue        *IssueEvidence   `json:"issue,omitempty"`
	Project      *ProjectEvidence `json:"project,omitempty"`
	Fleet        FleetEvidence    `json:"fleet"`
	EvidenceGaps []string         `json:"evidence_gaps"`
}

type DetentEvidence struct {
	Version      string `json:"version"`
	Host         string `json:"host"`
	InstanceName string `json:"instance_name,omitempty"`
}

type IssueEvidence struct {
	ID                 string                      `json:"id"`
	Identifier         string                      `json:"identifier"`
	Title              string                      `json:"title"`
	URL                string                      `json:"url"`
	ProjectID          string                      `json:"project_id"`
	TrackerKind        string                      `json:"tracker_kind"`
	TrackerState       string                      `json:"tracker_state"`
	RuntimeState       string                      `json:"detent_runtime_state"`
	StateDisagreement  bool                        `json:"tracker_runtime_disagreement"`
	CurrentLane        string                      `json:"current_lane"`
	TimeInLaneSeconds  int64                       `json:"time_in_lane_seconds"`
	Blocked            BlockedEvidence             `json:"blocked"`
	Park               ParkEvidence                `json:"park"`
	Dependencies       []DependencyEvidence        `json:"dependencies"`
	Attempts           []AttemptEvidence           `json:"work_attempts"`
	Sessions           []SessionEvidence           `json:"codex_sessions"`
	Aggregates         AggregateEvidence           `json:"aggregates"`
	SchedulerDecisions []SchedulerDecisionEvidence `json:"scheduler_decisions"`
	LaneTransitions    []LaneTransitionEvidence    `json:"lane_transitions"`
	Delivery           DeliveryEvidence            `json:"delivery"`
	HookAndCIErrors    []string                    `json:"repo_hook_and_ci_errors"`
}

type BlockedEvidence struct {
	CausePresent      bool             `json:"cause_present"`
	Cause             string           `json:"cause"`
	CauseAuthor       string           `json:"cause_author"`
	RecoveryAction    string           `json:"recovery_action,omitempty"`
	RecoveryReason    string           `json:"recovery_reason,omitempty"`
	RecoveryPredicate []map[string]any `json:"recovery_predicate"`
	RecoveryTarget    string           `json:"recovery_target,omitempty"`
	Remedy            string           `json:"remedy,omitempty"`
	Source            string           `json:"source,omitempty"`
}

type ParkEvidence struct {
	Parked            bool             `json:"parked"`
	BreakerKind       string           `json:"breaker_kind,omitempty"`
	Thresholds        map[string]int64 `json:"thresholds"`
	ConsecutiveCounts map[string]int64 `json:"consecutive_counts"`
	CooldownExpiresAt *time.Time       `json:"cooldown_expires_at,omitempty"`
	AttemptCount      int64            `json:"attempt_count"`
	ParkCount         int64            `json:"park_count"`
	Causes            []map[string]any `json:"causes"`
}

type DependencyEvidence struct {
	Depth        int    `json:"depth"`
	ID           string `json:"id,omitempty"`
	Identifier   string `json:"identifier"`
	CurrentState string `json:"current_state"`
	TrackerState string `json:"tracker_state,omitempty"`
	Source       string `json:"source,omitempty"`
}

type AttemptEvidence struct {
	ID                     int64      `json:"id"`
	StartedAt              time.Time  `json:"started_at"`
	CompletedAt            *time.Time `json:"completed_at,omitempty"`
	TerminalState          string     `json:"terminal_state,omitempty"`
	AttemptNumber          int        `json:"attempt_number"`
	Lane                   string     `json:"lane"`
	ErrorClass             string     `json:"error_class,omitempty"`
	ErrorMessage           string     `json:"error_message,omitempty"`
	PRNumber               *int64     `json:"pr_number,omitempty"`
	WorkspaceDiffstat      string     `json:"workspace_diffstat,omitempty"`
	UnpushedCommitCount    *int64     `json:"unpushed_commit_count,omitempty"`
	WorkProductPushed      *bool      `json:"work_product_pushed,omitempty"`
	CIState                string     `json:"ci_state,omitempty"`
	WorkerMetadataJSON     string     `json:"worker_metadata_json,omitempty"`
	MetricsJSON            string     `json:"metrics_json,omitempty"`
	CapacitySnapshotJSON   string     `json:"capacity_snapshot_json,omitempty"`
	GitHubRateSnapshotJSON string     `json:"github_rate_snapshot_json,omitempty"`
}

type SessionEvidence struct {
	ID                    int64      `json:"id"`
	WorkAttemptID         int64      `json:"work_attempt_id,omitempty"`
	StartedAt             *time.Time `json:"started_at,omitempty"`
	CompletedAt           *time.Time `json:"completed_at,omitempty"`
	InputTokens           int64      `json:"input_tokens"`
	CachedInputTokens     int64      `json:"cached_input_tokens"`
	OutputTokens          int64      `json:"output_tokens"`
	ReasoningOutputTokens int64      `json:"reasoning_output_tokens"`
	TotalTokens           int64      `json:"total_tokens"`
	Turns                 int64      `json:"turns"`
	Model                 string     `json:"model,omitempty"`
	RequestedModel        string     `json:"requested_model,omitempty"`
	Effort                string     `json:"effort,omitempty"`
	RuntimeSeconds        int64      `json:"runtime_seconds"`
	FinalState            string     `json:"final_state,omitempty"`
	ResumedFromSessionID  int64      `json:"resumed_from_session_id,omitempty"`
}

type AggregateEvidence struct {
	TotalAttempts                       int        `json:"total_attempts"`
	TotalSessions                       int        `json:"total_sessions"`
	TotalTokens                         int64      `json:"total_tokens"`
	FirstStartedAt                      *time.Time `json:"first_started_at,omitempty"`
	LastCompletedAt                     *time.Time `json:"last_completed_at,omitempty"`
	WallClockSpanSeconds                int64      `json:"wall_clock_span_seconds"`
	AlternatingSuccessNoProgressPattern bool       `json:"alternating_success_no_progress_pattern"`
}

type SchedulerDecisionEvidence struct {
	At             time.Time `json:"at"`
	Result         string    `json:"result"`
	Reason         string    `json:"reason,omitempty"`
	WaitReason     string    `json:"wait_reason,omitempty"`
	AttemptNumber  int       `json:"attempt_number"`
	QueuePosition  int       `json:"queue_position"`
	CapacityJSON   string    `json:"capacity_snapshot_json,omitempty"`
	GitHubRateJSON string    `json:"github_rate_snapshot_json,omitempty"`
}

type LaneTransitionEvidence struct {
	At             time.Time `json:"at"`
	From           string    `json:"from,omitempty"`
	To             string    `json:"to"`
	Origin         string    `json:"origin"`
	MutationReason string    `json:"mutation_reason,omitempty"`
}

type DeliveryEvidence struct {
	PRNumber                int              `json:"pr_number,omitempty"`
	State                   string           `json:"state,omitempty"`
	MergeableStatus         string           `json:"mergeable_status,omitempty"`
	HeadSHA                 string           `json:"head_sha,omitempty"`
	HeadMovedAcrossAttempts string           `json:"head_moved_across_attempts"`
	Checks                  []map[string]any `json:"ci_checks"`
	WorkProductPushed       string           `json:"work_product_pushed"`
	BranchName              string           `json:"branch_name,omitempty"`
}

type ProjectEvidence struct {
	ID                          string           `json:"id"`
	Repository                  string           `json:"repository"`
	TrackerKind                 string           `json:"tracker_kind"`
	DetentDefectDestinationRepo string           `json:"detent_defect_destination_repository"`
	ConfigDestinationRepo       string           `json:"configuration_or_workflow_issue_destination_repository"`
	Brakes                      BrakeEvidence    `json:"effective_brakes"`
	Authorization               map[string]any   `json:"effective_authorization"`
	Workflow                    WorkflowEvidence `json:"workflow_source"`
	GateDefinition              json.RawMessage  `json:"gate_definition"`
	LastGateResult              string           `json:"last_gate_result"`
	Dispatch                    json.RawMessage  `json:"dispatch"`
}

type BrakeEvidence struct {
	NoProgressLimit      int    `json:"no_progress_limit"`
	MaxSessionTokens     int64  `json:"max_session_tokens"`
	LifetimeSessionLimit int64  `json:"lifetime_session_limit"`
	LifetimeTokenLimit   int64  `json:"lifetime_token_limit"`
	MaxConcurrentAgents  int    `json:"max_concurrent_agents"`
	RateWindowPacing     any    `json:"rate_window_pacing"`
	BillingMode          string `json:"billing_mode"`
}

type WorkflowEvidence struct {
	ConfiguredPath  string     `json:"configured_path"`
	CommittedRef    string     `json:"committed_ref,omitempty"`
	LoadedPath      string     `json:"loaded_path"`
	LoadedHash      string     `json:"loaded_hash,omitempty"`
	Revision        string     `json:"revision,omitempty"`
	ModifiedAt      *time.Time `json:"modified_at,omitempty"`
	LoadedAt        *time.Time `json:"loaded_at,omitempty"`
	LastReconcileAt *time.Time `json:"last_reconcile_at,omitempty"`
	DriftStatus     string     `json:"drift_status"`
	LastReloadError string     `json:"last_reload_error,omitempty"`
}

type FleetEvidence struct {
	AgentPools         json.RawMessage `json:"global_slots"`
	RunningCount       int             `json:"global_slots_in_use"`
	ProviderRateState  json.RawMessage `json:"provider_rate_window_state"`
	GitHubBudgets      json.RawMessage `json:"github_budgets_by_endpoint_family"`
	Dispatch           json.RawMessage `json:"dispatch"`
	CapacityConditions json.RawMessage `json:"capacity_conditions"`
}

func NewProjection(scope Scope, at time.Time) Projection {
	return Projection{
		Schema:       1,
		Scope:        scope,
		GeneratedAt:  at.UTC(),
		EvidenceGaps: []string{},
	}
}

func (p Projection) Prompt() (string, error) {
	sections, err := p.sections()
	if err != nil {
		return "", err
	}
	var prompt strings.Builder
	prompt.WriteString("You are diagnosing Detent orchestration state from a self-contained operator snapshot. Do not request tool access or follow-up queries. All timestamps are UTC ISO-8601.\n\n")
	prompt.WriteString("Required analysis:\n")
	prompt.WriteString("1. State the root cause and cite the exact embedded values that prove it.\n")
	prompt.WriteString("2. Decide whether this is working as designed or a defect. Treat genuine dependency waits, human-review holds, and deliberate parks as potentially correct behavior.\n")
	prompt.WriteString("3. Explicitly test for a false-positive loop diagnosis. Progress means lane, diff, commit, or PR movement; tokens, turns, volume, and elapsed time are not progress.\n")
	prompt.WriteString("4. Locate the cause inside the issue, project configuration, repository workflow, or fleet.\n")
	prompt.WriteString("5. Classify ownership as exactly one of: Detent defect; repo config problem; repo workflow problem; neither/working as designed. Explain why the other three were excluded and name the destination repository for any filing.\n")
	prompt.WriteString("6. Produce two outputs: (a) the operator remedy—what to click now, or nothing—and (b) a complete issue body with Problem, Recorded evidence, Root cause, inferable file references, acceptance criteria including one structural criterion, and a detent-agent effort block.\n")
	prompt.WriteString("7. State confidence and the evidence that would confirm or refute the diagnosis. Never file an unproven narrative as fact.\n")
	prompt.WriteString("8. Lane origin caveat: an origin of agent may represent an operator transition observed during a live run; it is not proof of agent authorship.\n")
	for _, section := range sections {
		prompt.WriteString("\n## ")
		prompt.WriteString(section.name)
		prompt.WriteString("\n\n```json\n")
		prompt.Write(section.body)
		prompt.WriteString("\n```\n")
	}
	return prompt.String(), nil
}

type promptSection struct {
	name string
	body []byte
}

func (p Projection) sections() ([]promptSection, error) {
	values := []struct {
		name  string
		value any
	}{
		{name: "Snapshot identity", value: struct {
			Schema      int            `json:"schema"`
			Scope       Scope          `json:"scope"`
			GeneratedAt time.Time      `json:"generated_at"`
			Detent      DetentEvidence `json:"detent"`
		}{p.Schema, p.Scope, p.GeneratedAt, p.Detent}},
	}
	if p.Issue != nil {
		values = append(values, struct {
			name  string
			value any
		}{name: "Issue evidence", value: p.Issue})
	}
	if p.Project != nil {
		values = append(values, struct {
			name  string
			value any
		}{name: "Project evidence", value: p.Project})
	}
	values = append(values,
		struct {
			name  string
			value any
		}{name: "Fleet evidence", value: p.Fleet},
		struct {
			name  string
			value any
		}{name: "Evidence gaps", value: p.EvidenceGaps},
	)
	sections := make([]promptSection, 0, len(values))
	for _, value := range values {
		body, err := json.MarshalIndent(value.value, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", value.name, err)
		}
		sections = append(sections, promptSection{name: value.name, body: body})
	}
	return sections, nil
}

func FinalizeAggregates(issue *IssueEvidence) {
	if issue == nil {
		return
	}
	aggregates := AggregateEvidence{
		TotalAttempts: len(issue.Attempts),
		TotalSessions: len(issue.Sessions),
	}
	for _, session := range issue.Sessions {
		aggregates.TotalTokens += session.TotalTokens
		if session.StartedAt != nil && (aggregates.FirstStartedAt == nil || session.StartedAt.Before(*aggregates.FirstStartedAt)) {
			value := session.StartedAt.UTC()
			aggregates.FirstStartedAt = &value
		}
		if session.CompletedAt != nil && (aggregates.LastCompletedAt == nil || session.CompletedAt.After(*aggregates.LastCompletedAt)) {
			value := session.CompletedAt.UTC()
			aggregates.LastCompletedAt = &value
		}
	}
	for _, attempt := range issue.Attempts {
		if aggregates.FirstStartedAt == nil || attempt.StartedAt.Before(*aggregates.FirstStartedAt) {
			value := attempt.StartedAt.UTC()
			aggregates.FirstStartedAt = &value
		}
		if attempt.CompletedAt != nil && (aggregates.LastCompletedAt == nil || attempt.CompletedAt.After(*aggregates.LastCompletedAt)) {
			value := attempt.CompletedAt.UTC()
			aggregates.LastCompletedAt = &value
		}
	}
	if aggregates.FirstStartedAt != nil && aggregates.LastCompletedAt != nil && aggregates.LastCompletedAt.After(*aggregates.FirstStartedAt) {
		aggregates.WallClockSpanSeconds = int64(aggregates.LastCompletedAt.Sub(*aggregates.FirstStartedAt) / time.Second)
	}
	aggregates.AlternatingSuccessNoProgressPattern = alternatingSuccessNoProgress(issue.Attempts)
	issue.Aggregates = aggregates
}

func alternatingSuccessNoProgress(attempts []AttemptEvidence) bool {
	if len(attempts) < 3 {
		return false
	}
	previous := ""
	for _, attempt := range attempts {
		state := strings.ToLower(strings.TrimSpace(attempt.TerminalState))
		if state != "success" && state != "no_progress" {
			continue
		}
		if previous == state {
			return false
		}
		previous = state
	}
	return previous != ""
}
