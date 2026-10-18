package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
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
	last := events[len(events)-1]
	events[len(events)-1] = analyticsResidenceEvent{at: at(23), kind: "scheduler.decision"}
	events = append(events, last)
	timeline := buildAnalyticsResidenceTimeline(analyticsResidenceIssue{id: "done", created: base, initial: "Backlog", events: events}, states, at(48))
	timings := residencePipelineTimings(timeline, []nativeAnalyticsAttempt{{WorkItemID: "done", Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: at(24)}}})
	if len(timings) != 2 || !timings[0].Valid() || timings[0].Stage != "merging_queue" || timings[0].StartedAt != at(22) || timings[0].FinishedAt != at(23) || timings[1].Stage != "merging_to_landed" || timings[1].FinishedAt != at(24) {
		t.Fatalf("workflow timestamps not derivable: %+v", timings)
	}
	reclosed := append(append([]analyticsResidenceEvent{}, events...),
		analyticsResidenceEvent{at: at(30), kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Done", ToState: "Rework"}},
		analyticsResidenceEvent{at: at(34), kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Rework", ToState: "Done"}})
	for _, tt := range []struct {
		name                  string
		issue                 analyticsResidenceTimeline
		to                    int
		partial               bool
		count                 int
		system, lead, working float64
	}{
		{"whole history despite window starting after Todo", timeline, 48, false, 1, 11 * 3600, 25 * 3600, 3 * 3600},
		{"re-closed issue measured at its final close", buildAnalyticsResidenceTimeline(analyticsResidenceIssue{id: "done", created: base, initial: "Backlog", events: reclosed}, states, at(48)), 48, false, 1, 15 * 3600, 34 * 3600, 3 * 3600},
		{"close outside the read has no timing", timeline, 24, true, 0, 0, 0, 0},
		{"incomplete history omits timing", func() analyticsResidenceTimeline { item := timeline; item.issue.partial = true; return item }(), 48, true, 0, 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := reportsCompletions([]analyticsResidenceTimeline{tt.issue}, states, at(tt.to))
			if got.System.Count != tt.count || got.System.P50Seconds != tt.system || got.Lead.P50Seconds != tt.lead || got.Working.P50Seconds != tt.working || got.Partial != tt.partial {
				t.Fatalf("completion=%+v", got)
			}
		})
	}
}

func TestReportsShippedPopulation(t *testing.T) {
	f := newUsageHostedFixture(t)
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	to := f.service.config.now().UTC().Truncate(time.Hour)
	w := operatortool.AnalyticsWindow{From: to.Add(-48 * time.Hour), To: to, Bucket: time.Hour}
	type step struct {
		at       time.Time
		from, to string
	}
	seeds := map[string]struct {
		state string
		steps []step
	}{
		"reclosed":       {"Done", []step{{to.Add(-40 * time.Hour), "Todo", "Done"}, {to.Add(-39 * time.Hour), "Done", "Todo"}, {to.Add(-10 * time.Hour), "Todo", "Done"}}},
		"window start":   {"Done", []step{{w.From, "Todo", "Done"}}},
		"just outside":   {"Done", []step{{w.From.Add(-time.Second), "Todo", "Done"}}},
		"cancelled":      {"Cancelled", []step{{to.Add(-5 * time.Hour), "Todo", "Cancelled"}}},
		"archived":       {"Done", []step{{to.Add(-6 * time.Hour), "Todo", "Done"}}},
		"reopened":       {"Todo", []step{{to.Add(-30 * time.Hour), "Todo", "Done"}, {to.Add(-29 * time.Hour), "Done", "Todo"}}},
		"then cancelled": {"Cancelled", []step{{to.Add(-15 * time.Hour), "Todo", "Done"}, {to.Add(-14 * time.Hour), "Done", "Cancelled"}}},
	}
	for i := range 4 {
		var steps []step
		for j := range 300 {
			from, next := "Todo", "In Progress"
			if j%2 == 1 {
				from, next = next, from
			}
			steps = append(steps, step{to.Add(-46*time.Hour + time.Duration(i*300+j)*time.Minute), from, next})
		}
		seeds[fmt.Sprintf("busy %d", i)] = struct {
			state string
			steps []step
		}{"Done", append(steps, step{to.Add(-20*time.Hour + time.Duration(i)*time.Minute), "Todo", "Done"})}
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ids := map[string]string{}
	for name, seed := range seeds {
		issue, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: name, Body: nativeContractTestBody, State: seed.state}, to.Add(-50*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = string(issue.WorkItemID)
		for _, s := range seed.steps {
			if err := appendNativeHistory(t.Context(), tx, scope, ids[name], "workflow.transitioned", tracker.CollaborationData{FromState: s.from, ToState: s.to}, s.at); err != nil {
				t.Fatal(err)
			}
		}
	}
	for prefix, owner := range map[string]string{"attempt_browser_usage_1_%": ids["reclosed"], "attempt_browser_usage_2_%": ids["cancelled"]} {
		if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET work_item_id=? WHERE id LIKE ?", owner, prefix); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE native_id=?", ids["archived"]); err != nil {
		t.Fatal(err)
	}
	priorAt := w.From.Add(-2 * time.Hour)
	for attempt, at := range map[string]time.Time{"attempt_browser_usage_1_0": w.From.Add(-30 * time.Hour), "attempt_browser_usage_2_1": priorAt} {
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO attempt_usage SELECT attempt_id,organization_id,project_id,?,provider,model,input,cached_input,output,cost_estimate,currency,updated_at FROM attempt_usage WHERE attempt_id=?", at.Format(usagePeriodLayout), attempt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET status='failed',started_at=?,updated_at=? WHERE id='attempt_browser_usage_2_1'", formatHubTime(priorAt.Add(-time.Hour)), formatHubTime(priorAt)); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, f.service)
	drainTestAnalytics(t, f.service)
	var priorCost float64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT sum(cost_estimate) FROM attempt_usage WHERE period>=? AND period<?", w.From.Add(-48*time.Hour).Format(usagePeriodLayout), w.From.Format(usagePeriodLayout)).Scan(&priorCost); err != nil || priorCost == 0 {
		t.Fatalf("prior cost=%v err=%v", priorCost, err)
	}
	var shippedCost float64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT sum(cost_estimate) FROM attempt_usage WHERE attempt_id LIKE 'attempt_browser_usage_1_%'").Scan(&shippedCost); err != nil || shippedCost == 0 {
		t.Fatalf("shipped cost=%v err=%v", shippedCost, err)
	}
	var reports [2]hostedReportsReport
	for i, q := range [2]nativeQueryer{f.service.database.reader, analyticsRollupReader{f.service.database.reader}} {
		project, err := readNativeAnalytics(t.Context(), q, scope, operatortool.AnalyticsRequest{Limit: maxAnalyticsPopulation}, w)
		if err != nil {
			t.Fatal(err)
		}
		report, err := f.service.readHostedReportsFrom(t.Context(), nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: w, Projects: []nativeAnalyticsProject{project}}, q)
		if err != nil {
			t.Fatal(err)
		}
		reports[i] = report
	}
	assertAnalyticsJSONEqual(t, reports[0].Completion, reports[1].Completion)
	assertAnalyticsJSONEqual(t, reports[0].Previous, reports[1].Previous)
	report := reports[1]
	throughput := 0
	for _, bucket := range report.Throughput {
		throughput += bucket.Done
	}
	if c := report.Completion; c.Done != 7 || c.Cancelled != 2 || c.Partial || c.System.Count != 7 || throughput != 7 || c.Cost != shippedCost {
		t.Fatalf("completion=%+v throughput=%d want 7 shipped, 2 cancelled, lifetime cost %v", c, throughput, shippedCost)
	}
	if c := report.Completion; c.CostP50 == nil || c.CostP90 == nil || *c.CostP50 != 0 || *c.CostP90 <= 0 || *c.CostP90 > shippedCost {
		t.Fatalf("shipped spend p50=%v p90=%v, want 0 and a share of %v: one of seven shipped issues spent", c.CostP50, c.CostP90, shippedCost)
	}
	if p := report.Previous; p.Done != 1 || p.Cancelled != 0 || p.Partial || p.Cost != 0 {
		t.Fatalf("previous=%+v", p)
	}
	if report.Cancelled != 2 || report.PriorCost == nil || *report.PriorCost != priorCost || report.PriorFailed == nil || *report.PriorFailed != 1 || report.PriorFirst != nil {
		t.Fatalf("cancelled=%d prior cost=%v failed=%v first try=%v, want 2, %v, 1, nil", report.Cancelled, report.PriorCost, report.PriorFailed, report.PriorFirst, priorCost)
	}
	for _, window := range []struct {
		from, to time.Time
		want     bool
	}{{w.From.Add(-time.Hour), w.From, false}, {w.From, w.From.Add(time.Hour), true}} {
		population, err := readNativeClosedPopulation(t.Context(), f.service.database.reader, scope, "", nil, window.from, window.to)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(population.shippedIDs(), ids["window start"]); got != window.want {
			t.Fatalf("close on the boundary in [%s, %s) = %t, want %t", window.from, window.to, got, window.want)
		}
	}
	day := operatortool.AnalyticsWindow{From: to.Add(-24 * time.Hour), To: to, Bucket: time.Hour}
	dayProject, err := readNativeAnalytics(t.Context(), f.service.database.reader, scope, operatortool.AnalyticsRequest{Limit: maxAnalyticsPopulation}, day)
	if err != nil {
		t.Fatal(err)
	}
	dayReport, err := f.service.readHostedReportsFrom(t.Context(), nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: day, Projects: []nativeAnalyticsProject{dayProject}}, f.service.database.reader)
	if err != nil {
		t.Fatal(err)
	}
	if dayReport.Completion.Done != 6 || dayReport.Completion.Cancelled != 2 {
		t.Fatalf("24h reports done=%d cancelled=%d, want 6 and 2", dayReport.Completion.Done, dayReport.Completion.Cancelled)
	}
	board, err := readNativeBoardSummary(t.Context(), f.service.database.reader, scope, url.Values{"completed_window": {"24h"}}, to)
	if err != nil {
		t.Fatal(err)
	}
	work, err := readNativeWorkSummary(t.Context(), f.service.database.reader, scope, "SELECT i.native_id FROM issues i LEFT JOIN workflow_states ws ON ws.id = i.workflow_state_id WHERE i.organization_id = ? AND i.project_id = ? AND i.archived = 0", []any{scope.organization, scope.project}, 10, to, "24h", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, summary := range map[string]*tracker.NativeWorkSummary{"board": board, "work": work} {
		if summary.Completed != dayReport.Completion.Done || summary.Cancelled != dayReport.Completion.Cancelled {
			t.Fatalf("%s shipped=%d cancelled=%d, reports shipped=%d cancelled=%d", name, summary.Completed, summary.Cancelled, dayReport.Completion.Done, dayReport.Completion.Cancelled)
		}
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

func TestReportsRunnerTotals(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	minutes := func(n int) []tracker.NativePhase {
		return []tracker.NativePhase{{Name: "implement", StartedAt: at, FinishedAt: at.Add(time.Duration(n) * time.Minute)}}
	}
	attempts := []nativeAnalyticsAttempt{
		{AttemptID: "a", WorkItemID: "one", Status: "succeeded", Phases: minutes(2)},
		{AttemptID: "b", WorkItemID: "one", Status: "failed", Phases: minutes(4)},
		{AttemptID: "c", WorkItemID: "two", Status: "succeeded", Phases: minutes(1)},
		{AttemptID: "d", WorkItemID: "two", Status: "succeeded"},
		{AttemptID: "e", WorkItemID: "two", Status: "running"},
	}
	usage := map[string][]usageRow{
		"a": {{RunnerID: "run_studio", RunnerName: "studio", Cost: 1}, {RunnerID: "run_studio", RunnerName: "studio", Cost: 2}},
		"b": {{RunnerID: "run_studio", RunnerName: "studio", Cost: 1}},
		"c": {{RunnerID: "run_sprite", RunnerName: "sprite", Cost: 5}},
		"d": {{Cost: 9}},
	}
	got := reportsRunnerTotals(attempts, usage)
	want := []reportsRunner{
		{RunnerID: "run_sprite", Name: "sprite", Sessions: 1, Succeeded: 1, Duration: sampledDuration([]float64{60}), Cost: 5},
		{RunnerID: "run_studio", Name: "studio", Sessions: 2, Succeeded: 1, Duration: sampledDuration([]float64{120, 240}), Cost: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runners=%+v want %+v", got, want)
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
			held, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Rollback()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, available, err := f.service.reportsFirstTry(ctx, scope, report)
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

func TestReportsOpenWork(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	states := map[string]tracker.NativeState{"Done": {Terminal: true}, "Todo": {Dispatchable: true}, "Rework": {Dispatchable: true}, "Backlog": {}}
	move := func(hour int, from, to string) analyticsResidenceEvent {
		return analyticsResidenceEvent{at: at(hour), kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: from, ToState: to}}
	}
	w := operatortool.AnalyticsWindow{From: at(100), To: at(200), Bucket: time.Hour}
	issues := []analyticsResidenceIssue{
		{id: "backlog", created: at(0), initial: "Backlog"},
		{id: "todo", created: at(10), initial: "Todo"},
		{id: "held", created: at(150), initial: "Todo", events: []analyticsResidenceEvent{move(160, "Todo", "Blocked")}},
		{id: "working", created: at(190), initial: "Todo", events: []analyticsResidenceEvent{move(195, "Todo", "In Progress")}},
		{id: "done", created: at(20), initial: "Todo", events: []analyticsResidenceEvent{move(110, "Todo", "Done")}},
		{id: "late", created: at(60), initial: "Todo", events: []analyticsResidenceEvent{move(90, "Todo", "Rework"), move(120, "Rework", "Done")}},
	}
	timelines := make([]analyticsResidenceTimeline, 0, len(issues))
	for _, issue := range issues {
		timelines = append(timelines, buildAnalyticsResidenceTimeline(issue, states, w.To))
	}
	var in reportsInputs
	reportsTimelineResidence(&in, timelines, issues, states, w, false)
	got := reportsOpenWorkFor(in, nil, w)
	ids := []string{}
	for _, item := range got.Items {
		ids = append(ids, item.WorkItemID)
	}
	if got.Count != 2 || got.Held != 1 || got.Stuck != 2 || !slices.Equal(ids, []string{"todo", "held", "working"}) {
		t.Fatalf("open work=%+v ids=%v", got, ids)
	}
	if got.PreviousStuck == nil || *got.PreviousStuck != 2 {
		t.Fatalf("previous stuck=%v, want todo and done open over 24h at the prior window end", got.PreviousStuck)
	}
	if *got.Items[0].SinceTodoHours != 190 {
		t.Fatalf("since todo=%v", *got.Items[0].SinceTodoHours)
	}
	prior := operatortool.AnalyticsWindow{From: at(0), To: w.From}
	if lanes := in.lanes(prior.From, prior.To); lanes["Todo"] != (90+80+30)*3600 || lanes["Backlog"] != 0 || lanes["Rework"] != 10*3600 {
		t.Fatalf("prior lane seconds=%v", lanes)
	}
	in.clipped = true
	if clipped := reportsOpenWorkFor(in, nil, w); clipped.PreviousStuck != nil {
		t.Fatalf("clipped history reported a prior count")
	}
}

func TestReportsWindowStart(t *testing.T) {
	to := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	midnight := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		to    time.Time
		hours int
		want  time.Time
	}{
		{"hours stay rolling", to, 24, to.Add(-24 * time.Hour)},
		{"days end with today so far", to, 168, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)},
		{"thirty days", to, 720, time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)},
		{"window ending at midnight", midnight, 168, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := reportsWindowStart(tt.to, tt.hours); !got.Equal(tt.want) {
				t.Fatalf("start=%s want %s", got, tt.want)
			}
		})
	}
}

