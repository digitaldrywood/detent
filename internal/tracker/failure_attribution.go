package tracker

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/runtimeoutput"
)

type FailureEvidence struct {
	Signal                     string `json:"signal,omitempty"`
	SourceDiff                 string `json:"source_diff,omitempty"`
	Step                       string `json:"step"`
	ExitCode                   *int   `json:"exit_code,omitempty"`
	OutputTail                 string `json:"output_tail"`
	RecentOtherIssueSuccess    *bool  `json:"recent_other_issue_success,omitempty"`
	SourceChangedConfiguration *bool  `json:"source_changed_configuration,omitempty"`
	MissingEvidence            string `json:"missing_evidence,omitempty"`
}

type FailureAttribution struct {
	Evidence FailureEvidence `json:"evidence"`
	Outcome  string          `json:"outcome"`
	Reason   string          `json:"reason"`
}

type FailureStepSuccess struct {
	Step        string           `json:"step"`
	AttemptID   string           `json:"attempt_id"`
	WorkItemID  NativeWorkItemID `json:"work_item_id"`
	MachineID   MachineID        `json:"machine_id"`
	RunnerID    string           `json:"runner_id"`
	CompletedAt time.Time        `json:"completed_at"`
}

func (f NativeTerminalFailure) InfrastructureDecision() *FailureAttribution {
	if f.Rejection.TerminalStage() != "" {
		return &FailureAttribution{Outcome: "instance", Reason: "typed publication rejection"}
	}
	if f.Publication != nil && f.Publication.Valid() {
		return &FailureAttribution{Outcome: "instance", Reason: "typed audit publication failure"}
	}
	if f.RPCCode != nil {
		switch *f.RPCCode {
		case -32700, -32600, -32601, -32603:
			return &FailureAttribution{Outcome: "instance", Reason: "backend protocol unavailable"}
		}
	}
	switch f.ErrorClass {
	case "backend_startup", "workspace_hook":
		return &FailureAttribution{Outcome: "instance", Reason: "typed instance infrastructure failure"}
	case "protocol":
		if f.RPCCode == nil {
			return &FailureAttribution{Outcome: "instance", Reason: "backend protocol unavailable"}
		}
	}
	return nil
}

func (e FailureEvidence) Valid() bool {
	return len(e.Signal) <= 64 && utf8.ValidString(e.Signal) && len(e.SourceDiff) <= 4000 && len(e.Step) <= 512 && len(e.OutputTail) <= 8000 && len(e.MissingEvidence) <= 1024 && utf8.ValidString(e.SourceDiff) && utf8.ValidString(e.Step) && utf8.ValidString(e.OutputTail) && utf8.ValidString(e.MissingEvidence)
}
func (d FailureAttribution) Valid() bool {
	return (d.Outcome == "instance" || d.Outcome == "issue" || d.Outcome == "unknown") && strings.TrimSpace(d.Reason) != "" && len(d.Reason) <= 2048 && utf8.ValidString(d.Reason) && d.Evidence.Valid()
}

func (e FailureEvidence) Public() FailureEvidence {
	e.SourceDiff = runtimeoutput.Truncate(RedactGateOutput(e.SourceDiff), 4000).Value
	e.Step = runtimeoutput.Truncate(RedactGateOutput(e.Step), 512).Value
	e.Signal = runtimeoutput.Truncate(RedactGateOutput(e.Signal), 64).Value
	e.OutputTail = runtimeoutput.Truncate(RedactGateOutput(e.OutputTail), 8000).Value
	e.MissingEvidence = runtimeoutput.Truncate(RedactGateOutput(e.MissingEvidence), 1024).Value
	return e
}
