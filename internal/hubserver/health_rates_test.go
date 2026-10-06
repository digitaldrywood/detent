package hubserver

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

func healthRateFixture(now time.Time) healthRateProject {
	p := healthRateProject{ID: "project", Costs: map[string]healthItemCost{}}
	p.BaselineCosts = p.Costs
	for i := 0; i < 24; i++ {
		at := now.Add(-time.Duration(7+6*i) * time.Hour)
		p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("baseline-", i), Item: fmt.Sprint("done-", i), Status: "succeeded", At: at, Landing: true, Landed: true, LandingAt: at})
		t := analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: fmt.Sprint("done-", i)}, state: "Done", done: []time.Time{at}}
		for _, lane := range []string{"Todo", "In Progress", "Rework", "Merging"} {
			t.spans = append(t.spans, analyticsResidenceSpan{lane: lane, from: at.Add(-20 * time.Minute), to: at})
		}
		p.Timelines = append(p.Timelines, t)
		p.Costs[t.issue.id] = healthItemCost{Cost: 4, Known: true}
	}
	for i := 0; i < 24; i++ {
		at := now.Add(-healthBaselineWindow - time.Duration(1+i*10)*time.Minute)
		p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("last-week-", i), Item: fmt.Sprint("prior-", i), Status: "succeeded", At: at, Landing: true, Landed: true, LandingAt: at})
	}
	return p
}

func healthRateSignals(now time.Time, p healthRateProject) []string {
	out := []string{}
	for _, f := range evaluateHealthRates(now, []healthRateProject{p}) {
		out = append(out, f.Signal)
	}
	slices.Sort(out)
	return out
}

