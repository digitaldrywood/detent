package hubserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAnalyticsMaintenanceDeadline(t *testing.T) {
	for _, stop := range []string{"complete", "cancel", "deadline"} {
		t.Run(stop, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Service{config: Config{Logger: slog.Default()}}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var calls []string
				drains := 0
				steps := []analyticsMaintenanceStep{
					{"drain", func(ctx context.Context, _ time.Time) (bool, error) {
						calls = append(calls, "drain")
						drains++
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) > backgroundStorageBudget {
							t.Error("analytics maintenance lacks its small per-tick deadline")
						}
						switch stop {
						case "cancel":
							cancel()
						case "deadline":
							<-ctx.Done()
							if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
								t.Errorf("maintenance stopped without reaching its deadline: %v", ctx.Err())
							}
						}
						return drains < 2, ctx.Err()
					}},
					{"backfill", func(context.Context, time.Time) (bool, error) {
						calls = append(calls, "backfill")
						return false, nil
					}},
				}
				s.runAnalyticsMaintenance(ctx, time.Now(), steps)
				want := "drain,backfill,drain"
				if stop != "complete" {
					want = "drain"
				}
				if got := strings.Join(calls, ","); got != want {
					t.Fatalf("maintenance order=%s want=%s", got, want)
				}
			})
		})
	}
}

const analyticsProjectionTestEvents = 1000

