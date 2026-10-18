package orchestrator

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	hydrationStarvationIssueThreshold = 10
	hydrationStarvationTickThreshold  = 3
)

func (o *Orchestrator) observePullRequestHydrationSkips(issues []connector.Issue) {
	if o == nil {
		return
	}
	current := make(map[string]struct{})
	for _, issue := range issues {
		if issue.PullRequest == nil || strings.TrimSpace(issue.PullRequest.HydrationUnavailableReason) == "" {
			continue
		}
		identifier := strings.TrimSpace(issue.Identifier)
		if identifier == "" {
			identifier = strings.TrimSpace(issue.ID)
		}
		if identifier != "" {
			current[identifier] = struct{}{}
		}
	}

	if o.hydrationSkipStreaks == nil {
		o.hydrationSkipStreaks = make(map[string]int)
	}
	for identifier := range o.hydrationSkipStreaks {
		if _, ok := current[identifier]; !ok {
			delete(o.hydrationSkipStreaks, identifier)
		}
	}
	for identifier := range current {
		o.hydrationSkipStreaks[identifier]++
	}

	sustained := 0
	maxTicks := 0
	for _, ticks := range o.hydrationSkipStreaks {
		if ticks > maxTicks {
			maxTicks = ticks
		}
		if ticks > hydrationStarvationTickThreshold {
			sustained++
		}
	}
	starved := sustained > hydrationStarvationIssueThreshold
	if starved && !o.hydrationWarned && o.logger != nil {
		o.logger.Warn(
			"github pull request hydration starvation",
			"skipped_issue_count", sustained,
			"issue_count_threshold", hydrationStarvationIssueThreshold,
			"consecutive_tick_threshold", hydrationStarvationTickThreshold,
			"max_consecutive_ticks", maxTicks,
		)
	}
	o.hydrationWarned = starved
}
