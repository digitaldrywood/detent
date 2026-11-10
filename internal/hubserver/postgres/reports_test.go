package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresReportsReadTypedWorkItemFacts(t *testing.T) {
	const processEvidenceVersion = 20261109184650
	admin, fixture := migrationDatabaseAtVersion(t, 20261109182156)
	pool := rolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := t.Context()
	organization := tracker.OrganizationID("org_reports")
	project := tracker.ProjectID("project_reports")
	now := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	priority := 1
	state := "Todo"
	actorKind, actorID := "human", "actor_example"
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return fmt.Errorf("create report organization: %w", err)
		}
		if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}}); err != nil {
			return fmt.Errorf("create report project: %w", err)
		}
		for i, item := range []corestore.WorkItem{
			{OrganizationID: organization, ProjectID: project, NativeID: "wi_report_bug", Number: 1, Title: "Example bug", IssueType: "bug", Priority: &priority, WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actorID, CreatedAt: now},
			{OrganizationID: organization, ProjectID: project, NativeID: "wi_report_request", Number: 2, Title: "Example request", IssueType: "change_request", WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actorID, CreatedAt: now.Add(-2 * time.Minute)},
		} {
			item.ID = int64(i + 1)
			if _, err := w.CreateWorkItem(ctx, item); err != nil {
				return fmt.Errorf("create report work item %s: %w", item.NativeID, err)
			}
		}
		return nil
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Fatalf("write report fixture failed with SQLSTATE %s on constraint %s: %v", pgErr.Code, pgErr.ConstraintName, err)
		}
		t.Fatal(err)
	}
	objects := newHistoryObjects()
	content := eventstore.NewContent(objects)
	events := []eventstore.Event{}
	for i, value := range []tracker.CollaborationEvent{
		{AggregateID: "wi_report_bug", Type: "issue.created", RecordedAt: now},
		{AggregateID: "wi_report_request", Type: "issue.created", RecordedAt: now.Add(-2 * time.Minute)},
		{AggregateID: "wi_report_bug", Type: "workflow.transitioned", RecordedAt: now.Add(10 * time.Second), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Rework", ReasonDetail: "gate failed"}},
		{AggregateID: "wi_report_bug", Type: "workflow.transitioned", RecordedAt: now.Add(30 * time.Second), Data: tracker.CollaborationData{FromState: "Rework", ToState: "Done"}},
		{AggregateID: "wi_report_request", Type: "workflow.transitioned", RecordedAt: now.Add(-30 * time.Second), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Done"}},
		{AggregateID: "wi_report_request", Type: "workflow.transitioned", RecordedAt: now.Add(20 * time.Second), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}},
	} {
		value.ID = fmt.Sprintf("report_event_%d", i)
		value.OrganizationID, value.ProjectID, value.Actor, value.SchemaVersion = organization, project, historyActor(), 1
		event, err := content.PrepareEvent(ctx, organization, historyJSON(t, value))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	var lease leasestore.Lease
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "report_machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		var err error
		lease, err = w.Claim(ctx, leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "report_machine", SessionID: "report_session", LeaseID: "report_lease", TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run := tracker.NativeRunData{ProcessUsage: &procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: 8, SystemCPUSeconds: 2, WallSeconds: 10}, AttemptID: "attempt_report_failure", RunID: "run_report_failure", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "hub request timed out"}, Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "merge", ResolvedModel: agentidentity.NewValue("example-model", agentidentity.ProvenanceRuntime)}, Landing: &tracker.NativeLandingReceipt{Landed: false}}}
	runRef, err := objects.StoreDetail(ctx, string(organization), historyJSON(t, run))
	if err != nil {
		t.Fatal(err)
	}
	runEvent, err := content.PrepareEvent(ctx, organization, historyJSON(t, tracker.CollaborationEvent{ID: "report_run_failure", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: historyActor(), RecordedAt: now.Add(40 * time.Second), Data: tracker.CollaborationData{Run: &run}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: run.AttemptID, Fence: leaseFence(lease), RunID: run.RunID, Sequence: 1, Status: "failed", StartedAt: now.Add(5 * time.Second), PayloadObjectKey: runRef.Key}); err != nil {
			return err
		}
		for _, event := range append(events, runEvent) {
			event.Report = nil
			if _, err := w.AppendEvent(ctx, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range append(events, runEvent) {
		if event.Report == nil {
			continue
		}
		p := event.Report
		if _, err := admin.ExecContext(ctx, `INSERT INTO report_event_facts
(organization_id,event_id,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, organization, event.ID, p.RunSequence, p.RunStatus, p.Role, p.Model, p.Effort, p.Failed, p.FailureClass, p.FailureKind, p.LandingReceipt, p.Landed, p.LandingChangeID, p.ReworkClass, p.ReworkKind, p.DurationSeconds); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(ctx, `INSERT INTO report_attempt_facts
(organization_id,attempt_id,first_event_id,latest_event_id,outcome_event_id)
VALUES ($1,$2,$3,$3,$3)`, organization, run.AttemptID, runEvent.ID); err != nil {
		t.Fatal(err)
	}

	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, processEvidenceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	var upgradedStatus string
	if err := admin.QueryRowContext(ctx, "SELECT process_evidence_status FROM report_event_facts WHERE organization_id=$1 AND event_id=$2", organization, runEvent.ID).Scan(&upgradedStatus); err != nil || upgradedStatus != "unknown" {
		t.Fatalf("upgrade invented historical coverage: status=%s error=%v", upgradedStatus, err)
	}
	for _, test := range []struct {
		name string
		edit func(*eventstore.Event)
	}{
		{"object hash", func(e *eventstore.Event) { e.PayloadSHA256 = strings.Repeat("b", 64) }},
		{"object key", func(e *eventstore.Event) { e.PayloadObjectKey += "_foreign" }},
		{"project", func(e *eventstore.Event) { e.ProjectID = "foreign" }},
		{"item", func(e *eventstore.Event) { e.WorkItemID = "foreign" }},
		{"run", func(e *eventstore.Event) { e.RunID = "foreign" }},
		{"attempt", func(e *eventstore.Event) { e.AttemptID = "foreign" }},
		{"sequence", func(e *eventstore.Event) { p := *e.Report; p.RunSequence++; e.Report = &p }},
	} {
		t.Run("upgrade provenance/"+test.name, func(t *testing.T) {
			bad := runEvent
			test.edit(&bad)
			if err := repository.ImportReportEventProjection(ctx, bad); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("mismatched upgrade provenance accepted: %v", err)
			}
			var status string
			var peak int64
			if err := admin.QueryRowContext(ctx, "SELECT process_evidence_status,process_peak_memory_bytes FROM report_event_facts WHERE organization_id=$1 AND event_id=$2", organization, runEvent.ID).Scan(&status, &peak); err != nil || status != "unknown" || peak != 0 {
				t.Fatalf("failed upgrade changed evidence: %s %d %v", status, peak, err)
			}
		})
	}
	for _, mode := range []string{"event replay", "import replay"} {
		if mode == "import replay" {
			if _, err := admin.ExecContext(ctx, `UPDATE report_event_facts SET process_evidence_status='unknown',process_peak_memory_bytes=0,process_user_cpu_seconds=0,process_system_cpu_seconds=0,process_wall_seconds=0 WHERE organization_id=$1 AND event_id=$2`, organization, runEvent.ID); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "event replay" {
			if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(ctx, runEvent); return err }); err != nil {
				t.Fatal(err)
			}
		} else if err := repository.ImportReportEventProjection(ctx, runEvent); err != nil {
			t.Fatal(err)
		}
		var status string
		var peak int64
		if err := admin.QueryRowContext(ctx, `SELECT process_evidence_status,process_peak_memory_bytes FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, organization, runEvent.ID).Scan(&status, &peak); err != nil {
			t.Fatal(err)
		}
		if status != "usable" || peak != 4096 {
			t.Fatalf("%s lost verified process projection: status=%s peak=%d", mode, status, peak)
		}
	}
	var landedLease leasestore.Lease
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.Release(ctx, leaseFence(lease), "completed"); err != nil {
			return err
		}
		var err error
		landedLease, err = w.Claim(ctx, leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "report_machine", SessionID: "report_landed_session", LeaseID: "report_landed_lease", TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	landedRun := tracker.NativeRunData{AttemptID: "attempt_report_landed", RunID: "run_report_landed", LeaseID: tracker.LeaseID(landedLease.LeaseID), FencingToken: tracker.FencingToken(landedLease.FencingToken), Sequence: 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "merge", ResolvedModel: agentidentity.NewValue("other-model", agentidentity.ProvenanceRuntime)}, Landing: &tracker.NativeLandingReceipt{Landed: true, ChangeID: "change_report", VersionID: "version_report"}}}
	landedRef, err := objects.StoreDetail(ctx, string(organization), historyJSON(t, landedRun))
	if err != nil {
		t.Fatal(err)
	}
	landedEvent, err := content.PrepareEvent(ctx, organization, historyJSON(t, tracker.CollaborationEvent{ID: "report_run_landed", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: historyActor(), RecordedAt: now.Add(50 * time.Second), Data: tracker.CollaborationData{Run: &landedRun}}))
	if err != nil {
		t.Fatal(err)
	}
	change, err := content.PrepareChange(ctx, organization, historyJSON(t, tracker.ChangeRequest{ID: "change_report", OrganizationID: organization, ProjectID: project, WorkItemID: "wi_report_bug", Title: "Example landed change", Revision: 1, CreatedAt: now, UpdatedAt: now.Add(50 * time.Second), Landed: &tracker.ChangeLanding{VersionID: "version_report", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40), BaseRef: "develop", LandedAt: now.Add(50 * time.Second)}}))
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareVersion(ctx, organization, historyJSON(t, tracker.ChangeVersion{ID: "version_report", ChangeID: "change_report", Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("c", 40), Repository: "owner/repository", PolicyID: "policy_example", AttemptID: landedRun.AttemptID}, Actor: historyActor(), CreatedAt: now}))
	if err != nil {
		t.Fatal(err)
	}
	escapeEvidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: organization, ChangeID: "change_report", Kind: "discussion"}, historyJSON(t, tracker.ChangeDiscussion{ID: "discussion_report_escape", VersionID: "version_report", Actor: historyActor(), CreatedAt: now.Add(56 * time.Second), Escape: &tracker.QualityEscape{OccurrenceID: "occurrence_report", Kind: "scheduled_failure", Cause: "validator_miss", ObservedAt: now.Add(55 * time.Second), HumanOverride: true}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: landedRun.AttemptID, Fence: leaseFence(landedLease), RunID: landedRun.RunID, Sequence: 1, Status: "succeeded", StartedAt: now.Add(45 * time.Second), PayloadObjectKey: landedRef.Key}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, landedEvent); err != nil {
			return err
		}
		if _, err := w.CreateChange(ctx, change); err != nil {
			return err
		}
		if _, err := w.CreateVersion(ctx, version); err != nil {
			return err
		}
		stored, err := w.AppendEvidence(ctx, escapeEvidence)
		escapeEvidence.Sequence = stored.Sequence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := repository.ImportReportEscapeProjection(ctx, escapeEvidence); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []string{"attempt_report_failure", "attempt_report_landed"} {
		record := reportstore.UsageRecord{OrganizationID: organization, UsageFact: reportstore.UsageFact{ProjectID: project, WorkItemID: "wi_report_bug", AttemptID: id, Period: now, Provider: "example", Model: "example-model", Input: 10, CachedInput: 12, Output: 3, Cost: 1.25, Currency: "usd"}, UpdatedAt: now}
		for range 2 {
			if err := repository.ImportReportUsageFact(ctx, record); err != nil {
				t.Fatal(err)
			}
		}
		record.Cost = 2
		if err := repository.ImportReportUsageFact(ctx, record); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("conflicting usage: %v", err)
		}
	}
	err = repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.WorkItem(ctx, project, "wi_report_bug")
		if err != nil {
			return err
		}
		if stored.IssueType != "bug" || stored.Priority == nil || *stored.Priority != 1 {
			t.Fatalf("stored report metadata = type %q priority %v", stored.IssueType, stored.Priority)
		}
		persisted, err := r.WorkItems(ctx, project, 0, 10)
		if err != nil {
			return err
		}
		if len(persisted) != 2 {
			t.Fatalf("persisted report fixture count = %d; first created at %s", len(persisted), stored.CreatedAt)
		}

		usageReader := r.(reportstore.UsageReader)
		usageQuery := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), To: now.Add(time.Minute), Limit: 10}
		usage, err := usageReader.ReadReportUsage(ctx, usageQuery)
		if err != nil {
			return err
		}
		if len(usage) != 2 || usage[0].Cost+usage[1].Cost != 2.5 || usage[0].CachedInput != 12 {
			t.Fatalf("typed usage = %+v", usage)
		}
		usageQuery.Principal = "another_actor"
		hidden, err := usageReader.ReadReportUsage(ctx, usageQuery)
		if err != nil {
			return err
		}
		if len(hidden) != 0 {
			t.Fatal("usage leaked across principal scope")
		}
		usageQuery.Principal = ""
		usageQuery.Limit = 1
		if _, err := usageReader.ReadReportUsage(ctx, usageQuery); !errors.Is(err, reportstore.ErrPopulationExceeded) {
			t.Fatalf("usage overflow: %v", err)
		}
		reader, ok := r.(reportstore.Reader)
		if !ok {
			t.Fatal("tenant reader does not implement report reads")
		}
		allFacts, err := reader.ReadOverviewFacts(ctx, reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), Split: now, To: now.Add(time.Minute), Limit: 10})
		if err != nil {
			return err
		}
		if len(allFacts) != 2 {
			t.Fatalf("unfiltered report facts = %+v", allFacts)
		}
		for _, fact := range allFacts {
			if fact.WorkItemID == "wi_report_request" && (fact.CompletedAt != nil || fact.PreviousCompletedAt == nil || fact.PreviousCompletedState != "Done") {
				t.Fatalf("reopened issue lost prior-period completion: %+v", fact)
			}
		}
		facts, err := reader.ReadOverviewFacts(ctx, reportstore.OverviewQuery{
			Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), Split: now, To: now.Add(time.Minute), Limit: 10,
			Filter: reportstore.Filter{Types: []string{"bug"}, Priorities: []int{1}},
		})
		if err != nil {
			return err
		}
		if len(facts) != 1 || facts[0].WorkItemID != "wi_report_bug" || facts[0].CreatedAt.IsZero() {
			t.Fatalf("filtered report facts = %+v", facts)
		}
		attemptReader, ok := r.(reportstore.AttemptReader)
		if !ok {
			t.Fatal("tenant reader does not implement report attempt reads")
		}
		attempts, err := attemptReader.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), To: now.Add(time.Minute), Limit: 10})
		if err != nil {
			return err
		}
		if len(attempts) != 2 || !attempts[0].ProjectionKnown || attempts[0].Role != "merge" || attempts[0].Model != "example-model" || attempts[0].FailureClass != "Hub request timed out" || attempts[0].FailureKind != "infrastructure" {
			t.Fatalf("report attempt facts = %+v", attempts)
		}
		openReader, ok := r.(reportstore.OpenReader)
		if !ok {
			t.Fatal("tenant reader does not implement open report reads")
		}
		openFacts, err := openReader.ReadOpenFacts(ctx, reportstore.OpenQuery{Projects: []tracker.ProjectID{project}, At: now.Add(time.Hour), Limit: 10})
		if err != nil {
			return err
		}
		if len(openFacts) != 1 || openFacts[0].State != "Todo" || openFacts[0].Terminal || openFacts[0].EnteredAt == nil {
			t.Fatalf("typed current open facts = %+v", openFacts)
		}
		query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), Split: now, To: now.Add(2 * time.Minute), Limit: 10}
		history, err := r.(reportstore.HistoryReader).ReadHistory(ctx, query)
		if err != nil {
			return err
		}
		if len(history) != 1 || len(history[0].Issues) != 2 || len(history[0].Issues[0].Events) != 2 || history[0].Issues[0].Events[0].Class.Name != "Gate failed before landing" {
			t.Fatalf("typed report history = %+v", history)
		}
		landings, err := r.(reportstore.LandingReader).ReadLandingFacts(ctx, query)
		if err != nil {
			return err
		}
		if len(landings) != 1 || landings[0].Attempts != 2 || !landings[0].Complete {
			t.Fatalf("typed landing attempts = %+v", landings)
		}
		escapes, err := r.(reportstore.QualityReader).ReadEscapeFacts(ctx, query)
		if err != nil {
			return err
		}
		if len(escapes) != 1 || escapes[0].OccurrenceID != "occurrence_report" || escapes[0].Cause != "validator_miss" || !escapes[0].HumanOverride {
			t.Fatalf("typed report escape facts = %+v", escapes)
		}
		facets, err := reader.ReadFacets(ctx, []tracker.ProjectID{project}, query.From, query.To, "", 10)
		if err != nil {
			return err
		}
		if len(facets.FiledBy) != 1 || facets.FiledBy[0].Name != "actor_example" || len(facets.Models) != 2 || facets.Models[0] != "example-model" || len(facets.Stages) != 1 || facets.Stages[0] != "merge" {
			t.Fatalf("typed facets = %+v", facets)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("same attempt population for usage model filters", func(t *testing.T) {
		event, err := content.PrepareEvent(ctx, organization, historyJSON(t, tracker.CollaborationEvent{ID: "report_model_update", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: historyActor(), RecordedAt: now.Add(2 * time.Hour), Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: landedRun.AttemptID, RunID: landedRun.RunID, Sequence: 2, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "implement", ResolvedModel: agentidentity.NewValue("other-model", agentidentity.ProvenanceRuntime)}}}}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(ctx, event); return err }); err != nil {
			t.Fatal(err)
		}
		for _, filter := range []reportstore.Filter{{Models: []string{"missing-model"}}, {Stages: []string{"missing-stage"}}, {Runners: []string{"missing-runner"}}, {Models: []string{"example-model"}}} {
			if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				q := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), To: now.Add(time.Minute), Filter: filter, Limit: 10}
				attempts, err := r.(reportstore.AttemptReader).ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: q.Projects, From: q.From, To: q.To, Filter: filter, Limit: 10})
				if err != nil {
					return err
				}
				usage, err := r.(reportstore.UsageReader).ReadReportUsage(ctx, q)
				if err != nil {
					return err
				}
				selected := map[string]bool{}
				for _, a := range attempts {
					selected[a.AttemptID] = true
				}
				for _, u := range usage {
					if !selected[u.AttemptID] {
						t.Fatalf("unscoped usage: %+v", u)
					}
				}
				if len(usage) != len(attempts) {
					t.Fatalf("attempts=%d usage=%d filter=%+v", len(attempts), len(usage), filter)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("immutable outcome time survives late update", func(t *testing.T) {
		correction, err := content.PrepareEvent(ctx, organization, historyJSON(t, tracker.CollaborationEvent{ID: "report_failure_correction", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: historyActor(), RecordedAt: now.Add(2 * time.Hour), Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: run.AttemptID, RunID: run.RunID, Sequence: 2, Outcome: "succeeded"}}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(ctx, correction); return err }); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `DELETE FROM report_attempt_facts WHERE organization_id=$1 AND attempt_id=$2`, organization, run.AttemptID); err != nil {
			t.Fatal(err)
		}
		for _, event := range []eventstore.Event{correction, runEvent} {
			if err := repository.ImportReportEventProjection(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(ctx, `UPDATE native_attempts SET updated_at=$1,status='succeeded' WHERE organization_id=$2 AND id=$3`, now.Add(24*time.Hour), organization, run.AttemptID); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(reportstore.AttemptReader).ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: []tracker.ProjectID{project}, From: now, To: now.Add(time.Minute), Limit: 10})
			if err != nil {
				return err
			}
			for _, a := range rows {
				if a.AttemptID == run.AttemptID {
					if !a.Failed || a.Status != "failed" || !a.OutcomeAt.Equal(now.Add(40*time.Second)) {
						t.Fatalf("historical outcome changed: %+v", a)
					}
					return nil
				}
			}
			t.Fatal("historical failure disappeared")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("coverage requires verified import or current accepted revisions", func(t *testing.T) {
		query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 100}
		assertCoverage := func(want bool) {
			t.Helper()
			if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.(reportstore.UsageCoverageReader).ReadReportUsageCoverage(ctx, query)
				if err != nil {
					return err
				}
				if got != want {
					t.Fatalf("coverage=%v want %v", got, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		assertCoverage(false)
		for _, id := range []string{run.AttemptID, landedRun.AttemptID} {
			if err := repository.VerifyReportUsageImport(ctx, organization, id, strings.Repeat("a", 64), 1); err != nil {
				t.Fatal(err)
			}
		}
		assertCoverage(true)
		makeRecord := func(revision int64, amount int64, coverage string) billingstore.CostObservation {
			record := billingCostObservation(t, organization, project, "report_current", run.AttemptID, "runner_api", now.Add(-time.Minute), now, amount)
			o := &record.Observation
			o.AttemptID = run.AttemptID
			o.WorkItemID = "wi_report_bug"
			o.Model = "example-model"
			o.Input = 100
			o.CachedInput = 50
			o.Output = 10
			o.Coverage = coverage
			o.Revision = revision
			record.ReceivedAt = now
			raw, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(ctx, string(organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			record.Reference = billingstore.AuditReferenceFromArchive(ref)
			return record
		}
		first := makeRecord(1, 2000000, "complete")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{first})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(false)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{first, first})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(true)
		probe := reportObjectReadProbe{DetailStore: objects, check: func() {
			if repository.Stats().AcquiredConns() != 0 {
				t.Fatal("object read held target transaction")
			}
		}}
		after, count, err := repository.RefreshReportUsagePage(ctx, organization, probe, "", 1)
		if err != nil || count != 1 || after == "" {
			t.Fatalf("refresh page count=%d cursor=%q err=%v", count, after, err)
		}
		if _, count, err := repository.RefreshReportUsagePage(ctx, organization, probe, after, 1); err != nil || count != 0 {
			t.Fatalf("refresh page continuation count=%d err=%v", count, err)
		}
		assertCoverage(true)
		corrected := makeRecord(2, 3000000, "partial")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{corrected}); err != nil {
				return err
			}
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{first, corrected, corrected, first})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(false)
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(reportstore.UsageReader).ReadReportUsage(ctx, query)
			if err != nil {
				return err
			}
			total := 0.0
			for _, u := range rows {
				total += u.Cost
			}
			if total != 4.25 {
				t.Fatalf("corrected usage duplicated historical data: %v", total)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		zero := makeRecord(3, 0, "complete")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{zero}); err != nil {
				return err
			}
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{zero})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(true)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{first})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(true)
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(reportstore.UsageReader).ReadReportUsage(ctx, query)
			if err != nil {
				return err
			}
			for _, u := range rows {
				if u.AttemptID == run.AttemptID && u.Cost != 0 {
					t.Fatalf("explicit zero replaced by stale usage: %+v", u)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

	})
	t.Run("facet input overflow fails even with one distinct filer", func(t *testing.T) {
		flood := tracker.ProjectID("project_reports_flood")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: flood, Name: "Example flood", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO issues(organization_id,id,project_id,native_id,number,title,actor_kind,actor_principal_id) SELECT $1,10000+n,$2,'wi_facet_'||n,10000+n,'Example','human','actor_flood' FROM generate_series(1,5001) n`, organization, flood); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{flood}, now.Add(-time.Hour), now.Add(time.Hour), "", 10)
			if !errors.Is(err, reportstore.ErrPopulationExceeded) {
				t.Fatalf("facet source overflow: %v", err)
			}
			for _, principal := range []string{"", actorID} {
				facets, err := r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{project}, now.Add(-time.Hour), now.Add(time.Hour), principal, 10)
				if err != nil {
					t.Fatalf("unrelated project facets principal=%q failed after flood: %v", principal, err)
				}
				if len(facets.FiledBy) != 1 || facets.FiledBy[0].ID != actorID {
					t.Fatalf("unrelated project filers principal=%q = %+v", principal, facets.FiledBy)
				}
			}
			if _, err := r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{project, flood}, now.Add(-time.Hour), now.Add(time.Hour), actorID, 10); err != nil {
				t.Fatalf("principal-scoped facets failed after another filer's flood: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

}

type reportObjectReadProbe struct {
	attachment.DetailStore
	check func()
}

func (p reportObjectReadProbe) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	p.check()
	return p.DetailStore.ReadDetail(ctx, org, ref)
}
