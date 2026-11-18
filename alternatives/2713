package billingrecords_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportBulkImportsPreserveTenantFactsAndBoundStatements(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	for _, org := range []tracker.OrganizationID{"org_bulk_first", "org_bulk_second"} {
		if _, err := admin.ExecContext(ctx, `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, pgstoretest.HistoryAt()); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedLeaseFamily(t, repository, org)
		lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		objects := pgstoretest.NewHistoryObjects()
		content := eventstore.NewContent(objects)
		ref, err := objects.StoreDetail(ctx, string(org), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: "attempt_bulk", Fence: pgstoretest.LeaseFence(lease), RunID: "run_bulk", Sequence: 1, Status: "running", StartedAt: pgstoretest.HistoryAt(), PayloadObjectKey: ref.Key})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		events := make([]postgres.ReportEventProjectionImport, 100)
		usage := make([]reportstore.UsageRecord, 100)
		for i := range 100 {
			value := tracker.CollaborationEvent{ID: fmt.Sprintf("bulk_event_%03d", i), OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: pgstoretest.HistoryAt().Add(time.Duration(i) * time.Second), Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt_bulk", RunID: "run_bulk", Sequence: int64(i + 1), Outcome: "failed"}}}
			event, err := content.PrepareEvent(ctx, org, pgstoretest.HistoryJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			events[i].Event = event
			usage[i] = reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_bulk", Period: pgstoretest.HistoryAt().Add(time.Duration(i) * time.Hour), Provider: "example", Model: "example-model", Input: int64(i + 1), CachedInput: 2, Output: 3, Cost: 1.25, Currency: "usd"}, UpdatedAt: pgstoretest.HistoryAt()}
		}
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, e := range events {
				if _, err := w.AppendEvent(ctx, e.Event); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `DELETE FROM report_attempt_facts WHERE organization_id=$1;`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `DELETE FROM report_event_facts WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		change, err := content.PrepareChange(ctx, org, pgstoretest.HistoryJSON(t, tracker.ChangeRequest{ID: "change_bulk", OrganizationID: org, ProjectID: "prj_1", WorkItemID: "wi_1", Revision: 1, Title: "Example", CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt()}))
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareVersion(ctx, org, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: "version_bulk", ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", BaseSHA: "base", Repository: "owner/repository", PolicyID: "example", AttemptID: "attempt_bulk"}, Actor: pgstoretest.HistoryActor(), CreatedAt: pgstoretest.HistoryAt()}))
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := content.PrepareLandingReceipt(ctx, eventstore.LandingReceipt{OrganizationID: org, AttemptID: "attempt_bulk", VersionID: version.ID, Repository: "owner/repository", BaseRef: "develop"}, tracker.NativeLandingReceipt{VersionID: version.ID, MergeSHA: strings.Repeat("a", 40), BaseRef: "develop", Landed: true})
		if err != nil {
			t.Fatal(err)
		}
		escapes := make([]eventstore.Evidence, 100)
		current := make([]billingstore.CostObservation, 100)
		for i := range 100 {
			e, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: org, ChangeID: change.ID, Kind: "discussion"}, pgstoretest.HistoryJSON(t, tracker.ChangeDiscussion{ID: fmt.Sprintf("discussion_%d", i), Actor: pgstoretest.HistoryActor(), CreatedAt: pgstoretest.HistoryAt(), Escape: &tracker.QualityEscape{OccurrenceID: fmt.Sprintf("occurrence_%d", i), Kind: "scheduled_failure", Cause: "validator_miss", ObservedAt: pgstoretest.HistoryAt().Add(time.Duration(i) * time.Second)}}))
			if err != nil {
				t.Fatal(err)
			}
			escapes[i] = e
			c := billingCostObservation(t, org, "prj_1", fmt.Sprintf("bulk_source_%d", i), "attempt_bulk", "runner_api", pgstoretest.HistoryAt().Add(time.Duration(i)*time.Hour), pgstoretest.HistoryAt().Add(time.Duration(i+1)*time.Hour), 1000000)
			c.Observation.AttemptID = "attempt_bulk"
			c.Observation.WorkItemID = "wi_1"
			c.Observation.Model = "example-model"
			c.Observation.Input = int64(i + 1)
			c.Observation.Coverage = "complete"
			c.ReceivedAt = pgstoretest.HistoryAt()
			raw, err := json.Marshal(c.Observation)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(ctx, string(org), raw)
			if err != nil {
				t.Fatal(err)
			}
			c.Reference = billingstore.AuditReferenceFromArchive(ref)
			current[i] = c
		}
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateChange(ctx, change); err != nil {
				return fmt.Errorf("create change: %w", err)
			}
			if _, err := w.CreateVersion(ctx, version); err != nil {
				return fmt.Errorf("create version: %w", err)
			}
			if _, err := w.CreateLandingReceipt(ctx, receipt); err != nil {
				return fmt.Errorf("create receipt: %w", err)
			}
			for i, e := range escapes {
				stored, err := w.AppendEvidence(ctx, e)
				if err != nil {
					return fmt.Errorf("append evidence %d: %w", i, err)
				}
				e.Sequence = stored.Sequence
				escapes[i] = e
			}
			_, err := w.RecordBillingCosts(ctx, current)
			if err != nil {
				return fmt.Errorf("record costs: %w", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `UPDATE change_requests SET current_version_id=$1 WHERE organization_id=$2 AND id=$3`, version.ID, org, change.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `DELETE FROM report_escape_facts WHERE organization_id=$1`, org); err != nil {
			t.Fatal(err)
		}
		change.LandedVersionID = &version.ID
		change.LandedAt = new(pgstoretest.HistoryAt())
		change.LandedHeadSHA = &version.HeadSHA
		change.LandedMergeSHA = &receipt.MergeSHA
		change.LandedBaseRef = &receipt.BaseRef
		frozen, err := content.PrepareVersionLanding(ctx, eventstore.VersionLanding{OrganizationID: org, ProjectID: "prj_1", WorkItemID: "wi_1", ChangeID: change.ID, VersionID: version.ID}, tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: receipt.MergeSHA, BaseRef: receipt.BaseRef, Method: "merge", Actor: pgstoretest.HistoryActor(), LandedAt: pgstoretest.HistoryAt(), Quality: &tracker.LandingQualitySnapshot{IssueRevision: 1, IssueBody: "Frozen batch contract", Criteria: []string{"Preserved criterion"}}})
		if err != nil {
			t.Fatal(err)
		}
		landings := []postgres.ReportLandingImport{{Change: change, Receipt: receipt, Frozen: frozen}}

		counter := &pgstoretest.ReportCountingTracer{}
		countedPool := pgstoretest.TracedFeaturePool(t, pool, counter)
		counted, err := postgres.NewRepository(ctx, countedPool, pgstoretest.SmallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := counted.Close(); err != nil {
				t.Error(err)
			}
		})
		check := func(name string, limit int64, run func() error) {
			t.Helper()
			counter.Count.Store(0)
			if err := run(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			t.Logf("%s: %d statements", name, counter.Count.Load())
			if got := counter.Count.Load(); got > limit {
				t.Fatalf("%s used %d statements, limit %d", name, got, limit)
			}
		}
		for range 2 {
			check("escapes", 7, func() error { return counted.ImportReportEscapeProjections(ctx, escapes) })
			check("landings", 7, func() error { return counted.ImportReportChangeLandings(ctx, landings) })
			check("current refresh", 7, func() error {
				return counted.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error { return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, current) })
			})
			check("events", 8, func() error { _, err := counted.ImportReportEventProjections(ctx, events); return err })
			check("usage", 7, func() error { return counted.ImportReportUsageFacts(ctx, usage) })
			check("coverage", 7, func() error {
				return counted.VerifyReportUsageImports(ctx, org, strings.Repeat("a", 64), []postgres.ReportUsageImportCoverage{{AttemptID: "attempt_bulk", RowCount: 100}})
			})
		}
		conflict := append([]reportstore.UsageRecord(nil), usage...)
		conflict[99].Cost++
		if err := counted.ImportReportUsageFacts(ctx, conflict); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("usage conflict=%v", err)
		}
		conflict[0].Period = pgstoretest.HistoryAt().Add(1000 * time.Hour)
		if err := counted.ImportReportUsageFacts(ctx, conflict); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("partial usage conflict=%v", err)
		}
		mismatch := append([]reportstore.UsageRecord(nil), usage...)
		mismatch[99].WorkItemID = "wi_2"
		if err := counted.ImportReportUsageFacts(ctx, mismatch); !errors.Is(err, corestore.ErrTenantMismatch) {
			t.Fatalf("usage attribution=%v", err)
		}
		forged := append([]postgres.ReportEventProjectionImport(nil), events...)
		forged[99].Event.PayloadSHA256 = strings.Repeat("b", 64)
		if _, err := counted.ImportReportEventProjections(ctx, forged); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("event provenance=%v", err)
		}
		projection := *events[99].Event.Report
		projection.Model = "changed"
		forged[99] = events[99]
		forged[99].Event.Report = &projection
		if _, err := counted.ImportReportEventProjections(ctx, forged); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("event projection=%v", err)
		}
		legacy := events[0].Event
		forged = append([]postgres.ReportEventProjectionImport(nil), events...)
		forged[0].Event.PayloadSHA256 = strings.Repeat("c", 64)
		forged[0].Legacy = &legacy
		check("legacy anchors", 8, func() error {
			n, err := counted.ImportReportEventProjections(ctx, forged)
			if n != 1 && err == nil {
				return fmt.Errorf("legacy anchors=%d", n)
			}
			return err
		})
		if err := counted.VerifyReportUsageImports(ctx, org, strings.Repeat("b", 64), []postgres.ReportUsageImportCoverage{{AttemptID: "attempt_bulk", RowCount: 100}}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("coverage source conflict=%v", err)
		}
		badEscape := append([]eventstore.Evidence(nil), escapes...)
		badEscape[99].PayloadSHA256 = strings.Repeat("d", 64)
		if err := counted.ImportReportEscapeProjections(ctx, badEscape); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("escape provenance=%v", err)
		}
		badLanding := landings[0]
		badLanding.Change.Revision++
		if err := counted.ImportReportChangeLandings(ctx, []postgres.ReportLandingImport{badLanding}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("landing revision=%v", err)
		}
		badLanding = landings[0]
		badLanding.Receipt.PayloadSHA256 = strings.Repeat("e", 64)
		if err := counted.ImportReportChangeLandings(ctx, []postgres.ReportLandingImport{badLanding}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("landing receipt=%v", err)
		}

		if err := counted.Close(); err != nil {
			t.Fatal(err)
		}
		var rows int
		var total float64
		if err := admin.QueryRowContext(ctx, `SELECT count(*),sum(cost_estimate) FROM attempt_usage WHERE organization_id=$1`, org).Scan(&rows, &total); err != nil {
			t.Fatal(err)
		}
		if rows != 100 || total != 125 {
			t.Fatalf("usage fidelity/rollback rows=%d total=%v", rows, total)
		}
		var first, latest, outcome string
		if err := admin.QueryRowContext(ctx, `SELECT first_event_id,latest_event_id,outcome_event_id FROM report_attempt_facts WHERE organization_id=$1 AND attempt_id='attempt_bulk'`, org).Scan(&first, &latest, &outcome); err != nil {
			t.Fatal(err)
		}
		if first != "bulk_event_000" || latest != "bulk_event_099" || outcome != "bulk_event_000" {
			t.Fatalf("attempt order=%s/%s/%s", first, latest, outcome)
		}
		t.Run(string(org)+"/same hour current writes preserve immutable import replay", func(t *testing.T) {
			lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(2))
			if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: "attempt_same_hour", Fence: pgstoretest.LeaseFence(lease), RunID: "run_same_hour", Sequence: 1, Status: "running", StartedAt: pgstoretest.HistoryAt(), PayloadObjectKey: ref.Key})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, imported := range usage {
				imported.ProjectID = "prj_2"
				imported.WorkItemID = "wi_2"
				imported.AttemptID = "attempt_same_hour"
				if err := repository.ImportReportUsageFact(ctx, imported); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			record := reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_2", WorkItemID: "wi_2", AttemptID: "attempt_same_hour", Period: now.Truncate(time.Hour), Provider: "example", Model: "same-hour-model", Input: 100, Cost: 1, Currency: "USD"}, UpdatedAt: now}
			if err := repository.ImportReportUsageFact(ctx, record); err != nil {
				t.Fatal(err)
			}
			if err := repository.VerifyReportUsageImport(ctx, org, "attempt_same_hour", strings.Repeat("a", 64), 101); err != nil {
				t.Fatal(err)
			}
			command := reportstore.AttemptUsage{OrganizationID: org, ProjectID: "prj_2", WorkItemID: "wi_2", AttemptID: "attempt_same_hour", LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Sequence: 1, ReceivedAt: now, Currency: "USD", Totals: []reportstore.AttemptUsageTotal{{Provider: "example", Model: "same-hour-model", Input: 250, ReportedCost: new(2.5)}}}
			if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(reportstore.UsageWriter).RecordAttemptUsage(ctx, command)
			}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := repository.ImportReportUsageFact(ctx, record); err != nil {
					t.Fatal(err)
				}
				if err := repository.VerifyReportUsageImport(ctx, org, "attempt_same_hour", strings.Repeat("a", 64), 101); err != nil {
					t.Fatal(err)
				}
			}
			var historical, current int64
			var historicalCost, currentCost float64
			if err := admin.QueryRowContext(ctx, `SELECT sum(input) FILTER(WHERE usage_origin='import'),sum(cost_estimate) FILTER(WHERE usage_origin='import'),sum(input) FILTER(WHERE usage_origin='current'),sum(cost_estimate) FILTER(WHERE usage_origin='current') FROM attempt_usage WHERE organization_id=$1 AND attempt_id='attempt_same_hour' AND period=$2 AND provider='example' AND model='same-hour-model' AND currency='USD'`, org, record.Period).Scan(&historical, &historicalCost, &current, &currentCost); err != nil {
				t.Fatal(err)
			}
			if historical != 100 || historicalCost != 1 || current != 150 || currentCost != 1.5 || historical+current != 250 || historicalCost+currentCost != 2.5 {
				t.Fatalf("same hour origin fidelity=%d/%v + %d/%v", historical, historicalCost, current, currentCost)
			}
			query := reportstore.AnalyticsUsageQuery{ProjectID: "prj_2", From: record.Period, To: record.Period.Add(time.Hour), Limit: 1}
			read := func() reportstore.AnalyticsUsagePage {
				t.Helper()
				var page reportstore.AnalyticsUsagePage
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					page, err = r.(reportstore.AnalyticsUsageReader).ReadAnalyticsUsagePage(ctx, query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			head := read()
			if len(head.Items) != 1 || head.Items[0].Input != 150 || head.Items[0].Cost != 1.5 || head.Next.Origin != "current" {
				t.Fatalf("current origin head=%+v", head)
			}
			query.After = head.Next
			tail := read()
			if len(tail.Items) != 1 || tail.Items[0].Input != 100 || tail.Items[0].Cost != 1 || !tail.Next.Period.IsZero() {
				t.Fatalf("import origin continuation=%+v", tail)
			}
		})
		t.Run(string(org)+"/currency ties retain the usage continuation", func(t *testing.T) {
			period := pgstoretest.HistoryAt().Add(200 * time.Hour)
			for _, currency := range []string{"EUR", "USD"} {
				record := reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_bulk", Period: period, Provider: "example", Model: "currency-seek", Input: 7, Cost: 2, Currency: currency}, UpdatedAt: pgstoretest.HistoryAt()}
				if err := repository.ImportReportUsageFact(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			result, err := admin.ExecContext(ctx, `UPDATE attempt_usage SET currency='usd' WHERE organization_id=$1 AND attempt_id='attempt_bulk' AND period=$2 AND provider='example' AND model='currency-seek' AND currency='USD'`, org, period)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				t.Fatalf("legacy currency rows=%d error=%v", count, err)
			}
			last := reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_bulk", Period: period, Provider: "example", Model: "currency-tail", Input: 11, Cost: 3, Currency: "USD"}, UpdatedAt: pgstoretest.HistoryAt()}
			if err := repository.ImportReportUsageFact(ctx, last); err != nil {
				t.Fatal(err)
			}
			query := reportstore.AnalyticsUsageQuery{ProjectID: "prj_1", From: period, To: period.Add(time.Hour), Limit: 1}
			read := func() reportstore.AnalyticsUsagePage {
				t.Helper()
				var page reportstore.AnalyticsUsagePage
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					page, err = r.(reportstore.AnalyticsUsageReader).ReadAnalyticsUsagePage(ctx, query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			head := read()
			if len(head.Items) != 0 || head.Examined != 2 || head.Next.Source != 1 || head.Next.SourceKey != "EUR" || !head.Partial {
				t.Fatalf("currency head=%+v", head)
			}
			query.After = head.Next
			tail := read()
			if len(tail.Items) != 1 || tail.Items[0].Currency != "usd" || tail.Items[0].Input != 7 || tail.Items[0].Cost != 2 || tail.Examined != 2 || tail.Next.SourceKey != "usd" {
				t.Fatalf("currency tail=%+v", tail)
			}
			query.After = tail.Next
			lastPage := read()
			if len(lastPage.Items) != 1 || lastPage.Items[0].Model != "currency-tail" || lastPage.Items[0].Input != 11 || lastPage.Items[0].Cost != 3 || !lastPage.Next.Period.IsZero() {
				t.Fatalf("legacy currency continuation=%+v", lastPage)
			}
		})
		t.Run(string(org)+"/current usage preserves historical totals", func(t *testing.T) {
			historical := reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_bulk", Period: usage[0].Period, Provider: "example", Model: "example-model", Input: 50, CachedInput: 4, Output: 5, Cost: 2, Currency: "EUR"}, UpdatedAt: pgstoretest.HistoryAt()}
			for range 2 {
				if err := repository.ImportReportUsageFact(ctx, historical); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			for _, tc := range []struct {
				name                  string
				input, cached, output int64
				cost                  float64
				queued                bool
			}{
				{name: "plain", input: 6000, cached: 220, output: 350, cost: 150},
				{name: "queued", input: 7000, cached: 240, output: 400, cost: 175, queued: true},
				{name: "replay", input: 7000, cached: 240, output: 400, cost: 175, queued: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					command := reportstore.AttemptUsage{OrganizationID: org, ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_bulk", LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Sequence: 1, ReceivedAt: now, Currency: "USD", Totals: []reportstore.AttemptUsageTotal{{Provider: "example", Model: "example-model", Input: tc.input, CachedInput: tc.cached, Output: tc.output, ReportedCost: new(tc.cost)}}}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						apply := func(w tenantstore.Writer) error {
							return w.(reportstore.UsageWriter).RecordAttemptUsage(t.Context(), command)
						}
						if !tc.queued {
							return apply(w)
						}
						facts := w.(interface {
							WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
						})
						return facts.WithRunnerEventFacts(t.Context(), eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, AttemptID: "attempt_bulk"}, apply)
					}); err != nil {
						t.Fatal(err)
					}
					var input, cached, output int64
					var cost float64
					if err := admin.QueryRowContext(t.Context(), `SELECT input,cached_input,output,cost_estimate FROM attempt_usage WHERE organization_id=$1 AND attempt_id='attempt_bulk' AND period=$2 AND provider='example' AND model='example-model' AND currency='USD'`, org, now.Truncate(time.Hour)).Scan(&input, &cached, &output, &cost); err != nil {
						t.Fatal(err)
					}
					if input != tc.input-5100 || cached != tc.cached-204 || output != tc.output-305 || cost != tc.cost-125 {
						t.Fatalf("historical delta=%d/%d/%d/%v want=%d/%d/%d/%v", input, cached, output, cost, tc.input-5100, tc.cached-204, tc.output-305, tc.cost-125)
					}
				})
			}
			var input int64
			var cost float64
			if err := admin.QueryRowContext(ctx, `SELECT input,cost_estimate FROM attempt_usage WHERE organization_id=$1 AND attempt_id='attempt_bulk' AND period=$2 AND provider='example' AND model='example-model' AND currency='EUR'`, org, historical.Period).Scan(&input, &cost); err != nil {
				t.Fatal(err)
			}
			if input != 50 || cost != 2 {
				t.Fatalf("second-currency import changed=%d/%v", input, cost)
			}
			unknown := billingCostObservation(t, org, "prj_1", "unknown_baseline", "attempt_bulk", "runner_api", now.Add(-time.Hour), now, 1000000)
			unknown.Observation.AttemptID, unknown.Observation.WorkItemID = "attempt_bulk", "wi_1"
			unknown.Observation.Model, unknown.Observation.Input, unknown.Observation.Coverage = "unknown-model", 100, "complete"
			unknown.ReceivedAt = now
			raw, err := json.Marshal(unknown.Observation)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(ctx, string(org), raw)
			if err != nil {
				t.Fatal(err)
			}
			unknown.Reference = billingstore.AuditReferenceFromArchive(ref)
			if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{unknown}); err != nil {
					return err
				}
				u := w.(reportstore.UsageWriter)
				if err := u.RefreshReportUsage(ctx, []billingstore.CostObservation{unknown}); err != nil {
					return err
				}
				return u.RecordAttemptUsage(ctx, reportstore.AttemptUsage{OrganizationID: org, ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_bulk", LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Sequence: 1, ReceivedAt: now, Currency: "USD", Totals: []reportstore.AttemptUsageTotal{{Provider: unknown.Observation.Provider, Model: "unknown-model", Input: 200}}})
			}); err != nil {
				t.Fatal(err)
			}
			var guessed int
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM attempt_usage WHERE organization_id=$1 AND attempt_id='attempt_bulk' AND model='unknown-model'`, org).Scan(&guessed); err != nil {
				t.Fatal(err)
			}
			if guessed != 0 {
				t.Fatalf("unknown historical baseline became %d hourly facts", guessed)
			}
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.(reportstore.AnalyticsUsageReader).ReadAnalyticsUsagePage(ctx, reportstore.AnalyticsUsageQuery{ProjectID: "prj_1", From: now.Truncate(time.Hour), To: now.Truncate(time.Hour).Add(time.Hour), Limit: 1000})
				if err == nil && !page.Partial {
					t.Fatal("unknown baseline reported complete usage")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})

	}
}