func TestReportsSeriesBuckets(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	day := func(n, hour int) int { return n*24 + hour }
	states := map[string]tracker.NativeState{"Done": {Terminal: true}, "Todo": {Dispatchable: true}}
	transition := func(hour int, from, to string) analyticsResidenceEvent {
		return analyticsResidenceEvent{at: at(hour), kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: from, ToState: to}}
	}
	end := at(day(14, 10))
	shippedIssue := buildAnalyticsResidenceTimeline(analyticsResidenceIssue{id: "shipped", created: at(day(7, 1)), initial: "Backlog", events: []analyticsResidenceEvent{
		transition(day(7, 12), "Backlog", "Todo"), transition(day(7, 14), "Todo", "In Progress"), transition(day(8, 1), "In Progress", "Done"),
		transition(day(9, 2), "Done", "Todo"), transition(day(10, 3), "Todo", "Done"),
	}}, states, end)
	priorIssue := buildAnalyticsResidenceTimeline(analyticsResidenceIssue{id: "prior", created: at(1), initial: "Todo", events: []analyticsResidenceEvent{
		transition(day(1, 5), "Todo", "Done"),
	}}, states, end)
	open := buildAnalyticsResidenceTimeline(analyticsResidenceIssue{id: "open", created: at(day(7, 0)), initial: "Todo"}, states, end)
	timelines := []analyticsResidenceTimeline{shippedIssue, priorIssue, open}
	identity := agentidentity.Configured("codex", "codex", "", "code", "model-a", "openai", "high", "", base)
	attempts := []nativeAnalyticsAttempt{
		{AttemptID: "a", WorkItemID: "shipped", Status: "succeeded", Identity: identity},
		{AttemptID: "b", WorkItemID: "open", Status: "failed", ObservedAt: at(day(8, 3)), Identity: identity},
	}
	usage := map[string][]usageRow{"a": {{Period: at(day(7, 12)), Cost: 2}}, "b": {{Period: at(day(8, 3)), Cost: 1.5}}, "": {{Period: at(day(14, 5)), Cost: 0.5}}}
	w := operatortool.AnalyticsWindow{From: at(day(7, 10)), To: end, Bucket: time.Hour}
	in := reportsInputs{
		done:      []reportsDone{{id: "shipped", at: at(day(10, 3))}},
		priorDone: []reportsDone{{id: "prior", at: at(day(1, 5))}},
		timing: func(ids []string, to time.Time) (reportsCompletion, error) {
			var picked []analyticsResidenceTimeline
			for _, timeline := range timelines {
				if slices.Contains(ids, timeline.issue.id) {
					picked = append(picked, timeline)
				}
			}
			return reportsCompletions(picked, states, to), nil
		},
	}
	reportsTimelineResidence(&in, timelines, nil, states, w, false)
	series, err := reportsSeries(in, attempts, usage, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 8 || !series[0].From.Equal(w.From) || !series[0].To.Equal(at(day(8, 0))) || !series[7].From.Equal(at(day(14, 0))) || !series[7].To.Equal(end) {
		t.Fatalf("buckets=%d first=%s..%s last=%s..%s, want UTC days clipped to the window", len(series), series[0].From, series[0].To, series[len(series)-1].From, series[len(series)-1].To)
	}
	var done int
	var total float64
	for _, bucket := range series {
		done += bucket.Done
		total += bucket.NotShippedCost
		for _, cost := range bucket.StageCost {
			total += cost
		}
	}
	if done != 1 || total != 4 {
		t.Fatalf("done=%d spend=%v, want each issue once and every usage row", done, total)
	}
	for _, tt := range []struct {
		name                  string
		bucket                int
		done, priorDone       int
		code, other, notShip  float64
		cycleCount            int
		inProgress, todoHours float64
	}{
		{"partial first day", 0, 0, 0, 2, 0, 0, 0, 10, 16},
		{"first Done is not counted", 1, 0, 1, 0, 0, 1.5, 0, 1, 24},
		{"reopened", 2, 0, 0, 0, 0, 0, 0, 0, 46},
		{"last Done counts", 3, 1, 0, 0, 0, 0, 1, 0, 27},
		{"today so far", 7, 0, 0, 0, 0.5, 0, 0, 0, 10},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := series[tt.bucket]
			if got.Done != tt.done || got.PriorDone != tt.priorDone || got.StageCost["code"] != tt.code || got.StageCost[reportsOtherStage] != tt.other || got.NotShippedCost != tt.notShip || got.Cycle.Count != tt.cycleCount {
				t.Fatalf("bucket=%+v", got)
			}
			if got.LaneSeconds["In Progress"] != tt.inProgress*3600 || got.LaneSeconds["Todo"] != tt.todoHours*3600 {
				t.Fatalf("lanes=%+v", got.LaneSeconds)
			}
		})
	}
	returns := []qualityReturn{
		{item: "open", at: at(day(7, 13)), class: classifyReworkReason("Merging", "conflict on landing")},
		{item: "shipped", at: at(day(8, 2)), class: classifyReworkReason("Merging", "conflict on landing")},
		{item: "open", at: at(day(8, 4)), class: classifyReworkReason("Merging", "base protected")},
	}
	failures := []nativeFailure{
		{workItem: "open", at: at(day(8, 3)), example: "hub request timed out"},
		{workItem: "open", at: at(day(8, 5)), example: "landing gate failed"},
		{workItem: "open", at: at(day(6, 0)), example: "hub request timed out"},
	}
	reportsSeriesClasses(series, returns, failures, w)
	if got := series[1].ReworkClasses; !reflect.DeepEqual(got, map[string]int{"Conflict on landing": 1, "Landing refused: protected base": 1}) {
		t.Fatalf("rework classes=%v", got)
	}
	if got := series[1]; got.Failed != 2 || got.FailedInfrastructure != 1 || got.FailedIssue != 1 || series[0].Failed != 0 {
		t.Fatalf("failures=%+v first=%+v", got, series[0])
	}
	rework := reportReworkClasses(returns, nil, nil)
	failureTotals := reportFailureClasses(failures)
	reportClassDetails(rework, failureTotals, returns, failures)
	conflict := rework[0]
	if conflict.Class != "Conflict on landing" || !reflect.DeepEqual(conflict.WorkItems, []string{"open", "shipped"}) || !conflict.FirstSeen.Equal(at(day(7, 13))) || !conflict.LastSeen.Equal(at(day(8, 2))) {
		t.Fatalf("rework detail=%+v", conflict)
	}
	timeout := failureTotals[0]
	if timeout.Class != "Hub request timed out" || !reflect.DeepEqual(timeout.WorkItems, []string{"open"}) || !timeout.FirstSeen.Equal(at(day(6, 0))) {
		t.Fatalf("failure detail=%+v", timeout)
	}
	spend := []reportsSpend{{WorkItemID: "shipped", Cost: 2}, {WorkItemID: "open", Cost: 1.5}}
	reportsSpendOutcomes(spend, attempts, in)
	if !reflect.DeepEqual(spend, []reportsSpend{{WorkItemID: "shipped", Cost: 2, Attempts: 1, Shipped: true, State: "Done"}, {WorkItemID: "open", Cost: 1.5, Attempts: 1, State: "Todo"}}) {
		t.Fatalf("spend=%+v", spend)
	}
}

