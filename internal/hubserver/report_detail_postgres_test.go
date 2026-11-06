package hubserver

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestPostgresHostedReportUsesTypedFactsAndMarksMissingSubtabData(t *testing.T) {
	from := time.Date(2026, time.November, 3, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	project := tracker.ProjectID("project_reports")
	item := tracker.NativeWorkItemID("wi_report_done")
	cycle := 1800.0
	w := operatortool.AnalyticsWindow{From: from, To: to, Bucket: time.Hour}
	facts := []reportstore.OverviewFact{{ProjectID: project, WorkItemID: item, Title: "Example report", CreatedAt: from.Add(-30 * time.Minute), CompletedAt: timePointer(from.Add(30 * time.Minute)), CompletedState: "Done", CycleSeconds: &cycle, Worked: true, Reworked: true, ChangesLanded: true}}
	attempts := []reportstore.AttemptFact{{ProjectID: project, WorkItemID: item, AttemptID: "attempt_report", Status: "failed", UpdatedAt: from.Add(45 * time.Minute), OutcomeAt: from.Add(45 * time.Minute)}}
	key := reportstore.CostKey{ProjectID: project, WorkItemID: item, From: from, To: to}
	got := summarizePostgresHostedReport(facts, attempts, map[reportstore.CostKey]float64{key: 4.5}, true, true, false, []string{string(project)}, w, operatortool.AnalyticsWindow{From: from.Add(-time.Hour), To: from}, true)
	if got.Completion.Done != 1 || got.Completion.Cost != 4.5 || got.Cost != 4.5 || got.FailedIssues != 1 || got.Series[0].Done != 1 || got.Series[0].Failed != 1 {
		t.Fatalf("typed report totals = completion %+v cost %.2f failures %d series %+v", got.Completion, got.Cost, got.FailedIssues, got.Series[0])
	}
	if got.Analytics.Quality.WorkedItems != 1 || got.Analytics.Quality.ReworkedItems != 1 || got.Analytics.Quality.LandedVersions != 1 || got.Analytics.Quality.ReworkPercent == nil || *got.Analytics.Quality.ReworkPercent != 100 {
		t.Fatalf("typed report quality = %+v", got.Analytics.Quality)
	}
	if len(got.Rework) != 1 || got.Rework[0].Class != "Not classified" || !slices.Contains(got.Unavailable, "rework_timeline_unavailable") || !slices.Contains(got.Unavailable, "failure_classification") {
		t.Fatalf("unclassified report evidence = %+v unavailable %v", got.Rework, got.Unavailable)
	}
	if !slices.Contains(got.Unavailable, "lane_residence_and_unclaimed_waits_unavailable") || !slices.Contains(got.Unavailable, "stage_usage") || !slices.Contains(got.Unavailable, "runner_usage") {
		t.Fatalf("missing report sources are not explicit: %v", got.Unavailable)
	}

	withoutCosts := summarizePostgresHostedReport(facts, attempts, nil, false, true, false, []string{string(project)}, w, operatortool.AnalyticsWindow{From: from.Add(-time.Hour), To: from}, false)
	if !slices.Contains(withoutCosts.Unavailable, "recorded_usage") || len(withoutCosts.Spend) != 0 {
		t.Fatalf("missing usage was presented as a zero-cost population: unavailable %v spend %+v", withoutCosts.Unavailable, withoutCosts.Spend)
	}
	if completeReportCosts([]reportstore.CostKey{key}, nil) {
		t.Fatal("incomplete typed usage was accepted as complete zero-cost data")
	}
	if completeReportCosts(nil, map[reportstore.CostKey]float64{}) {
		t.Fatal("empty typed usage was accepted as a recorded zero-cost population")
	}

	entered := from.Add(-48 * time.Hour)
	started := from.Add(-24 * time.Hour)
	open, unavailable := postgresReportOpenWork([]reportstore.OpenFact{{ProjectID: project, WorkItemID: "wi_open", State: "Todo", EnteredAt: &entered, FirstWorkedAt: &started}}, to, nil)
	if open.Count != 1 || open.Stuck != 1 || len(open.Items) != 1 || open.Items[0].Hours != 49 || open.Items[0].SinceTodoHours == nil || *open.Items[0].SinceTodoHours != 25 || len(unavailable) != 0 {
		t.Fatalf("typed current open work = %+v unavailable %v", open, unavailable)
	}
	open, unavailable = postgresReportOpenWork([]reportstore.OpenFact{{ProjectID: project, WorkItemID: "wi_open_unknown_age", State: "Todo"}}, to, nil)
	if open.Count != 1 || open.Stuck != 0 || len(open.Items) != 0 || !slices.Contains(unavailable, "current_open_work_age_partial") {
		t.Fatalf("open work without typed age evidence = %+v unavailable %v", open, unavailable)
	}

	prior := operatortool.AnalyticsWindow{From: from.Add(-time.Hour), To: from}
	priorClosed := from.Add(-30 * time.Minute)
	priorSnapshot := summarizePostgresHostedReport([]reportstore.OverviewFact{{ProjectID: project, WorkItemID: "wi_reopened", CreatedAt: from.Add(-2 * time.Hour), PreviousCompletedAt: &priorClosed, PreviousCompletedState: "Done"}}, nil, nil, false, true, true, []string{string(project)}, w, prior, true)
	if priorSnapshot.Completion.Done != 0 || priorSnapshot.Previous.Done != 1 || priorSnapshot.Series[0].PriorDone != 1 {
		t.Fatalf("reopened prior-period snapshot = current %+v prior %+v series %+v", priorSnapshot.Completion, priorSnapshot.Previous, priorSnapshot.Series)
	}
	gateClass := reportstore.Class{Name: "Gate failed before landing", Kind: reportKindIssue}
	history := []reportstore.ProjectHistory{{ProjectID: project, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "In Progress", Dispatchable: true}, {Name: "Rework", Dispatchable: true}, {Name: "Human Review"}, {Name: "Done", Terminal: true}}, Issues: []reportstore.HistoryIssue{
		{WorkItemID: item, CreatedAt: from.Add(-30 * time.Minute), InitialState: "Todo", Events: []reportstore.WorkflowEvent{
			{At: from.Add(-15 * time.Minute), Type: "workflow.transitioned", FromState: "Todo", ToState: "In Progress"},
			{At: from.Add(10 * time.Minute), Type: "workflow.transitioned", FromState: "In Progress", ToState: "Rework", Class: gateClass},
			{At: from.Add(20 * time.Minute), Type: "workflow.transitioned", FromState: "Rework", ToState: "In Progress"},
			{At: from.Add(30 * time.Minute), Type: "workflow.transitioned", FromState: "In Progress", ToState: "Done"},
		}},
		{WorkItemID: "wi_old_open", CreatedAt: from.Add(-48 * time.Hour), InitialState: "Todo"},
	}}}
	applyPostgresReportHistory(&got, history, facts, w, prior)
	if got.Completion.System.P50Seconds != 3600 || got.Completion.Working.P50Seconds != 2100 || got.Completion.Lead.P50Seconds != 3600 || got.Series[0].LaneSeconds["Rework"] != 600 || got.OpenWork.Count != 1 || got.OpenWork.Stuck != 1 {
		t.Fatalf("historical report flow = completion %+v series %+v open %+v", got.Completion, got.Series, got.OpenWork)
	}
	if len(got.Rework) != 1 || got.Rework[0].Class != gateClass.Name || got.Series[0].ReworkClasses[gateClass.Name] != 1 || slices.Contains(got.Unavailable, "rework_timeline_unavailable") || slices.Contains(got.Unavailable, "lane_residence_and_unclaimed_waits_unavailable") {
		t.Fatalf("historical rework = %+v series %+v unavailable %v", got.Rework, got.Series, got.Unavailable)
	}
	landingFacts := []reportstore.LandingFact{{ProjectID: project, WorkItemID: item, ChangeID: "change_current", VersionID: "version_current", At: from.Add(30 * time.Minute), Attempts: 2, Complete: true}, {ProjectID: project, WorkItemID: "wi_prior", ChangeID: "change_prior", VersionID: "version_prior", At: from.Add(-30 * time.Minute), Attempts: 1, Complete: true}}
	applyPostgresReportLandings(&got, landingFacts, w, prior, true)
	if got.FirstTry == nil || *got.FirstTry != 0 || got.PriorFirst == nil || *got.PriorFirst != 100 || slices.Contains(got.Unavailable, "landed_first_try") {
		t.Fatalf("typed first try current %v prior %v", got.FirstTry, got.PriorFirst)
	}
	applyPostgresReportQuality(&got, history, landingFacts, []reportstore.EscapeFact{{EscapeProjection: reportstore.EscapeProjection{OccurrenceID: "occurrence_quality", Kind: "scheduled_failure", Cause: "validator_miss", ObservedAt: from.Add(40 * time.Minute)}, ChangeID: "change_current", VersionID: "version_current", CreatedAt: from.Add(45 * time.Minute)}}, facts, w, prior)
	if got.Analytics.Quality.Escapes != 1 || got.Analytics.Quality.EscapedVersions != 1 || got.Analytics.Quality.EscapePercent == nil || *got.Analytics.Quality.EscapePercent != 100 || got.Analytics.Quality.ReworkedItems != 1 || got.Rework[0].Class != gateClass.Name {
		t.Fatalf("typed quality counts = %+v rework %+v", got.Analytics.Quality, got.Rework)
	}
	attempts[0].StartedAt = from.Add(30 * time.Minute)
	attempts[0].ProjectionKnown = true
	attempts[0].Failed = true
	attempts[0].FailureClass = "Hub request timed out"
	attempts[0].FailureKind = reportKindInfrastructure
	attempts[0].Role, attempts[0].Model, attempts[0].Effort = "merge", "example-model", "high"
	attempts[0].RunnerID, attempts[0].RunnerName = "runner_example", "Example runner"
	attempts[0].DurationSeconds = 600
	attempts = append(attempts, reportstore.AttemptFact{ProjectID: project, WorkItemID: item, AttemptID: "attempt_before_rework", Status: "succeeded", StartedAt: from.Add(-15 * time.Minute), UpdatedAt: from.Add(5 * time.Minute), OutcomeAt: from.Add(5 * time.Minute), ProjectionKnown: true, Role: "merge", Model: "example-model", Effort: "high", RunnerID: "runner_example", RunnerName: "Example runner", DurationSeconds: 300})
	applyPostgresReportFailures(&got, attempts, w, prior, true)
	if len(got.Failures) != 1 || got.Failures[0].Kind != reportKindInfrastructure || got.Series[0].FailedInfrastructure != 1 || slices.Contains(got.Unavailable, "failure_classification") {
		t.Fatalf("typed failure classes = %+v series %+v", got.Failures, got.Series)
	}
	usage := []reportstore.UsageFact{{ProjectID: project, WorkItemID: item, AttemptID: "attempt_report", Period: from.Add(45 * time.Minute), Provider: "example", Model: "example-model", Input: 20, Output: 2, CachedInput: 2, Cost: 3, Currency: "usd", RunnerID: "runner_example", RunnerName: "Example runner"}, {ProjectID: project, WorkItemID: item, AttemptID: "attempt_before_rework", Period: from.Add(5 * time.Minute), Provider: "example", Model: "example-model", Input: 10, Output: 1, Cost: 1.5, Currency: "usd", RunnerID: "runner_example", RunnerName: "Example runner"}}
	if err := applyPostgresReportUsage(&got, facts, attempts, usage, w, prior, true); err != nil {
		t.Fatal(err)
	}
	if got.Cost != 4.5 || got.Tokens != 33 || len(got.Stages) != 1 || got.Stages[0].Sessions != 2 || got.Stages[0].Cost != 4.5 || got.Stages[0].Duration.P50Seconds != 450 || len(got.Runners) != 1 || got.Runners[0].Cost != 4.5 || got.Failures[0].Cost != 3 || got.Rework[0].Cost != 3 || got.Series[0].StageCost["merge"] != 4.5 {
		t.Fatalf("typed usage report = cost %.2f tokens %d stages %+v runners %+v failures %+v rework %+v", got.Cost, got.Tokens, got.Stages, got.Runners, got.Failures, got.Rework)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func TestReportCurrentUsageIngestionPreservesCoverage(t *testing.T) {
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
			service := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(organization)}}}
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

