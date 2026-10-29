package progresscheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MaxToolCalls       = 80
	MaxToolResultBytes = 1 << 20
	MaxDuration        = 3 * time.Minute
	MaxAttempts        = 12
	MaxReceipts        = 24
	MaxComments        = 24
	MaxInputBytes      = 64 << 10
	MaxOutputBytes     = 8 << 10
)

var ErrBackend = errors.New("progress check backend failed")

type Outcome string

const (
	Progressing  Outcome = "progressing"
	Reroute      Outcome = "reroute"
	SystemDefect Outcome = "system_defect"
)

type Attempt struct {
	FinalizationChanged *bool  `json:"finalization_changed,omitempty"`
	WorkProduct         bool   `json:"work_product,omitempty"`
	Disposition         string `json:"disposition,omitempty"`
	ReasonCode          string `json:"reason_code,omitempty"`
	RefusalKind         string `json:"refusal_kind,omitempty"`
	RefusalBaseSHA      string `json:"refusal_base_sha,omitempty"`
	RefusalHeadSHA      string `json:"refusal_head_sha,omitempty"`
	ID                  string `json:"id"`
	Step                string `json:"step"`
	Outcome             string `json:"outcome"`
	Error               string `json:"error"`
	FailureClass        string `json:"failure_class"`
	Attribution         string `json:"attribution"`
	Instance            string `json:"instance"`
	InstanceVersion     string `json:"instance_version,omitempty"`
	SourceSHA           string `json:"source_sha"`
	BaseSHA             string `json:"base_sha"`
	CheckpointSHA       string `json:"checkpoint_sha"`
}

type Receipt struct {
	ID      string `json:"id"`
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
	Detail  string `json:"detail,omitempty"`
}

type Comment struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Human    bool   `json:"human"`
	Body     string `json:"body"`
}

type Request struct {
	ProjectID        string    `json:"project_id"`
	IssueID          string    `json:"issue_id"`
	PolicyID         string    `json:"policy_id"`
	EvidenceRevision string    `json:"evidence_revision"`
	SourceSHA        string    `json:"source_sha"`
	BaseSHA          string    `json:"base_sha"`
	Instance         string    `json:"instance"`
	InstanceVersion  string    `json:"instance_version,omitempty"`
	Attempts         []Attempt `json:"attempts"`
	Receipts         []Receipt `json:"receipts"`
	Comments         []Comment `json:"comments"`
	AvailableSteps   []string  `json:"available_steps"`
	Context          string    `json:"context,omitempty"`
}

type Decision struct {
	Diagnosis   string   `json:"diagnosis,omitempty"`
	Instruction string   `json:"instruction,omitempty"`
	Convergence string   `json:"convergence,omitempty"`
	Outcome     Outcome  `json:"outcome"`
	Step        string   `json:"step,omitempty"`
	Reason      string   `json:"reason"`
	Evidence    []string `json:"evidence"`
	DefectKey   string   `json:"defect_key,omitempty"`
	DefectTitle string   `json:"defect_title,omitempty"`
	DefectBody  string   `json:"defect_body,omitempty"`
}

type Record struct {
	RequestID        string
	ProjectID        string
	IssueID          string
	EvidenceRevision string
	Decision         Decision
	CommentID        string
	DefectIssueID    string
}

type Backend interface {
	Decide(context.Context, Request) (Decision, error)
}

type Owner interface {
	Load(context.Context, string, string, string) (Record, bool, error)
	Publish(context.Context, Record) (Record, error)
}

func Check(ctx context.Context, request Request, backend Backend, owner Owner) (Record, bool, error) {
	if !NeedsCheck(request) {
		return Record{}, false, nil
	}
	id, err := request.ID()
	if err != nil {
		return Record{}, false, err
	}
	if backend == nil || owner == nil {
		return Record{}, false, errors.New("progress check requires backend and tracker owner")
	}
	record, found, err := owner.Load(ctx, request.ProjectID, request.IssueID, id)
	if err != nil {
		return Record{}, false, fmt.Errorf("load progress decision: %w", err)
	}
	if found {
		if err := validateRecord(request, id, record); err != nil {
			return Record{}, false, fmt.Errorf("validate retained progress decision: %w", err)
		}
		return record, true, nil
	}
	decision, err := backend.Decide(ctx, request)
	if err != nil {
		return Record{}, false, fmt.Errorf("%w: %w", ErrBackend, err)
	}
	if err := validateDecision(request, decision); err != nil {
		return Record{}, false, fmt.Errorf("%w: %w", ErrBackend, err)
	}
	record, err = owner.Publish(ctx, Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision})
	if err != nil {
		return Record{}, false, fmt.Errorf("publish progress decision: %w", err)
	}
	if err := validateRecord(request, id, record); err != nil {
		return Record{}, false, fmt.Errorf("validate published progress decision: %w", err)
	}
	return record, true, nil
}

func NeedsCheck(request Request) bool {
	if len(request.Attempts) == 0 {
		return false
	}
	last := request.Attempts[len(request.Attempts)-1]
	if last.Outcome != "success" {
		return last.Outcome != "cancelled"
	}
	if last.Disposition == "blocked" && last.ReasonCode == "source_recovery_required" {
		return true
	}
	if last.FinalizationChanged != nil && !*last.FinalizationChanged {
		return true
	}
	if last.FinalizationChanged == nil && !last.WorkProduct && (last.Step == "code" || last.Step == "rework" || last.Step == "implement") {
		return true
	}
	if last.RefusalKind == "nothing_to_land" {
		return true
	}
	if last.RefusalKind == "" || last.RefusalBaseSHA == "" || last.RefusalHeadSHA == "" || len(request.Attempts) < 2 {
		return false
	}
	previous := request.Attempts[len(request.Attempts)-2]
	return last.RefusalKind == previous.RefusalKind && last.RefusalBaseSHA == previous.RefusalBaseSHA && last.RefusalHeadSHA == previous.RefusalHeadSHA
}