func TestReportsPipelineTiming(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	window := operatortool.AnalyticsWindow{From: at, To: at.Add(time.Hour), Bucket: time.Hour}
	for _, tt := range []struct {
		name              string
		events            []gate.PipelineTiming
		count             int
		seconds, p50, p90 float64
		reused            int
		partial           bool
		missingIdentity   bool
	}{
		{name: "executed totals and quantiles", events: []gate.PipelineTiming{{ReceiptID: "first", Stage: "finalization", Execution: "executed", StartedAt: at, FinishedAt: at.Add(10 * time.Second)}, {ReceiptID: "second", Stage: "finalization", Execution: "executed", StartedAt: at, FinishedAt: at.Add(30 * time.Second)}}, count: 2, seconds: 40, p50: 20, p90: 28},
		{name: "duplicate receipts counted once", events: []gate.PipelineTiming{{ReceiptID: "same", Stage: "barrier", Execution: "executed", StartedAt: at, FinishedAt: at.Add(10 * time.Second)}, {ReceiptID: "same", Stage: "barrier", Execution: "executed", StartedAt: at, FinishedAt: at.Add(10 * time.Second)}}, count: 1, seconds: 10, p50: 10, p90: 10},
		{name: "reuse has no executed duration", events: []gate.PipelineTiming{{ReceiptID: "reuse", Stage: "landing_validation", Execution: "reused", ReusedReceiptID: "source", StartedAt: at, FinishedAt: at}}, count: 0, reused: 1},
		{name: "interval clipped to window", events: []gate.PipelineTiming{{ReceiptID: "clip", Stage: "rebase", Outcome: "clean", StartedAt: at.Add(-10 * time.Second), FinishedAt: at.Add(10 * time.Second)}}, count: 1, seconds: 10, p50: 10, p90: 10},
		{name: "exclusive window end", events: []gate.PipelineTiming{{ReceiptID: "later", Stage: "repair", Execution: "executed", StartedAt: window.To, FinishedAt: window.To.Add(time.Second)}}},
		{name: "dirty checkout still contributes elapsed timing", events: []gate.PipelineTiming{{ReceiptID: "dirty", Stage: "worker_check_land", Execution: "executed", StartedAt: at, FinishedAt: at.Add(10 * time.Second)}}, count: 1, seconds: 10, p50: 10, p90: 10, partial: true, missingIdentity: true},
		{name: "historical receipt missing timestamps", events: []gate.PipelineTiming{{ReceiptID: "old", Stage: "finalization", Execution: "executed"}}, partial: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.missingIdentity {
				for i := range tt.events {
					tt.events[i].HeadSHA, tt.events[i].TreeSHA = strings.Repeat("a", 40), strings.Repeat("b", 40)
				}
			}
			got := summarizePipelineTimings(tt.events, window)
			if got.Partial != tt.partial {
				t.Fatalf("partial=%v", got.Partial)
			}
			if tt.count+tt.reused == 0 {
				if len(got.Stages) != 0 {
					t.Fatalf("stages=%+v", got.Stages)
				}
				return
			}
			if len(got.Stages) != 1 {
				t.Fatalf("stages=%+v", got.Stages)
			}
			stage := got.Stages[0]
			if stage.Duration.Count != tt.count || stage.Duration.Seconds != tt.seconds || stage.Duration.P50Seconds != tt.p50 || stage.Duration.P90Seconds != tt.p90 || stage.Reused != tt.reused {
				t.Fatalf("stage=%+v", stage)
			}
		})
	}
}
