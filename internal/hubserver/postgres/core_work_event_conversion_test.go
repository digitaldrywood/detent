package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreWorkEventObjectsConvertPopulatedUpgrade(t *testing.T) {
	db, fixture := migrationDatabaseAtVersion(t, 20261109002731)
	seedCore(t, db)
	payloads := map[string][]byte{}
	for _, org := range []string{"org_first", "org_second"} {
		for _, id := range []int64{7, 8} {
			raw := []byte(fmt.Sprintf(" {\"tenant\":%q, \"event\":%d, \"detail\": [1, 2], \"text\": \"Café 世界\"} \n", org, id))
			payloads[fmt.Sprintf("%s/%d", org, id)] = raw
			if _, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,$2,41,'progress',$3,$4,$4)`, org, id, string(raw), historyAt()); err != nil {
				t.Fatal(err)
			}
		}
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_first", "org_second"} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,9,41,'progress','{}',$2,$2)`, org, historyAt())
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" || constraint.ConstraintName != "core_work_events_object_writes" {
			t.Fatalf("new inline write was not rejected: %v", err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE core_work_events VALIDATE CONSTRAINT core_work_events_object_writes`); err == nil {
		t.Fatal("contract allowed before source conversion")
	}
	repository, _ := coreWorkEventRepository(t, db, fixture)
	objects := newHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		storage := postgres.NewConversionStorage(repository, org, objects, convertPayload)
		families, err := storage.Start(t.Context())
		if err != nil || !slices.Contains(families, "core-work-events") {
			t.Fatalf("work-event conversion owner missing: families=%v error=%v", families, err)
		}
		if err := hubserver.ConvertStorageBatch(t.Context(), storage); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{7, 8} {
			var ref attachment.ArchiveReference
			var issueID int64
			var kind string
			var legacy *string
			if err := db.QueryRowContext(t.Context(), `SELECT issue_id,kind,payload_json,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM core_work_events WHERE organization_id=$1 AND source_id=$2`, org, id).Scan(&issueID, &kind, &legacy, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize); err != nil {
				t.Fatal(err)
			}
			ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention = "gzip", 1, "archive", attachment.ArchiveRetention
			got, err := objects.ReadDetail(t.Context(), string(org), ref)
			if err != nil || !bytes.Equal(got, payloads[fmt.Sprintf("%s/%d", org, id)]) || legacy != nil || issueID != 41 || kind != "progress" {
				t.Fatalf("converted source changed: org=%s id=%d issue=%d kind=%s inline=%v error=%v", org, id, issueID, kind, legacy, err)
			}
			for _, tenant := range []tracker.OrganizationID{"org_first", "org_second"} {
				if err := repository.ReadTenant(t.Context(), tenant, corestore.Serving, func(r tenantstore.Reader) error {
					exists, err := r.AttachmentObjectExists(t.Context(), "", ref.Key)
					if err == nil && exists != (tenant == org) {
						t.Fatalf("work event reference lost tenant fence: owner=%s tenant=%s exists=%t", org, tenant, exists)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if org == "org_first" {
			if _, err := db.ExecContext(t.Context(), `ALTER TABLE core_work_events VALIDATE CONSTRAINT core_work_events_object_writes`); err == nil {
				t.Fatal("contract allowed while another tenant retains sources")
			}
			var pending int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM core_work_events WHERE organization_id='org_second' AND payload_json IS NOT NULL`).Scan(&pending); err != nil || pending != 2 {
				t.Fatalf("other tenant conversion changed: pending=%d error=%v", pending, err)
			}
		}
		if err := hubserver.ConvertStorageBatch(t.Context(), postgres.NewConversionStorage(repository, org, objects, convertPayload)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE core_work_events VALIDATE CONSTRAINT core_work_events_object_writes`); err != nil {
		t.Fatal(err)
	}
	_, rollbackErr := provider.DownTo(t.Context(), 20261109180410)
	var rejected *pgconn.PgError
	if !errors.As(rollbackErr, &rejected) || rejected.Code != "P0001" {
		t.Fatalf("core work event rollback was not rejected: %v", rollbackErr)
	}
	if version, err := provider.GetDBVersion(t.Context()); err != nil || version != 20261109182156 {
		t.Fatalf("rollback did not retain core work event migration: version=%d error=%v", version, err)
	}
	var retained int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM core_work_events WHERE payload_json IS NULL AND payload_object_key IS NOT NULL`).Scan(&retained); err != nil || retained != 4 {
		t.Fatalf("rollback did not retain verified work event references: rows=%d error=%v", retained, err)
	}

}

type coreWorkEventFaultObjects struct {
	*historyObjects
	mode    string
	cancel  func()
	onStore func(context.Context) error
}

func (s *coreWorkEventFaultObjects) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	if err := ctx.Err(); err != nil {
		return attachment.ArchiveReference{}, err
	}
	if s.mode == "rejected" {
		return attachment.ArchiveReference{}, io.ErrUnexpectedEOF
	}
	ref, err := s.historyObjects.StoreDetail(ctx, org, raw)
	if err != nil {
		return ref, err
	}
	if s.mode == "corrupt-object" {
		s.mu.Lock()
		s.objects[ref.Key][0] ^= 1
		s.mu.Unlock()
	}
	if s.onStore != nil {
		if err := s.onStore(ctx); err != nil {
			return ref, err
		}
	}
	if s.mode == "uncertain" {
		return ref, io.ErrUnexpectedEOF
	}
	if s.mode == "interrupted" {
		s.cancel()
	}
	return ref, nil
}

