package hubserver

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeAnalyticsLaneResidence(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	transition := func(hour int, from, to, detail string) analyticsResidenceEvent {
		return analyticsResidenceEvent{At: at(hour), Kind: "workflow.transitioned", Data: tracker.CollaborationData{FromState: from, ToState: to, ReasonDetail: detail}}
	}
	claim := func(hour int) analyticsResidenceEvent {
		return analyticsResidenceEvent{At: at(hour), Kind: "scheduler.decision", Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", At: at(hour)}}}
	}
	fixtures := []analyticsResidenceIssue{
		{Id: "a", Created: base, Initial: "Backlog", Events: []analyticsResidenceEvent{
			transition(2, "Backlog", "Todo", ""), transition(4, "Todo", "In Progress", ""),
			transition(6, "In Progress", "Blocked", ""), transition(8, "Blocked", "In Progress", ""),
			transition(8, "In Progress", "Rework", "tests"), claim(9), transition(10, "Rework", "In Progress", ""),
			transition(12, "In Progress", "Human Review", ""), transition(14, "Human Review", "Rework", "review"),
			transition(16, "Rework", "Merging", ""), transition(18, "Merging", "Done", ""),
		}},
		{Id: "b", Created: base, Initial: "Todo", Events: []analyticsResidenceEvent{
			transition(4, "Todo", "In Progress", ""), transition(8, "In Progress", "Rework", "tests"), claim(10), transition(14, "Rework", "Done", ""),
		}},
		{Id: "c", Created: base, Initial: "Backlog", Events: []analyticsResidenceEvent{
			transition(1, "Backlog", "Blocked", ""), transition(3, "Blocked", "Todo", ""),
		}},
		{Id: "d", Created: base, Initial: "Triage"},
	}
	details := &memoryRunDetails{}
	f := newPostgresDefaultNativeFixture(t, Config{now: func() time.Time { return base }, RunDetails: details})
	scope := analyticsResidenceTestScope(t, f)
	ids := map[string]string{}
	for _, fixture := range fixtures {
		id := seedAnalyticsResidenceIssue(t, f, fixture.Id, fixture.Initial, fixture.Created, false)
		ids[fixture.Id] = string(id)
		for _, event := range fixture.Events {
			appendAnalyticsResidenceTestEvent(t, f, id, event)
		}
	}
	for _, test := range []struct {
		name                      string
		from, to                  int
		system, held, queue, lead float64
		todoCount                 int
		todoP50, todoP90          float64
		causes                    []operatortool.AnalyticsReworkCause
	}{
		{name: "whole window", from: 0, to: 24, system: 47, held: 4, queue: 32, lead: 32, todoCount: 3, todoP50: 4, todoP90: 17.6,
			causes: []operatortool.AnalyticsReworkCause{{FromState: "Human Review", ReasonDetail: "review", Count: 1}, {FromState: "In Progress", ReasonDetail: "tests", Count: 2}}},
		{name: "first bucket", from: 0, to: 12, system: 29, held: 2, queue: 18, todoCount: 3, todoP50: 4, todoP90: 8,
			causes: []operatortool.AnalyticsReworkCause{{FromState: "In Progress", ReasonDetail: "tests", Count: 2}}},
		{name: "second bucket with carry in", from: 12, to: 24, system: 18, held: 2, queue: 14, lead: 32, todoCount: 1, todoP50: 12, todoP90: 12,
			causes: []operatortool.AnalyticsReworkCause{{FromState: "Human Review", ReasonDetail: "review", Count: 1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			window := operatortool.AnalyticsWindow{From: at(test.from), To: at(test.to), Bucket: 12 * time.Hour}
			report, err := readAnalyticsResidenceTestReport(t, t.Context(), f, details, scope, operatortool.AnalyticsRequest{Limit: 10}, window)
			if err != nil {
				t.Fatal(err)
			}
			residence := report.LaneResidence
			for name, pair := range map[string][2]float64{"system": {residence.SystemTotal.Seconds, test.system * 3600}, "held": {residence.HeldTotal.Seconds, test.held * 3600}, "queue": {report.QueueTime.Seconds, test.queue * 3600}, "lead": {residence.LeadTotal.Seconds, test.lead * 3600}} {
				if pair[0] != pair[1] {
					t.Fatalf("%s got %v want %v", name, pair[0], pair[1])
				}
			}
			if residence.Partial || report.QueueTime.Partial || slices.Contains(report.Unavailable, "lane_residence_and_unclaimed_waits_unavailable") || !reflect.DeepEqual(residence.ReworkCauses, test.causes) {
				t.Fatalf("residence %#v unavailable=%v", residence, report.Unavailable)
			}
			for _, lane := range residence.Lanes {
				if lane.Lane == "Backlog" || lane.Lane == "Triage" || lane.Lane == "Done" {
					t.Fatalf("excluded lane %#v", lane)
				}
				if lane.Lane == "Todo" && (lane.Count != test.todoCount || lane.P50Seconds != test.todoP50*3600 || math.Abs(lane.P90Seconds-test.todoP90*3600) > 0.001) {
					t.Fatalf("quantiles %#v", lane)
				}
				if (lane.Lane == "Blocked" || lane.Lane == "Human Review") && lane.Group != "held" {
					t.Fatalf("held group %#v", lane)
				}
			}
			var bucketQueue, bucketSystem float64
			for _, bucket := range report.Digest {
				bucketQueue += bucket.QueueSeconds
				bucketSystem += bucket.LaneResidence.SystemTotal.Seconds
			}
			if bucketQueue != report.QueueTime.Seconds || bucketSystem != residence.SystemTotal.Seconds {
				t.Fatalf("bucket attribution %#v", report.Digest)
			}
			for _, item := range residence.Issues.Items {
				if item.WorkItemID == ids["a"] && (item.SystemStartedAt == nil || !item.SystemStartedAt.Equal(at(2))) {
					t.Fatalf("clock start %#v", item)
				}
				if test.name == "whole window" {
					want := map[string]float64{}
					switch item.WorkItemID {
					case ids["a"]:
						want = map[string]float64{"Todo": 2 * 3600, "In Progress": 4 * 3600, "Blocked": 2 * 3600, "Human Review": 2 * 3600, "Rework": 4 * 3600, "Merging": 2 * 3600}
						if item.QueueSeconds != 5*3600 || item.SystemSeconds != 12*3600 || item.HeldSeconds != 4*3600 || item.LeadSeconds == nil || *item.LeadSeconds != 18*3600 {
							t.Fatalf("per issue totals %#v", item)
						}
					case ids["b"]:
						want = map[string]float64{"Todo": 4 * 3600, "In Progress": 4 * 3600, "Rework": 6 * 3600}
					case ids["c"]:
						want = map[string]float64{"Todo": 21 * 3600}
					}
					if !reflect.DeepEqual(item.LaneSeconds, want) {
						t.Fatalf("per issue lanes %#v want=%v", item, want)
					}
				}
			}
			if test.to == 24 {
				if len(residence.Aging.Items) != 2 {
					t.Fatalf("terminal items aging %#v", residence.Aging)
				}
				for _, item := range residence.Aging.Items {
					if item.WorkItemID == ids["c"] && (item.Lane != "Todo" || item.Hours != 21 || item.LaneP90Hours == nil || math.Abs(*item.LaneP90Hours-test.todoP90) > 0.001 || item.SinceTodoHours == nil || *item.SinceTodoHours != 21) {
						t.Fatalf("open lane aging %#v", item)
					}
				}
			}
		})
	}
	for _, test := range []struct {
		name          string
		offset, limit int
	}{{"first page", 0, 2}, {"second page", 2, 2}} {
		t.Run(test.name, func(t *testing.T) {
			report, err := readAnalyticsResidenceTestReport(t, t.Context(), f, details, scope, operatortool.AnalyticsRequest{Limit: test.limit, RowOffset: test.offset}, operatortool.AnalyticsWindow{From: base, To: at(24), Bucket: 12 * time.Hour})
			if err != nil || len(report.LaneResidence.Issues.Items) != 2 || report.LaneResidence.Issues.Offset != test.offset || (report.LaneResidence.Issues.NextOffset != nil) != (test.offset == 0) || report.LaneResidence.SystemTotal.Seconds != 47*3600 {
				t.Fatalf("page %#v error=%v", report.LaneResidence, err)
			}
		})
	}
}

func TestNativeAnalyticsResidencePopulation(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"issues", "events", "missing history", "archived live filter"} {
		t.Run(kind, func(t *testing.T) {
			details := &memoryRunDetails{}
			f := newPostgresDefaultNativeFixture(t, Config{now: func() time.Time { return base }, RunDetails: details})
			scope := analyticsResidenceTestScope(t, f)
			window := operatortool.AnalyticsWindow{From: base, To: base.Add(time.Hour), Bucket: time.Hour}
			if kind == "missing history" {
				seedAnalyticsResidenceIssue(t, f, "unrecorded", "Todo", base, true)
				report, err := readAnalyticsResidenceTestReport(t, t.Context(), f, details, scope, operatortool.AnalyticsRequest{Limit: 1}, window)
				if err != nil {
					t.Fatal(err)
				}
				if !report.Partial || !report.LaneResidence.Partial || !report.QueueTime.Partial || !slices.Contains(report.Unavailable, "lane_residence_and_unclaimed_waits_unavailable") || report.QueueTime.Seconds != 0 || len(report.LaneResidence.Aging.Items) != 0 {
					t.Fatalf("invented unrecorded residence %#v", report)
				}
				return
			}
			issue := seedAnalyticsResidenceIssue(t, f, "population", "Todo", base, false)
			if kind == "archived live filter" {
				gated := seedAnalyticsResidenceIssue(t, f, "gated", "Human Review", base, false)
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					row, err := w.WorkItem(t.Context(), scope.project, issue)
					if err != nil {
						return err
					}
					row.Archived = true
					_, err = w.UpdateWorkItem(t.Context(), row)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for _, live := range []bool{false, true} {
					ctx := t.Context()
					if live {
						ctx = withAnalyticsLiveIssues(ctx)
					}
					report, err := readAnalyticsResidenceTestReport(t, ctx, f, details, scope, operatortool.AnalyticsRequest{Limit: 10}, window)
					if err != nil {
						t.Fatal(err)
					}
					present := slices.ContainsFunc(report.LaneResidence.Issues.Items, func(value operatortool.AnalyticsIssueResidence) bool { return value.WorkItemID == string(issue) })
					if present == live || !slices.ContainsFunc(report.LaneResidence.Issues.Items, func(value operatortool.AnalyticsIssueResidence) bool { return value.WorkItemID == string(gated) }) {
						t.Fatalf("archived present=%v live=%v issues=%d", present, live, len(report.LaneResidence.Issues.Items))
					}
				}
				return
			}
			for i := range maxAnalyticsPopulation {
				if kind == "issues" {
					seedAnalyticsResidenceIssue(t, f, "population-"+strconv.Itoa(i), "Todo", base, false)
				} else {
					from, to := "Todo", "In Progress"
					if i%2 == 1 {
						from, to = to, from
					}
					appendAnalyticsResidenceTestEvent(t, f, issue, analyticsResidenceEvent{At: base.Add(time.Duration(i+1) * time.Second), Kind: "workflow.transitioned", Data: tracker.CollaborationData{FromState: from, ToState: to}})
				}
			}
			if kind == "events" {
				appendAnalyticsResidenceTestEvent(t, f, issue, analyticsResidenceEvent{At: base.Add(1001 * time.Second), Kind: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}})
			}
			report, err := readAnalyticsResidenceTestReport(t, t.Context(), f, details, scope, operatortool.AnalyticsRequest{Limit: 1}, window)
			if err != nil {
				t.Fatal(err)
			}
			residence := report.LaneResidence
			population := "residence_" + kind
			cursor := report.PopulationCursors[population]
			if cursor == "" {
				t.Fatal("missing residence continuation")
			}
			next, err := readAnalyticsResidenceTestReport(t, t.Context(), f, details, scope, operatortool.AnalyticsRequest{Limit: 1, PopulationCursor: cursor}, window)
			if err != nil {
				t.Fatal(err)
			}
			if !next.Partial || !next.LaneResidence.Partial || next.PopulationCursors[population] != "" {
				t.Fatalf("residence continuation: %+v", next.LaneResidence)
			}
			if kind == "events" && (next.LaneResidence.EventsObserved != 1 || next.QueueTime.Seconds != 0) || kind == "issues" && next.LaneResidence.IssuesObserved != 1 {
				t.Fatalf("continuation populations: %+v", next.LaneResidence)
			}
			if !residence.Partial || !report.QueueTime.Partial || !slices.Contains(report.Unavailable, "lane_residence_complete_population") {
				t.Fatalf("bounded population %#v", residence)
			}
			if kind == "events" && (len(residence.Aging.Items) != 0 || report.QueueTime.Seconds != 500 || residence.EventsObserved != 1000) {
				t.Fatalf("extrapolated clipped history %#v", residence)
			}
			if kind == "issues" && residence.IssuesObserved != 1000 {
				t.Fatalf("issues observed %d", residence.IssuesObserved)
			}
		})
	}
}

