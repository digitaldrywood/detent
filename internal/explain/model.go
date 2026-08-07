package explain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const SchemaVersion = 1

var (
	ErrProjectRequired      = errors.New("project reference is required")
	ErrIssueReferenceNeeded = errors.New("issue reference is required")
	ErrNotFound             = errors.New("issue explanation not found")
)

type SourceState string

const (
	SourceAvailable   SourceState = "available"
	SourceLive        SourceState = "live"
	SourceLastKnown   SourceState = "last_known"
	SourceExpired     SourceState = "expired"
	SourceUnavailable SourceState = "unavailable"
	SourceCorrupt     SourceState = "corrupt"
)

type EligibilityState string

const (
	EligibilityUnknown  EligibilityState = "unknown"
	EligibilityEligible EligibilityState = "eligible"
	EligibilityRefused  EligibilityState = "refused"
)

type GateState string

const (
	GateUnknown       GateState = "unknown"
	GateUnavailable   GateState = "unavailable"
	GateNotApplicable GateState = "not_applicable"
	GatePending       GateState = "pending"
	GateFailed        GateState = "failed"
	GatePassed        GateState = "passed"
)

type EvidenceKind string

const (
	EvidenceSnapshot    EvidenceKind = "snapshot"
	EvidenceWorkflow    EvidenceKind = "workflow"
	EvidenceAttempt     EvidenceKind = "attempt"
	EvidenceScheduler   EvidenceKind = "scheduler"
	EvidenceAdmission   EvidenceKind = "admission"
	EvidenceSession     EvidenceKind = "session"
	EvidenceProvider    EvidenceKind = "provider_session"
	EvidencePullRequest EvidenceKind = "pull_request"
)

type Query struct {
	ProjectID  string
	Reference  string
	IssueID    string
	Identifier string
	IssueURL   string
}

type IssueExplanation struct {
	Schema           int                 `json:"schema"`
	Found            bool                `json:"found"`
	ObservedAt       time.Time           `json:"observed_at"`
	Identity         Identity            `json:"identity"`
	CurrentLane      Lane                `json:"current_lane"`
	LatestTransition *Transition         `json:"latest_transition,omitempty"`
	Eligibility      Eligibility         `json:"eligibility"`
	Attempt          *Attempt            `json:"attempt,omitempty"`
	Sessions         Sessions            `json:"sessions"`
	PullRequest      *PullRequest        `json:"pull_request,omitempty"`
	RequiredGate     Gate                `json:"required_gate"`
	Sources          []SourceStatus      `json:"sources"`
	Evidence         []EvidenceReference `json:"evidence"`
}

type Identity struct {
	ProjectID  string `json:"project_id"`
	IssueID    string `json:"issue_id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	IssueURL   string `json:"issue_url,omitempty"`
	Number     int    `json:"number,omitempty"`
	Title      string `json:"title,omitempty"`
}

type Lane struct {
	Name       string      `json:"name,omitempty"`
	ObservedAt *time.Time  `json:"observed_at,omitempty"`
	Freshness  SourceState `json:"freshness"`
	Degraded   bool        `json:"degraded"`
	EvidenceID string      `json:"evidence_id,omitempty"`
}

type Transition struct {
	EvidenceID string     `json:"evidence_id"`
	From       string     `json:"from,omitempty"`
	To         string     `json:"to"`
	At         time.Time  `json:"at"`
	Source     string     `json:"source,omitempty"`
	Actor      *Actor     `json:"actor,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Provenance Provenance `json:"provenance"`
	PRNumber   *int64     `json:"pr_number,omitempty"`
	SessionID  int64      `json:"session_id,omitempty"`
}

type Actor struct {
	Login string `json:"login,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

type Provenance struct {
	State     SourceState `json:"state"`
	Origin    string      `json:"origin"`
	Admission *Admission  `json:"admission,omitempty"`
}

type Admission struct {
	ProposalID string `json:"proposal_id,omitempty"`
	Attributed bool   `json:"attributed"`
}

type Eligibility struct {
	State    EligibilityState      `json:"state"`
	Latest   *EligibilityDecision  `json:"latest,omitempty"`
	Refusals []EligibilityDecision `json:"refusals"`
	Source   SourceState           `json:"source_state"`
}

type EligibilityDecision struct {
	EvidenceID string           `json:"evidence_id"`
	Source     string           `json:"source"`
	State      EligibilityState `json:"state"`
	Outcome    string           `json:"outcome"`
	Reason     string           `json:"reason,omitempty"`
	At         time.Time        `json:"at"`
}

type Attempt struct {
	EvidenceID        string     `json:"evidence_id"`
	ID                int64      `json:"id"`
	Selection         string     `json:"selection"`
	Status            string     `json:"status"`
	TerminalState     string     `json:"terminal_state,omitempty"`
	AttemptNumber     int        `json:"attempt_number,omitempty"`
	Lane              string     `json:"lane,omitempty"`
	Phase             string     `json:"phase,omitempty"`
	StatusMessage     string     `json:"status_message,omitempty"`
	WaitReason        string     `json:"wait_reason,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	HeartbeatAt       *time.Time `json:"heartbeat_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	DetentSessionID   int64      `json:"detent_session_id,omitempty"`
	ProviderSessionID string     `json:"provider_session_id,omitempty"`
}

type Sessions struct {
	Detent   *Session    `json:"detent,omitempty"`
	Provider *Session    `json:"provider,omitempty"`
	Source   SourceState `json:"source_state"`
}

type Session struct {
	EvidenceID  string     `json:"evidence_id"`
	ID          string     `json:"id"`
	Backend     string     `json:"backend,omitempty"`
	Selection   string     `json:"selection"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type PullRequest struct {
	EvidenceID string     `json:"evidence_id"`
	Number     int64      `json:"number"`
	URL        string     `json:"url,omitempty"`
	State      string     `json:"state,omitempty"`
	HeadSHA    string     `json:"head_sha,omitempty"`
	Source     string     `json:"source"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

type Gate struct {
	State       GateState   `json:"state"`
	SourceState SourceState `json:"source_state"`
	EvidenceID  string      `json:"evidence_id,omitempty"`
	Source      string      `json:"source,omitempty"`
	ObservedAt  *time.Time  `json:"observed_at,omitempty"`
	Failures    []string    `json:"failures"`
	Running     []string    `json:"running"`
}

type SourceStatus struct {
	Name  string      `json:"name"`
	State SourceState `json:"state"`
	Code  string      `json:"code,omitempty"`
}

type EvidenceReference struct {
	ID         string       `json:"id"`
	Kind       EvidenceKind `json:"kind"`
	ObservedAt *time.Time   `json:"observed_at,omitempty"`
}

type AmbiguousIdentityError struct {
	ProjectID string
	Field     string
	Values    []string
}

func (e *AmbiguousIdentityError) Error() string {
	values := append([]string(nil), e.Values...)
	slices.Sort(values)
	return fmt.Sprintf("ambiguous issue identity in project %q: %s has values %s", strings.TrimSpace(e.ProjectID), e.Field, strings.Join(values, ", "))
}
