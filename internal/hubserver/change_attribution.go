package hubserver

import (
	"fmt"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attributionSlot struct {
	value    *string
	evidence **failureattribution.Evidence
}

func reviewAttributionSlots(review *tracker.ChangeReview) map[string][]attributionSlot {
	slots := make(map[string][]attributionSlot)
	for role, result := range map[string]*gate.ValidatorResult{"validator": review.Validator, "implementation_review": review.ImplementationReview} {
		if result == nil {
			continue
		}
		gate.NormalizeAttribution(result)
		list := []attributionSlot{{value: &result.Attribution}}
		for i := range result.Findings {
			f := &result.Findings[i]
			list = append(list, attributionSlot{&f.Attribution, &f.AttributionEvidence})
		}
		slots[role] = list
		for i := range result.Commands {
			commandAttributionSlots(slots, fmt.Sprintf("%s.command.%d", role, i), &result.Commands[i])
		}
	}
	if review.SecurityAudit != nil {
		run := &review.SecurityAudit.Run
		securityaudit.NormalizeAttribution(run)
		list := []attributionSlot{{value: &run.Attribution}}
		for i := range run.Findings {
			f := &run.Findings[i]
			list = append(list, attributionSlot{&f.Attribution, &f.AttributionEvidence})
		}
		slots["security_audit"] = list
	}
	return slots
}

func commandAttributionSlots(slots map[string][]attributionSlot, role string, command *gate.CommandResult) {
	if command.Attribution == "" {
		command.Attribution = failureattribution.Unknown
	}
	list := []attributionSlot{{value: &command.Attribution}}
	for i := range command.Findings {
		f := &command.Findings[i]
		if f.Attribution == "" {
			f.Attribution = failureattribution.Unknown
		}
		list = append(list, attributionSlot{&f.Attribution, &f.AttributionEvidence})
	}
	slots[role] = list
}
