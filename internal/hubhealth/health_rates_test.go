package hubhealth

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
)

func healthRateFixture(now time.Time) RateProject {
	p := RateProject{ID: "project", Costs: map[string]ItemCost{}}
	p.BaselineCosts = p.Costs
	for i := range 24 {
		at := now.Add(-time.Duration(7+6*i) * time.Hour)
		p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("baseline-", i), Item: fmt.Sprint("done-", i), Status: "succeeded", At: at, Landing: true, Landed: true, LandingAt: at})
		t := hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: fmt.Sprint("done-", i)}, State: "Done", Done: []time.Time{at}}
		for _, lane := range []string{"Todo", "In Progress", "Rework", "Merging"} {
			t.Spans = append(t.Spans, hubanalytics.ResidenceSpan{Lane: lane, From: at.Add(-20 * time.Minute), To: at})
		}
		p.Timelines = append(p.Timelines, t)
		p.Costs[t.Issue.ID] = ItemCost{Cost: 4, Known: true}
	}
	for i := range 24 {
		at := now.Add(-BaselineWindow - time.Duration(1+i*10)*time.Minute)
		p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("last-week-", i), Item: fmt.Sprint("prior-", i), Status: "succeeded", At: at, Landing: true, Landed: true, LandingAt: at})
	}
	return p
}

func healthRateSignals(now time.Time, p RateProject) []string {
	out := []string{}
	for _, f := range evaluateHealthRates(now, []RateProject{p}) {
		out = append(out, f.Signal)
	}
	slices.Sort(out)
	return out
}

