package changeattribution

import (
	"fmt"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Slot struct {
	Value    *string
	Evidence **failureattribution.Evidence
}

func ReviewSlots(review *tracker.ChangeReview) map[string][]Slot {
	slots := make(map[string][]Slot)
	for role, result := range map[string]*gate.ValidatorResult{"validator": review.Validator, "implementation_review": review.ImplementationReview} {
		if result == nil {
			continue
		}
		gate.NormalizeAttribution(result)
		list := []Slot{{Value: &result.Attribution}}
		for i := range result.Findings {
			f := &result.Findings[i]
			list = append(list, Slot{&f.Attribution, &f.AttributionEvidence})
		}
		slots[role] = list
		for i := range result.Commands {
			CommandSlots(slots, fmt.Sprintf("%s.command.%d", role, i), &result.Commands[i])
		}
	}
	if review.SecurityAudit != nil {
		run := &review.SecurityAudit.Run
		securityaudit.NormalizeAttribution(run)
		list := []Slot{{Value: &run.Attribution}}
		for i := range run.Findings {
			f := &run.Findings[i]
			list = append(list, Slot{&f.Attribution, &f.AttributionEvidence})
		}
		slots["security_audit"] = list
	}
	return slots
}

func CommandSlots(slots map[string][]Slot, role string, command *gate.CommandResult) {
	if command.Attribution == "" {
		command.Attribution = failureattribution.Unknown
	}
	list := []Slot{{Value: &command.Attribution}}
	for i := range command.Findings {
		f := &command.Findings[i]
		if f.Attribution == "" {
			f.Attribution = failureattribution.Unknown
		}
		list = append(list, Slot{&f.Attribution, &f.AttributionEvidence})
	}
	slots[role] = list
}
