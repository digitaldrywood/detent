package progresscheck

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const InvestigationCapability = "progress_investigation"

const RetrievedEvidenceCapability = "progress_retrieved_evidence"

type Publication struct {
	tracker.Mutation
	RoundInput
	Checklist        *tracker.ExecutionChecklist        `json:"checklist,omitempty"`
	LoopIntervention *tracker.ExecutionLoopIntervention `json:"loop_intervention,omitempty"`
	AttemptID        string                             `json:"attempt_id,omitempty"`
	EvidenceRevision string                             `json:"evidence_revision,omitempty"`
	ExpectedRevision tracker.Revision                   `json:"expected_revision,string,omitempty"`
	RequestID        string                             `json:"request_id"`
	PolicyID         string                             `json:"policy_id"`
	Instance         string                             `json:"instance"`
	Decision         Decision                           `json:"decision"`
}

type TerminalEvidence struct {
	OrganizationID string `json:"organization_id"`
	AttemptID      string `json:"attempt_id"`
	Step           string `json:"step"`
	Outcome        string `json:"outcome"`
	Basis          string `json:"basis"`
	NextStep       string `json:"next_step,omitempty"`
	Disposition    string `json:"disposition,omitempty"`
}

const (
	LandedDisposition   = "landed"
	NoChangeDisposition = "accepted_no_change"
	StoppedDisposition  = "stopped"
)

func (d Decision) ValidateTerminal(r Request) error {
	if r.Terminal == nil || d.Succeeded == nil || strings.TrimSpace(d.Advancement) == "" || strings.TrimSpace(d.Reason) == "" || len(d.Reason) > 512 {
		return errors.New("terminal decision requires success, advancement and reason")
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > MaxOutputBytes {
		return errors.New("terminal decision exceeds its detail bound")
	}
	if d.Outcome != Progressing && d.Outcome != Reroute && d.Outcome != SystemDefect {
		return errors.New("unknown progress decision")
	}
	if d.Outcome == SystemDefect && (strings.TrimSpace(d.DefectKey) == "" || strings.TrimSpace(d.DefectTitle) == "" || strings.TrimSpace(d.DefectBody) == "") {
		return errors.New("system defect decision requires filing evidence")
	}
	if len(d.RetrievedEvidence) != 0 || len(d.Evidence) == 0 || len(d.Evidence) > MaxReceipts {
		return errors.New("terminal decision requires bounded citations to supplied evidence")
	}
	known := r.evidenceIDs()
	for _, id := range d.Evidence {
		if !known[id] {
			return errors.New("terminal decision cites unknown evidence")
		}
	}
	if d.Disposition != "" {
		if d.Step != "" || d.Disposition != r.Terminal.Disposition {
			return errors.New("terminal disposition has no matching authoritative evidence")
		}
	} else if !slices.Contains([]string{"implement", "project_gate", "implementation_review", "security_audit", "acceptance", "publication", "merge", CompleteStep}, d.Step) {
		return errors.New("terminal decision requires a supported next step")
	}
	return nil
}

type TerminalView struct {
	Request         Request  `json:"request"`
	RequestID       string   `json:"request_id"`
	Current         bool     `json:"current"`
	Record          *Record  `json:"record,omitempty"`
	RecordedRequest *Request `json:"recorded_request,omitempty"`
}