func analyticsResidenceTestScope(t *testing.T, f nativeFixture) nativeScope {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.States = nil
		for _, name := range []string{"Backlog", "Triage", "Todo", "In Progress", "Blocked", "Rework", "Human Review", "Merging", "Done"} {
			project.States = append(project.States, tracker.NativeState{Name: name, Terminal: name == "Done", Dispatchable: name == "Todo" || name == "In Progress" || name == "Rework" || name == "Merging"})
		}
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("analytics authority status=%d error=%v", status, err)
	}
	return nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
}

func seedAnalyticsResidenceIssue(t *testing.T, f nativeFixture, id, state string, at time.Time, unrecorded bool) tracker.NativeWorkItemID {
	t.Helper()
	item := tracker.NativeWorkItemID("residence-" + id)
	var event eventstore.Event
	if !unrecorded {
		var err error
		event, err = eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: item, Type: "issue.created", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: at, Data: tracker.CollaborationData{Revision: 1}}))
		if err != nil {
			t.Fatal(err)
		}
	}
	err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		number, err := w.NextWorkItemNumber(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, NativeID: item, Number: number, Revision: 1, Title: id, WorkflowStateName: &state, CreatedAt: at, UpdatedAt: at, NativeCreatedAt: at, NativeUpdatedAt: at, LastActivityAt: at})
		if err != nil {
			return err
		}
		if unrecorded {
			return nil
		}
		_, err = w.AppendEvent(t.Context(), event)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func appendAnalyticsResidenceTestEvent(t *testing.T, f nativeFixture, id tracker.NativeWorkItemID, event analyticsResidenceEvent) {
	t.Helper()
	content := eventstore.NewContent(f.service.runDetailStore())
	row, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: id, Type: event.Kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: event.At, Data: event.Data}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.WorkItem(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		if event.Kind == "workflow.transitioned" {
			current.WorkflowStateName = &event.Data.ToState
			current.NativeUpdatedAt = event.At
			current.UpdatedAt = event.At
			if _, err := w.UpdateWorkItem(t.Context(), current); err != nil {
				return err
			}
		}
		_, err = w.AppendEvent(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func historyTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func readAnalyticsResidenceTestReport(t *testing.T, ctx context.Context, f nativeFixture, details *memoryRunDetails, scope nativeScope, request operatortool.AnalyticsRequest, window operatortool.AnalyticsWindow) (nativeAnalyticsProject, error) {
	t.Helper()
	previous := details.beforeRead
	details.beforeRead = func(context.Context) error {
		if held := f.service.config.TenantRepository.(*postgres.Repository).Stats().AcquiredConns(); held != 0 {
			t.Fatalf("residence object read held %d database connections", held)
		}
		return nil
	}
	defer func() { details.beforeRead = previous }()
	ctx, paging, err := analyticsPagingContext(ctx, request.PopulationCursor)
	if err != nil {
		return nativeAnalyticsProject{}, err
	}
	out := nativeAnalyticsProject{ProjectID: string(scope.project), Window: window, Unavailable: []string{}}
	for from := window.From; from.Before(window.To); from = from.Add(window.Bucket) {
		out.Digest = append(out.Digest, nativeAnalyticsBucket{From: from, To: minTime(from.Add(window.Bucket), window.To)})
	}
	err = f.service.projectNativeAnalyticsResidenceTenant(ctx, scope, request, &out)
	out.PopulationCursors = paging.Next
	return out, err
}