func Failed(outcome string) bool {
	switch outcome {
	case "failed", "timed_out", "abandoned":
		return true
	default:
		return false
	}
}

func (request Request) ID() (string, error) {
	encoded, err := encodeRequest(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (record Record) Validate(request Request) error {
	id, err := request.ID()
	if err != nil {
		return err
	}
	return validateRecord(request, id, record)
}

func (record Record) ValidatePublication(request Request) error {
	id, err := request.ID()
	if err != nil {
		return err
	}
	if err := validateRecordBinding(request, id, record); err != nil {
		return err
	}
	return validateDecision(request, record.Decision)
}

func validateRecord(request Request, id string, record Record) error {
	if err := validateRecordBinding(request, id, record); err != nil {
		return err
	}
	return validateDecisionVersion(request, record.Decision, record.Decision.Legacy())
}

func validateRecordBinding(request Request, id string, record Record) error {
	if record.RequestID != id || record.ProjectID != request.ProjectID || record.IssueID != request.IssueID || record.EvidenceRevision != request.EvidenceRevision {
		return errors.New("progress decision binding differs")
	}
	if record.CommentID == "" || record.Decision.Outcome == SystemDefect && record.DefectIssueID == "" {
		return errors.New("progress decision publication receipts missing")
	}
	return nil
}

func encodeRequest(request Request) ([]byte, error) {
	if request.ProjectID == "" || request.IssueID == "" || request.PolicyID == "" || request.EvidenceRevision == "" {
		return nil, errors.New("progress request requires project, issue, policy and evidence revision")
	}
	if len(request.Attempts) == 0 || len(request.Attempts) > MaxAttempts || len(request.Receipts) > MaxReceipts || len(request.Comments) > MaxComments || len(request.AvailableSteps) == 0 || len(request.AvailableSteps) > MaxReceipts {
		return nil, errors.New("progress request evidence count outside bounds")
	}
	ids := make(map[string]bool)
	for _, attempt := range request.Attempts {
		if attempt.ID == "" || ids[attempt.ID] {
			return nil, errors.New("progress attempts require unique evidence IDs")
		}
		ids[attempt.ID] = true
		switch attempt.Outcome {
		case "success", "failed", "timed_out", "abandoned", "cancelled":
		default:
			return nil, errors.New("progress request contains nonterminal attempt")
		}
	}
	for _, receipt := range request.Receipts {
		if receipt.ID == "" || ids[receipt.ID] {
			return nil, errors.New("progress receipts require unique evidence IDs")
		}
		ids[receipt.ID] = true
	}
	for _, comment := range request.Comments {
		if comment.ID == "" || ids[comment.ID] {
			return nil, errors.New("progress comments require unique evidence IDs")
		}
		ids[comment.ID] = true
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode progress evidence: %w", err)
	}
	if len(encoded) > MaxInputBytes {
		return nil, errors.New("progress request evidence exceeds byte bound")
	}
	return encoded, nil
}

func (decision Decision) Legacy() bool {
	return decision.Diagnosis == "" && decision.Instruction == "" && decision.Convergence == ""
}

func validateDecision(request Request, decision Decision) error {
	return validateDecisionVersion(request, decision, false)
}

func validateDecisionVersion(request Request, decision Decision, legacy bool) error {
	encoded, err := json.Marshal(decision)
	if err != nil || len(encoded) > MaxOutputBytes || strings.TrimSpace(decision.Reason) == "" || len(decision.Evidence) == 0 || len(decision.Evidence) > MaxReceipts {
		return errors.New("progress decision requires bounded reasoning and evidence")
	}
	known := make(map[string]bool)
	for _, attempt := range request.Attempts {
		known[attempt.ID] = true
	}
	for _, receipt := range request.Receipts {
		known[receipt.ID] = true
	}
	for _, comment := range request.Comments {
		known[comment.ID] = true
	}
	for _, evidence := range decision.Evidence {
		if evidence == "" || !known[evidence] {
			return errors.New("progress decision cites unavailable evidence")
		}
	}
	if !legacy && strings.TrimSpace(decision.Diagnosis) == "" {
		return errors.New("progress decision requires a diagnosis of the failure and its cause")
	}
	cited := false
	for _, id := range decision.Evidence {
		cited = cited || strings.Contains(decision.Diagnosis, id)
	}
	if !legacy && !cited {
		return errors.New("progress diagnosis must cite its evidence")
	}
	if decision.Outcome != SystemDefect && (decision.DefectKey != "" || decision.DefectTitle != "" || decision.DefectBody != "") {
		return errors.New("progress decision has unrelated defect fields")
	}
	switch decision.Outcome {
	case Progressing:
		if !legacy && strings.TrimSpace(decision.Convergence) == "" {
			return errors.New("progressing decision requires evidence of convergence")
		}
	case Reroute:
		if !legacy && strings.TrimSpace(decision.Instruction) == "" {
			return errors.New("reroute decision requires an actionable instruction")
		}
		for _, step := range request.AvailableSteps {
			if step != "" && decision.Step == step {
				return nil
			}
		}
		return errors.New("progress decision names unavailable step")
	case SystemDefect:
		if decision.Step != "" || strings.TrimSpace(decision.DefectKey) == "" || strings.TrimSpace(decision.DefectTitle) == "" || strings.TrimSpace(decision.DefectBody) == "" {
			return errors.New("system defect decision requires filing evidence")
		}
	default:
		return errors.New("unknown progress decision")
	}
	return nil
}
