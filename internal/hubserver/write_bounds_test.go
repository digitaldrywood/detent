package hubserver

import (
	"database/sql/driver"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestINV18RequestCommitDoesNoDerivedWork(t *testing.T) {
	now := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	issue := f.create(t, "bounded write")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	worker := f.worker(t, "bounded-worker")
	var expired [2]tracker.NativeWorkItemID
	for index := range 2 {
		item := f.create(t, "Retained completed execution "+strconv.Itoa(index))
		lease := claimNativeAttempt(t, f, worker, "bounded-machine-"+strconv.Itoa(index), "bounded-session-"+strconv.Itoa(index), item.WorkItemID)
		event := nativeStartedEvent(lease)
		event.IdempotencyKey = "bounded-start-" + strconv.Itoa(index)
		path := f.base + "/work-items/" + string(item.WorkItemID) + "/events"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
		event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "bounded-completed-"+strconv.Itoa(index), 2
		event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
		expired[index] = item.WorkItemID
	}
	drainTestNativeBoard(t, f.service)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_items SET expires_at=? WHERE work_item_id=?", now.UTC().Format("2006-01-02T15:04:05.000000000Z"), issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationFiles.ReadFile("migrations/20261017153000_board_execution_expiry.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(migration), "-- +goose Down")
	if _, err := f.service.database.db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	var retainedExpiry int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_items WHERE work_item_id IN (?,?) AND expires_at<>''", expired[0], expired[1]).Scan(&retainedExpiry); err != nil || retainedExpiry != 2 {
		t.Fatalf("migration retained expiry=%d want 2 err=%v", retainedExpiry, err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	var statements []string
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		statements = append(statements, query)
	}})
	tx, err := f.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := appendNativeHistory(ctx, tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	for _, id := range expired {
		if err := appendNativeHistory(ctx, tx, scope, string(id), "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "ready"}}, now); err != nil {
			t.Fatal(err)
		}
	}
	statements = statements[:0]
	if err := f.service.database.commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if strings.Contains(statement, "analytics_") || strings.Contains(statement, "native_board") {
			t.Fatalf("request commit ran derived projection work: %s", statement)
		}
	}
	for want := 3; want > 0; want-- {
		var dirty int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_dirty").Scan(&dirty); err != nil || dirty != want {
			t.Fatalf("request left dirty cards=%d want %d err=%v", dirty, want, err)
		}
		more, err := f.service.database.drainNativeBoardDirty(t.Context(), now)
		if err != nil || !more {
			t.Fatalf("background dirty refresh: more=%v err=%v", more, err)
		}
	}
	now = now.Add(91 * time.Second)
	tx, err = f.service.database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.service.database.commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	retained := func() int {
		t.Helper()
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_items WHERE work_item_id IN (?,?) AND json_extract(record_json,'$.execution') IS NOT NULL", expired[0], expired[1]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if count := retained(); count != 2 {
		t.Fatalf("request commit refreshed unrelated expired executions: retained=%d want 2", count)
	}
	for want := 1; want >= 0; want-- {
		statements = statements[:0]
		more, err := f.service.database.drainNativeBoardDirty(ctx, now)
		if err != nil || !more {
			t.Fatalf("background expiry refresh: more=%v err=%v", more, err)
		}
		if len(statements) == 0 {
			t.Fatal("background expiry refresh recorded no query")
		}
		plan := explainQueryPlan(t, f.service.database.reader, statements[0], []any{now.UTC().Format("2006-01-02T15:04:05.000000000Z")})
		indexed := false
		for _, detail := range plan {
			if strings.Contains(detail, "SCAN native_board_items") || strings.Contains(detail, "TEMP B-TREE") {
				t.Fatalf("unbounded background expiry discovery: %v", plan)
			}
			indexed = indexed || strings.Contains(detail, "native_board_expiry_idx")
		}
		if !indexed {
			t.Fatalf("background expiry discovery lacks an expiry index: %v", plan)
		}
		if count := retained(); count != want {
			t.Fatalf("background expiry refreshed more than one execution: retained=%d want %d", count, want)
		}
	}
	if more, err := f.service.database.drainNativeBoardDirty(t.Context(), now); err != nil || more {
		t.Fatalf("completed expiry refresh still has work: more=%v err=%v", more, err)
	}
}

func TestINV18HubMigrationsUseTypedColumns(t *testing.T) {
	const adopted = "20261015000000"
	jsonColumn := regexp.MustCompile(`(?i)\b\w*json\w*\s+(TEXT|BLOB)\b|json_valid\s*\(`)
	paths, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if filepath.Base(path) < adopted {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		if match := jsonColumn.FindString(up); match != "" {
			t.Errorf("%s adds a JSON column (%q); store typed columns", filepath.Base(path), match)
		}
	}
}
