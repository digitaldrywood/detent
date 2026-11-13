package schemaupgrade_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreWorkEventObjectsConvertPopulatedUpgrade(t *testing.T) {
	db, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261109002731)
	pgstoretest.SeedCore(t, db)
	payloads := map[string][]byte{}
	for _, org := range []string{"org_first", "org_second"} {
		for _, id := range []int64{7, 8} {
			raw := []byte(fmt.Sprintf(" {\"tenant\":%q, \"event\":%d, \"detail\": [1, 2], \"text\": \"Café 世界\"} \n", org, id))
			payloads[fmt.Sprintf("%s/%d", org, id)] = raw
			if _, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,$2,41,'progress',$3,$4,$4)`, org, id, string(raw), pgstoretest.HistoryAt()); err != nil {
				t.Fatal(err)
			}
		}
	}
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 20261109184650); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org_first", "org_second"} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,9,41,'progress','{}',$2,$2)`, org, pgstoretest.HistoryAt())
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" || constraint.ConstraintName != "core_work_events_object_writes" {
			t.Fatalf("new inline write was not rejected: %v", err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE core_work_events VALIDATE CONSTRAINT core_work_events_object_writes`); err == nil {
		t.Fatal("contract allowed before source conversion")
	}
	repository, _ := pgstoretest.CoreWorkEventRepository(t, db, fixture)
	objects := pgstoretest.NewHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		storage := postgres.NewConversionStorage(repository, org, objects, pgstoretest.ConvertPayload)
		families, err := storage.Start(t.Context())
		if err != nil || !slices.Contains(families, "core-work-events") {
			t.Fatalf("work-event conversion owner missing: families=%v error=%v", families, err)
		}
		if err := storageconversion.ConvertStorageBatch(t.Context(), storage); err != nil {
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
		if err := storageconversion.ConvertStorageBatch(t.Context(), postgres.NewConversionStorage(repository, org, objects, pgstoretest.ConvertPayload)); err != nil {
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
	*pgstoretest.HistoryObjects
	mode      string
	cancel    func()
	onStore   func(context.Context) error
	onContent func(context.Context, []byte) error
}

func (s *coreWorkEventFaultObjects) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	if err := ctx.Err(); err != nil {
		return attachment.ArchiveReference{}, err
	}
	if s.mode == "rejected" {
		return attachment.ArchiveReference{}, io.ErrUnexpectedEOF
	}
	ref, err := s.HistoryObjects.StoreDetail(ctx, org, raw)
	if err != nil {
		return ref, err
	}
	if s.mode == "corrupt-object" {
		s.Mu.Lock()
		s.Objects[ref.Key][0] ^= 1
		s.Mu.Unlock()
	}
	if s.onStore != nil {
		if err := s.onStore(ctx); err != nil {
			return ref, err
		}
	}
	if s.onContent != nil {
		if err := s.onContent(ctx, raw); err != nil {
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
	raw, err := s.HistoryObjects.ReadDetail(ctx, org, ref)
	if err == nil && s.mode == "corrupt" {
		raw[0] ^= 1
	}
	return raw, err
}

func TestCoreWorkEventObjectsRetainSourcesUntilVerifiedReplay(t *testing.T) {
	for _, mode := range []string{"rejected", "uncertain", "corrupt", "corrupt-object", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			db, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261109002731)
			pgstoretest.SeedCore(t, db)
			for _, org := range []string{"org_first", "org_second"} {
				for _, id := range []int64{7, 8} {
					if _, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,$2,41,'progress',' {"step": "compile"} ',$3,$3)`, org, id, pgstoretest.HistoryAt()); err != nil {
						t.Fatal(err)
					}
				}
			}
			provider, err := pgstoretest.MigrationProvider(db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Up(t.Context()); err != nil {
				t.Fatal(err)
			}
			repository, _ := pgstoretest.CoreWorkEventRepository(t, db, fixture)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			objects := &coreWorkEventFaultObjects{HistoryObjects: pgstoretest.NewHistoryObjects(), mode: mode, cancel: cancel}
			err = storageconversion.ConvertStorageBatch(ctx, postgres.NewConversionStorage(repository, "org_first", objects, pgstoretest.ConvertPayload))
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
					if err := storageconversion.ConvertStorageBatch(t.Context(), postgres.NewConversionStorage(repository, org, objects, pgstoretest.ConvertPayload)); err != nil {
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
	for _, aggregates := range []int{1, 3} {
		t.Run(fmt.Sprintf("interrupted progress/%d aggregates", aggregates), func(t *testing.T) {
			db, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261109002731)
			pgstoretest.SeedCore(t, db)
			payloads := map[int64][]byte{}
			for _, org := range []string{"org_first", "org_second"} {
				if aggregates == 3 {
					for _, id := range []int64{43, 44} {
						if _, err := db.ExecContext(t.Context(), `INSERT INTO issues (organization_id,id,native_id,project_id,number,title,native_source_key) VALUES ($1,$2::bigint,'wi_conversion_'||($2::bigint)::text,'prj_shared',$2::bigint-41,'Example','conversion_'||($2::bigint)::text)`, org, id); err != nil {
							t.Fatal(err)
						}
					}
				}
				for index, id := range []int64{7, 8, 9} {
					issue := int64(41)
					if aggregates == 3 && index > 0 {
						issue = int64(42 + index)
					}
					raw := []byte(fmt.Sprintf(" {\"step\":\"compile\",\"source_id\":%d} ", id))
					payloads[id] = raw
					if _, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,$2,$3,'progress',$4,$5,$5)`, org, id, issue, string(raw), pgstoretest.HistoryAt()); err != nil {
						t.Fatal(err)
					}
				}
			}
			provider, err := pgstoretest.MigrationProvider(db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Up(t.Context()); err != nil {
				t.Fatal(err)
			}
			repository, _ := pgstoretest.CoreWorkEventRepository(t, db, fixture)
			objects := &coreWorkEventFaultObjects{HistoryObjects: pgstoretest.NewHistoryObjects()}
			count := func(org string) int {
				t.Helper()
				var count int
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM core_work_events WHERE organization_id=$1 AND payload_json IS NULL`, org).Scan(&count); err != nil {
					t.Fatal(err)
				}
				return count
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			writes := 0
			objects.onContent = func(_ context.Context, raw []byte) error {
				if held := repository.Stats().AcquiredConns(); held != 0 {
					t.Fatalf("conversion object write holds %d PostgreSQL connections", held)
				}
				writes++
				if got := count("org_first"); got != writes-1 {
					t.Fatalf("committed units=%d before unit%d", got, writes)
				}
				var source struct {
					ID int64 `json:"source_id"`
				}
				if err := json.Unmarshal(raw, &source); err != nil {
					return err
				}
				if source.ID != int64(writes+6) {
					t.Fatalf("conversion source order=%d want%d", source.ID, writes+6)
				}
				if writes == 2 {
					cancel()
				}
				return nil
			}
			if err := storageconversion.ConvertStorageBatch(ctx, postgres.NewConversionStorage(repository, "org_first", objects, pgstoretest.ConvertPayload)); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled run: %v", err)
			}
			if got := count("org_first"); got != 1 {
				t.Fatalf("committed progress after cancellation=%d", got)
			}
			if got := count("org_second"); got != 0 {
				t.Fatalf("cancellation crossed tenant: converted=%d", got)
			}
			resumed := []int64{}
			objects.onContent = func(_ context.Context, raw []byte) error {
				if held := repository.Stats().AcquiredConns(); held != 0 {
					t.Fatalf("conversion object write holds %d PostgreSQL connections", held)
				}
				var source struct {
					ID int64 `json:"source_id"`
				}
				if err := json.Unmarshal(raw, &source); err != nil {
					return err
				}
				if source.ID == 7 {
					t.Fatal("repeated committed unit")
				}
				if len(resumed) == 0 && source.ID == 8 {
					t.Fatal("interrupted unit starved unattempted work")
				}
				if len(resumed) == 1 && source.ID != 8 {
					t.Fatal("wrapped pass did not revisit interrupted unit")
				}
				resumed = append(resumed, source.ID)
				return nil
			}
			if err := storageconversion.ConvertStorageBatch(t.Context(), postgres.NewConversionStorage(repository, "org_first", objects, pgstoretest.ConvertPayload)); err != nil {
				t.Fatal(err)
			}
			if got := count("org_first"); got != 3 || len(resumed) != 2 {
				t.Fatalf("resumed units=%d committed=%d want3", len(resumed), got)
			}
			objects.onContent = nil
			for range 2 {
				if err := storageconversion.ConvertStorageBatch(t.Context(), postgres.NewConversionStorage(repository, "org_second", objects, pgstoretest.ConvertPayload)); err != nil {
					t.Fatal(err)
				}
			}
			for _, org := range []string{"org_first", "org_second"} {
				if got := count(org); got != 3 {
					t.Fatalf("tenant%s committed=%d want3", org, got)
				}
				var rows int64
				if err := db.QueryRowContext(t.Context(), `SELECT sum(converted_rows) FROM storage_conversion_progress WHERE organization_id=$1 AND family='core-work-events'`, org).Scan(&rows); err != nil || rows != 3 {
					t.Fatalf("durable converted rows=%d error=%v", rows, err)
				}
				for _, id := range []int64{7, 8, 9} {
					ref := attachment.ArchiveReference{Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
					var legacy *string
					if err := db.QueryRowContext(t.Context(), `SELECT payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_json FROM core_work_events WHERE organization_id=$1 AND source_id=$2`, org, id).Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &legacy); err != nil {
						t.Fatal(err)
					}
					raw, err := objects.ReadDetail(t.Context(), org, ref)
					if err != nil || legacy != nil || !bytes.Equal(raw, payloads[id]) {
						t.Fatalf("resumed source%d changed: inline=%v error=%v", id, legacy, err)
					}
				}
			}
		})
	}

}

func TestCoreWorkEventObjectsFenceConcurrentReplay(t *testing.T) {
	db, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261109002731)
	pgstoretest.SeedCore(t, db)
	for _, org := range []string{"org_first", "org_second"} {
		for _, id := range []int64{7, 8} {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO core_work_events (organization_id,source_id,issue_id,kind,payload_json,occurred_at,recorded_at) VALUES ($1,$2,41,'progress',' {"step": "compile"} ',$3,$3)`, org, id, pgstoretest.HistoryAt()); err != nil {
				t.Fatal(err)
			}
		}
	}
	provider, err := pgstoretest.MigrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	repository, pool := pgstoretest.CoreWorkEventRepository(t, db, fixture)
	base := pgstoretest.NewHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		objects := &coreWorkEventFaultObjects{HistoryObjects: base}
		objects.onStore = func(ctx context.Context) error {
			objects.onStore = nil
			if pool.Stat().AcquiredConns() != 0 {
				return errors.New("conversion held a database connection during object publication")
			}
			return storageconversion.ConvertStorageBatch(ctx, postgres.NewConversionStorage(repository, org, base, pgstoretest.ConvertPayload))
		}
		storage := postgres.NewConversionStorage(repository, org, objects, pgstoretest.ConvertPayload)
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