func TestPostgresReportMissingImportsRemainUnknown(t *testing.T) {
	for _, check := range []struct {
		name     string
		history  []reportstore.ProjectHistory
		attempts []reportstore.AttemptFact
		missing  []string
	}{
		{name: "absent historical facts"},
		{name: "two missing history issues", history: []reportstore.ProjectHistory{{Issues: []reportstore.HistoryIssue{{WorkItemID: "wi_first"}, {WorkItemID: "wi_second"}}}}, missing: []string{"lane_residence_and_unclaimed_waits_unavailable", "completion_complete_population", "rework_timeline_unavailable"}},
		{name: "missing attempt projection", attempts: []reportstore.AttemptFact{{AttemptID: "attempt_example"}}, missing: []string{"attempt_outcomes_unavailable", "failure_classification"}},
		{name: "missing rework projection", history: []reportstore.ProjectHistory{{Issues: []reportstore.HistoryIssue{{Events: []reportstore.WorkflowEvent{{ToState: "Rework"}}}}}}, missing: []string{"rework_timeline_unavailable"}},
	} {
		t.Run(check.name, func(t *testing.T) {
			out := hostedReportsReport{}
			out.Completion.Done = 2
			out.FirstTry = new(100.0)
			out.Analytics.Quality.ReworkPercent = new(0.0)
			out.Prior.ReworkPercent = new(0.0)
			out.Prior.InfrastructureReturns = new(0)
			out.Analytics.Quality.EscapePercent = new(0.0)
			markPostgresReportCoverage(&out, check.history, check.attempts)
			if out.Completion.Done != 2 || !out.Analytics.Quality.Partial || out.FirstTry != nil || out.Analytics.Quality.EscapePercent != nil || !slices.Contains(out.Analytics.Quality.Unavailable, "escape_classification") || !slices.Contains(out.Unavailable, "landed_first_try") {
				t.Fatalf("missing import coverage = %+v", out)
			}
			for _, markers := range [][]string{out.Unavailable, out.Analytics.Unavailable, out.Analytics.Quality.Unavailable} {
				seen := map[string]bool{}
				for _, marker := range markers {
					if seen[marker] {
						t.Errorf("duplicate unavailable marker %q in %v", marker, markers)
					}
					seen[marker] = true
				}
			}
			if len(check.history) > 0 && (out.Analytics.Quality.ReworkPercent != nil || out.Prior.ReworkPercent != nil || out.Prior.InfrastructureReturns != nil) {
				t.Fatalf("missing history outcomes must remain unknown: %+v", out)
			}
			for _, marker := range check.missing {
				if !slices.Contains(out.Unavailable, marker) {
					t.Errorf("missing unavailable marker %q in %v", marker, out.Unavailable)
				}
			}
		})
	}
}

func TestPostgresReportRendersMissingImportsAcrossTenants(t *testing.T) {
	repository := projectRecordPostgres(t)
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
			item, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: "wi_partial", Number: 1, Title: string(organization), WorkflowStateName: &state, CreatedAt: now})
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
			service := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(organization)}}}
			out, err := service.readPostgresHostedReport(t.Context(), apiCredential{}, []string{string(project)}, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}, reportsFilter{}, false)
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
				filtered, err := service.readPostgresHostedReport(t.Context(), apiCredential{}, []string{string(project)}, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}, filter, false)
				if err != nil {
					t.Fatal(err)
				}
				if filtered.OpenWork.Count != 0 || len(filtered.OpenWork.Items) != 0 {
					t.Fatalf("open work escaped report filter %+v: %+v", filter, filtered.OpenWork)
				}
			}
		})
	}
}