func TestHealthRateSignals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	retry := func(p *healthRateProject, age time.Duration, n int) {
		for i := 0; i < n; i++ {
			p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("fail-", age, "-", i), Item: "active", Status: "failed", Signature: "protocol error", At: now.Add(-age), Cost: 2})
		}
	}
	conflict := func(p *healthRateProject, age time.Duration, n, failed int) {
		for i := 0; i < n; i++ {
			p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("landing-", age, "-", i), Item: fmt.Sprint("item-", age, "-", i), At: now.Add(-age), Landing: true, Conflict: i < failed, Landed: i >= failed, LandingAt: now.Add(-age)})
		}
	}
	queue := func(p *healthRateProject, short, long time.Duration) {
		p.Candidates = 1
		p.Runners = []healthRunner{{ID: "runner", Leases: 4, Heartbeat: now}}
		p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "queued"}, state: "Todo", entered: now.Add(-short), spans: []analyticsResidenceSpan{{lane: "Todo", from: now.Add(-short), to: now}}})
		for i := 0; i < 10; i++ {
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: fmt.Sprint("old-queue-", i)}, state: "Done", spans: []analyticsResidenceSpan{{lane: "Todo", from: now.Add(-2*time.Hour - long), to: now.Add(-2 * time.Hour)}}})
		}
	}
	for _, test := range []struct {
		name, signal, severity string
		fire, silent           func(*healthRateProject)
	}{
		{"same signature count", "retry_storm", "attention", func(p *healthRateProject) { retry(p, 10*time.Minute, 5) }, func(p *healthRateProject) { retry(p, 10*time.Minute, 4) }},
		{"retry ratio both windows", "retry_storm", "attention", func(p *healthRateProject) {
			retry(p, 10*time.Minute, 2)
			p.Attempts = append(p.Attempts, healthRateAttempt{ID: "success", Item: "active", Status: "succeeded", At: now.Add(-5 * time.Minute)})
		}, func(p *healthRateProject) {
			retry(p, 10*time.Minute, 2)
			for i := 0; i < 10; i++ {
				p.Attempts = append(p.Attempts, healthRateAttempt{Item: "active", Status: "succeeded", At: now.Add(-2 * time.Hour)})
			}
			p.Attempts = append(p.Attempts, healthRateAttempt{Item: "active", Status: "succeeded", At: now.Add(-5 * time.Minute)})
		}},
		{"recovery reason count", "recovery_loop", "attention", func(p *healthRateProject) {
			for i := 0; i < 3; i++ {
				p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("recovery-", i), Item: "active", Recovery: "checkpoint_unavailable", At: now.Add(-10 * time.Minute)})
			}
		}, func(p *healthRateProject) {
			for i := 0; i < 3; i++ {
				p.Attempts = append(p.Attempts, healthRateAttempt{Item: "active", Recovery: fmt.Sprint("reason-", i), At: now.Add(-10 * time.Minute)})
			}
		}},
		{"stalled heartbeat", "stalled_active_item", "attention", func(p *healthRateProject) {
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "active"}, state: "In Progress", entered: now.Add(-3 * time.Hour)})
		}, func(p *healthRateProject) {
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "active"}, state: "In Progress", entered: now.Add(-3 * time.Hour)})
			p.Attempts = append(p.Attempts, healthRateAttempt{ID: "alive", Item: "active", Heartbeat: now.Add(-time.Minute)})
		}},
		{"conflict both windows", "conflict_burn", "watch", func(p *healthRateProject) { conflict(p, 10*time.Minute, 10, 3); conflict(p, 2*time.Hour, 10, 3) }, func(p *healthRateProject) { conflict(p, 10*time.Minute, 10, 3); conflict(p, 2*time.Hour, 30, 0) }},
		{"queue both windows", "capacity_starvation", "watch", func(p *healthRateProject) { queue(p, 2*time.Hour, time.Hour) }, func(p *healthRateProject) { queue(p, 2*time.Hour, time.Minute) }},
		{"cost median and dollar floor", "cost_anomaly", "watch", func(p *healthRateProject) {
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "expensive"}, state: "Todo"})
			p.Costs["expensive"] = healthItemCost{Cost: 30, Known: true}
		}, func(p *healthRateProject) {
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "expensive"}})
			p.Costs["expensive"] = healthItemCost{Cost: 25, Known: true}
		}},
		{"throughput same weekday hours", "throughput_collapse", "watch", func(p *healthRateProject) { p.Candidates = 1 }, func(p *healthRateProject) {
			p.Candidates = 1
			for i := 0; i < 24; i++ {
				p.Attempts = append(p.Attempts, healthRateAttempt{Landed: true, LandingAt: now.Add(-2 * time.Hour)})
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := healthRateFixture(now)
			test.fire(&p)
			findings := evaluateHealthRates(now, []healthRateProject{p})
			var finding *healthFinding
			for i := range findings {
				if findings[i].Signal == test.signal {
					finding = &findings[i]
				}
			}
			if finding == nil || finding.Severity != test.severity {
				t.Fatalf("missing %s: %+v", test.signal, findings)
			}
			repeated := evaluateHealthRates(now, []healthRateProject{p})
			for _, f := range repeated {
				if f.Signal == test.signal && f.Fingerprint != finding.Fingerprint {
					t.Fatal("unstable fingerprint")
				}
			}
			silent := healthRateFixture(now)
			test.silent(&silent)
			if slices.Contains(healthRateSignals(now, silent), test.signal) {
				t.Fatalf("isolated breach fired %s", test.signal)
			}
			noBaseline := p
			noBaseline.Attempts = slices.DeleteFunc(slices.Clone(p.Attempts), func(a healthRateAttempt) bool { return a.At.Before(now.Add(-healthLongWindow)) })
			noBaseline.Timelines = slices.DeleteFunc(slices.Clone(p.Timelines), func(t analyticsResidenceTimeline) bool { return len(t.done) > 0 })
			if slices.Contains(healthRateSignals(now, noBaseline), test.signal) {
				t.Fatalf("missing baseline fired %s", test.signal)
			}
			gaps := healthBaselineGaps(noBaseline, buildHealthRateBaseline(now, noBaseline))
			if !slices.ContainsFunc(gaps, func(g healthBaselineUnavailable) bool { return g.Signal == test.signal }) {
				t.Fatalf("missing unavailable marker for %s", test.signal)
			}
			recovered := healthRateFixture(now)
			if slices.Contains(healthRateSignals(now, recovered), test.signal) {
				t.Fatalf("recovered signal remains open: %s", test.signal)
			}
		})
	}
}

