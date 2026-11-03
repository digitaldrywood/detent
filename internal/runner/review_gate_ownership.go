package runner

import (
	"context"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) reviewGateOwner(ctx context.Context, req ValidatorRequest, workflow config.Workflow, runWorkspace workspace.Backend, info workspace.Info, issue workspace.Issue) string {
	v := req.NativeVersion
	if v == nil || v.ID == "" || v.ChangeID == "" || v.PolicyID == "" || v.Policy.ID != v.PolicyID || !v.Policy.Gates.Validator || v.Source == nil || v.RunID == "" || v.AttemptID == "" {
		return ""
	}
	if _, ok := req.usageExecution.(NativeReviewExecution); !ok {
		return ""
	}
	if _, ok := req.usageExecution.(NativeValidatorExecution); !ok {
		return ""
	}
	if v.Source.BaseSHA != v.BaseSHA || v.Source.HeadSHA != v.HeadSHA || v.Source.DiffSHA256 != req.Diff.Digest {
		return ""
	}
	approved, err := config.ResolveNativePolicy(v.Policy)
	if err != nil {
		return ""
	}
	frozen, err := approved.Workflow()
	if err != nil {
		return ""
	}
	cfg := gate.Effective(frozen.Config.Gate)
	current := gate.Effective(workflow.Config.Gate)
	if cfg.Kind != gate.KindCommand || cfg.Run == "" || !cfg.Validator.Enabled || cfg.AutomatedReview != gate.AutomatedReviewRequired || current.Kind != cfg.Kind || current.Run != cfg.Run || !current.Validator.Enabled {
		return ""
	}
	_, runtime, _, _ := r.runtimeSnapshot()
	review, _, _, err := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), RoleCode)
	if err != nil {
		return ""
	}
	validator, _, _, err := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), RoleValidator)
	if err != nil || review.RouteName == "" || validator.RouteName == "" || review.RouteName == validator.RouteName {
		return ""
	}
	receipt := v.Validation
	if !workspace.ValidationCoversHead(receipt, cfg.Run, v.HeadSHA) || receipt.ReceiptID == "" || receipt.TreeSHA == "" || receipt.Stage != gate.StageSourceFinalization || receipt.Execution != "executed" || receipt.ReusedReceiptID != "" || receipt.StartedAt.IsZero() || receipt.Evidence == nil || !receipt.Evidence.Valid() {
		return ""
	}
	stat, err := runWorkspace.DiffStat(ctx, info, issue)
	if err != nil || stat.HeadSHA != v.HeadSHA || stat.TreeSHA != receipt.TreeSHA || !stat.IsEmpty() {
		return ""
	}
	seen := map[string]bool{receipt.ReceiptID: true}
	for _, later := range req.Commands {
		if later.ReceiptID != "" && seen[later.ReceiptID] {
			continue
		}
		if later.ReceiptID != "" {
			seen[later.ReceiptID] = true
		}
		if later.Command == cfg.Run && later.HeadSHA == v.HeadSHA && later.TreeSHA == receipt.TreeSHA && later.ExitCode != 0 {
			return ""
		}
	}
	return receipt.ReceiptID
}