func seedAnalyticsProjectionHistory(t *testing.T, f usageHostedFixture, attempts int) []tracker.NativeWorkItemID {
	t.Helper()
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var machine string
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM machines LIMIT 1").Scan(&machine); err != nil {
		t.Fatal(err)
	}
	var items []tracker.NativeWorkItemID
	for entity := range 2 {
		item, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: fmt.Sprintf("Example history %d", entity), Body: nativeContractTestBody, State: "Todo"}, f.service.config.now().Add(-time.Duration(max(attempts, analyticsProjectionTestEvents)+2)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item.WorkItemID)
		var issue int64
		if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", item.WorkItemID).Scan(&issue); err != nil {
			t.Fatal(err)
		}
		for i := range attempts {
			id := fmt.Sprintf("example-history-%d-%d", entity, i)
			at := formatHubTime(f.service.config.now().Add(-time.Duration(i+1) * time.Hour))
			end := formatHubTime(f.service.config.now().Add(-time.Duration(i+1)*time.Hour + time.Minute))
			result, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, issue, machine, id, end, at, at, end, at, end)
			if err != nil {
				t.Fatal(err)
			}
			fence, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES(?,?,?,?,?,?,?,1,'succeeded','{}',?,?)`, id, scope.organization, scope.project, item.WorkItemID, id, fence, id, at, end); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return items
}

func TestNativeBoardProjectionWriterBound(t *testing.T) {
	for _, attempts := range []int{1, 1000} {
		t.Run(strconv.Itoa(attempts), func(t *testing.T) {
			f := newUsageHostedFixture(t)
			items := seedAnalyticsProjectionHistory(t, f, attempts)
			for _, item := range items {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_board_dirty(work_item_id) VALUES(?) ON CONFLICT DO NOTHING", item); err != nil {
					t.Fatal(err)
				}
				historyReads, writes := 0, 0
				timing := &requestTiming{trace: func(query string, _ []driver.NamedValue) {
					if strings.Contains(query, "FROM native_attempts") {
						historyReads++
						if f.service.database.db.Stats().InUse != 0 {
							t.Error("board attempt history was read while holding the writer")
						}
					}
					if f.service.database.db.Stats().InUse != 0 && (strings.HasPrefix(query, "INSERT") || strings.HasPrefix(query, "UPDATE") || strings.HasPrefix(query, "DELETE")) {
						writes++
					}
				}}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_board_dirty WHERE work_item_id<>?", item); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_board_items"); err != nil {
					t.Fatal(err)
				}
				ctx, writer := withWriterTiming(withRequestTiming(t.Context(), timing))
				more, id, err := f.service.database.drainNativeBoardDirtyAfter(ctx, f.service.config.now(), "")
				if err != nil {
					t.Fatal(err)
				}
				if !more || id != string(item) {
					t.Fatalf("candidate=%s more=%t", id, more)
				}
				if historyReads == 0 || writes > 12 || writer.txs.Load() > 1 {
					t.Fatalf("history=%d reads=%d writes=%d transactions=%d", attempts, historyReads, writes, writer.txs.Load())
				}
			}
		})
	}
}

func TestNativeBoardProjectionPreservesConcurrentDirtyWork(t *testing.T) {
	for _, mutation := range []string{"title", "delete", "remark", "reinsert", "expired"} {
		t.Run(mutation, func(t *testing.T) {
			f := newUsageHostedFixture(t)
			items := seedAnalyticsProjectionHistory(t, f, 1)
			if mutation == "delete" {
				for i, item := range items {
					id := item + "-deletable"
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO issues
(native_id,organization_id,project_id,workflow_state_id,number,title,body,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at)
SELECT ?,organization_id,project_id,workflow_state_id,number+10000,title,body,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at
FROM issues WHERE native_id=?`, id, item); err != nil {
						t.Fatal(err)
					}
					items[i] = id
				}
			}
			drainTestNativeBoard(t, f.service)
			for _, item := range items {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT OR IGNORE INTO native_board_dirty(work_item_id) VALUES(?)", item); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_board_dirty WHERE work_item_id<>?", item); err != nil {
					t.Fatal(err)
				}
				if mutation == "expired" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_board_dirty WHERE work_item_id=?", item); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_items SET expires_at=? WHERE work_item_id=?", formatHubTime(f.service.config.now().Add(-time.Hour)), item); err != nil {
						t.Fatal(err)
					}
				}
				var oldJSON string
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM native_board_items WHERE work_item_id=?", item).Scan(&oldJSON); err != nil {
					t.Fatal(err)
				}
				changed := false
				timing := &requestTiming{trace: func(query string, _ []driver.NamedValue) {
					if changed || !strings.HasPrefix(query, "SELECT i.organization_id,i.project_id, NOT") {
						return
					}
					changed = true
					if f.service.database.db.Stats().InUse != 0 {
						t.Error("card preparation holds the writer")
						return
					}
					var err error
					switch mutation {
					case "title", "expired":
						_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title='Concurrent example' WHERE native_id=?", item)
					case "delete":
						_, err = f.service.database.db.ExecContext(t.Context(), "DELETE FROM issues WHERE native_id=?", item)
					case "reinsert":
						_, err = f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_board_dirty WHERE work_item_id=?", item)
						if err != nil {
							t.Error(err)
							return
						}
						fallthrough
					case "remark":
						_, err = f.service.database.db.ExecContext(t.Context(), "INSERT OR IGNORE INTO native_board_dirty(work_item_id) VALUES(?)", item)
					}
					if err != nil {
						t.Error(err)
					}
				}}
				cursor := ""
				for {
					more, id, err := f.service.database.drainNativeBoardDirtyAfter(withRequestTiming(t.Context(), timing), f.service.config.now(), cursor)
					if err != nil {
						t.Fatal(err)
					}
					if id == string(item) {
						break
					}
					if !more {
						t.Fatal("candidate missing")
					}
					cursor = id
				}
				var dirty bool
				var raw string
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json,EXISTS(SELECT 1 FROM native_board_dirty WHERE work_item_id=?) FROM native_board_items WHERE work_item_id=?", item, item).Scan(&raw, &dirty); err != nil {
					t.Fatal(err)
				}
				if !changed || !dirty || raw != oldJSON {
					t.Fatalf("stale card published: changed=%t dirty=%t", changed, dirty)
				}
				drainTestNativeBoard(t, f.service)
				if mutation == "delete" {
					var exists bool
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM native_board_items WHERE work_item_id=?)", item).Scan(&exists); err != nil || exists {
						t.Fatalf("deleted card remains=%t error=%v", exists, err)
					}
				} else {
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM native_board_items WHERE work_item_id=?", item).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var card tracker.NativeBoardCard
					if err := json.Unmarshal([]byte(raw), &card); err != nil {
						t.Fatal(err)
					}
					if (mutation == "title" || mutation == "expired") && card.Issue.Title != "Concurrent example" {
						t.Fatalf("updated title=%s", card.Issue.Title)
					}
					if (mutation == "remark" || mutation == "reinsert") && raw != oldJSON {
						t.Fatal("unchanged card changed after retry")
					}
				}
			}
		})
	}
}
