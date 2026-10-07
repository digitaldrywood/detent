package hubserver

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportsCompletionPopulation(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	states := map[string]tracker.NativeState{"Done": {Terminal: true}, "Todo": {Dispatchable: true}}
	events := []analyticsResidenceEvent{}
	for _, step := range []struct {
		hour     int
		from, to string
	}{{10, "Backlog", "Todo"}, {15, "Todo", "In Progress"}, {18, "In Progress", "Blocked"}, {22, "Blocked", "Merging"}, {25, "Merging", "Done"}} {
		events = append(events, analyticsResidenceEvent{at: at(step.hour), kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: step.from, ToState: step.to}})
	}
	timeline := buildAnalyticsResidenceTimeline(analyticsResidenceIssue{id: "done", created: base, initial: "Backlog", events: events}, states, at(48))
	open := buildAnalyticsResidenceTimeline(analyticsResidenceIssue{id: "open", created: base, initial: "Todo"}, states, at(48))
	for _, tt := range []struct {
		name                  string
		from, to              int
		partial               bool
		done, count           int
		system, lead, working float64
	}{{"whole history despite window starting after Todo", 24, 48, false, 1, 1, 11 * 3600, 25 * 3600, 3 * 3600}, {"prior window excludes later completion", 0, 24, false, 0, 0, 0, 0, 0}, {"incomplete history omits timing", 24, 48, true, 1, 0, 0, 0, 0}} {
		t.Run(tt.name, func(t *testing.T) {
			item := timeline
			item.issue.partial = tt.partial
			got := reportsCompletions([]analyticsResidenceTimeline{item, open}, states, at(tt.from), at(tt.to))
			if got.Done != tt.done || got.System.Count != tt.count || got.System.P50Seconds != tt.system || got.Lead.P50Seconds != tt.lead || got.Working.P50Seconds != tt.working || got.Partial != tt.partial {
				t.Fatalf("completion=%+v", got)
			}
		})
	}
}

func TestReportsStageAndSpendAttribution(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	attempts := []nativeAnalyticsAttempt{
		{AttemptID: "a", WorkItemID: "one", Status: "succeeded", Identity: agentidentity.Configured("codex", "codex", "", "code", "model-a", "openai", "high", "", at), Phases: []tracker.NativePhase{{Name: "implement", StartedAt: at, FinishedAt: at.Add(time.Minute)}}},
		{AttemptID: "b", WorkItemID: "one", Status: "failed", Identity: agentidentity.Configured("codex", "codex", "", "code", "model-b", "openai", "medium", "", at), Phases: []tracker.NativePhase{{Name: "implement", StartedAt: at, FinishedAt: at.Add(3 * time.Minute)}}},
		{AttemptID: "c", WorkItemID: "two", Status: "running", Identity: agentidentity.Configured("codex", "codex", "", "code", "model-a", "openai", "high", "", at)},
	}
	stages, spend := reportsAttemptTotals(attempts, map[string][]usageRow{"a": {{Input: 100, CachedInput: 80, Output: 20, Cost: 2}}, "b": {{Input: 20, Output: 10, Cost: 1}}})
	if len(stages) != 2 || stages[0].Sessions != 2 || stages[0].Issues != 2 || stages[0].Succeeded != 1 || stages[0].Duration.Count != 1 || stages[0].Duration.P90Seconds != 60 || stages[0].Tokens != 120 || stages[0].Cached != 80 || stages[0].Cost != 2 || stages[0].UsageObserved != 1 || stages[1].Duration.P50Seconds != 180 {
		t.Fatalf("stages=%+v", stages)
	}
	if !reflect.DeepEqual(spend, []reportsSpend{{WorkItemID: "one", Cost: 3}}) {
		t.Fatalf("spend=%+v", spend)
	}
}

func TestReportsFirstTryHistory(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	var item string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT work_item_id FROM native_attempts WHERE id='attempt_browser_usage_0_0'").Scan(&item); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now().UTC()
	report := nativeAnalyticsProject{Landings: operatortool.ReadPage[nativeAnalyticsLanding]{Items: []nativeAnalyticsLanding{{ChangeID: "change", WorkItemID: tracker.NativeWorkItemID(item), Landing: tracker.ChangeLanding{LandedAt: now}}}}}
	for _, tt := range []struct {
		name, role string
		receipt    *tracker.NativeLandingReceipt
		want       *float64
	}{
		{name: "first try", role: "code", want: reportsPercent(100)},
		{name: "conflict before reporting window", role: "merge", receipt: &tracker.NativeLandingReceipt{ChangeID: "change", RefusalKind: "conflict", ObservedAt: now.Add(-72 * time.Hour)}, want: reportsPercent(0)},
		{name: "unknown earlier merge cannot imply first try", role: "merge"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_remove(data_json,'$.runtime')"); err != nil {
				t.Fatal(err)
			}
			for _, entry := range []struct {
				id, role string
				at       time.Time
				receipt  *tracker.NativeLandingReceipt
			}{{"attempt_browser_usage_0_0", tt.role, now.Add(-72 * time.Hour), tt.receipt}, {"attempt_browser_usage_0_1", "merge", now.Add(-time.Hour), &tracker.NativeLandingReceipt{ChangeID: "change", Landed: true, ObservedAt: now}}} {
				raw, err := json.Marshal(entry.receipt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET started_at=?,data_json=json_set(data_json,'$.runtime.identity.role',?,'$.runtime.landing',json(?)) WHERE id=?`, formatHubTime(entry.at), entry.role, string(raw), entry.id); err != nil {
					t.Fatal(err)
				}
			}
			got, available, err := f.service.reportsFirstTry(t.Context(), scope, report)
			if err != nil {
				t.Fatal(err)
			}
			if available != (tt.want != nil) || available && got != *tt.want {
				t.Fatalf("first try=%v, want=%v", got, tt.want)
			}
		})
	}
}

func reportsPercent(value float64) *float64 { return &value }
