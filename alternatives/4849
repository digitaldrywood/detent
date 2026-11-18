package prompt

import (
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RoutineRequest struct {
	Name     string
	Schedule string
	Prompt   string
}

type AdmissionRequest struct {
	Schedule        string
	TargetState     string
	CriteriaSection string
	CriteriaText    string
	Dimensions      []AdmissionDimension
	EffortSection   string
	EffortText      string
	AllowedEfforts  []string
	Candidates      []AdmissionCandidate
}

type AdmissionDimension struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type AdmissionCandidate struct {
	Dependencies *AdmissionDependencies `json:"dependencies,omitempty"`
	ID           string                 `json:"id"`
	Identifier   string                 `json:"identifier"`
	Title        string                 `json:"title"`
	Description  string                 `json:"description"`
	State        string                 `json:"state"`
	AuthorID     string                 `json:"author_id,omitempty"`
	Labels       []string               `json:"labels,omitempty"`
}

type AdmissionDependencies struct {
	ObservedAt time.Time             `json:"observed_at"`
	Readiness  string                `json:"readiness"`
	Ready      bool                  `json:"ready"`
	References []AdmissionDependency `json:"references"`
}

type AdmissionDependency struct {
	Identifier       string `json:"identifier"`
	State            string `json:"state,omitempty"`
	Closed           bool   `json:"closed"`
	PullRequestState string `json:"pull_request_state,omitempty"`
	Ready            bool   `json:"ready"`
	Error            string `json:"error,omitempty"`
}

type PriorAttempt struct {
	Source    string
	Reason    string
	Validator gate.ValidatorResult
}

type NativeSecurityAuditEvidence struct {
	ReviewID        string
	ProjectID       string
	WorkItemID      string
	ChangeID        string
	VersionID       string
	Repository      string
	BaseSHA         string
	HeadSHA         string
	DiffDigest      string
	ReviewerVersion string
	ReviewerDigest  string
	Verdict         string
	SessionID       int64
}

type NativeReviewDiscussion struct {
	Issue  []tracker.NativeComment    `json:"issue"`
	Change []tracker.ChangeDiscussion `json:"change"`
}
