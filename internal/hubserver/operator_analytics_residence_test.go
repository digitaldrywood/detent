package hubserver

import (
	"math"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeAnalyticsLaneResidence(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }
	transition := func(hour int, from, to, detail string) analyticsResidenceEvent {
		return analyticsResidenceEvent{at: at(hour), kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: from, ToState: to, ReasonDetail: detail}}
	}
	claim := func(hour int) analyticsResidenceEvent {
		return analyticsResidenceEvent{at: at(hour), kind: "scheduler.decision", data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", At: at(hour)}}}
	}
	fixtures := []analyticsResidenceIssue{
		{id: "a", created: base, initial: "Backlog", events: []analyticsResidenceEvent{
			transition(2, "Backlog", "Todo", ""), transition(4, "Todo", "In Progress", ""),
			transition(6, "In Progress", "Blocked", ""), transition(8, "Blocked", "In Progress", ""),
			transition(8, "In Progress", "Rework", "tests"), claim(9), transition(10, "Rework", "In Progress", ""),
			transition(12, "In Progress", "Human Review", ""), transition(14, "Human Review", "Rework", "review"),
			transition(16, "Rework", "Merging", ""), transition(18, "Merging", "Done", ""),
		}},
		{id: "b", created: base, initial: "Todo", events: []analyticsResidenceEvent{
			transition(4, "Todo", "In Progress", ""), transition(8, "In Progress", "Rework", "tests"), claim(10), transition(14, "Rework", "Done", ""),
		}},
		{id: "c", created: base, initial: "Backlog", events: []analyticsResidenceEvent{
			transition(1, "Backlog", "Blocked", ""), transition(3, "Blocked", "Todo", ""),
		}},
		{id: "d", created: base, initial: "Triage"},
	}
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return base }})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	states := []tracker.NativeState{}
	for _, name := range []string{"Backlog", "Triage", "Todo", "In Progress", "Blocked", "Rework", "Human Review", "Merging", "Done"} {
		states = append(states, tracker.NativeState{Name: name, Dispatchable: name == "Todo" || name == "In Progress" || name == "Rework" || name == "Merging", Terminal: name == "Done"})
	}
	if err := applyNativeProjectStates(t.Context(), tx, scope, states, base); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, fixture := range fixtures {
		issue, err := createNativeIssueDraft(t.Context(), tx, scope, tracker.CreateIssue{Title: fixture.id, State: fixture.initial}, fixture.created, false)
		if err != nil {
			t.Fatal(err)
		}
		ids[fixture.id] = string(issue.WorkItemID)
		for _, event := range fixture.events {
			if event.kind == "workflow.transitioned" {
				issue.State = event.data.ToState
				issue, err = persistNativeIssue(t.Context(), tx, scope, issue, event.kind, event.data, event.at)
			} else {
				err = appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), event.kind, event.data, event.at)
			}
			if err != nil {
				t.Fatal(err)
			}
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
			report, err := readNativeAnalytics(t.Context(), tx, scope, operatortool.AnalyticsRequest{Limit: 10}, window)
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
					if item.WorkItemID == ids["c"] && (item.Lane != "Todo" || item.Hours != 21 || item.LaneP90Hours == nil || math.Abs(*item.LaneP90Hours-test.todoP90) > 0.001) {
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
			report, err := readNativeAnalytics(t.Context(), tx, scope, operatortool.AnalyticsRequest{Limit: test.limit, RowOffset: test.offset}, operatortool.AnalyticsWindow{From: base, To: at(24), Bucket: 12 * time.Hour})
			if err != nil || len(report.LaneResidence.Issues.Items) != 2 || report.LaneResidence.Issues.Offset != test.offset || (report.LaneResidence.Issues.NextOffset != nil) != (test.offset == 0) || report.LaneResidence.SystemTotal.Seconds != 47*3600 {
				t.Fatalf("page %#v error=%v", report.LaneResidence, err)
			}
		})
	}
}

func TestNativeAnalyticsResidencePopulation(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"issues", "events", "missing history"} {
		t.Run(kind, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return base }})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			window := operatortool.AnalyticsWindow{From: base, To: base.Add(time.Hour), Bucket: time.Hour}
			if kind == "missing history" {
				_, err := tx.ExecContext(t.Context(), `INSERT INTO issues(organization_id,project_id,number,workflow_state_id,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at)
VALUES (?,?,1,(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Todo'),'unrecorded','','open','','','',?,?)`, scope.organization, scope.project, scope.project, formatHubTime(base), formatHubTime(base))
				if err != nil {
					t.Fatal(err)
				}
				report, err := readNativeAnalytics(t.Context(), tx, scope, operatortool.AnalyticsRequest{Limit: 1}, window)
				if err != nil {
					t.Fatal(err)
				}
				if !report.Partial || !report.LaneResidence.Partial || !report.QueueTime.Partial || !slices.Contains(report.Unavailable, "lane_residence_and_unclaimed_waits_unavailable") || report.QueueTime.Seconds != 0 || len(report.LaneResidence.Aging.Items) != 0 {
					t.Fatalf("invented unrecorded residence %#v", report)
				}
				return
			}
			issue, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "population", State: "Todo"}, base)
			if err != nil {
				t.Fatal(err)
			}
			for i := range maxAnalyticsPopulation {
				if kind == "issues" {
					_, err = createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: strconv.Itoa(i), State: "Todo"}, base)
				} else {
					from, to := "Todo", "In Progress"
					if i%2 == 1 {
						from, to = to, from
					}
					err = appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: to}, base.Add(time.Duration(i+1)*time.Second))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "events" {
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, base.Add(1001*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			report, err := readNativeAnalytics(t.Context(), tx, scope, operatortool.AnalyticsRequest{Limit: 1}, window)
			if err != nil {
				t.Fatal(err)
			}
			residence := report.LaneResidence
			if residence.Partial || report.QueueTime.Partial || slices.Contains(report.Unavailable, "lane_residence_complete_population") {
				t.Fatalf("bounded population %#v", residence)
			}
			if kind == "events" && (len(residence.Aging.Items) != 1 || report.QueueTime.Seconds != 501 || residence.EventsObserved != 1001) {
				t.Fatalf("extrapolated clipped history %#v", residence)
			}
			if kind == "issues" && residence.IssuesObserved != 1001 {
				t.Fatalf("issues observed %d", residence.IssuesObserved)
			}
		})
	}
}
