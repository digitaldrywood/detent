package templates

import (
	"maps"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RunnerFleetData struct {
	ManagementToken string
	Fleet           runnerauth.Fleet
	SelectedRunner  string
	Eligibility     *runnerauth.ProjectEligibility
	Error           string
}

func (d RunnerFleetData) projectNames() []string { return slices.Sorted(maps.Keys(d.Fleet.Projects)) }

func (d RunnerFleetData) exclusions(id string) []runnerauth.Exclusion {
	if d.Eligibility != nil {
		for _, row := range d.Eligibility.Runners {
			if row.Runner.RunnerID == id {
				return row.Exclusions
			}
		}
	}
	return nil
}

func runnerProjectIDs(ids []tracker.ProjectID) string {
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = string(id)
	}
	return strings.Join(values, ", ")
}

func runnerRequirement(value, empty string) string {
	if value == "" {
		return empty
	}
	return value
}

func runnerIsolationLabel(tier string) string {
	if tier == "native-trusted" {
		return "Trusted only: full host access"
	}
	return "Sandbox"
}

func runnerAvailabilityLabel(availability runnerauth.Availability) string {
	if len(availability.Windows) == 0 {
		return "Always available"
	}
	return strings.Join(availability.Windows, ", ") + " (" + availability.Timezone + ")"
}