func TestHealthRateSignals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	retry := func(p *RateProject, age time.Duration, n int) {
		for i := range n {
			p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("fail-", age, "-", i), Item: "active", Status: "failed", Signature: "protocol error", At: now.Add(-age), Cost: 2})
		}
	}
	mixedRetry := func(p *RateProject) {
		retry(p, 10*time.Minute, 5)
		for i := range 5 {
			p.Attempts[len(p.Attempts)-5+i].Signature = fmt.Sprint("failure-", i)
		}
	}
	conflict := func(p *RateProject, age time.Duration, n, failed int) {
		for i := range n {
			p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("landing-", age, "-", i), Item: fmt.Sprint("item-", age, "-", i), At: now.Add(-age), Landing: true, Conflict: i < failed, Landed: i >= failed, LandingAt: now.Add(-age)})
		}
	}
	queue := func(p *RateProject, short, long time.Duration) {
		p.Candidates = 1
		p.Runners = []Runner{{ID: "runner", Leases: 4, Heartbeat: now}}
		p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "queued"}, State: "Todo", Entered: now.Add(-short), Spans: []hubanalytics.ResidenceSpan{{Lane: "Todo", From: now.Add(-short), To: now}}})
		for i := range 10 {
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: fmt.Sprint("old-queue-", i)}, State: "Done", Spans: []hubanalytics.ResidenceSpan{{Lane: "Todo", From: now.Add(-2*time.Hour - long), To: now.Add(-2 * time.Hour)}}})
		}
	}
	for _, test := range []struct {
		name, signal, severity string
		fire, silent           func(*RateProject)
		absolute               bool
	}{
		{"same signature count", "retry_storm", "attention", func(p *RateProject) { retry(p, 10*time.Minute, 5) }, func(p *RateProject) { retry(p, 10*time.Minute, 4) }, true},
		{"retry ratio both windows", "retry_storm", "attention", func(p *RateProject) {
			mixedRetry(p)
			p.Attempts = append(p.Attempts, RateAttempt{ID: "success", Item: "active", Status: "succeeded", At: now.Add(-5 * time.Minute)})
		}, func(p *RateProject) {
			mixedRetry(p)
			for range 20 {
				p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Status: "succeeded", At: now.Add(-2 * time.Hour)})
			}
			p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Status: "succeeded", At: now.Add(-5 * time.Minute)})
		}, false},
		{"recovery reason count", "recovery_loop", "attention", func(p *RateProject) {
			for i := range 3 {
				p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("recovery-", i), Item: "active", Recovery: "checkpoint_unavailable", At: now.Add(-10 * time.Minute)})
			}
		}, func(p *RateProject) {
			for i := range 3 {
				p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Recovery: fmt.Sprint("reason-", i), At: now.Add(-10 * time.Minute)})
			}
		}, false},
		{"stalled heartbeat", "stalled_active_item", "attention", func(p *RateProject) {
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "active"}, State: "In Progress", Entered: now.Add(-3 * time.Hour)})
		}, func(p *RateProject) {
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "active"}, State: "In Progress", Entered: now.Add(-3 * time.Hour)})
			p.Attempts = append(p.Attempts, RateAttempt{ID: "alive", Item: "active", Heartbeat: now.Add(-time.Minute)})
		}, true},
		{"conflict both windows", "conflict_burn", "watch", func(p *RateProject) { conflict(p, 10*time.Minute, 10, 3); conflict(p, 2*time.Hour, 10, 3) }, func(p *RateProject) { conflict(p, 10*time.Minute, 10, 3); conflict(p, 2*time.Hour, 30, 0) }, false},
		{"queue both windows", "capacity_starvation", "watch", func(p *RateProject) { queue(p, 2*time.Hour, time.Hour) }, func(p *RateProject) { queue(p, 2*time.Hour, time.Minute) }, false},
		{"cost median and dollar floor", "cost_anomaly", "watch", func(p *RateProject) {
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "expensive"}, State: "Todo"})
			p.Costs["expensive"] = ItemCost{Cost: 30, Known: true}
		}, func(p *RateProject) {
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "expensive"}})
			p.Costs["expensive"] = ItemCost{Cost: 25, Known: true}
		}, false},
		{"throughput same weekday hours", "throughput_collapse", "watch", func(p *RateProject) { p.Candidates = 1 }, func(p *RateProject) {
			p.Candidates = 1
			for range 24 {
				p.Attempts = append(p.Attempts, RateAttempt{Landed: true, LandingAt: now.Add(-2 * time.Hour)})
			}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := healthRateFixture(now)
			test.fire(&p)
			findings := evaluateHealthRates(now, []RateProject{p})
			var finding *Finding
			for i := range findings {
				if findings[i].Signal == test.signal {
					finding = &findings[i]
				}
			}
			if finding == nil || finding.Severity != test.severity {
				t.Fatalf("missing %s: %+v", test.signal, findings)
			}
			repeated := evaluateHealthRates(now, []RateProject{p})
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
			noBaseline.Attempts = slices.DeleteFunc(slices.Clone(p.Attempts), func(a RateAttempt) bool { return a.At.Before(now.Add(-LongWindow)) })
			noBaseline.Timelines = slices.DeleteFunc(slices.Clone(p.Timelines), func(t hubanalytics.ResidenceTimeline) bool { return len(t.Done) > 0 })
			floor := slices.ContainsFunc(evaluateHealthRates(now, []RateProject{noBaseline}), func(f Finding) bool {
				return f.Signal == test.signal && f.Evidence.Reason == healthAbsoluteFloor
			})
			if fired := slices.Contains(healthRateSignals(now, noBaseline), test.signal); fired != test.absolute || fired != floor {
				t.Fatalf("missing baseline fired %s=%v floor=%v want %v", test.signal, fired, floor, test.absolute)
			}
			gaps := BaselineGaps(noBaseline, BuildRateBaseline(now, noBaseline))
			if !slices.ContainsFunc(gaps, func(g BaselineUnavailable) bool { return g.Signal == test.signal }) {
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
	for i := range 116 {
		item := fmt.Sprint("storm-", i%2)
		p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("failure-", i), Item: item, Status: "failed", Signature: "protocol error", At: now.Add(-time.Duration(i*75/116+1) * time.Minute), Cost: 0.25})
	}
	for i := range 3 {
		p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("recovery-", i), Item: "recovery", Recovery: "checkpoint_unavailable", At: now.Add(-time.Duration(i+1) * 5 * time.Minute)})
	}
	for _, lane := range []string{"In Progress", "Rework", "Merging"} {
		p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "stalled-" + lane}, State: lane, Entered: now.Add(-4 * time.Hour)})
	}
	for i := range 116 {
		p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("landing-", i), Item: fmt.Sprint("landing-item-", i), Landing: true, Conflict: i < 34, Landed: i >= 34, LandingAt: now.Add(-time.Duration(i*75/116+1) * time.Minute)})
	}
	p.Candidates = 12
	p.Runners = []Runner{{ID: "runner", Leases: 4, Heartbeat: now}}
	for i := range 12 {
		p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: fmt.Sprint("queue-", i)}, State: "Todo", Entered: now.Add(-3 * time.Hour), Spans: []hubanalytics.ResidenceSpan{{Lane: "Todo", From: now.Add(-3 * time.Hour), To: now}}})
	}
	p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "costly"}, State: "Todo"})
	p.Costs["costly"] = ItemCost{Cost: 45, Known: true}
	findings := Evaluate(now, Snapshot{Rates: []RateProject{p}})
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
	failing := func(p *RateProject, at time.Time, n int) {
		for range n {
			p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Status: "failed", Signature: "failure", At: at})
		}
	}
	landing := func(p *RateProject, at time.Time, n, bad int) {
		for i := range n {
			p.Attempts = append(p.Attempts, RateAttempt{Landing: true, Landed: i >= bad, Conflict: i < bad, LandingAt: at})
		}
	}
	stalled := func(p *RateProject, gap time.Duration) {
		p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "active"}, State: "Merging", Entered: now.Add(-gap)})
	}
	for _, test := range []struct {
		name, signal string
		change       func(*RateProject)
		want         bool
	}{
		{"retry exact hour boundary", "retry_storm", func(p *RateProject) { failing(p, now.Add(-time.Hour), 5) }, true},
		{"retry outside hour boundary", "retry_storm", func(p *RateProject) { failing(p, now.Add(-time.Hour-time.Nanosecond), 5) }, false},
		{"retry future failures", "retry_storm", func(p *RateProject) { failing(p, now.Add(time.Minute), 5) }, false},
		{"retry long breach alone", "retry_storm", func(p *RateProject) {
			failing(p, now.Add(-2*time.Hour), 2)
			p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Status: "succeeded", At: now.Add(-time.Minute)})
		}, false},
		{"retry ratio below repetition threshold", "retry_storm", func(p *RateProject) {
			failing(p, now.Add(-time.Minute), 4)
			p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Status: "succeeded", At: now.Add(-2 * time.Minute)})
		}, false},
		{"retry usual repeated failures", "retry_storm", func(p *RateProject) {
			for _, a := range slices.Clone(p.Attempts[:24]) {
				for range 5 {
					p.Attempts = append(p.Attempts, RateAttempt{Item: a.Item, Status: "failed", Signature: "failure", At: a.At})
				}
			}
			failing(p, now.Add(-time.Minute), 5)
		}, false},
		{"recovery exact thirty minutes", "recovery_loop", func(p *RateProject) {
			for range 3 {
				p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Recovery: "checkpoint_unavailable", At: now.Add(-30 * time.Minute)})
			}
		}, true},
		{"recovery expired", "recovery_loop", func(p *RateProject) {
			for range 3 {
				p.Attempts = append(p.Attempts, RateAttempt{Item: "active", Recovery: "checkpoint_unavailable", At: now.Add(-30*time.Minute - time.Nanosecond)})
			}
		}, false},
		{"stall exact minimum", "stalled_active_item", func(p *RateProject) { stalled(p, 2*time.Hour) }, false},
		{"stall just above minimum", "stalled_active_item", func(p *RateProject) { stalled(p, 2*time.Hour+time.Nanosecond) }, true},
		{"stall lane p90", "stalled_active_item", func(p *RateProject) {
			for i := range p.Timelines {
				for j := range p.Timelines[i].Spans {
					if p.Timelines[i].Spans[j].Lane == "Merging" {
						p.Timelines[i].Spans[j].From = p.Timelines[i].Spans[j].To.Add(-time.Hour)
					}
				}
			}
			stalled(p, 3*time.Hour)
		}, false},
		{"stall maximum clamps long history", "stalled_active_item", func(p *RateProject) {
			for i := range p.Timelines {
				for j := range p.Timelines[i].Spans {
					if p.Timelines[i].Spans[j].Lane == "Merging" {
						p.Timelines[i].Spans[j].From = p.Timelines[i].Spans[j].To.Add(-10 * time.Hour)
					}
				}
			}
			stalled(p, 13*time.Hour)
		}, true},
		{"conflict exact short floor", "conflict_burn", func(p *RateProject) { landing(p, now.Add(-time.Minute), 10, 2) }, false},
		{"conflict exact long floor", "conflict_burn", func(p *RateProject) {
			landing(p, now.Add(-time.Minute), 10, 3)
			landing(p, now.Add(-2*time.Hour), 20, 0)
		}, false},
		{"conflict long breach alone", "conflict_burn", func(p *RateProject) {
			landing(p, now.Add(-time.Minute), 10, 1)
			landing(p, now.Add(-2*time.Hour), 10, 4)
		}, false},
		{"conflict minimum landings", "conflict_burn", func(p *RateProject) { landing(p, now.Add(-time.Minute), 4, 4) }, false},
		{"conflict usual high rate", "conflict_burn", func(p *RateProject) {
			for i := range p.Attempts {
				p.Attempts[i].Conflict = true
			}
			landing(p, now.Add(-time.Minute), 10, 3)
		}, false},
		{"cost exceeds dollars alone", "cost_anomaly", func(p *RateProject) {
			for id := range p.Costs {
				p.Costs[id] = ItemCost{Cost: 10, Known: true}
			}
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "active"}})
			p.Costs["active"] = ItemCost{Cost: 45, Known: true}
		}, false},
		{"cost unknown usage", "cost_anomaly", func(p *RateProject) {
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "active"}})
			p.Costs["active"] = ItemCost{Cost: 100}
		}, false},
		{"throughput short recovered", "throughput_collapse", func(p *RateProject) { p.Candidates = 1; landing(p, now.Add(-time.Minute), 2, 0) }, false},
		{"throughput no ready work", "throughput_collapse", func(p *RateProject) {}, false},
		{"retry ceiling above usual repeated failures", "retry_storm", func(p *RateProject) {
			for _, a := range slices.Clone(p.Attempts[:24]) {
				for range healthRetryCeiling {
					p.Attempts = append(p.Attempts, RateAttempt{Item: a.Item, Status: "failed", Signature: "failure", At: a.At})
				}
			}
			failing(p, now.Add(-time.Minute), healthRetryCeiling)
		}, true},
		{"retry floor without history", "retry_storm", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			failing(p, now.Add(-time.Minute), healthRetryCount)
		}, true},
		{"retry floor below count without history", "retry_storm", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			failing(p, now.Add(-time.Minute), healthRetryCount-1)
		}, false},
		{"stall floor without history", "stalled_active_item", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			stalled(p, healthStallMinimum+time.Nanosecond)
		}, true},
		{"stall floor exact minimum without history", "stalled_active_item", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			stalled(p, healthStallMinimum)
		}, false},
		{"throughput floor waiting work without landings", "throughput_collapse", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "gated"}, State: "Todo", Entered: now.Add(-LongWindow)})
		}, true},
		{"throughput floor recent waiting work", "throughput_collapse", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "gated"}, State: "Todo", Entered: now.Add(-LongWindow + time.Nanosecond)})
		}, false},
		{"throughput floor parked work", "throughput_collapse", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "held"}, State: "Blocked", Entered: now.Add(-24 * time.Hour)})
		}, false},
		{"throughput floor recent landing", "throughput_collapse", func(p *RateProject) {
			*p = RateProject{ID: "project"}
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "gated"}, State: "Todo", Entered: now.Add(-24 * time.Hour)})
			landing(p, now.Add(-time.Hour), 1, 0)
		}, false},
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
		failing(&p, now.Add(-time.Minute), healthRetryCount-1)
		b := BuildRateBaseline(now, p)
		if gaps := BaselineGaps(p, b); len(gaps) != 9 || len(evaluateHealthRates(now, []RateProject{p})) != 0 {
			t.Fatalf("nineteen samples not silent: %+v", gaps)
		}
	})
	t.Run("shared runner preserves every project queue", func(t *testing.T) {
		projects := []RateProject{}
		for _, id := range []string{"first", "second"} {
			p := healthRateFixture(now)
			p.ID = id
			p.Candidates = 2
			p.Runners = []Runner{{ID: "runner", Leases: 4, Heartbeat: now}}
			p.Timelines = append(p.Timelines, hubanalytics.ResidenceTimeline{Issue: hubanalytics.ResidenceIssue{ID: "queued"}, State: "Todo", Spans: []hubanalytics.ResidenceSpan{{Lane: "Todo", From: now.Add(-2 * time.Hour), To: now}}})
			projects = append(projects, p)
		}
		findings := slices.DeleteFunc(Evaluate(now, Snapshot{Rates: projects}), func(f Finding) bool { return f.Signal != "capacity_starvation" })
		if len(findings) != 1 || len(findings[0].Projects) != 2 || len(findings[0].Evidence.Queues) != 2 || findings[0].Evidence.Counts["queue_depth"] != 2 {
			t.Fatalf("lost shared-runner queues: %+v", findings)
		}
	})
}
