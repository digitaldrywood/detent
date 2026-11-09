package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptEvidenceIndexMigrationResumesAfterInterruption(t *testing.T) {
	assertConcurrentIndexMigrationRecovery(t, 20261106222200, "attachments_evidence_history_order", "CREATE INDEX attachments_evidence_history_order ON attachments (id)", "CREATE INDEX attachments_evidence_history_order ON public.attachments USING btree (organization_id, project_id, work_item_id, evidence_attempt_id, created_at, id)")
}

func assertConcurrentIndexMigrationRecovery(t *testing.T, version int64, index, initial, want string) {
	t.Helper()
	quotedIndex := `"` + strings.ReplaceAll(index, `"`, `""`) + `"`
	_, fixture := migrationDatabaseAtVersion(t, version-1)
	for _, state := range []string{"absent", "valid", "invalid"} {
		t.Run(state, func(t *testing.T) {
			db := fixture.Database(t)
			db.SetMaxOpenConns(1)
			if state != "absent" {
				if _, err := db.ExecContext(t.Context(), initial); err != nil {
					t.Fatal(err)
				}
				if state == "invalid" {
					if _, err := db.ExecContext(t.Context(), `UPDATE pg_index SET indisvalid=false WHERE indexrelid=$1::regclass`, index); err != nil {
						t.Fatal(err)
					}
				}
				var valid bool
				if err := db.QueryRowContext(t.Context(), `SELECT indisvalid FROM pg_index WHERE indexrelid=$1::regclass`, index).Scan(&valid); err != nil || valid != (state == "valid") {
					t.Fatalf("initial index state=%s valid=%t error=%v", state, valid, err)
				}
			}
			if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
				t.Fatal(err)
			}
			provider, err := migrationProvider(db)
			if err != nil {
				t.Fatal(err)
			}
			for _, rollbackState := range []string{"existing", "already dropped"} {
				if _, err := provider.ApplyVersion(t.Context(), version, true); err != nil {
					t.Fatalf("resume Up from %s: %v", state, err)
				}
				var valid bool
				var definition string
				if err := db.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready,pg_get_indexdef(indexrelid) FROM pg_index WHERE indexrelid=$1::regclass`, index).Scan(&valid, &definition); err != nil {
					t.Fatal(err)
				}
				if !valid || definition != want {
					t.Fatalf("rebuilt index valid=%t definition=%s", valid, definition)
				}
				if rollbackState == "already dropped" {
					if _, err := db.ExecContext(t.Context(), "DROP INDEX CONCURRENTLY "+quotedIndex); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := provider.ApplyVersion(t.Context(), version, false); err != nil {
					t.Fatalf("resume Down with index %s: %v", rollbackState, err)
				}
				var exists bool
				if err := db.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists); err != nil || exists {
					t.Fatalf("rolled back index exists=%t error=%v", exists, err)
				}
			}
		})
	}
}

func TestAttemptEvidenceDoesNotScanHistory(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, repositoryPool := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
	}
	repository, counts := attemptCountedRepository(t, repository, repositoryPool)
	pool := rolePool(t, admin, "detent_runtime_v2", "")
	previous := 0
	for _, history := range []int{2, 50} {
		if err := copyAttemptEvidenceHistory(t.Context(), admin, previous*201+1, history*201); err != nil {
			t.Fatal(err)
		}
		previous = history
		if _, err := admin.ExecContext(t.Context(), `ANALYZE attachments`); err != nil {
			t.Fatal(err)
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, size := range []int{2, 50} {
				t.Run(fmt.Sprintf("history_%dx/%s/page_%d", history, org, size), func(t *testing.T) {
					if err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(tenantstore.Reader) error { return nil }); err != nil {
						t.Fatal(err)
					}
					attempts, ids := make([]leasestore.Attempt, size), make([]string, size)
					for i := range attempts {
						ids[i] = fmt.Sprintf("attempt_%d", i+1)
						attempts[i] = leasestore.Attempt{OrganizationID: org, ID: ids[i]}
					}
					var statement string
					ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
						if strings.Contains(query, "LIMIT 201") {
							statement = query
						}
					})
					counts.exchanges.Store(0)
					counts.statements.Store(0)
					if err := repository.ReadTenantSnapshot(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
						metadata, err := r.(tenantstore.AttemptMetadataReader).AttemptMetadata(ctx, "prj_shared", "wi_shared", attempts, true)
						if err != nil {
							return err
						}
						if len(metadata.Evidence) != size {
							t.Fatalf("evidence attempts=%d, want %d", len(metadata.Evidence), size)
						}
						for i, id := range ids {
							records := metadata.Evidence[id]
							if len(records) != 201 {
								t.Fatalf("attempt %s evidence=%d, want overflow sentinel 201", id, len(records))
							}
							for j, record := range records {
								wantID := fmt.Sprintf("target_%d_%06d", i+1, j+1)
								wantTime := historyAt().Add(time.Duration(j/2) * time.Second)
								if record.OrganizationID != org || record.Metadata.ProjectID != "prj_shared" || record.Metadata.WorkItemID != "wi_shared" || record.EvidenceAttemptID != id || record.Metadata.ID != wantID || !record.Metadata.CreatedAt.Equal(wantTime) || (record.Metadata.DeletedAt != nil) != (j%2 == 1) {
									t.Fatalf("evidence identity, order or deletion changed: attempt=%s row=%d", id, j)
								}
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if statement == "" {
						t.Fatal("evidence statement was not observed")
					}
					if exchanges, statements := counts.exchanges.Load(), counts.statements.Load(); exchanges != 3 || statements != 8 {
						t.Fatalf("metadata snapshot exchanges=%d statements=%d, want 3/8", exchanges, statements)
					}
					tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
						t.Fatal(err)
					}
					var raw []byte
					if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, org, "prj_shared", "wi_shared", ids).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var plan []struct {
						Plan struct {
							searchPlanNode
							Hits  float64 `json:"Shared Hit Blocks"`
							Reads float64 `json:"Shared Read Blocks"`
						}
					}
					if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
						t.Fatalf("decode evidence plan: %v", err)
					}
					var visits func(searchPlanNode) float64
					visits = func(node searchPlanNode) float64 {
						count := 0.0
						if node.Relation == "attachments" {
							count = (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
						}
						for _, child := range node.Plans {
							count += visits(child)
						}
						return count
					}
					count := visits(plan[0].Plan.searchPlanNode)
					blocks := plan[0].Plan.Hits + plan[0].Plan.Reads
					t.Logf("history_rows_per_tenant=%d attempts=%d visited=%.0f buffers=%.0f exchanges=%d statements=%d", history*201*53, size, count, blocks, counts.exchanges.Load(), counts.statements.Load())
					if count != float64(size*201) {
						t.Errorf("evidence scanned %.0f history rows for %d attempts, want %d", count, size, size*201)
					}
					if !plan[0].Plan.usesIndex("attachments_evidence_history_order") || strings.Contains(string(raw), `"Node Type": "Sort"`) {
						t.Error("evidence must use its ordered history index without sorting")
					}
					if blocks > float64(size*240) {
						t.Errorf("evidence buffers=%.0f, want <=%d", blocks, size*240)
					}
				})
			}
		}
	}
}

func copyAttemptEvidenceHistory(ctx context.Context, admin *sql.DB, first, last int) (resultErr error) {
	connection, err := admin.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	return connection.Raw(func(driverConnection any) (resultErr error) {
		native, ok := driverConnection.(*stdlib.Conn)
		if !ok {
			return errors.New("native PostgreSQL fixture connection is required")
		}
		tx, err := native.Conn().Begin(ctx)
		if err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		type group struct{ prefix, project, item, attempt string }
		groups := make([]group, 0, 53)
		for attempt := 1; attempt <= 50; attempt++ {
			groups = append(groups, group{fmt.Sprintf("target_%d_", attempt), "prj_shared", "wi_shared", fmt.Sprintf("attempt_%d", attempt)})
		}
		groups = append(groups, group{"other_project_1_", "prj_other", "wi_shared", "attempt_1"}, group{"other_item_1_", "prj_shared", "wi_other", "attempt_1"}, group{"other_attempt_1_", "prj_shared", "wi_shared", "unrequested"})
		organizations := []string{"org_first", "org_second"}
		count := last - first + 1
		ids := make([]string, count)
		timestamps := make([]time.Time, count)
		for i := range count {
			n := first + i
			ids[i] = strconv.Itoa(n)
			if len(ids[i]) < 6 {
				ids[i] = strings.Repeat("0", 6-len(ids[i])) + ids[i]
			} else {
				ids[i] = ids[i][:6]
			}
			timestamps[i] = historyAt().Add(time.Duration((n-1)/2) * time.Second)
		}
		perTenant := count * len(groups)
		want := len(organizations) * perTenant
		row := 0
		values := []any{nil, nil, nil, "first", "example.txt", "text/plain", int64(1), strings.Repeat("a", 64), nil, nil, nil, nil}
		copied, err := tx.CopyFrom(ctx, pgx.Identifier{"attachments"}, []string{"organization_id", "project_id", "id", "uploader", "name", "content_type", "size", "sha256", "created_at", "work_item_id", "evidence_attempt_id", "deleted_at"}, pgx.CopyFromFunc(func() ([]any, error) {
			if row == want {
				return nil, nil
			}
			tenant := row / perTenant
			offset := row % perTenant
			nIndex := offset / len(groups)
			g := groups[offset%len(groups)]
			values[0], values[1], values[2] = organizations[tenant], g.project, g.prefix+ids[nIndex]
			values[8], values[9], values[10] = timestamps[nIndex], g.item, g.attempt
			values[11] = nil
			if (first+nIndex)%2 == 0 {
				values[11] = historyAt()
			}
			row++
			return values, nil
		}))
		if err != nil {
			return fmt.Errorf("copy attempt history: %w", err)
		}
		if copied != int64(want) {
			return fmt.Errorf("history rows=%d, want%d", copied, want)
		}
		return tx.Commit(ctx)
	})
}
