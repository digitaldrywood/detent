package model

import "time"

type ProposalStatus string

const (
	ProposalOpen       ProposalStatus = "open"
	ProposalAccepted   ProposalStatus = "accepted"
	ProposalRejected   ProposalStatus = "rejected"
	ProposalExpired    ProposalStatus = "expired"
	ProposalSuperseded ProposalStatus = "superseded"
)

type Proposal struct {
	ID                 string
	ProjectID          string
	IssueID            string
	IssueIdentifier    string
	IssueURL           string
	TargetState        string
	Fingerprint        string
	CriteriaSection    string
	CriteriaText       string
	Findings           []Finding
	Confidence         float64
	RecommendedEffort  string
	EffortRationale    string
	Status             ProposalStatus
	CreatedAt          time.Time
	ExpiresAt          time.Time
	ResolvedAt         time.Time
	CommentedAt        time.Time
	DecisionCommentID  string
	DecisionActorLogin string
	DecisionActorKind  string
	TransitionAt       time.Time
	DecisionSeconds    int64
	ResolutionReason   string
}

type Finding struct {
	Dimension      string `json:"dimension"`
	CriterionQuote string `json:"criterion_quote"`
	Matched        bool   `json:"matched"`
	Rationale      string `json:"rationale"`
}

type Decline struct {
	ID              string
	ProjectID       string
	IssueID         string
	IssueIdentifier string
	IssueURL        string
	Fingerprint     string
	Reason          string
	Detail          string
	Confidence      *float64
	FailedDimension string
	FailedCriterion string
	CreatedAt       time.Time
	CommentedAt     time.Time
}

type RunRecord struct {
	ProjectID       string
	ScheduledFor    time.Time
	StartedAt       time.Time
	CompletedAt     time.Time
	Outcome         string
	DeferredReason  string
	ProposalReason  string
	CandidatesFound int
	Candidates      int
	Proposed        int
	Skipped         map[string]int
	Truncated       map[string]int
	Issues          []IssueRecord
	Error           string
}

type IssueRecord struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	URL        string `json:"url,omitempty"`
	ProposalID string `json:"proposal_id,omitempty"`
}

type Decision struct {
	ProposalID        string
	Outcome           ProposalStatus
	DecidedAt         time.Time
	CommentID         string
	ActorLogin        string
	ActorKind         string
	TransitionAt      time.Time
	TransitionEventID int64
	Reason            string
	Automatic         bool
	Implicit          bool
}

type TargetTransitionQuery struct {
	ProjectID   string
	IssueID     string
	TargetState string
	NotBefore   time.Time
}

type TargetTransition struct {
	EventID    int64
	EnteredAt  time.Time
	ActorLogin string
	ActorKind  string
}

type OutcomeRefresh struct {
	ProjectID      string
	TerminalStates []string
	ReworkState    string
	ObservedAt     time.Time
}

type DownstreamOutcome struct {
	ProposalID       string
	ProjectID        string
	IssueID          string
	CompletedAt      time.Time
	ReworkCount      int
	ReviewChurnCount int
	SpendUSD         float64
	UpdatedAt        time.Time
}
