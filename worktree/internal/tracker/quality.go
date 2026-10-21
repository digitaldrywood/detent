package tracker

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

type LandingQualitySnapshot struct {
	IssueRevision Revision                 `json:"issue_revision,string"`
	IssueBody     string                   `json:"issue_body"`
	Criteria      []string                 `json:"criteria"`
	Missing       []string                 `json:"missing_sections"`
	Evidence      []gate.CriterionEvidence `json:"criteria_evidence"`
	ReviewID      string                   `json:"review_id,omitempty"`
}

type QualityEscape struct {
	OccurrenceID      string    `json:"occurrence_id"`
	Kind              string    `json:"kind"`
	ObservedAt        time.Time `json:"observed_at"`
	Cause             string    `json:"cause"`
	EvidenceReference string    `json:"evidence_reference"`
	EvidenceQuote     string    `json:"evidence_quote"`
	BasisQuote        string    `json:"basis_quote"`
	Criterion         string    `json:"criterion,omitempty"`
	InstanceID        string    `json:"instance_id,omitempty"`
	HumanOverride     bool      `json:"human_override,omitempty"`
}

func (e QualityEscape) Validate() error {
	if !slices.Contains([]string{"revert", "scheduled_failure", "reopened"}, e.Kind) || !slices.Contains([]string{"underspecified_issue", "missing_criterion", "validator_miss", "infrastructure"}, e.Cause) || e.ObservedAt.IsZero() {
		return errors.New("escape requires a supported kind, cause and occurrence time")
	}
	for _, field := range []struct {
		value string
		limit int
	}{{e.OccurrenceID, 256}, {e.EvidenceReference, 2048}, {e.EvidenceQuote, 4096}, {e.BasisQuote, 4096}} {
		if strings.TrimSpace(field.value) == "" || len(field.value) > field.limit {
			return errors.New("escape requires bounded occurrence identity, evidence reference, evidence quote and classification basis quote")
		}
	}
	if len(e.Criterion) > 4096 || len(e.InstanceID) > 256 {
		return errors.New("escape criterion or instance identity exceeds its bound")
	}
	if e.Cause == "infrastructure" && strings.TrimSpace(e.InstanceID) == "" {
		return errors.New("infrastructure escapes must identify the instance")
	}
	if e.Cause != "infrastructure" && e.InstanceID != "" {
		return errors.New("only infrastructure escapes are attributed to an instance")
	}
	return nil
}
