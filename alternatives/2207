package hubserver

import (
	"context"
	"database/sql/driver"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
	prepared := &claimPreparation{}
	for _, id := range expired {
		prepared.decisions = append(prepared.decisions, preparedClaimDecision{scope: scope, item: string(id), data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "ready"}}})
	}
	payloadContext := context.WithValue(f.service.runDetailContext(t.Context()), claimPreparationKey{}, prepared)
	if err := prepareClaimDecisionPayloads(payloadContext, prepared); err != nil {
		t.Fatal(err)
	}
	var statements []string
	ctx := withRequestTiming(payloadContext, &requestTiming{trace: func(query string, _ []driver.NamedValue) {
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
	ctx = context.WithValue(ctx, claimPreparationKey{}, nil)
	for _, statement := range statements {
		if strings.Contains(statement, "analytics_") || strings.Contains(statement, "native_board") {
			t.Fatalf("request commit ran derived projection work: %s", statement)
		}
	}
	var cursor string
	for want := 3; want > 0; want-- {
		var dirty int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_dirty").Scan(&dirty); err != nil || dirty != want {
			t.Fatalf("request left dirty cards=%d want %d err=%v", dirty, want, err)
		}
		more, id, err := f.service.database.drainNativeBoardDirtyAfter(f.service.runDetailContext(t.Context()), now, cursor)
		if err != nil || !more {
			t.Fatalf("background dirty refresh: more=%v err=%v", more, err)
		}
		cursor = id
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
	cursor = ""
	want := 2
	for {
		statements = statements[:0]
		more, id, err := f.service.database.drainNativeBoardDirtyAfter(f.service.runDetailContext(ctx), now, cursor)
		if err != nil {
			t.Fatalf("background expiry refresh: more=%v err=%v", more, err)
		}
		if !more {
			break
		}
		if len(statements) == 0 {
			t.Fatal("background expiry refresh recorded no query")
		}
		plan := explainQueryPlan(t, f.service.database.reader, statements[0], []any{cursor, now.UTC().Format("2006-01-02T15:04:05.000000000Z"), cursor})
		indexed := false
		for _, detail := range plan {
			if strings.Contains(detail, "SCAN native_board_items") || strings.Contains(detail, "TEMP B-TREE") {
				t.Fatalf("unbounded background expiry discovery: %v", plan)
			}
			indexed = indexed || strings.Contains(detail, "SEARCH native_board_items USING INDEX sqlite_autoindex_native_board_items_1 (work_item_id>?)")
		}
		if !indexed {
			t.Fatalf("background expiry discovery lacks a work item range search: %v", plan)
		}
		if id != string(issue.WorkItemID) {
			want--
		}
		if count := retained(); count != want {
			t.Fatalf("background expiry refreshed more than one execution: retained=%d want %d", count, want)
		}
		cursor = id
	}
	if want != 0 {
		t.Fatalf("background expiry refresh left %d retained executions", want)
	}
}

const inv18MigrationAdoption = "20261015000000"

func TestINV18HubMigrationsUseTypedColumns(t *testing.T) {
	jsonColumn := regexp.MustCompile(`(?i)\b\w*json\w*\s+(TEXT|BLOB)\b|json_valid\s*\(`)
	paths, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if filepath.Base(path) < inv18MigrationAdoption {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		if filepath.Base(path) == "20261022040000_storage_conversion_progress.sql" && regexp.MustCompile(`(?i)\b(UPDATE|DELETE\s+FROM|INSERT|REPLACE|SELECT|VACUUM|DROP|ALTER|CREATE\s+(UNIQUE\s+)?INDEX)\b`).MatchString(up) {
			t.Errorf("%s performs startup conversion or populated-table work", filepath.Base(path))
		}
		if filepath.Base(path) >= "20261021143500" && regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\b[^;]*\bjulianday\s*\(`).Match(raw) {
			t.Errorf("%s adds a nonportable julianday expression index", filepath.Base(path))
		}
		if match := jsonColumn.FindString(up); match != "" {
			t.Errorf("%s adds a JSON column (%q); store typed columns", filepath.Base(path), match)
		}
	}
}

var inv20JSONFunction = regexp.MustCompile(`(?i)\bjson(?:b)?(?:_(?:array(?:_length)?|each|error_position|extract|group_(?:array|object)|insert|object|patch|pretty|quote|remove|replace|set|tree|type|valid))?\s*\(`)

func TestINV20NoSQLOverJSON(t *testing.T) {
	raw, err := os.ReadFile("testdata/inv20_json_sql_baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	baseline := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("invalid baseline line: %q", line)
		}
		count, err := strconv.Atoi(fields[1])
		if err != nil || count <= 0 {
			t.Fatalf("invalid baseline count: %q", line)
		}
		if _, exists := baseline[fields[0]]; exists {
			t.Fatalf("duplicate baseline path: %s", fields[0])
		}
		baseline[fields[0]] = count
	}
	root := filepath.Join("..", "..")
	for _, directory := range []string{"internal/hubserver", "internal/store", "db/queries"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") || (filepath.Ext(path) != ".go" && filepath.Ext(path) != ".sql") {
				return nil
			}
			if strings.Contains(filepath.ToSlash(path), "/migrations/") && entry.Name() < inv18MigrationAdoption {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			count, err := inv20JSONFunctionCount(path, raw)
			if err != nil {
				return err
			}
			want := baseline[relative]
			if count > want {
				t.Errorf("%s: %d JSON SQL calls exceed baseline %d; use typed columns", relative, count, want)
			}
			if count < want {
				t.Errorf("%s: %d JSON SQL calls below baseline %d; lower the baseline", relative, count, want)
			}
			delete(baseline, relative)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for path := range baseline {
		t.Errorf("%s no longer scanned; remove its baseline entry", path)
	}
}

func inv20JSONFunctionCount(path string, raw []byte) (int, error) {
	if filepath.Ext(path) != ".go" {
		return len(inv20JSONFunction.FindAll(raw, -1)), nil
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
	if err != nil {
		return 0, err
	}
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, unquoteErr := strconv.Unquote(literal.Value)
		if unquoteErr != nil {
			err = unquoteErr
			return false
		}
		count += len(inv20JSONFunction.FindAllString(value, -1))
		return true
	})
	return count, err
}

func TestINV20JSONFunctionPatterns(t *testing.T) {
	for _, test := range []struct {
		input    string
		want     int
		goSource bool
	}{
		{"JSON_EXTRACT (data, '$.id')", 1, false},
		{"json(data), jsonb(data), json_quote(data), json_pretty(data)", 4, false},
		{"json_each(data), json_tree(data)", 2, false},
		{"json_set(data), json_insert(data), json_replace(data), json_patch(data), json_remove(data)", 5, false},
		{"json_object(data), json_array(data), json_group_array(data), json_group_object(data)", 4, false},
		{"json_valid(data), json_type(data), json_array_length(data)", 3, false},
		{"not_json_extract(data), json_extract_suffix(data), json_extract", 0, false},
		{"package example; func json_extract(data []byte) {}; func json_type(value any) {}; func use() { json_extract(nil); json_type(nil) }", 0, true},
		{"package example; const query = `SELECT json_extract(data, '$.id')`", 1, true},
		{`package example; const query = "SELECT json_type(data), json_extract(data, '$.id')"`, 2, true},
		{`package example; const query = "SELECT json_extract\n(data, '$.id')"`, 1, true},
	} {
		t.Run(test.input, func(t *testing.T) {
			path := "query.sql"
			if test.goSource {
				path = "query.go"
			}
			got, err := inv20JSONFunctionCount(path, []byte(test.input))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got %d calls, want %d", got, test.want)
			}
		})
	}
}
