package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

type completionCostFailureReader struct {
	tenantstore.Reader
	err error
}

func (r completionCostFailureReader) ReadCompletionCosts(context.Context, reportstore.CompletionCostQuery) ([]reportstore.CompletionCostFact, error) {
	return nil, r.err
}

func TestReportCurrentUsageIngestionPreservesCoverage(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	ctx := t.Context()
	organization := tracker.OrganizationID("org_report_ingestion")
	project := tracker.ProjectID("project_report_ingestion")
	now := time.Now().UTC().Truncate(time.Microsecond)
	state := "Todo"
	objects := &memoryRunDetails{}
	payload, err := objects.StoreDetail(ctx, string(organization), []byte(`{"attempt_id":"attempt_ingestion"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: state, Dispatchable: true}}}); err != nil {
			return err
		}
		item, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: "wi_ingestion", Number: 1, Title: "Example", WorkflowStateName: &state, CreatedAt: now})
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "machine_ingestion", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		lease, err := w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_ingestion", SessionID: "session_ingestion", LeaseID: "lease_ingestion", TTL: time.Hour})
		if err != nil {
			return err
		}
		_, err = w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: "attempt_ingestion", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run_ingestion", Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: payload.Key})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	participant := tenantNativeRunEventParticipant{}
	for index, check := range []struct {
		name, coverage string
		micros         int64
		known          bool
	}{{"complete", "complete", 2000000, true}, {"partial correction", "partial", 3000000, false}, {"explicit zero", "complete", 0, true}} {
		t.Run(check.name, func(t *testing.T) {
			amount := check.micros
			observation := usagecost.Observation{Provider: "example", ProviderAccount: "runner", SourceID: "attempt_ingestion:model", ResourceID: "attempt_ingestion", AttemptID: "attempt_ingestion", WorkItemID: "wi_ingestion", Model: "example-model", Bucket: "runner_api", Metric: "example-model", Unit: "token", Currency: "USD", Revision: int64(index + 1), From: now.Add(-time.Minute), To: now, AmountMicros: &amount, Coverage: check.coverage, Input: 10, Output: 1}
			raw, err := json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(ctx, string(organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			record := billingstore.CostObservation{OrganizationID: organization, ProjectID: project, Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: now}
			if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
				return participant.ApplyNativeRunEvent(ctx, w, NativeRunEventAuthority{UsageCosts: []billingstore.CostObservation{record}})
			}); err != nil {
				t.Fatal(err)
			}
			service := &Service{config: Config{TenantRepository: repository, Hosted: &hostedplans.HostedConfig{OrganizationID: string(organization)}}}
			usage, err := service.usageRows(ctx, usageWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, []string{string(project)})
			if err != nil || len(usage) != 1 || usage[0].Cost != float64(amount)/1e6 {
				t.Fatalf("usage rows=%+v err=%v", usage, err)
			}
			if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				window := operatortool.AnalyticsWindow{From: now.Truncate(time.Hour), To: now.Truncate(time.Hour).Add(time.Hour), Bucket: time.Hour}
				prior := operatortool.AnalyticsWindow{From: window.From.Add(-time.Hour), To: window.From, Bucket: time.Hour}
				query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: prior.From, To: window.To, Limit: 10}
				facts := []reportstore.OverviewFact{{ProjectID: project, WorkItemID: "wi_ingestion"}}
				result, err := readPostgresReportUsage(ctx, r, query, facts, window, prior)
				if err != nil {
					return err
				}
				if result.CostsAvailable != check.known || result.UsageAvailable != check.known || len(result.Facts) != 1 || result.Facts[0].Cost != float64(amount)/1e6 {
					t.Fatalf("current ingestion result = %+v", result)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPostgresReportRendersMissingImportsAcrossTenants(t *testing.T) {
	t.Parallel()
	repository, _ := projectRecordPostgresWithAdmin(t, 4)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	project := tracker.ProjectID("project_partial_reports")
	state := "Todo"
	objects := &memoryRunDetails{}
	for _, organization := range []tracker.OrganizationID{"org_partial_first", "org_partial_second"} {
		payload, err := objects.StoreDetail(ctx, string(organization), []byte(`{"attempt_id":"attempt_partial"}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: state, Dispatchable: true}}}); err != nil {
				return err
			}
			item, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: "wi_partial", Number: 1, Title: string(organization), WorkflowStateName: &state, ActorKind: new("human"), ActorPrincipalID: new("reporter_partial"), CreatedAt: now})
			if err != nil {
				return err
			}
			if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "machine_partial", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			lease, err := w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "machine_partial", SessionID: "session_partial", LeaseID: "lease_partial", TTL: time.Hour})
			if err != nil {
				return err
			}
			_, err = w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: "attempt_partial", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run_partial", Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: payload.Key})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_partial_first", "org_partial_second"} {
		t.Run(string(organization), func(t *testing.T) {
			service := &Service{config: Config{TenantRepository: repository, Hosted: &hostedplans.HostedConfig{OrganizationID: string(organization)}}}
			out, err := service.readPostgresHostedReport(t.Context(), hubapi.APICredential{}, []string{string(project)}, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}, reportsFilter{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if !out.Analytics.Quality.Partial || out.FirstTry != nil || out.Analytics.Quality.EscapePercent != nil {
				t.Fatalf("unproven historic completeness = %+v", out.Analytics.Quality)
			}
			for _, marker := range []string{"attempt_outcomes_unavailable", "failure_classification", "landed_first_try"} {
				if !slices.Contains(out.Unavailable, marker) {
					t.Errorf("missing marker %q in %v", marker, out.Unavailable)
				}
			}
			if out.OpenWork.Count != 1 {
				t.Fatalf("tenant open work = %+v", out.OpenWork)
			}
			for _, filter := range []reportsFilter{{Models: []string{"unrecorded-model"}}, {Stages: []string{"unrecorded-stage"}}} {
				filtered, err := service.readPostgresHostedReport(t.Context(), hubapi.APICredential{}, []string{string(project)}, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}, filter, false)
				if err != nil {
					t.Fatal(err)
				}
				if filtered.OpenWork.Count != 0 || len(filtered.OpenWork.Items) != 0 {
					t.Fatalf("open work escaped report filter %+v: %+v", filter, filtered.OpenWork)
				}
			}
		})
	}
	for _, organization := range []tracker.OrganizationID{"org_partial_first", "org_partial_second"} {
		t.Run(string(organization)+"/reporter entry points", func(t *testing.T) {
			const principal = "reporter_partial"
			const user = "user_partial"
			payload, err := objects.StoreDetail(t.Context(), string(organization), []byte(`{"attempt_id":"attempt_private"}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Name: "Reporter", Hash: apikey.HashToken(string(organization) + principal), Scope: "operator", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: user, PrincipalID: principal, MembershipID: "membership_partial", Email: "reporter@example.test", Role: "reporter", Active: true, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: user, ProjectID: project}); err != nil {
					return err
				}
				if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: principal, ProjectID: project}); err != nil {
					return err
				}
				if err := w.SetWorkItemLabels(t.Context(), project, "wi_partial", []string{"own-label"}); err != nil {
					return err
				}
				private, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: "wi_private", Number: 2, Title: "Private", WorkflowStateName: &state, ActorKind: new("human"), ActorPrincipalID: new("another_reporter"), CreatedAt: now})
				if err != nil {
					return err
				}
				if err := w.SetWorkItemLabels(t.Context(), project, "wi_private", []string{"private-label"}); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_private", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: private.ID, ExpectedRevision: private.Revision, MachineID: "machine_private", SessionID: "session_private", LeaseID: "lease_private", TTL: time.Hour})
				if err != nil {
					return err
				}
				_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt_private", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run_private", Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: payload.Key})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			service := &Service{database: &database{}, config: Config{TenantRepository: repository, Hosted: &hostedplans.HostedConfig{OrganizationID: string(organization)}, Clock: func() time.Time { return now }}}
			credential := hubapi.APICredential{Hosted: &auth.HostedIdentity{Subject: user}, HostedRole: "reporter", HostedPrincipal: principal, ID: principal, Scope: hubapi.APIScopeOperator}
			identity := operatortool.Identity{PrincipalID: principal, OrganizationID: string(organization), CredentialID: principal}
			ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(_ context.Context, requirement operatortool.Requirement) error {
					if requirement.ProjectID != string(project) {
						return operatortool.ErrAccessDenied
					}
					return nil
				}}, nil
			}})
			window := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
			assertReport := func(t *testing.T, report hostedReportsReport) {
				t.Helper()
				if report.Analytics.Quality.WorkedItems != 1 || report.OpenWork.Count != 1 || report.Titles["wi_partial"] != string(organization) {
					t.Fatalf("reporter tenant population: worked=%d open=%+v titles=%v", report.Analytics.Quality.WorkedItems, report.OpenWork, report.Titles)
				}
				if len(report.OpenWork.Items) != 0 || !slices.Contains(report.Unavailable, "current_open_work_age_partial") {
					t.Fatalf("missing-history age was presented as known: open=%+v unavailable=%v", report.OpenWork, report.Unavailable)
				}
				if _, exists := report.Titles["wi_private"]; exists {
					t.Fatal("reporter report includes another reporter's work item")
				}
			}
			for _, test := range []struct {
				name string
				read func(*testing.T)
			}{
				{"project report", func(t *testing.T) {
					report, readable, err := service.readProjectReport(ctx, credential, string(project), window, reportsFilter{})
					if err != nil || !readable {
						t.Fatalf("report readable=%t error=%v", readable, err)
					}
					assertReport(t, report)
				}},
				{"report reads committed state while a PostgreSQL writer is held", func(t *testing.T) {
					want, readable, err := service.readProjectReport(ctx, credential, string(project), window, reportsFilter{})
					if err != nil || !readable {
						t.Fatalf("report before held PostgreSQL writer readable=%t error=%v", readable, err)
					}
					ready, finished := make(chan struct{}), make(chan struct{})
					var writerError error
					release := make(chan struct{})
					abort := errors.New("rollback held report writer")
					go func() {
						defer close(finished)
						writerError = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							item, err := w.WorkItem(t.Context(), project, "wi_partial")
							if err != nil {
								return err
							}
							item.Title = "Uncommitted report title"
							if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
								return err
							}
							close(ready)
							<-release
							return abort
						})
					}()
					t.Cleanup(func() {
						close(release)
						<-finished
						if !errors.Is(writerError, abort) {
							t.Errorf("held writer rollback=%v", writerError)
						}
					})
					select {
					case <-ready:
					case <-finished:
						t.Fatalf("writer exited before holding the row: %v", writerError)
					}
					report, readable, err := service.readProjectReport(ctx, credential, string(project), window, reportsFilter{})
					if err != nil || !readable {
						t.Fatalf("report under held PostgreSQL writer readable=%t error=%v", readable, err)
					}
					if !reflect.DeepEqual(report, want) {
						t.Fatalf("report changed beside held PostgreSQL writer: got%+v want%+v", report, want)
					}
					assertReport(t, report)
				}},
				{"revoked project grant", func(t *testing.T) {
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.DeleteAccessProjectGrant(t.Context(), user, project)
					}); err != nil {
						t.Fatal(err)
					}
					restored := false
					t.Cleanup(func() {
						if restored {
							return
						}
						cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
						defer cancel()
						if err := repository.WriteTenant(cleanup, organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.SetAccessProjectGrant(cleanup, accessstore.ProjectGrant{OrganizationID: organization, UserID: user, ProjectID: project})
						}); err != nil {
							t.Error(err)
						}
					})
					report, readable, err := service.readProjectReport(ctx, credential, string(project), window, reportsFilter{})
					if err != nil || readable || len(report.Titles) != 0 || report.Completion.Done != 0 || report.OpenWork.Count != 0 {
						t.Fatalf("revoked report readable=%t error=%v data=%+v", readable, err, report)
					}
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: user, ProjectID: project})
					}); err != nil {
						t.Fatal(err)
					}
					restored = true
					report, readable, err = service.readProjectReport(ctx, credential, string(project), window, reportsFilter{})
					if err != nil || !readable {
						t.Fatalf("regranted report readable=%t error=%v", readable, err)
					}
					assertReport(t, report)
				}},
				{"missing report owner", func(t *testing.T) {
					unavailable := &Service{database: &database{}, config: Config{Hosted: service.config.Hosted}}
					if _, readable, err := unavailable.readProjectReport(ctx, credential, string(project), window, reportsFilter{}); err == nil || readable {
						t.Fatalf("missing report owner readable=%t error=%v", readable, err)
					}
				}},
				{"missing facet owner", func(t *testing.T) {
					unavailable := &Service{database: &database{}, config: Config{Hosted: service.config.Hosted}}
					if _, err := unavailable.readReportsFacets(ctx, []string{string(project)}, window); err == nil {
						t.Fatal("missing facet owner served a report")
					}
				}},
				{"missing coordinator report owner", func(t *testing.T) {
					unavailable := &Service{database: &database{}, config: Config{Hosted: service.config.Hosted, Clock: func() time.Time { return now }}}
					if _, err := unavailable.coordinatorScreenReport(ctx, conversationRecord{ProjectID: project}, credential, runner.AgentToolCall{Name: "get_project_reports", Arguments: json.RawMessage(`{"range":"24h"}`)}); err == nil {
						t.Fatal("missing coordinator report owner served a report")
					}
				}},
				{"unreadable project", func(t *testing.T) {
					_, readable, err := service.readProjectReport(ctx, credential, "project_ungranted", window, reportsFilter{})
					if err != nil || readable {
						t.Fatalf("ungranted report readable=%t error=%v", readable, err)
					}
				}},
				{"report facets", func(t *testing.T) {
					scope := hubscope.NativeScope{Organization: organization, Project: project, Credential: credential}
					facets, err := service.readReportsFacets(reporterReadContext(ctx, scope), []string{string(project)}, window)
					if err != nil || len(facets.Labels) != 1 || facets.Labels[0] != "own-label" || len(facets.FiledBy) != 1 || facets.FiledBy[0].ID != principal {
						t.Fatalf("reporter facets=%+v error=%v", facets, err)
					}
				}},
				{"coordinator report", func(t *testing.T) {
					coordinator := &Service{database: &database{}, config: Config{TenantRepository: repository, Hosted: &hostedplans.HostedConfig{OrganizationID: string(organization)}, Clock: func() time.Time { return now.Add(3 * time.Hour) }}}
					value, err := coordinator.coordinatorScreenReport(ctx, conversationRecord{ProjectID: project}, credential, runner.AgentToolCall{Name: "get_project_reports", Arguments: json.RawMessage(`{"range":"24h"}`)})
					if err != nil {
						t.Fatal(err)
					}
					report, ok := value.(hostedReportsReport)
					if !ok {
						t.Fatalf("coordinator report type=%T", value)
					}
					assertReport(t, report)
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					defer func() {
						if recovered := recover(); recovered != nil {
							t.Errorf("report read panicked: %v", recovered)
						}
					}()
					test.read(t)
				})
			}
			t.Run("completed report retains prior-window spend", func(t *testing.T) {
				closed := now.Add(2 * time.Hour)
				var attempt leasestore.Attempt
				var item corestore.WorkItem
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					attempt, err = r.Attempt(t.Context(), "attempt_partial")
					if err != nil {
						return err
					}
					item, err = r.WorkItem(t.Context(), project, "wi_partial")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				fence := leasestore.Fence{LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken}
				run := tracker.NativeRunData{AttemptID: attempt.ID, RunID: attempt.RunID, LeaseID: tracker.LeaseID(attempt.LeaseID), FencingToken: tracker.FencingToken(attempt.FencingToken), Sequence: 2, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "implement", ResolvedModel: agentidentity.NewValue("example-model", agentidentity.ProvenanceRuntime)}}}
				raw, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				runRef, err := objects.StoreDetail(t.Context(), string(organization), raw)
				if err != nil {
					t.Fatal(err)
				}
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision + 1), Profile: "native"}, Title: item.Title, Body: "Example accepted report source", State: "Done", Terminal: true, Actor: tracker.Actor{Kind: "human", PrincipalID: principal}, CreatedAt: item.CreatedAt, UpdatedAt: closed, ClosedAt: &closed}
				raw, err = json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				content := eventstore.NewContent(objects)
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: item.NativeID, RecordID: string(item.NativeID), Revision: int64(issue.Revision)}, raw)
				if err != nil {
					t.Fatal(err)
				}
				events := []eventstore.Event{}
				for _, event := range []tracker.CollaborationEvent{
					{ID: "partial_created", Type: "issue.created", RecordedAt: now, Data: tracker.CollaborationData{ToState: "Todo"}},
					{ID: "partial_finished", Type: "run.finished", RecordedAt: closed.Add(-time.Second), Data: tracker.CollaborationData{Run: &run}},
					{ID: "partial_closed", Type: "workflow.transitioned", RecordedAt: closed, Data: tracker.CollaborationData{FromState: "Todo", ToState: "Done"}},
				} {
					event.OrganizationID, event.ProjectID, event.AggregateID, event.SchemaVersion, event.Actor = organization, project, item.NativeID, 1, issue.Actor
					raw, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					prepared, err := content.PrepareEvent(t.Context(), organization, raw)
					if err != nil {
						t.Fatal(err)
					}
					events = append(events, prepared)
				}
				observation := usagecost.Observation{Provider: "example", ProviderAccount: "runner", SourceID: "attempt_partial:model", ResourceID: attempt.ID, AttemptID: attempt.ID, WorkItemID: string(item.NativeID), Model: "example-model", Bucket: "runner_api", Metric: "example-model", Unit: "token", Currency: "USD", Revision: 1, From: now.Add(-time.Minute), To: now, AmountMicros: new(int64(5000000)), Coverage: "complete", Input: 10, Output: 1}
				raw, err = json.Marshal(observation)
				if err != nil {
					t.Fatal(err)
				}
				costRef, err := objects.StoreDetail(t.Context(), string(organization), raw)
				if err != nil {
					t.Fatal(err)
				}
				usage := billingstore.CostObservation{OrganizationID: organization, ProjectID: project, Observation: observation, Reference: billingstore.AuditReferenceFromArchive(costRef), ReceivedAt: now}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: attempt.ID, RunID: attempt.RunID, Fence: fence, Sequence: 2, Status: "succeeded", StartedAt: attempt.StartedAt, PayloadObjectKey: runRef.Key, PayloadReference: runRef}); err != nil {
						return err
					}
					if _, err := w.Release(t.Context(), fence, "completed"); err != nil {
						return err
					}
					configuration, err := w.ProjectRecord(t.Context(), project)
					if err != nil {
						return err
					}
					configuration.States = append(configuration.States, tracker.NativeState{Name: "Done", Terminal: true})
					if _, err := w.UpdateProjectRecord(t.Context(), configuration); err != nil {
						return err
					}
					item.WorkflowStateName, item.UpdatedAt = &issue.State, closed
					updated, err := w.UpdateWorkItem(t.Context(), item)
					if err != nil {
						return err
					}
					if updated.Revision != version.Revision {
						t.Fatalf("accepted report source revision=%d canonical=%d", version.Revision, updated.Revision)
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					for _, event := range events {
						if _, err := w.AppendEvent(t.Context(), event); err != nil {
							return err
						}
					}
					return (tenantNativeRunEventParticipant{}).ApplyNativeRunEvent(t.Context(), w, NativeRunEventAuthority{UsageCosts: []billingstore.CostObservation{usage}})
				}); err != nil {
					t.Fatal(err)
				}
				window := operatortool.AnalyticsWindow{From: now.Add(time.Hour), To: now.Add(3 * time.Hour), Bucket: time.Hour}
				report, readable, err := service.readProjectReport(ctx, credential, string(project), window, reportsFilter{})
				if err != nil || !readable {
					t.Fatalf("completed reporter report readable=%t error=%v", readable, err)
				}
				if report.Completion.Done != 1 || report.Completion.Cost != 5 || report.Previous.Cost != 0 || report.Cost != 0 || report.PriorCost == nil || *report.PriorCost != 5 || report.Completion.CostP50 == nil || *report.Completion.CostP50 != 5 || report.Completion.CostP90 == nil || *report.Completion.CostP90 != 5 || slices.Contains(report.Unavailable, "completion_usage") || report.Titles["wi_partial"] != string(organization) {
					t.Fatalf("public completion lifetime/window costs=%+v current%v prior%v titles%v unavailable%v", report.Completion, report.Cost, report.PriorCost, report.Titles, report.Unavailable)
				}
				if _, exists := report.Titles["wi_private"]; exists {
					t.Fatal("completion lifetime report includes another reporter's item")
				}
			})

		})
	}
}
