package executionrecords_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptEvidenceIndexMigrationResumesAfterInterruption(t *testing.T) {
	t.Parallel()
	const version = int64(20261106222200)
	_, fixture := pgstoretest.MigrationDatabaseAtVersion(t, version-1)
	for _, state := range []string{"absent", "valid", "invalid"} {
		t.Run(state, func(t *testing.T) {
			db := fixture.Database(t)
			db.SetMaxOpenConns(1)
			if state != "absent" {
				if _, err := db.ExecContext(t.Context(), `CREATE INDEX attachments_evidence_history_order ON attachments (id)`); err != nil {
					t.Fatal(err)
				}
				if state == "invalid" {
					if _, err := db.ExecContext(t.Context(), `UPDATE pg_index SET indisvalid=false WHERE indexrelid='attachments_evidence_history_order'::regclass`); err != nil {
						t.Fatal(err)
					}
				}
				var valid bool
				if err := db.QueryRowContext(t.Context(), `SELECT indisvalid FROM pg_index WHERE indexrelid='attachments_evidence_history_order'::regclass`).Scan(&valid); err != nil || valid != (state == "valid") {
					t.Fatalf("initial index state=%s valid=%t error=%v", state, valid, err)
				}
			}
			if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
				t.Fatal(err)
			}
			provider, err := pgstoretest.MigrationProvider(db)
			if err != nil {
				t.Fatal(err)
			}
			for _, rollbackState := range []string{"existing", "already dropped"} {
				if _, err := provider.ApplyVersion(t.Context(), version, true); err != nil {
					t.Fatalf("resume Up from %s: %v", state, err)
				}
				var valid bool
				var definition string
				if err := db.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready,pg_get_indexdef(indexrelid) FROM pg_index WHERE indexrelid='attachments_evidence_history_order'::regclass`).Scan(&valid, &definition); err != nil {
					t.Fatal(err)
				}
				const want = "CREATE INDEX attachments_evidence_history_order ON public.attachments USING btree (organization_id, project_id, work_item_id, evidence_attempt_id, created_at, id)"
				if !valid || definition != want {
					t.Fatalf("rebuilt index valid=%t definition=%s", valid, definition)
				}
				if rollbackState == "already dropped" {
					if _, err := db.ExecContext(t.Context(), `DROP INDEX CONCURRENTLY attachments_evidence_history_order`); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := provider.ApplyVersion(t.Context(), version, false); err != nil {
					t.Fatalf("resume Down with index %s: %v", rollbackState, err)
				}
				var exists bool
				if err := db.QueryRowContext(t.Context(), `SELECT to_regclass('attachments_evidence_history_order') IS NOT NULL`).Scan(&exists); err != nil || exists {
					t.Fatalf("rolled back index exists=%t error=%v", exists, err)
				}
			}
		})
	}
}

func TestAttemptEvidencePreservesOrderAndTenantScope(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, admin)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
	}
	if err := copyAttemptEvidenceHistory(t.Context(), admin, 1, 202); err != nil {
		t.Fatal(err)
	}
	const size = 2
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			ctx := t.Context()
			attempts, ids := make([]leasestore.Attempt, size), make([]string, size)
			for i := range attempts {
				ids[i] = fmt.Sprintf("attempt_%d", i+1)
				attempts[i] = leasestore.Attempt{OrganizationID: org, ID: ids[i]}
			}
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
						wantTime := pgstoretest.HistoryAt().Add(time.Duration(j/2) * time.Second)
						if record.OrganizationID != org || record.Metadata.ProjectID != "prj_shared" || record.Metadata.WorkItemID != "wi_shared" || record.EvidenceAttemptID != id || record.Metadata.ID != wantID || !record.Metadata.CreatedAt.Equal(wantTime) || (record.Metadata.DeletedAt != nil) != (j%2 == 1) {
							t.Fatalf("evidence identity, order or deletion changed: attempt=%s row=%d", id, j)
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
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
		foreignKeys, err := attemptEvidenceForeignKeys(ctx, tx)
		if err != nil {
			return err
		}
		for _, constraint := range foreignKeys {
			if _, err := tx.Exec(ctx, "ALTER TABLE attachments DROP CONSTRAINT "+pgx.Identifier{constraint.name}.Sanitize()); err != nil {
				return err
			}
		}
		indexes, err := attemptEvidenceIndexes(ctx, tx)
		if err != nil {
			return err
		}
		for _, index := range indexes {
			if _, err := tx.Exec(ctx, "DROP INDEX "+pgx.Identifier{"public", index.name}.Sanitize()); err != nil {
				return fmt.Errorf("defer history index: %w", err)
			}
		}
		type group struct{ prefix, project, item, attempt string }
		groups := make([]group, 0, 5)
		for attempt := 1; attempt <= 2; attempt++ {
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
			timestamps[i] = pgstoretest.HistoryAt().Add(time.Duration((n-1)/2) * time.Second)
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
				values[11] = pgstoretest.HistoryAt()
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
		for _, index := range indexes {
			if _, err := tx.Exec(ctx, index.definition); err != nil {
				return fmt.Errorf("build history index: %w", err)
			}
		}
		restoredIndexes, err := attemptEvidenceIndexes(ctx, tx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(restoredIndexes, indexes) {
			return errors.New("history index definitions changed")
		}
		for _, constraint := range foreignKeys {
			if _, err := tx.Exec(ctx, "ALTER TABLE attachments ADD CONSTRAINT "+pgx.Identifier{constraint.name}.Sanitize()+" "+constraint.definition); err != nil {
				return fmt.Errorf("validate history foreign key: %w", err)
			}
		}
		restored, err := attemptEvidenceForeignKeys(ctx, tx)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(restored, foreignKeys) {
			return errors.New("history foreign key definitions changed")
		}
		return tx.Commit(ctx)
	})
}

type attemptEvidenceForeignKey struct{ name, definition string }

type attemptEvidenceIndex struct{ name, definition string }

func attemptEvidenceIndexes(ctx context.Context, tx pgx.Tx) ([]attemptEvidenceIndex, error) {
	rows, err := tx.Query(ctx, `SELECT c.relname,pg_get_indexdef(i.indexrelid),i.indisvalid AND i.indisready
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
WHERE i.indrelid='public.attachments'::regclass AND NOT i.indisprimary AND NOT i.indisunique
 AND NOT EXISTS(SELECT 1 FROM pg_constraint k WHERE k.conindid=i.indexrelid)
ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var indexes []attemptEvidenceIndex
	for rows.Next() {
		var index attemptEvidenceIndex
		var ready bool
		if err := rows.Scan(&index.name, &index.definition, &ready); err != nil {
			return nil, err
		}
		if !ready {
			return nil, errors.New("history fixture requires valid indexes")
		}
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(indexes) == 0 {
		return nil, errors.New("history fixture indexes are missing")
	}
	return indexes, nil
}

func attemptEvidenceForeignKeys(ctx context.Context, tx pgx.Tx) ([]attemptEvidenceForeignKey, error) {
	rows, err := tx.Query(ctx, `SELECT conname,pg_get_constraintdef(oid),convalidated FROM pg_constraint WHERE conrelid='attachments'::regclass AND contype='f' ORDER BY conname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var constraints []attemptEvidenceForeignKey
	for rows.Next() {
		var constraint attemptEvidenceForeignKey
		var validated bool
		if err := rows.Scan(&constraint.name, &constraint.definition, &validated); err != nil {
			return nil, err
		}
		if !validated {
			return nil, errors.New("history fixture requires validated foreign keys")
		}
		constraints = append(constraints, constraint)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(constraints) == 0 {
		return nil, errors.New("history fixture foreign keys are missing")
	}
	return constraints, nil
}
