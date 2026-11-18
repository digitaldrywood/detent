package claimheld_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var planTables = []string{"issues", "native_attempts", "collaboration_events", "queue_entries", "issue_claim_facts", "leases"}

func openPlanHub(t *testing.T) (*hubfixture.Hub, *countingDetails, *candidateTrace) {
	t.Helper()
	details := &countingDetails{DetailStore: hubtest.NewDetails()}
	trace := &candidateTrace{}
	h := hubfixture.OpenPool(t, hubfixture.Config{RunDetails: details}, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, trace)
	})
	for _, table := range planTables {
		if _, err := h.Admin.ExecContext(t.Context(), "ALTER TABLE "+table+" SET (autovacuum_enabled=false)"); err != nil {
			t.Fatal(err)
		}
	}
	return h, details, trace
}

func (f heldFixture) candidatePage(t *testing.T) (claimstore.CandidatePage, capturedCandidate) {
	t.Helper()
	query := claimstore.CandidateQuery{ProjectID: f.Project.ID, Scope: string(f.Project.ID), States: []string{"todo"}, RunnerSelection: true, AvailableAt: f.Hub.Now(), Limit: 100}
	var page claimstore.CandidatePage
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		page, err = r.CandidatePage(t.Context(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return page, f.trace.captured()
}

func growAttempts(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, count int) {
	t.Helper()
	tx, err := h.Admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	result, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts SELECT (jsonb_populate_record(NULL::native_attempts, to_jsonb(a) || jsonb_build_object('id', a.id||'_grown_'||g, 'issue_id', 900000000+g, 'lease_id', a.lease_id||'_grown_'||g, 'fencing_token', a.fencing_token+1000000+g))).*
		FROM (SELECT * FROM native_attempts WHERE organization_id=$1 ORDER BY id LIMIT 1) a, generate_series(1,$2::int) g`, organization, count)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != int64(count) {
		t.Fatalf("grown attempts=%d: %v", rows, err)
	}
	result, err = tx.ExecContext(t.Context(), `INSERT INTO collaboration_events SELECT (jsonb_populate_record(NULL::collaboration_events, to_jsonb(e) || jsonb_build_object('id', e.id||'_grown_'||g, 'work_item_id', e.work_item_id||'_grown_'||g))).*
		FROM (SELECT * FROM collaboration_events WHERE organization_id=$1 AND attempt_id<>'' ORDER BY id LIMIT 1) e, generate_series(1,$2::int) g`, organization, count)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != int64(count) {
		t.Fatalf("grown events=%d: %v", rows, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedHeld(t *testing.T, f heldFixture, held int) {
	t.Helper()
	for index := range held {
		f.hold(t, f.create(t, fmt.Sprintf("Held %s %d", f.Project.ID, index), 0))
	}
}

func TestHeldExclusionPlans(t *testing.T) {
	t.Parallel()
	type measured struct {
		candidates int
		summaries  map[string]planSummary
	}
	results := map[int]measured{}
	for _, held := range []int{20, 60} {
		t.Run(fmt.Sprintf("held %d", held), func(t *testing.T) {
			h, details, trace := openPlanHub(t)
			f := newHeldProject(t, h, "", details, trace, hubfixture.States())
			seedHeld(t, f, held)
			tail := f.create(t, "Eligible tail", 3)
			f.capacity(t)
			growAttempts(t, h, f.Organization(), 5000)
			page, query := f.candidatePage(t)
			if len(page.Items) != 1 || page.Items[0].WorkItemID != tail.WorkItemID {
				t.Fatalf("candidate page=%+v, want only the eligible tail", page.Items)
			}
			var reltuples float64
			if err := h.Admin.QueryRowContext(t.Context(), "SELECT reltuples FROM pg_class WHERE relname='native_attempts'").Scan(&reltuples); err != nil || reltuples >= 0 {
				t.Fatalf("native_attempts statistics already exist: reltuples=%v err=%v", reltuples, err)
			}
			summaries := map[string]planSummary{}
			candidates := held + 1
			check := func(state string) planSummary {
				t.Helper()
				summary := explainCandidate(t, h, query)
				if len(summary.sequentialScan) != 0 || summary.attemptProbes > float64(2*candidates) || summary.eventProbes > float64(candidates) {
					t.Fatalf("%s plan scans=%v attempt probes=%v event probes=%v for %d candidates", state, summary.sequentialScan, summary.attemptProbes, summary.eventProbes, candidates)
				}
				t.Logf("%s held=%d buffers=%d attempt_probes=%v event_probes=%v", state, held, summary.buffers, summary.attemptProbes, summary.eventProbes)
				summaries[state] = summary
				return summary
			}
			check("never analyzed")
			for _, table := range planTables {
				if _, err := h.Admin.ExecContext(t.Context(), "ANALYZE "+table); err != nil {
					t.Fatal(err)
				}
			}
			check("analyzed")
			for _, table := range planTables {
				if _, err := h.Admin.ExecContext(t.Context(), "VACUUM ANALYZE "+table); err != nil {
					t.Fatal(err)
				}
			}
			before := check("vacuum analyzed")
			other := newHeldProject(t, h, hubfixture.Secondary, details, trace, hubfixture.States())
			seedHeld(t, other, held)
			growAttempts(t, h, hubfixture.Secondary, 5000)
			for _, table := range planTables {
				if _, err := h.Admin.ExecContext(t.Context(), "VACUUM ANALYZE "+table); err != nil {
					t.Fatal(err)
				}
			}
			after := check("unrelated tenant")
			if after.attemptProbes != before.attemptProbes || after.eventProbes != before.eventProbes || after.buffers > before.buffers+before.buffers/4 {
				t.Fatalf("unrelated tenant changed the plan work: before=%+v after=%+v", before, after)
			}
			results[held] = measured{candidates: candidates, summaries: summaries}
		})
	}
	small, large := results[20], results[60]
	if small.summaries == nil || large.summaries == nil {
		return
	}
	for state := range small.summaries {
		perSmall := float64(small.summaries[state].buffers) / float64(small.candidates)
		perLarge := float64(large.summaries[state].buffers) / float64(large.candidates)
		t.Logf("%s buffers per candidate: %.1f at %d candidates, %.1f at %d candidates", state, perSmall, small.candidates, perLarge, large.candidates)
		if perLarge > 2*perSmall {
			t.Fatalf("%s per-candidate work grew from %.1f to %.1f", state, perSmall, perLarge)
		}
	}
}

func TestAllHeldScanStopsAfterOnePage(t *testing.T) {
	t.Parallel()
	h, details, trace := openPlanHub(t)
	f := newHeldProject(t, h, "", details, trace, hubfixture.States())
	seedHeld(t, f, 30)
	f.capacity(t)
	page, query := f.candidatePage(t)
	if len(page.Items) != 0 {
		t.Fatalf("all-held candidate page=%+v", page.Items)
	}
	if summary := explainCandidate(t, h, query); len(summary.sequentialScan) != 0 || summary.attemptProbes > 60 || summary.eventProbes > 30 {
		t.Fatalf("all-held plan=%+v", summary)
	}
	before := details.heldReads.Load()
	response := f.claim(t, "", "all-held")
	if response.Code != http.StatusConflict || details.heldReads.Load() != before {
		t.Fatalf("all-held scan status=%d held payload reads=%d: %s", response.Code, details.heldReads.Load()-before, response.Body.Bytes())
	}
}
