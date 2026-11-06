package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reportCountingTracer struct {
	count atomic.Int64
}

func (c *reportCountingTracer) record(query string) {
	command := strings.ToUpper(strings.Fields(query)[0])
	if command != "BEGIN" && command != "COMMIT" && command != "ROLLBACK" {
		c.count.Add(1)
	}
}
func (c *reportCountingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.record(data.SQL)
	return ctx
}
func (c *reportCountingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (c *reportCountingTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (c *reportCountingTracer) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	c.record(data.SQL)
}
func (c *reportCountingTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func tracedFeaturePool(t *testing.T, source *pgxpool.Pool, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	config := source.Config()
	config.MaxConns = int32(smallPool().MaxConnections)
	if config.ConnConfig.Tracer == nil {
		config.ConnConfig.Tracer = tracer
	} else {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, tracer)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestReportBulkImportsPreserveTenantFactsAndBoundStatements(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	for _, org := range []tracker.OrganizationID{"org_bulk_first", "org_bulk_second"} {
		if _, err := admin.ExecContext(ctx, `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		seedLeaseFamily(t, repository, org)
		lease := claimLease(t, repository, org, leaseClaim(1))
		objects := newHistoryObjects()
		content := eventstore.NewContent(objects)
		ref, err := objects.StoreDetail(ctx, string(org), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: "attempt_bulk", Fence: leaseFence(lease), RunID: "run_bulk", Sequence: 1, Status: "running", StartedAt: historyAt(), PayloadObjectKey: ref.Key})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		events := make([]postgres.ReportEventProjectionImport, 100)
		usage := make([]reportstore.UsageRecord, 100)
		for i := range 100 {
			value := tracker.CollaborationEvent{ID: fmt.Sprintf("bulk_event_%03d", i), OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: "run.finished", SchemaVersion: 1, Actor: historyActor(), RecordedAt: historyAt().Add(time.Duration(i) * time.Second), Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt_bulk", RunID: "run_bulk", Sequence: int64(i + 1), Outcome: "failed"}}}
			event, err := content.PrepareEvent(ctx, org, historyJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			events[i].Event = event
			usage[i] = reportstore.UsageRecord{OrganizationID: org, UsageFact: reportstore.UsageFact{ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_bulk", Period: historyAt().Add(time.Duration(i) * time.Hour), Provider: "example", Model: "example-model", Input: int64(i + 1), CachedInput: 2, Output: 3, Cost: 1.25, Currency: "usd"}, UpdatedAt: historyAt()}
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
		change, err := content.PrepareChange(ctx, org, historyJSON(t, tracker.ChangeRequest{ID: "change_bulk", OrganizationID: org, ProjectID: "prj_1", WorkItemID: "wi_1", Revision: 1, Title: "Example", CreatedAt: historyAt(), UpdatedAt: historyAt()}))
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareVersion(ctx, org, historyJSON(t, tracker.ChangeVersion{ID: "version_bulk", ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", BaseSHA: "base", Repository: "owner/repository", PolicyID: "example", AttemptID: "attempt_bulk"}, Actor: historyActor(), CreatedAt: historyAt()}))
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
			e, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: org, ChangeID: change.ID, Kind: "discussion"}, historyJSON(t, tracker.ChangeDiscussion{ID: fmt.Sprintf("discussion_%d", i), Actor: historyActor(), CreatedAt: historyAt(), Escape: &tracker.QualityEscape{OccurrenceID: fmt.Sprintf("occurrence_%d", i), Kind: "scheduled_failure", Cause: "validator_miss", ObservedAt: historyAt().Add(time.Duration(i) * time.Second)}}))
			if err != nil {
				t.Fatal(err)
			}
			escapes[i] = e
			c := billingCostObservation(t, org, "prj_1", fmt.Sprintf("bulk_source_%d", i), "attempt_bulk", "runner_api", historyAt().Add(time.Duration(i)*time.Hour), historyAt().Add(time.Duration(i+1)*time.Hour), 1000000)
			c.Observation.AttemptID = "attempt_bulk"
			c.Observation.WorkItemID = "wi_1"
			c.Observation.Model = "example-model"
			c.Observation.Input = int64(i + 1)
			c.Observation.Coverage = "complete"
			c.ReceivedAt = historyAt()
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
		change.LandedAt = new(historyAt())
		change.LandedHeadSHA = &version.HeadSHA
		change.LandedMergeSHA = &receipt.MergeSHA
		change.LandedBaseRef = &receipt.BaseRef
		landings := []postgres.ReportLandingImport{{Change: change, Receipt: receipt}}

		counter := &reportCountingTracer{}
		countedPool := tracedFeaturePool(t, pool, counter)
		counted, err := postgres.NewRepository(ctx, countedPool, smallPool())
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
			counter.count.Store(0)
			if err := run(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			t.Logf("%s: %d statements", name, counter.count.Load())
			if got := counter.count.Load(); got > limit {
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
		conflict[0].Period = historyAt().Add(1000 * time.Hour)
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
	}
}
