package workload

import (
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
)

type Class string

const (
	ClassLocalHeavy Class = "local-heavy"
	ClassCloudOnly  Class = "cloud-only"
)

type Signals struct {
	LocalGate bool
	CITrigger bool
}

func Classify(cfg workflowconfig.Config) (Class, Signals) {
	signals := Signals{
		LocalGate: cfg.Gate.Kind == gate.KindCommand && strings.TrimSpace(cfg.Gate.Run) != "" ||
			cfg.Gate.Validator.Enabled,
		CITrigger: strings.TrimSpace(cfg.Gate.CITriggerLabel) != "" ||
			len(cfg.Gate.RequiredStatusChecks) > 0,
	}
	if signals.LocalGate || signals.CITrigger {
		return ClassLocalHeavy, signals
	}
	return ClassCloudOnly, signals
}
