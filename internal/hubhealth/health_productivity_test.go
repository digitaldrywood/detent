package hubhealth

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
)

func TestHealthProductivityFindings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*RateProject)
		want   []string
	}{
		{name: "repeated failures", want: []string{"activity_without_delivery", "unproductive_attempts"}},
		{name: "landing clears project", mutate: func(p *RateProject) {
			p.Attempts = append(p.Attempts, RateAttempt{Landed: true, LandingAt: now})
		}, want: []string{"unproductive_attempts"}},
		{name: "Done clears project", mutate: func(p *RateProject) {
			p.Timelines = []hubanalytics.ResidenceTimeline{{Done: []time.Time{now.Add(-time.Minute)}}}
		}, want: []string{"unproductive_attempts"}},
		{name: "landing before window", mutate: func(p *RateProject) {
			p.Attempts = append(p.Attempts, RateAttempt{Landed: true, LandingAt: now.Add(-30*time.Minute - time.Nanosecond)})
		}, want: []string{"activity_without_delivery", "unproductive_attempts"}},
		{name: "landing at window start", mutate: func(p *RateProject) {
			p.Attempts = append(p.Attempts, RateAttempt{Landed: true, LandingAt: now.Add(-30 * time.Minute)})
		}, want: []string{"unproductive_attempts"}},
		{name: "changed signature breaks streak", mutate: func(p *RateProject) {
			p.Attempts[4].Signature = "different failure"
		}, want: []string{"activity_without_delivery"}},
		{name: "changed stage breaks streak", mutate: func(p *RateProject) {
			p.Attempts[4].Stage = "event_publication"
		}, want: []string{"activity_without_delivery"}},
		{name: "verified source clears streak", mutate: func(p *RateProject) {
			p.Attempts[4].SourceVersion = "version-new"
		}, want: []string{"activity_without_delivery"}},
		{name: "replayed source does not clear streak", mutate: func(p *RateProject) {
			first := p.Attempts[0]
			first.SourceVersion = "version-original"
			p.Attempts = append([]RateAttempt{first}, p.Attempts...)
			for i := range p.Attempts {
				p.Attempts[i].SourceVersion = "version-original"
			}
		}, want: []string{"activity_without_delivery", "unproductive_attempts"}},
		{name: "private stage is omitted", mutate: func(p *RateProject) {
			p.Attempts[4].Stage = "private-customer-content"
		}, want: []string{"activity_without_delivery"}},
		{name: "successful attempt breaks streak", mutate: func(p *RateProject) {
			p.Attempts[4].Status, p.Attempts[4].Signature = "succeeded", ""
		}, want: []string{"activity_without_delivery"}},
		{name: "below repetition threshold", mutate: func(p *RateProject) {
			p.Attempts = p.Attempts[:4]
		}},
		{name: "running attempt spans window with no free slots", mutate: func(p *RateProject) {
			p.Attempts = p.Attempts[:1]
			p.Attempts[0].Status, p.Attempts[0].Signature = "running", ""
			p.Attempts[0].Started = now.Add(-30 * time.Minute)
		}, want: []string{"activity_without_delivery"}},
		{name: "new running attempt", mutate: func(p *RateProject) {
			p.Attempts = p.Attempts[:1]
			p.Attempts[0].Status, p.Attempts[0].Signature = "running", ""
		}},
		{name: "partial residence avoids false project finding", mutate: func(p *RateProject) {
			p.ResidencePartial = true
		}, want: []string{"unproductive_attempts"}},
		{name: "future attempts excluded", mutate: func(p *RateProject) {
			p.Attempts[4].At = now.Add(time.Second)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := RateProject{ID: "project"}
			for i := range 5 {
				p.Attempts = append(p.Attempts, RateAttempt{ID: fmt.Sprint("attempt-", i), Item: "item", Status: "failed", Session: fmt.Sprint("session-", i), Stage: "finalization", Signature: "private-customer-content", Started: now.Add(-time.Duration(10-i)*time.Minute - time.Second), At: now.Add(-time.Duration(10-i) * time.Minute)})
			}
			if test.mutate != nil {
				test.mutate(&p)
			}
			before, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			got := Evaluate(now, Snapshot{Rates: []RateProject{p}})
			signals := []string{}
			for _, finding := range got {
				if finding.Signal != "activity_without_delivery" && finding.Signal != "unproductive_attempts" {
					continue
				}
				signals = append(signals, finding.Signal)
				if finding.Class != "human" || finding.Severity != "attention" || finding.Evidence.Productivity == nil || finding.Evidence.Productivity.WindowFrom.IsZero() || finding.Evidence.Productivity.WindowTo.IsZero() || finding.Evidence.Productivity.LatestFailureStage == "" {
					t.Fatalf("incomplete observation: %+v", finding)
				}
				if finding.Signal == "unproductive_attempts" && (finding.Evidence.Counts["consecutive_unproductive_attempts"] != 5 || len(finding.Evidence.AttemptIDs) != 5 || finding.Evidence.Productivity.LatestFailureStage != "finalization") {
					t.Fatalf("streak evidence = %+v", finding.Evidence)
				}
				if finding.Signal == "activity_without_delivery" && len(p.Attempts) == 5 && (finding.Evidence.Counts["attempts"] != 5 || finding.Evidence.Counts["sessions"] != 5) {
					t.Fatalf("activity evidence = %+v", finding.Evidence)
				}
				raw, err := json.Marshal(finding)
				if err != nil || strings.Contains(string(raw), "private-customer-content") {
					t.Fatalf("unsafe finding: %s error=%v", raw, err)
				}
			}
			slices.Sort(signals)
			if !slices.Equal(signals, test.want) {
				t.Fatalf("signals=%v want=%v", signals, test.want)
			}
			after, err := json.Marshal(p)
			if err != nil || string(before) != string(after) {
				t.Fatalf("evaluation changed inputs: error=%v", err)
			}
		})
	}
}
