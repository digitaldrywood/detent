package onboardingpage

import (
	"slices"

	"github.com/digitaldrywood/detent/internal/onboarding"
)

func Summary(value onboarding.Project) onboarding.Project {
	if value.Policy != nil {
		approval := *value.Policy
		approval.Policy.Configuration = nil
		approval.ExecutionConfiguration = nil
		approval.History = nil
		value.Policy = &approval
	}
	value.ObservedPolicies = slices.Clone(value.ObservedPolicies)
	for i := range value.ObservedPolicies {
		value.ObservedPolicies[i].Policy.Configuration = nil
	}
	return value
}
