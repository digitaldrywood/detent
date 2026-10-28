package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAnalyticsAggregateBytesIgnorePayloadSize(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 30, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	var bytes int
	for _, test := range []struct {
		name    string
		size    int
		fixture nativeFixture
	}{{"small", 128, f}, {"large", 1 << 20, other}} {
		t.Run(test.name, func(t *testing.T) {
			issue := test.fixture.create(t, "aggregate size")
			scope := nativeScope{organization: test.fixture.project.OrganizationID, project: test.fixture.project.ID}
			commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
				data := tracker.CollaborationData{ReasonDetail: strings.Repeat("x", test.size), Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "ready", At: now}}
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", data, now); err != nil {
					t.Fatal(err)
				}
			})
			drainTestAnalytics(t, f.service)
			var payloadColumns int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM pragma_table_info('analytics_hourly_rollups') WHERE name='data_json'`).Scan(&payloadColumns); err != nil || payloadColumns != 0 {
				t.Fatalf("aggregate retains payload column: columns=%d error=%v", payloadColumns, err)
			}
			var count, stored int
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT value,length(metric)+length(dimension)+length(skip_source)+length(skip_reason)+length(last_at)+length(CAST(last_ns AS TEXT))+length(CAST(hour AS TEXT))+length(CAST(value AS TEXT)) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='ready'`, scope.organization, scope.project).Scan(&count, &stored); err != nil || count != 1 {
				t.Fatalf("ready count=%d error=%v", count, err)
			}
			if bytes != 0 && stored != bytes {
				t.Fatalf("inline aggregate bytes grew with payload: small=%d large=%d", bytes, stored)
			}
			bytes = stored
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT OR IGNORE INTO analytics_rollup_dirty VALUES(?)`, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			drainTestAnalytics(t, f.service)
			if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT value FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='ready'`, scope.organization, scope.project).Scan(&count); err != nil || count != 1 {
				t.Fatalf("repeated drain double-counted: count=%d error=%v", count, err)
			}
		})
	}
}

