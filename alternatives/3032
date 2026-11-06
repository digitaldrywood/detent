package reportstore

import (
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type EventProjection struct {
	DurationSeconds float64
	RunSequence     int64
	RunStatus       string
	Role            string
	Model           string
	Effort          string
	Failed          bool
	FailureClass    string
	FailureKind     string
	LandingReceipt  bool
	Landed          bool
	LandingChangeID string
	ReworkClass     string
	ReworkKind      string
}

func ProjectEvent(event tracker.CollaborationEvent) *EventProjection {
	if event.Data.Run == nil && !(event.Type == "workflow.transitioned" && event.Data.ToState == "Rework") {
		return nil
	}
	out := &EventProjection{}
	if event.Type == "workflow.transitioned" && event.Data.ToState == "Rework" {
		class := ClassifyReworkReason(event.Data.FromState, event.Data.ReasonDetail)
		out.ReworkClass, out.ReworkKind = class.Name, class.Kind
	}
	if run := event.Data.Run; run != nil {
		out.RunSequence, out.RunStatus = run.Sequence, "running"
		if event.Type == "run.finished" {
			out.RunStatus = run.Outcome
		}
		if runtime := run.Runtime; runtime != nil {
			for _, phase := range runtime.Phases {
				if !phase.FinishedAt.IsZero() {
					out.DurationSeconds += max(0, phase.FinishedAt.Sub(phase.StartedAt).Seconds())
				}
			}
			out.Role, out.Model, out.Effort = runtime.Identity.Role, runtime.Identity.Model(), runtime.Identity.ReasoningEffort.Value
			if landing := runtime.Landing; landing != nil {
				out.LandingReceipt, out.Landed, out.LandingChangeID = true, landing.Landed, landing.ChangeID
			}
		}
		text := failureText(*run, out.RunStatus)
		if text != "" {
			class := ClassifyFailure(text)
			out.Failed, out.FailureClass, out.FailureKind = true, class.Name, class.Kind
		}
	}
	return out
}

func failureText(data tracker.NativeRunData, status string) string {
	if r := data.Runtime; r != nil && r.Landing != nil && !r.Landing.Landed {
		if r.Landing.RefusalKind == "conflict" || r.Landing.RefusalKind == "base_protected" {
			return "landing refused: " + r.Landing.RefusalKind
		}
	}
	if f := data.TerminalFailure; f != nil && f.Error != "" && status != "cancelled" {
		return f.Error
	}
	if r := data.Runtime; r != nil && r.Landing != nil && r.Landing.GateFailed && r.Landing.Gate != nil {
		return fmt.Sprintf("landing gate failed: exit status %d", r.Landing.Gate.ExitCode)
	}
	if status != "failed" && status != "interrupted" {
		return ""
	}
	if r := data.Runtime; r != nil && r.Validation != nil && r.Validation.ExitCode != 0 && r.Validation.Stage == gate.StageSourceFinalization {
		return fmt.Sprintf("source finalization gate failed: exit status %d", r.Validation.ExitCode)
	}
	if f := data.Finalization; f != nil {
		if f.VersionError != "" {
			return f.VersionCode + ": " + f.VersionError
		}
		if f.Error != "" {
			return f.Error
		}
	}
	if f := data.TerminalFailure; f != nil {
		return strings.TrimSpace(f.Provider + " " + f.Operation + " " + f.ProviderCode + ": " + f.Summary)
	}
	if status == "failed" {
		return "attempt failed: recorded error unavailable"
	}
	return ""
}
