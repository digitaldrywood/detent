package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
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
	t.Run("incremental residence preserves queue boundaries and partial history", func(t *testing.T) {
		transition := func(hour int, from, to string) analyticsResidenceEvent {
			return analyticsResidenceEvent{at: at(hour), kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: from, ToState: to}}
		}
		claim := func(hour int) analyticsResidenceEvent {
			return analyticsResidenceEvent{at: at(hour), kind: "scheduler.decision"}
		}
		todo := analyticsResidenceSpan{lane: "Todo", from: base, to: at(2), queueTo: at(2)}
		rework := analyticsResidenceSpan{lane: "Rework", from: at(2), to: at(5), queueTo: at(3)}
		for _, check := range []struct {
			name    string
			events  []analyticsResidenceEvent
			spans   []analyticsResidenceSpan
			partial bool
		}{
			{name: "first rework claim ends queue time", events: []analyticsResidenceEvent{transition(2, "Todo", "Rework"), claim(3), claim(4), transition(5, "Rework", "Done")}, spans: []analyticsResidenceSpan{todo, rework}},
			{name: "out of order history invalidates completion", events: []analyticsResidenceEvent{transition(2, "Todo", "Rework"), claim(3), claim(1), transition(5, "Rework", "Done")}, spans: []analyticsResidenceSpan{todo, rework}, partial: true},
			{name: "missing destination invalidates completion", events: []analyticsResidenceEvent{transition(2, "Todo", "Rework"), claim(3), transition(4, "Rework", ""), transition(5, "Rework", "Done")}, spans: []analyticsResidenceSpan{todo, rework}, partial: true},
			{name: "mismatched source does not invent residence", events: []analyticsResidenceEvent{transition(2, "Todo", "Rework"), claim(3), transition(5, "In Progress", "Done")}, spans: []analyticsResidenceSpan{todo}, partial: true},
		} {
			t.Run(check.name, func(t *testing.T) {
				fold := newAnalyticsResidenceFold(analyticsResidenceIssue{id: "streamed", created: base, initial: "Todo"}, states, at(8))
				var spans []analyticsResidenceSpan
				done, reworked := 0, 0
				for _, event := range check.events {
					delta := fold.append(event)
					if delta.hasSpan {
						spans = append(spans, delta.span)
					}
					if delta.done {
						done++
					}
					if delta.rework {
						reworked++
					}
				}
				if !reflect.DeepEqual(spans, check.spans) || fold.issue.partial != check.partial || fold.state != "Done" || fold.entered != at(5) || fold.started == nil || *fold.started != base || done != 1 || reworked != 1 {
					t.Fatalf("incremental residence spans=%+v partial=%t state=%q entered=%v started=%v done=%d reworked=%d", spans, fold.issue.partial, fold.state, fold.entered, fold.started, done, reworked)
				}
				final, ok := fold.finish()
				if ok == check.partial {
					t.Fatalf("partial=%t final residence available=%t", check.partial, ok)
				}
				if ok && final != (analyticsResidenceSpan{lane: "Done", from: at(5), to: at(8), queueTo: at(8)}) {
					t.Fatalf("final residence=%+v", final)
				}
			})
		}
	})

}

