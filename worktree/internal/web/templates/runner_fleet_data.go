package templates

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RunnerFleetData struct {
	ManagementToken string
	Fleet           runnerauth.Fleet
	SelectedRunner  string
	AttentionOnly   bool
	Eligibility     *runnerauth.ProjectEligibility
	Error           string
}

func (d RunnerFleetData) projectNames() []string { return slices.Sorted(maps.Keys(d.Fleet.Projects)) }

func (d RunnerFleetData) attentionCount() int {
	count := 0
	for _, runner := range d.Fleet.Runners {
		if runner.Health == "needs_attention" {
			count++
		}
	}
	return count
}

func (d RunnerFleetData) visible(r runnerauth.Runner) bool {
	return (d.SelectedRunner == "" || d.SelectedRunner == r.RunnerID) && (!d.AttentionOnly || r.Health == "needs_attention")
}

func runnerAttentionVerb(count int) string {
	if count == 1 {
		return "runner needs"
	}
	return "runners need"
}

func runnerProblemRole(health string) string {
	if health == "needs_attention" {
		return "alert"
	}
	return "list"
}

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

func runnerHealthLabel(r runnerauth.Runner, now time.Time) string {
	switch status := r.Status(now); status {
	case "needs_attention":
		return "Needs attention"
	case "outside_hours":
		return "Outside hours"
	default:
		return status
	}
}
