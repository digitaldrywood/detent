package doctor

import (
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

const (
	reviewFlowAutopilot  = "autopilot"
	reviewFlowReviewGate = "review-gate"
)

func ReviewFlowChoice(cfg workflowconfig.Config) string {
	if cfg.Agent.AutoPromote.Enabled &&
		cfg.Agent.AutoPromote.QuietSeconds == 0 &&
		ReviewFlowGateWaitState(cfg) == workflowconfig.AutoPromoteGateWaitStateSource {
		return reviewFlowAutopilot
	}
	return reviewFlowReviewGate
}

func ReviewFlowGateWaitState(cfg workflowconfig.Config) string {
	state := strings.ToLower(strings.TrimSpace(cfg.Agent.AutoPromote.GateWaitState))
	if state == "" {
		return workflowconfig.AutoPromoteGateWaitStateSource
	}
	return state
}