func TestReportsShippedPopulation(t *testing.T) {
	t.Run("PostgreSQL report board and work include archived shipped items", func(t *testing.T) {
		to := time.Date(2026, time.November, 6, 18, 0, 0, 0, time.UTC)
		from := to.Add(-24 * time.Hour)
		created := from.Add(-time.Hour)
		service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return to }})
		repository := service.config.TenantRepository
		content := eventstore.NewContent(service.runDetailStore())
		for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			t.Run(string(org), func(t *testing.T) {
				ctx := t.Context()
				for _, archivedOnly := range []bool{false, true} {
					t.Run(fmt.Sprintf("all archived %t", archivedOnly), func(t *testing.T) {
						fixture := newNativeFixture(t, service, org, fmt.Sprintf("Example reports all archived %t", archivedOnly))
						project := fixture.project.ID
						if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
							record, err := w.ProjectRecord(ctx, project)
							if err != nil {
								return err
							}
							record.States = append(record.States, tracker.NativeState{Name: "Cancelled", Terminal: true})
							_, err = w.UpdateProjectRecord(ctx, record)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						for i := range 8 {
							state := "Done"
							if i >= 6 {
								state = "Cancelled"
							}
							id := tracker.NativeWorkItemID(fmt.Sprintf("wi_shipped_%t_%d", archivedOnly, i))
							closed := to.Add(-time.Duration(i+1) * time.Hour)
							issue := tracker.NativeIssue{
								NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: i + 1, Revision: 1, Profile: "native"},
								Title:           fmt.Sprintf("Example shipped item %d", i), Body: "Example body", State: state, Terminal: true, Archived: archivedOnly || i == 0,
								Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: created, UpdatedAt: closed, ClosedAt: &closed,
							}
							raw, err := json.Marshal(issue)
							if err != nil {
								t.Fatal(err)
							}
							version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
							if err != nil {
								t.Fatal(err)
							}
							events := []eventstore.Event{}
							for _, event := range []tracker.CollaborationEvent{
								{ID: string(id) + "_created", Type: "issue.created", RecordedAt: created},
								{ID: string(id) + "_closed", Type: "workflow.transitioned", RecordedAt: closed, Data: tracker.CollaborationData{FromState: "Todo", ToState: state}},
							} {
								event.OrganizationID, event.ProjectID, event.AggregateID, event.SchemaVersion, event.Actor = org, project, id, 1, issue.Actor
								raw, err := json.Marshal(event)
								if err != nil {
									t.Fatal(err)
								}
								prepared, err := content.PrepareEvent(ctx, org, raw)
								if err != nil {
									t.Fatal(err)
								}
								events = append(events, prepared)
							}
							if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
								if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: int64(i + 1), Title: issue.Title, Archived: issue.Archived, WorkflowStateName: &state, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, CreatedAt: created, UpdatedAt: closed}); err != nil {
									return err
								}
								if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
									return err
								}
								for _, event := range events {
									if _, err := w.AppendEvent(ctx, event); err != nil {
										return err
									}
								}
								return nil
							}); err != nil {
								t.Fatal(err)
							}
							if err := service.refreshBoardRecord(ctx, org, viewstore.BoardCandidate{ProjectID: project, WorkItemID: id}, to); err != nil {
								t.Fatal(err)
							}
						}
						credential, status, err := service.authenticateTenantAPIHash(ctx, org, apikey.HashToken(fixture.token), "", "")
						if err != nil {
							t.Fatalf("fixture credential status=%d: %v", status, err)
						}
						reportService := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org)}}}
						report, err := reportService.readPostgresHostedReport(ctx, credential, []string{string(project)}, operatortool.AnalyticsWindow{From: from, To: to, Bucket: time.Hour}, reportsFilter{}, true)
						if err != nil {
							t.Fatal(err)
						}
						if report.Completion.Done != 6 || report.Completion.Cancelled != 2 {
							t.Fatalf("24h report completed=%d cancelled=%d, want 6 and 2", report.Completion.Done, report.Completion.Cancelled)
						}
						scope := nativeScope{organization: org, project: project, credential: credential}
						board, err := service.readWorkBoardProject(ctx, scope, savedWorkState{View: "board", Lanes: []string{"Todo"}, CompletedWindow: "24h"})
						if err != nil {
							t.Fatal(err)
						}
						work, err := service.readBoardRecordsFor(ctx, scope, url.Values{"include": {"work"}, "completed_window": {"24h"}}, true)
						if err != nil {
							t.Fatal(err)
						}
						for name, summary := range map[string]*tracker.NativeWorkSummary{"board": board.Work, "work": work.Work} {
							if summary == nil || summary.Completed != report.Completion.Done || summary.Cancelled != report.Completion.Cancelled {
								t.Errorf("%s summary=%+v, reports shipped=%d cancelled=%d", name, summary, report.Completion.Done, report.Completion.Cancelled)
							}
						}
					})
				}
			})
		}
	})
	t.Run("PostgreSQL lifetime shipped population and prior boundaries", func(t *testing.T) {
		to := time.Date(2026, time.November, 6, 18, 0, 0, 0, time.UTC)
		w := operatortool.AnalyticsWindow{From: to.Add(-48 * time.Hour), To: to, Bucket: time.Hour}
		service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return to }})
		repository := service.config.TenantRepository
		content := eventstore.NewContent(service.runDetailStore())
		for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			t.Run(string(org), func(t *testing.T) {
				ctx := t.Context()
				fixture := newNativeFixture(t, service, org, "Example shipped history")
				project := fixture.project.ID
				encode := func(value any) []byte {
					raw, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					return raw
				}
				type step struct {
					at       time.Time
					from, to string
				}
				type seed struct {
					name, state string
					archived    bool
					steps       []step
				}
				seeds := []seed{
					{"reclosed", "Done", false, []step{{to.Add(-40 * time.Hour), "Todo", "Done"}, {to.Add(-39 * time.Hour), "Done", "Todo"}, {to.Add(-10 * time.Hour), "Todo", "Done"}}},
					{"window start", "Done", false, []step{{w.From, "Todo", "Done"}}},
					{"just outside", "Done", false, []step{{w.From.Add(-time.Second), "Todo", "Done"}}},
					{"cancelled", "Cancelled", false, []step{{to.Add(-5 * time.Hour), "Todo", "Cancelled"}}},
					{"archived", "Done", true, []step{{to.Add(-6 * time.Hour), "Todo", "Done"}}},
					{"reopened", "Todo", false, []step{{to.Add(-30 * time.Hour), "Todo", "Done"}, {to.Add(-29 * time.Hour), "Done", "Todo"}}},
					{"then cancelled", "Cancelled", false, []step{{to.Add(-15 * time.Hour), "Todo", "Done"}, {to.Add(-14 * time.Hour), "Done", "Cancelled"}}},
				}
				for i := range 4 {
					steps := []step{}
					for j := range 300 {
						from, next := "Todo", "In Progress"
						if j%2 == 1 {
							from, next = next, from
						}
						steps = append(steps, step{to.Add(-46*time.Hour + time.Duration(i*300+j)*time.Minute), from, next})
					}
					seeds = append(seeds, seed{fmt.Sprintf("busy %d", i), "Done", false, append(steps, step{to.Add(-20*time.Hour + time.Duration(i)*time.Minute), "Todo", "Done"})})
				}
				if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					configuration, err := w.ProjectRecord(ctx, project)
					if err != nil {
						return err
					}
					configuration.States = append(configuration.States, tracker.NativeState{Name: "Cancelled", Terminal: true})
					if _, err := w.UpdateProjectRecord(ctx, configuration); err != nil {
						return err
					}
					return w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: org, ID: string(project) + "_machine", Hostname: "example", Capacity: 1, Version: "example"})
				}); err != nil {
					t.Fatal(err)
				}
				ids := map[string]tracker.NativeWorkItemID{}
				for index, seed := range seeds {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_shipped_%d", index))
					ids[seed.name] = id
					created := to.Add(-100 * time.Hour)
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: index + 1, Revision: 1, Profile: "native"}, Title: seed.name, Body: nativeContractTestBody, State: seed.state, Terminal: seed.state == "Done" || seed.state == "Cancelled", Archived: seed.archived, Actor: tracker.Actor{Kind: "human", PrincipalID: "owner"}, CreatedAt: created, UpdatedAt: seed.steps[len(seed.steps)-1].at}
					version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: 1}, encode(issue))
					if err != nil {
						t.Fatal(err)
					}
					events := []eventstore.Event{}
					values := []tracker.CollaborationEvent{{ID: string(id) + "_created", OrganizationID: org, ProjectID: project, AggregateID: id, Type: "issue.created", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: created}}
					for n, step := range seed.steps {
						values = append(values, tracker.CollaborationEvent{ID: fmt.Sprintf("%s_step_%d", id, n), OrganizationID: org, ProjectID: project, AggregateID: id, Type: "workflow.transitioned", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: step.at, Data: tracker.CollaborationData{FromState: step.from, ToState: step.to}})
					}
					for _, value := range values {
						event, err := content.PrepareEvent(ctx, org, encode(value))
						if err != nil {
							t.Fatal(err)
						}
						events = append(events, event)
					}
					if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: int64(index + 1), Title: issue.Title, Archived: issue.Archived, WorkflowStateName: &issue.State, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, CreatedAt: created, UpdatedAt: issue.UpdatedAt}); err != nil {
							return err
						}
						if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
							return err
						}
						for _, event := range events {
							if _, err := w.AppendEvent(ctx, event); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := service.refreshBoardRecord(ctx, org, viewstore.BoardCandidate{ProjectID: project, WorkItemID: id}, to); err != nil {
						t.Fatal(err)
					}
				}
				seedAttempt := func(id tracker.NativeWorkItemID, name string, at time.Time, outcome string, amount *int64) {
					t.Helper()
					var lease leasestore.Lease
					if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						item, err := w.WorkItem(ctx, project, id)
						if err != nil {
							return err
						}
						lease, err = w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(project) + "_machine", SessionID: name + "_session", LeaseID: name + "_lease", TTL: time.Hour})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
					run := tracker.NativeRunData{AttemptID: name, RunID: name + "_run", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: outcome, Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "implement", ResolvedModel: agentidentity.NewValue("example-model", agentidentity.ProvenanceRuntime)}}}
					if outcome == "failed" {
						run.TerminalFailure = &tracker.NativeTerminalFailure{Error: "Example failure"}
					}
					ref, err := content.PreparePayload(ctx, org, encode(run))
					if err != nil {
						t.Fatal(err)
					}
					event, err := content.PrepareEvent(ctx, org, encode(tracker.CollaborationEvent{ID: name + "_event", OrganizationID: org, ProjectID: project, AggregateID: id, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "system", PrincipalID: "example-runner"}, RecordedAt: at, Data: tracker.CollaborationData{Run: &run}}))
					if err != nil {
						t.Fatal(err)
					}
					var cost billingstore.CostObservation
					if amount != nil {
						observation := usagecost.Observation{Provider: "example", ProviderAccount: "example", SourceID: name, ResourceID: name, AttemptID: name, WorkItemID: string(id), Model: "example-model", Bucket: "runner_api", Metric: "tokens", Unit: "tokens", Currency: "USD", Revision: 1, From: at.Add(-time.Minute), To: at, AmountMicros: amount, Coverage: "complete"}
						costRef, err := service.runDetailStore().StoreDetail(ctx, string(org), encode(observation))
						if err != nil {
							t.Fatal(err)
						}
						cost = billingstore.CostObservation{OrganizationID: org, ProjectID: project, Observation: observation, Reference: billingstore.AuditReferenceFromArchive(costRef), ReceivedAt: at}
					}
					if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: run.AttemptID, RunID: run.RunID, Fence: fence, Sequence: 1, Status: outcome, StartedAt: at.Add(-time.Minute), PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
							return err
						}
						if _, err := w.AppendEvent(ctx, event); err != nil {
							return err
						}
						if amount != nil {
							if _, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{cost}); err != nil {
								return err
							}
							if err := w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{cost}); err != nil {
								return err
							}
						}
						_, err := w.Release(ctx, fence, "completed")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				seedAttempt(ids["reclosed"], "shipped_cost", w.From.Add(-30*time.Hour), "succeeded", new(int64(5000000)))
				seedAttempt(ids["cancelled"], "prior_failure_cost", w.From.Add(-2*time.Hour), "failed", new(int64(2000000)))
				credential, status, err := service.authenticateTenantAPIHash(ctx, org, apikey.HashToken(fixture.token), "", "")
				if err != nil {
					t.Fatalf("fixture credential status=%d: %v", status, err)
				}
				reader := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org)}}}
				report, err := reader.readPostgresHostedReport(ctx, credential, []string{string(project)}, w, reportsFilter{}, true)
				if err != nil {
					t.Fatal(err)
				}
				throughput := 0
				for _, bucket := range report.Throughput {
					throughput += bucket.Done
				}
				if c := report.Completion; c.Done != 7 || c.Cancelled != 2 || c.Partial || c.System.Count != 7 || throughput != 7 || c.Cost != 5 {
					t.Fatalf("completion=%+v throughput=%d want7shipped2cancelled lifetime5", c, throughput)
				}
				if c := report.Completion; c.CostP50 == nil || c.CostP90 == nil || *c.CostP50 != 0 || *c.CostP90 <= 0 || *c.CostP90 > 5 {
					t.Fatalf("knownzero shipped spend quantiles=%v/%v", c.CostP50, c.CostP90)
				}
				if p := report.Previous; p.Done != 1 || p.Cancelled != 0 || p.Partial || p.Cost != 0 {
					t.Fatalf("previous=%+v", p)
				}
				if report.Cancelled != 2 || report.PriorCost == nil || *report.PriorCost != 7 || report.PriorFailed == nil || *report.PriorFailed != 1 || report.PriorFirst != nil {
					t.Fatalf("prior cost/failure/first=%+v", report)
				}
				for _, window := range []struct {
					from, to time.Time
					want     bool
				}{{w.From.Add(-time.Hour), w.From, false}, {w.From, w.From.Add(time.Hour), true}} {
					if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
						facts, err := r.(reportstore.Reader).ReadOverviewFacts(ctx, reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: window.from, Split: window.from, To: window.to, Limit: 100})
						if err != nil {
							return err
						}
						found := slices.ContainsFunc(facts, func(f reportstore.OverviewFact) bool {
							return f.WorkItemID == ids["window start"] && f.CompletedAt != nil && !f.CompletedAt.Before(window.from) && f.CompletedAt.Before(window.to)
						})
						if found != window.want {
							t.Fatalf("close boundary[%s,%s)=%t want%t", window.from, window.to, found, window.want)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				day := operatortool.AnalyticsWindow{From: to.Add(-24 * time.Hour), To: to, Bucket: time.Hour}
				dayReport, err := reader.readPostgresHostedReport(ctx, credential, []string{string(project)}, day, reportsFilter{}, true)
				if err != nil {
					t.Fatal(err)
				}
				if dayReport.Completion.Done != 6 || dayReport.Completion.Cancelled != 2 {
					t.Fatalf("24h report=%+v", dayReport.Completion)
				}
				scope := nativeScope{organization: org, project: project, credential: credential}
				refreshNativeDuplicateSearchFixture(t, service, org)
				board, err := service.readWorkBoardProject(ctx, scope, savedWorkState{View: "board", Lanes: []string{"Todo"}, CompletedWindow: "24h"})
				if err != nil {
					t.Fatal(err)
				}
				work, err := service.readBoardRecordsFor(ctx, scope, url.Values{"include": {"work"}, "completed_window": {"24h"}}, true)
				if err != nil {
					t.Fatal(err)
				}
				for name, summary := range map[string]*tracker.NativeWorkSummary{"board": board.Work, "work": work.Work} {
					if summary == nil || summary.Completed != dayReport.Completion.Done || summary.Cancelled != dayReport.Completion.Cancelled {
						t.Fatalf("%s summary=%+v reports=%+v", name, summary, dayReport.Completion)
					}
				}
				seedAttempt(ids["busy 0"], "unknown_completion_cost", to.Add(-21*time.Hour), "succeeded", nil)
				unknown, err := reader.readPostgresHostedReport(ctx, credential, []string{string(project)}, w, reportsFilter{}, true)
				if err != nil {
					t.Fatal(err)
				}
				if unknown.Completion.Done != 7 || !unknown.Completion.Partial || unknown.Completion.CostP50 != nil || unknown.Completion.CostP90 != nil || !slices.Contains(unknown.Unavailable, "completion_usage") {
					t.Fatalf("unproven attempt cost masquerades as knownzero: %+v unavailable%v", unknown.Completion, unknown.Unavailable)
				}
			})
		}
	})
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
	now := time.Date(2026, time.November, 6, 18, 0, 0, 0, time.UTC)
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return now.Add(-96 * time.Hour) }})
	repository := service.config.TenantRepository
	content := eventstore.NewContent(service.runDetailStore())
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		for _, tt := range []struct {
			name, role string
			receipt    *tracker.NativeLandingReceipt
			want       *float64
		}{
			{name: "first try", role: "code", want: reportsPercent(100)},
			{name: "conflict before reporting window", role: "merge", receipt: &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: now.Add(-72 * time.Hour)}, want: reportsPercent(0)},
			{name: "unknown earlier merge cannot imply first try", role: "merge"},
		} {
			t.Run(string(org)+"/"+tt.name, func(t *testing.T) {
				ctx := t.Context()
				fixture := newNativeFixture(t, service, org, "Example "+tt.name)
				project := fixture.project.ID
				issue := fixture.create(t, "Example merge history")
				changeID, versionID := "change_"+string(project), "version_"+string(project)
				if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: org, ID: string(project) + "_machine", Hostname: "example", Capacity: 1, Version: "example"})
				}); err != nil {
					t.Fatal(err)
				}
				for index, entry := range []struct {
					role    string
					at      time.Time
					receipt *tracker.NativeLandingReceipt
				}{
					{tt.role, now.Add(-72 * time.Hour), tt.receipt},
					{"merge", now, &tracker.NativeLandingReceipt{Landed: true, ChangeID: changeID, VersionID: versionID, ObservedAt: now}},
				} {
					name := fmt.Sprintf("%s_attempt_%d", project, index)
					var lease leasestore.Lease
					if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						item, err := w.WorkItem(ctx, project, issue.WorkItemID)
						if err != nil {
							return err
						}
						lease, err = w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(project) + "_machine", SessionID: name + "_session", LeaseID: name + "_lease", TTL: time.Hour})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					var landing *tracker.NativeLandingReceipt
					if entry.receipt != nil {
						copy := *entry.receipt
						copy.ChangeID = changeID
						landing = &copy
					}
					outcome := "succeeded"
					if index == 0 && entry.role == "merge" {
						outcome = "failed"
					}
					run := tracker.NativeRunData{AttemptID: name, RunID: name + "_run", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: outcome, Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: entry.role, ResolvedModel: agentidentity.NewValue("example-model", agentidentity.ProvenanceRuntime)}, Landing: landing}}
					if outcome == "failed" {
						run.TerminalFailure = &tracker.NativeTerminalFailure{Error: "Example merge failure"}
					}
					ref, err := content.PreparePayload(ctx, org, encode(run))
					if err != nil {
						t.Fatal(err)
					}
					event, err := content.PrepareEvent(ctx, org, encode(tracker.CollaborationEvent{ID: name + "_event", OrganizationID: org, ProjectID: project, AggregateID: issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: entry.at, Data: tracker.CollaborationData{Run: &run}}))
					if err != nil {
						t.Fatal(err)
					}
					fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
					if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: run.AttemptID, RunID: run.RunID, Fence: fence, Sequence: 1, Status: outcome, StartedAt: entry.at.Add(-time.Minute), PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
							return err
						}
						if _, err := w.AppendEvent(ctx, event); err != nil {
							return err
						}
						_, err := w.Release(ctx, fence, "completed")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				change, err := content.PrepareChange(ctx, org, encode(tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, Title: "Example landed change", Revision: 1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, Landed: &tracker.ChangeLanding{VersionID: versionID, HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40), BaseRef: "develop", LandedAt: now}}))
				if err != nil {
					t.Fatal(err)
				}
				version, err := content.PrepareVersion(ctx, org, encode(tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("c", 40), Repository: "owner/repository", PolicyID: "policy_example", AttemptID: string(project) + "_attempt_1"}, Actor: issue.Actor, CreatedAt: now.Add(-time.Hour)}))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateChange(ctx, change); err != nil {
						return err
					}
					_, err := w.CreateVersion(ctx, version)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				ready, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				abort := errors.New("rollback held first-try writer")
				var writerErr error
				go func() {
					defer close(finished)
					writerErr = repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						item, err := w.WorkItem(ctx, project, issue.WorkItemID)
						if err != nil {
							return err
						}
						item.Title = "Uncommitted first-try title"
						if _, err := w.UpdateWorkItem(ctx, item); err != nil {
							return err
						}
						close(ready)
						<-release
						return abort
					})
				}()
				var releaseOnce sync.Once
				releaseWriter := func() {
					releaseOnce.Do(func() {
						close(release)
						<-finished
						if !errors.Is(writerErr, abort) {
							t.Errorf("held first-try rollback=%v", writerErr)
						}
					})
				}
				defer releaseWriter()
				t.Cleanup(releaseWriter)
				select {
				case <-ready:
				case <-finished:
					t.Fatalf("first-try writer exited before holding row: %v", writerErr)
				}
				w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
				var facts []reportstore.LandingFact
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					facts, err = r.(reportstore.LandingReader).ReadLandingFacts(ctx, reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: w.From, To: w.To, Limit: 10})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if len(facts) != 1 || facts[0].ChangeID != changeID {
					t.Fatalf("typed landing facts=%+v", facts)
				}
				out := hostedReportsReport{}
				applyPostgresReportLandings(&out, facts, w, operatortool.AnalyticsWindow{}, false)
				available := out.FirstTry != nil
				if available != (tt.want != nil) || available && *out.FirstTry != *tt.want {
					t.Fatalf("first try=%v want=%v", out.FirstTry, tt.want)
				}
			})
		}
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
