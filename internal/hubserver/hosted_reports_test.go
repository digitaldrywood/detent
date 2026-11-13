package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

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
						scope := hubscope.NativeScope{Organization: org, Project: project, Credential: credential}
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
					for _, step := range seed.steps {
						switch terminal := step.to == "Done" || step.to == "Cancelled"; {
						case !terminal:
							issue.ClosedAt = nil
						case issue.ClosedAt == nil:
							issue.ClosedAt = &step.at
						}
					}
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
				scope := hubscope.NativeScope{Organization: org, Project: project, Credential: credential}
				var refreshed []tracker.NativeWorkItemID
				for cursor := tracker.NativeWorkItemID(""); ; {
					next, processed, err := service.maintainBoardRecord(ctx, org, cursor, refreshed...)
					if err != nil {
						t.Fatal(err)
					}
					if !processed {
						break
					}
					cursor, refreshed = next, append(refreshed, next)
				}
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