func TestHealthRateIncidentReplay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	p := healthRateFixture(now)
	for i := 0; i < 116; i++ {
		item := fmt.Sprint("storm-", i%2)
		p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("failure-", i), Item: item, Status: "failed", Signature: "protocol error", At: now.Add(-time.Duration(i*75/116+1) * time.Minute), Cost: 0.25})
	}
	for i := 0; i < 3; i++ {
		p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("recovery-", i), Item: "recovery", Recovery: "checkpoint_unavailable", At: now.Add(-time.Duration(i+1) * 5 * time.Minute)})
	}
	for _, lane := range []string{"In Progress", "Rework", "Merging"} {
		p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "stalled-" + lane}, state: lane, entered: now.Add(-4 * time.Hour)})
	}
	for i := 0; i < 116; i++ {
		p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("landing-", i), Item: fmt.Sprint("landing-item-", i), Landing: true, Conflict: i < 34, Landed: i >= 34, LandingAt: now.Add(-time.Duration(i*75/116+1) * time.Minute)})
	}
	p.Candidates = 12
	p.Runners = []healthRunner{{ID: "runner", Leases: 4, Heartbeat: now}}
	for i := 0; i < 12; i++ {
		p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: fmt.Sprint("queue-", i)}, state: "Todo", entered: now.Add(-3 * time.Hour), spans: []analyticsResidenceSpan{{lane: "Todo", from: now.Add(-3 * time.Hour), to: now}}})
	}
	p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "costly"}, state: "Todo"})
	p.Costs["costly"] = healthItemCost{Cost: 45, Known: true}
	findings := evaluateHealth(now, healthSnapshot{Rates: []healthRateProject{p}})
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.Signal]++
		if f.Signal == "retry_storm" && (len(f.Evidence.AttemptIDs) != 20 || len(f.Evidence.Signatures) != 1 || f.Evidence.Values["wasted_cost_usd"] != 14.5) {
			t.Fatalf("storm evidence=%+v", f.Evidence)
		}
	}
	want := map[string]int{"retry_storm": 2, "recovery_loop": 1, "stalled_active_item": 3, "conflict_burn": 1, "capacity_starvation": 1, "cost_anomaly": 1}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("replay findings=%v want %v", counts, want)
	}
}

func TestHealthRateBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	failing := func(p *healthRateProject, at time.Time, n int) {
		for i := 0; i < n; i++ {
			p.Attempts = append(p.Attempts, healthRateAttempt{Item: "active", Status: "failed", Signature: "failure", At: at})
		}
	}
	landing := func(p *healthRateProject, at time.Time, n, bad int) {
		for i := 0; i < n; i++ {
			p.Attempts = append(p.Attempts, healthRateAttempt{Landing: true, Landed: i >= bad, Conflict: i < bad, LandingAt: at})
		}
	}
	stalled := func(p *healthRateProject, gap time.Duration) {
		p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "active"}, state: "Merging", entered: now.Add(-gap)})
	}
	for _, test := range []struct {
		name, signal string
		change       func(*healthRateProject)
		want         bool
	}{
		{"retry exact hour boundary", "retry_storm", func(p *healthRateProject) { failing(p, now.Add(-time.Hour), 5) }, true},
		{"retry outside hour boundary", "retry_storm", func(p *healthRateProject) { failing(p, now.Add(-time.Hour-time.Nanosecond), 5) }, false},
		{"retry future failures", "retry_storm", func(p *healthRateProject) { failing(p, now.Add(time.Minute), 5) }, false},
		{"retry long breach alone", "retry_storm", func(p *healthRateProject) {
			failing(p, now.Add(-2*time.Hour), 2)
			p.Attempts = append(p.Attempts, healthRateAttempt{Item: "active", Status: "succeeded", At: now.Add(-time.Minute)})
		}, false},
		{"retry usual repeated failures", "retry_storm", func(p *healthRateProject) {
			for _, a := range slices.Clone(p.Attempts[:24]) {
				for i := 0; i < 5; i++ {
					p.Attempts = append(p.Attempts, healthRateAttempt{Item: a.Item, Status: "failed", Signature: "failure", At: a.At})
				}
			}
			failing(p, now.Add(-time.Minute), 5)
		}, false},
		{"recovery exact thirty minutes", "recovery_loop", func(p *healthRateProject) {
			for i := 0; i < 3; i++ {
				p.Attempts = append(p.Attempts, healthRateAttempt{Item: "active", Recovery: "checkpoint_unavailable", At: now.Add(-30 * time.Minute)})
			}
		}, true},
		{"recovery expired", "recovery_loop", func(p *healthRateProject) {
			for i := 0; i < 3; i++ {
				p.Attempts = append(p.Attempts, healthRateAttempt{Item: "active", Recovery: "checkpoint_unavailable", At: now.Add(-30*time.Minute - time.Nanosecond)})
			}
		}, false},
		{"stall exact minimum", "stalled_active_item", func(p *healthRateProject) { stalled(p, 2*time.Hour) }, false},
		{"stall just above minimum", "stalled_active_item", func(p *healthRateProject) { stalled(p, 2*time.Hour+time.Nanosecond) }, true},
		{"stall lane p90", "stalled_active_item", func(p *healthRateProject) {
			for i := range p.Timelines {
				for j := range p.Timelines[i].spans {
					if p.Timelines[i].spans[j].lane == "Merging" {
						p.Timelines[i].spans[j].from = p.Timelines[i].spans[j].to.Add(-time.Hour)
					}
				}
			}
			stalled(p, 3*time.Hour)
		}, false},
		{"stall maximum clamps long history", "stalled_active_item", func(p *healthRateProject) {
			for i := range p.Timelines {
				for j := range p.Timelines[i].spans {
					if p.Timelines[i].spans[j].lane == "Merging" {
						p.Timelines[i].spans[j].from = p.Timelines[i].spans[j].to.Add(-10 * time.Hour)
					}
				}
			}
			stalled(p, 13*time.Hour)
		}, true},
		{"conflict exact short floor", "conflict_burn", func(p *healthRateProject) { landing(p, now.Add(-time.Minute), 10, 2) }, false},
		{"conflict exact long floor", "conflict_burn", func(p *healthRateProject) {
			landing(p, now.Add(-time.Minute), 10, 3)
			landing(p, now.Add(-2*time.Hour), 20, 0)
		}, false},
		{"conflict long breach alone", "conflict_burn", func(p *healthRateProject) {
			landing(p, now.Add(-time.Minute), 10, 1)
			landing(p, now.Add(-2*time.Hour), 10, 4)
		}, false},
		{"conflict minimum landings", "conflict_burn", func(p *healthRateProject) { landing(p, now.Add(-time.Minute), 4, 4) }, false},
		{"conflict usual high rate", "conflict_burn", func(p *healthRateProject) {
			for i := range p.Attempts {
				p.Attempts[i].Conflict = true
			}
			landing(p, now.Add(-time.Minute), 10, 3)
		}, false},
		{"cost exceeds dollars alone", "cost_anomaly", func(p *healthRateProject) {
			for id := range p.Costs {
				p.Costs[id] = healthItemCost{Cost: 10, Known: true}
			}
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "active"}})
			p.Costs["active"] = healthItemCost{Cost: 45, Known: true}
		}, false},
		{"cost unknown usage", "cost_anomaly", func(p *healthRateProject) {
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "active"}})
			p.Costs["active"] = healthItemCost{Cost: 100}
		}, false},
		{"throughput short recovered", "throughput_collapse", func(p *healthRateProject) { p.Candidates = 1; landing(p, now.Add(-time.Minute), 2, 0) }, false},
		{"throughput no ready work", "throughput_collapse", func(p *healthRateProject) {}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := healthRateFixture(now)
			test.change(&p)
			if got := slices.Contains(healthRateSignals(now, p), test.signal); got != test.want {
				t.Fatalf("%s active=%v want %v", test.signal, got, test.want)
			}
		})
	}
	t.Run("nineteen samples stay silent", func(t *testing.T) {
		p := healthRateFixture(now)
		p.Attempts = slices.Clone(p.Attempts[:19])
		p.Timelines = p.Timelines[:19]
		failing(&p, now.Add(-time.Minute), 5)
		b := buildHealthRateBaseline(now, p)
		if gaps := healthBaselineGaps(p, b); len(gaps) != 9 || len(evaluateHealthRates(now, []healthRateProject{p})) != 0 {
			t.Fatalf("nineteen samples not silent: %+v", gaps)
		}
	})
	t.Run("shared runner preserves every project queue", func(t *testing.T) {
		projects := []healthRateProject{}
		for _, id := range []string{"first", "second"} {
			p := healthRateFixture(now)
			p.ID = id
			p.Candidates = 2
			p.Runners = []healthRunner{{ID: "runner", Leases: 4, Heartbeat: now}}
			p.Timelines = append(p.Timelines, analyticsResidenceTimeline{issue: analyticsResidenceIssue{id: "queued"}, state: "Todo", spans: []analyticsResidenceSpan{{lane: "Todo", from: now.Add(-2 * time.Hour), to: now}}})
			projects = append(projects, p)
		}
		findings := slices.DeleteFunc(evaluateHealth(now, healthSnapshot{Rates: projects}), func(f healthFinding) bool { return f.Signal != "capacity_starvation" })
		if len(findings) != 1 || len(findings[0].Projects) != 2 || len(findings[0].Evidence.Queues) != 2 || findings[0].Evidence.Counts["queue_depth"] != 2 {
			t.Fatalf("lost shared-runner queues: %+v", findings)
		}
	})
}
