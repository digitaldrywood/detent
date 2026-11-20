package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type sourceTurnBudgetKey struct{}

type sourceTurnBudget struct {
	remaining int
}

func sourceBudgetRemaining(ctx context.Context) int {
	if budget, ok := ctx.Value(sourceTurnBudgetKey{}).(*sourceTurnBudget); ok {
		return budget.remaining
	}
	return -1
}

func sourceBudgetExhaustedMessage(message string) string {
	return message + "\n\nThe shared implementation and review turn budget is exhausted; preserve the current source and review findings for Rework.\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
}

func completedSourceDisposition(message string) bool {
	disposition := NativeDispositionFromMessage(message)
	return disposition != nil && disposition.Status == "complete" && !disposition.Blockers && !disposition.HumanAction && disposition.ReasonCode == ""
}

func nativeSourceRework(change *NativeChange, message string) string {
	if !completedSourceDisposition(message) {
		return ""
	}
	if change == nil || change.Reviewed || change.VersionID == "" && change.NoChange == nil || change.Error != "" || change.VersionError != "" || change.ImplementationReviewError != "" || change.Landing != nil {
		return ""
	}
	version := change.VersionID
	if change.NoChange != nil {
		version = change.NoChange.VersionID()
	}
	var findings []string
	for _, result := range []*gate.ValidatorResult{change.ImplementationReview, change.Validator} {
		if result != nil && result.Verdict == gate.ValidatorVerdictRework && result.VersionID == version && result.HeadSHA == change.HeadSHA {
			body, err := json.Marshal(result)
			if err == nil {
				findings = append(findings, string(body))
			}
		}
	}
	if audit := change.SecurityAudit; audit != nil && audit.Verdict == "rework" && audit.VersionID == version && audit.Run.HeadSHA == change.HeadSHA {
		body, err := json.Marshal(audit)
		if err == nil {
			findings = append(findings, string(body))
		}
	}
	return strings.Join(findings, "\n\n")
}

type sourceGateRepair struct {
	err    error
	output string
}

func (e *sourceGateRepair) Error() string { return e.err.Error() }
func (e *sourceGateRepair) Unwrap() error { return e.err }

type PhaseReceiptExecution interface {
	PhaseReceipts(context.Context, string) (*tracker.NativePhaseContinuation, error)
	VerifyPhaseAuthority(context.Context) error
	RecordPhaseReceipt(context.Context, string, string, string, string, string) error
}

type itemPhaseWorkspace interface {
	PushItemPhase(context.Context, workspace.Info, workspace.Issue, func(context.Context) error) (string, string, string, error)
	ItemPhaseHead(context.Context, workspace.Info, workspace.Issue) (string, string, error)
	PreserveItemPhaseHistory(context.Context, workspace.Info, workspace.Issue, tracker.NativePhaseReceipt, func(context.Context) error) error
}

func publishItemPhase(ctx context.Context, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, phase, input string) error {
	if !req.phaseReceipts {
		return nil
	}
	publisher, ok := backend.(itemPhaseWorkspace)
	if !ok {
		return failedStep("publication", errors.New("workspace cannot publish an item phase"))
	}
	if carry := req.phaseContinuation; carry != nil {
		for _, receipt := range carry.Receipts {
			if receipt.Phase != phase {
				continue
			}
			head, tree, err := publisher.ItemPhaseHead(ctx, info, issue)
			if err != nil {
				return failedStep("publication", err)
			}
			if receipt.HeadSHA == head && receipt.TreeSHA == tree {
				return nil
			}
		}
	}
	execution := req.Execution.(PhaseReceiptExecution)
	branch, head, tree, err := publisher.PushItemPhase(ctx, info, issue, execution.VerifyPhaseAuthority)
	if err != nil {
		return failedStep("publication", err)
	}
	return failedStep("publication", execution.RecordPhaseReceipt(ctx, phase, branch, head, tree, input))
}

func publishReviewedPhase(ctx context.Context, req RunRequest, phase string, version *tracker.ChangeVersion) error {
	source := req.retainedReview
	if source == nil || !completedSourceDisposition(source.message) {
		return nil
	}
	identity := ""
	if version != nil {
		identity = version.ID
	}
	return publishItemPhase(ctx, req, source.backend, source.info, source.issue, phase, identity)
}