func (s *coreWorkEventFaultObjects) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := s.historyObjects.ReadDetail(ctx, org, ref)
	if err == nil && s.mode == "corrupt" {
		raw[0] ^= 1
	}
	return raw, err
}

func TestCoreWorkEventObjectsRetainSourcesUntilVerifiedReplay(t *testing.T) {
	for _, mode := range []string{"rejected", "uncertain", "corrupt", "corrupt-object", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			db, fixture := migrationDatabaseAtVersion(t, 20261109002731)
			seedCore(t, db)
			for _, org := range []string{"org_first", "org_second"} {
				for _, id := range []int64{7, 8} {
					if _, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,$2,41,'progress',' {"step": "compile"} ',$3,$3)`, org, id, historyAt()); err != nil {
						t.Fatal(err)
					}
				}
			}
			provider, err := migrationProvider(db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Up(t.Context()); err != nil {
				t.Fatal(err)
			}
			repository, _ := coreWorkEventRepository(t, db, fixture)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			objects := &coreWorkEventFaultObjects{historyObjects: newHistoryObjects(), mode: mode, cancel: cancel}
			err = hubserver.ConvertStorageBatch(ctx, postgres.NewConversionStorage(repository, "org_first", objects, convertPayload))
			expected := error(io.ErrUnexpectedEOF)
			if mode == "corrupt" || mode == "corrupt-object" {
				expected = artifact.ErrIntegrity
			}
			if mode == "interrupted" {
				expected = context.Canceled
			}
			if !errors.Is(err, expected) {
				t.Fatalf("wrong verification failure: got=%v want=%v", err, expected)
			}
			for _, org := range []string{"org_first", "org_second"} {
				var retained int
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM core_work_events WHERE organization_id=$1 AND payload_json=' {"step": "compile"} ' AND payload_object_key IS NULL`, org).Scan(&retained); err != nil || retained != 2 {
					t.Fatalf("source loss: tenant=%s retained=%d error=%v", org, retained, err)
				}
			}
			objects.mode = ""
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				for range 2 {
					if err := hubserver.ConvertStorageBatch(t.Context(), postgres.NewConversionStorage(repository, org, objects, convertPayload)); err != nil {
						t.Fatal(err)
					}
				}
				var rows, size int64
				if err := db.QueryRowContext(t.Context(), `SELECT converted_rows,converted_bytes FROM storage_conversion_progress WHERE organization_id=$1 AND project_id='prj_shared' AND family='core-work-events' AND aggregate_id='41'`, org).Scan(&rows, &size); err != nil || rows != 2 || size != 2*int64(len(` {"step": "compile"} `)) {
					t.Fatalf("replayed progress changed: rows=%d bytes=%d error=%v", rows, size, err)
				}
				var inline int
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM core_work_events WHERE organization_id=$1 AND payload_json IS NOT NULL`, org).Scan(&inline); err != nil || inline != 0 {
					t.Fatalf("replay did not convert sources: inline=%d error=%v", inline, err)
				}
			}
		})
	}
}

func coreWorkEventRepository(t *testing.T, db *sql.DB, fixture *pgtest.Template) (*postgres.Repository, *pgxpool.Pool) {
	t.Helper()
	pool := rolePoolFromTemplate(t, fixture, db, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository, pool
}

func TestCoreWorkEventObjectsFenceConcurrentReplay(t *testing.T) {
	db, fixture := migrationDatabaseAtVersion(t, 20261109002731)
	seedCore(t, db)
	for _, org := range []string{"org_first", "org_second"} {
		for _, id := range []int64{7, 8} {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,$2,41,'progress',' {"step": "compile"} ',$3,$3)`, org, id, historyAt()); err != nil {
				t.Fatal(err)
			}
		}
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	repository, pool := coreWorkEventRepository(t, db, fixture)
	base := newHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		objects := &coreWorkEventFaultObjects{historyObjects: base}
		objects.onStore = func(ctx context.Context) error {
			objects.onStore = nil
			if pool.Stat().AcquiredConns() != 0 {
				return errors.New("conversion held a database connection during object publication")
			}
			return hubserver.ConvertStorageBatch(ctx, postgres.NewConversionStorage(repository, org, base, convertPayload))
		}
		storage := postgres.NewConversionStorage(repository, org, objects, convertPayload)
		if _, err := storage.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		turn := storageconversion.Turn{Visit: func(storageconversion.Candidate) bool { return false }, Charge: func(int) error { return nil }, Expand: func(int) error { return nil }, Advance: func(func(context.Context) error) {}}
		if err := storage.Convert(t.Context(), storageconversion.FamilyCursor{Family: "core-work-events"}, turn); err != nil {
			t.Fatal(err)
		}
		var rows int64
		if err := db.QueryRowContext(t.Context(), `SELECT converted_rows FROM storage_conversion_progress WHERE organization_id=$1 AND project_id='prj_shared' AND family='core-work-events' AND aggregate_id='41'`, org).Scan(&rows); err != nil || rows != 2 {
			t.Fatalf("concurrent replay double-counted history: rows=%d error=%v", rows, err)
		}
	}
}