func TestAnalyticsLegacyDeletionResumes(t *testing.T) {
	for _, test := range []struct {
		name      string
		interrupt bool
	}{{"completed", false}, {"interrupted", true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			for _, fixture := range []nativeFixture{f, other} {
				for i := range storageConversionRows + 1 {
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,0,'attempt',?,1,?)`, fixture.project.OrganizationID, fixture.project.ID, fmt.Sprint(i), `{"payload":"`+strings.Repeat("x", 4096)+`"}`); err != nil {
						t.Fatal(err)
					}
				}
			}
			remaining := func() int {
				t.Helper()
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_rollup_legacy`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			if test.interrupt {
				if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER interrupt_analytics_delete BEFORE DELETE ON analytics_rollup_legacy WHEN OLD.dimension='1' BEGIN SELECT RAISE(ABORT,'interrupted deletion'); END`); err != nil {
					t.Fatal(err)
				}
				if err := f.service.discoverStorageFamily(t.Context(), storageConversionFamilies[2], f.service.config.now()); err == nil {
					t.Fatal("deletion did not report interruption")
				}
				if got := remaining(); got != 2*(storageConversionRows+1)-1 {
					t.Fatalf("committed deletion was lost: remaining=%d", got)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `DROP TRIGGER interrupt_analytics_delete`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := f.service.discoverStorageFamily(ctx, storageConversionFamilies[2], f.service.config.now()); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled deletion error=%v", err)
			}
			for before := remaining(); before > 0; before = remaining() {
				if err := f.service.discoverStorageFamily(t.Context(), storageConversionFamilies[2], f.service.config.now()); err != nil {
					t.Fatal(err)
				}
				if delta := before - remaining(); delta <= 0 || delta > storageConversionRows {
					t.Fatalf("deletion exceeded batch or stopped: rows=%d", delta)
				}
			}
			for _, fixture := range []nativeFixture{f, other} {
				scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
				progress, err := readStorageConversionProgress(t.Context(), f.service.database.reader, scope)
				if err != nil || len(progress) != 1 || progress[0].Rows != storageConversionRows+1 || !progress[0].Complete || progress[0].Bytes < (storageConversionRows+1)*4096 {
					t.Fatalf("deletion progress=%+v error=%v", progress, err)
				}
			}
		})
	}
}

func TestAnalyticsResponsesSurviveLegacyDeletion(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archived=%t", archive), func(t *testing.T) {
			store := &memoryRunDetails{}
			f := newUsageHostedFixture(t, func(cfg *Config) { cfg.RunDetails = store })
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
			now := f.service.config.now()
			w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
			request := operatortool.AnalyticsRequest{Limit: 100}
			read := func(q nativeQueryer) []any {
				t.Helper()
				ctx := f.service.runDetailContext(t.Context())
				project, err := readNativeAnalytics(ctx, q, scope, request, w)
				if err != nil {
					t.Fatal(err)
				}
				report := nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: w, Projects: []nativeAnalyticsProject{project}}
				diagnostics, err := f.service.readHostedDiagnosticsFrom(ctx, report, q)
				if err != nil {
					t.Fatal(err)
				}
				reports, err := f.service.readHostedReportsFrom(ctx, report, q)
				if err != nil {
					t.Fatal(err)
				}
				return []any{project, diagnostics, reports}
			}
			var compact string
			if archive {
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT token_id FROM token_grants WHERE organization_id=? AND project_id=? ORDER BY token_id LIMIT 1`, scope.organization, scope.project).Scan(&scope.credential.ID); err != nil {
					t.Fatal(err)
				}
				var raw, item string
				var data tracker.NativeRunData
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT data_json,work_item_id FROM native_attempts WHERE id='attempt_browser_usage_0_0'`).Scan(&raw, &item); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(raw), &data); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT id,lease_id,run_id,fencing_token,sequence FROM native_attempts WHERE id='attempt_browser_usage_0_0'`).Scan(&data.AttemptID, &data.LeaseID, &data.RunID, &data.FencingToken, &data.Sequence); err != nil {
					t.Fatal(err)
				}
				runs := map[string]tracker.NativeRunData{runDetailEntry(data): data}
				payload, err := json.Marshal(runs)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := store.StoreDetail(t.Context(), string(scope.organization), payload)
				if err != nil {
					t.Fatal(err)
				}
				ctx := f.service.runDetailContext(t.Context())
				owner := ctx.Value(runDetailContextKey{}).(*runDetailContext)
				owner.reference, owner.runs = &ref, runs
				compact, err = encodeRunDetail(ctx, data, false)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := f.service.database.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := recordRunDetailReference(ctx, tx, scope, tracker.NativeWorkItemID(item), data, formatHubTime(now)); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			want := read(f.service.database.reader)
			if archive {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json=? WHERE id='attempt_browser_usage_0_0'`, compact); err != nil {
					t.Fatal(err)
				}
				if _, err := readAnalyticsCounter(t.Context(), f.service.database.reader, scope, "ready", w.From, w.To); err != nil {
					t.Fatal(err)
				}
				if store.reads != 0 {
					t.Fatalf("counter read hydrated %d objects", store.reads)
				}
			}
			for {
				var pending bool
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM analytics_rollup_legacy)`).Scan(&pending); err != nil {
					t.Fatal(err)
				}
				if !pending {
					break
				}
				if err := f.service.discoverStorageFamily(t.Context(), storageConversionFamilies[2], now); err != nil {
					t.Fatal(err)
				}
			}
			for _, q := range []nativeQueryer{f.service.database.reader, analyticsRollupReader{f.service.database.reader}} {
				assertAnalyticsJSONEqual(t, want, read(q))
			}
			if archive && (store.reads == 0 || store.reads > 2) {
				t.Fatalf("detail reads=%d, want one per request", store.reads)
			}
		})
	}
}
