package activity

import (
	"cmp"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type activityWorkInterval struct{ from, to time.Time }
type activityIssueAttempt struct {
	run                   tracker.NativeRunData
	started, ended        time.Time
	role, runner, session string
	live                  bool
}

func activityLaneRole(lane tracker.NativeState) string {
	if lane.Terminal {
		return "terminal"
	}
	if lane.Dispatchable {
		return "active"
	}
	return "holding"
}

func activityIssueResidence(issue operatortool.ActivityIssue, lanes []tracker.NativeState, samples []reportdata.ResidenceSample, now time.Time) operatortool.ActivityIssue {
	issue.Lanes = []operatortool.ActivityLane{}
	issue.RunnerIDs = []string{}
	issue.WorkedByRole = []operatortool.ActivityRoleWork{}
	if len(samples) == 0 {
		issue.Partial = true
		for _, lane := range lanes {
			if activityLaneRole(lane) == "active" {
				issue.Lanes = append(issue.Lanes, operatortool.ActivityLane{Name: lane.Name, Role: "active"})
			}
		}
		return issue
	}
	slices.SortFunc(samples, func(a, b reportdata.ResidenceSample) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Sequence, b.Sequence))
	})
	firstActive := slices.IndexFunc(lanes, func(l tracker.NativeState) bool { return l.Dispatchable && !l.Terminal })
	durations := map[string]float64{}
	entries := map[string]int{}
	state := ""
	entered := time.Time{}
	known := samples[0].Sequence == 0
	visit := func(name string, at time.Time) {
		if name == state {
			return
		}
		if !issue.ClockStartedAt.IsZero() && !entered.IsZero() {
			durations[state] += max(0, at.Sub(entered).Seconds())
		}
		state, entered = name, at
		index := slices.IndexFunc(lanes, func(l tracker.NativeState) bool { return l.Name == name })
		if index < 0 {
			issue.Partial = true
			return
		}
		lane := lanes[index]
		if !lane.Dispatchable && !lane.Terminal && index < firstActive {
			known = true
			issue.ClockStartedAt = time.Time{}
			issue.ClockLane = ""
			issue.FinishedAt = nil
			issue.TerminalLane = ""
			durations = map[string]float64{}
			entries = map[string]int{}
			return
		}
		if known && issue.ClockStartedAt.IsZero() && lane.Dispatchable && !lane.Terminal {
			issue.ClockStartedAt = at
			issue.ClockLane = name
		}
		if !issue.ClockStartedAt.IsZero() {
			entries[name]++
			if lane.Terminal {
				end := at
				issue.FinishedAt = &end
				issue.TerminalLane = name
			}
			if !lane.Terminal {
				issue.FinishedAt = nil
				issue.TerminalLane = ""
			}
		}
	}
	first := samples[0]
	if first.Sequence == 0 {
		visit(first.Initial, first.Created)
	} else {
		issue.Partial = true
		visit(first.State, first.Entered)
	}
	for _, sample := range samples {
		if sample.At.After(now) {
			break
		}
		issue.Partial = issue.Partial || sample.Partial
		visit(sample.State, sample.Entered)
	}
	issue.CurrentLane = state
	end := now
	if issue.FinishedAt != nil {
		end = *issue.FinishedAt
	}
	if !issue.ClockStartedAt.IsZero() {
		durations[state] += max(0, end.Sub(entered).Seconds())
		issue.TotalSeconds = max(0, end.Sub(issue.ClockStartedAt).Seconds())
	}
	for _, lane := range lanes {
		role := activityLaneRole(lane)
		if role == "terminal" || role == "holding" && entries[lane.Name] == 0 {
			continue
		}
		issue.Lanes = append(issue.Lanes, operatortool.ActivityLane{Name: lane.Name, Role: role, Seconds: durations[lane.Name], Entries: entries[lane.Name], Current: lane.Name == state && issue.FinishedAt == nil})
	}
	return issue
}

func activityIntervalSeconds(intervals []activityWorkInterval) float64 {
	slices.SortFunc(intervals, func(a, b activityWorkInterval) int { return a.from.Compare(b.from) })
	var total float64
	var end time.Time
	for _, interval := range intervals {
		from := hubapi.MaxTime(interval.from, end)
		if interval.to.After(from) {
			total += interval.to.Sub(from).Seconds()
		}
		end = hubapi.MaxTime(end, interval.to)
	}
	return total
}

func activityIssueWork(issue *operatortool.ActivityIssue, attempts []activityIssueAttempt, now time.Time) bool {
	end := now
	if issue.FinishedAt != nil {
		end = *issue.FinishedAt
	}
	byRole := map[string][]activityWorkInterval{}
	roles := map[string]operatortool.ActivityRoleWork{}
	all := []activityWorkInterval{}
	live := false
	slices.SortFunc(attempts, func(a, b activityIssueAttempt) int { return a.started.Compare(b.started) })
	for _, attempt := range attempts {
		live = live || attempt.live
		if attempt.ended.Before(issue.ClockStartedAt) || !attempt.started.Before(end) {
			continue
		}
		if attempt.runner != "" && !slices.Contains(issue.RunnerIDs, attempt.runner) {
			issue.RunnerIDs = append(issue.RunnerIDs, attempt.runner)
		}
		if attempt.live || issue.RunnerID == "" || !live {
			issue.RunnerID = attempt.runner
			issue.SessionID = attempt.session
		}
		if attempt.run.Finalization != nil {
			if attempt.run.Finalization.ChangeID != "" {
				issue.ChangeID = attempt.run.Finalization.ChangeID
			}
			if publication := attempt.run.Finalization.Publication; publication != nil && publication.External.URL != "" {
				issue.PullRequestURL = publication.External.URL
			}
		}
		if issue.ClockStartedAt.IsZero() {
			continue
		}
		role := roles[attempt.role]
		role.Role = attempt.role
		role.Attempts++
		role.Running = role.Running || attempt.live
		roles[attempt.role] = role
		runtime := attempt.run.Runtime
		if runtime == nil || len(runtime.Phases) == 0 {
			issue.Partial = true
			continue
		}
		issue.Partial = issue.Partial || runtime.PhasesDropped > 0
		for _, phase := range runtime.Phases {
			to := phase.FinishedAt
			if to.IsZero() {
				issue.WorkedRunning = issue.WorkedRunning || attempt.live
				to = attempt.ended
			}
			from := hubapi.MaxTime(phase.StartedAt, issue.ClockStartedAt)
			to = hubapi.MinTime(to, hubapi.MinTime(end, attempt.ended))
			if !phase.StartedAt.IsZero() && to.After(from) {
				interval := activityWorkInterval{from, to}
				all = append(all, interval)
				byRole[attempt.role] = append(byRole[attempt.role], interval)
			}
		}
	}
	issue.WorkedSeconds = activityIntervalSeconds(all)
	for key, role := range roles {
		role.Seconds = activityIntervalSeconds(byRole[key])
		issue.WorkedByRole = append(issue.WorkedByRole, role)
	}
	slices.SortFunc(issue.WorkedByRole, func(a, b operatortool.ActivityRoleWork) int { return cmp.Compare(a.Role, b.Role) })
	return live
}
