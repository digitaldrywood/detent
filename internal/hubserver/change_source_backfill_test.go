package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeSourceBackfillPreservesReadersAndUsage(t *testing.T) {
	t.Parallel()
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}
	f := newChangeFixture(t, openTestService(t, config))
	ctx := f.service.runDetailContext(t.Context())
	stopMaintenance := func() {
		t.Helper()
		if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
	}
	stopMaintenance()
	sources := []struct {
		content []byte
		version tracker.ChangeVersion
		bundle  []byte
	}{
		{content: bytes.Repeat([]byte("retained source\n"), 100000)},
		{content: bytes.Repeat([]byte("other source\n"), 100000)},
		{content: changeSourceBinaryFixture()},
	}
	expected := ""
	for index, source := range sources {
		input := changeTestInput()
		bundle := changeTestSource(t, &input, source.content)
		request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source-" + strconv.Itoa(index)}, ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: bundle}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		var version tracker.ChangeVersion
		decodeHubResponse(t, response, &version)
		var stored []byte
		if err := f.service.database.reader.QueryRowContext(ctx, "SELECT bundle FROM change_sources WHERE version_id=?", version.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if len(stored) != 0 {
			t.Fatal("publication retained inline source bytes")
		}
		sources[index].version, sources[index].bundle, expected = version, bundle, version.ID
	}
	var trigger string
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='change_sources_no_update'").Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	var referenceTrigger string
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='change_sources_immutable_reference'").Scan(&referenceTrigger); err != nil {
		t.Fatal(err)
	}
	restoreLegacy := func() {
		t.Helper()
		if _, err := f.service.database.db.ExecContext(ctx, "DROP TRIGGER change_sources_no_update"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(ctx, "DROP TRIGGER change_sources_immutable_reference"); err != nil {
			t.Fatal(err)
		}
		for _, source := range sources {
			if _, err := f.service.database.db.ExecContext(ctx, "UPDATE change_sources SET bundle=?,object_key='',object_sha256='',object_bytes=0,content_bytes=0 WHERE version_id=?", source.bundle, source.version.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.service.database.db.ExecContext(ctx, trigger); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(ctx, referenceTrigger); err != nil {
			t.Fatal(err)
		}
	}
	restoreLegacy()
	checkReaders := func() {
		t.Helper()
		for _, source := range sources {
			version := source.version
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+version.ID+"/source", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			if !bytes.Equal(response.Body.Bytes(), source.bundle) || response.Header().Get("Content-Type") != "application/x-git-bundle" {
				t.Fatal("backfill changed downloaded source")
			}
			stored, err := nativeChangeSourceStored(ctx, f.service.database.reader, version)
			if err != nil || !stored {
				t.Fatalf("source placement lost restorable source: stored=%v err=%v", stored, err)
			}
		}
	}
	usage := func() int64 {
		t.Helper()
		var value int64
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&value); err != nil {
			t.Fatal(err)
		}
		requireUsageCountersMatchRecount(t, f.service.database)
		return value
	}
	framedCount := func() int {
		t.Helper()
		var count int
		if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM change_sources WHERE substr(bundle,1,4)=?", []byte(changeSourceMagic)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	checkReaders()
	initialUsage := usage()
	var initialBytes int
	for _, source := range sources {
		initialBytes += len(source.bundle)
	}
	for processed := 1; processed <= len(sources); processed++ {
		updates := 0
		ctx := withRequestTiming(ctx, &requestTiming{trace: func(query string, _ []driver.NamedValue) {
			if strings.HasPrefix(query, "UPDATE change_sources") {
				updates++
			}
		}})
		more, err := f.service.database.backfillChangeSource(ctx)
		if err != nil || !more || updates != 1 || framedCount() != processed {
			t.Fatalf("one-row transaction: more=%v err=%v updates=%d framed=%d want=%d", more, err, updates, framedCount(), processed)
		}
		var retainedBytes int
		if err := f.service.database.reader.QueryRowContext(ctx, "SELECT sum(length(bundle)) FROM change_sources").Scan(&retainedBytes); err != nil {
			t.Fatal(err)
		}
		if got, want := usage(), initialUsage-int64(initialBytes-retainedBytes); got != want {
			t.Fatalf("usage=%d want=%d after compression", got, want)
		}
		checkReaders()
		var authorized int
		if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM storage_conversion_progress WHERE conversion_rowid<>0 OR conversion_before_sha256<>'' OR conversion_after_sha256<>''").Scan(&authorized); err != nil || authorized != 0 {
			t.Fatalf("encoding permissions=%d: %v", authorized, err)
		}
		if processed == 1 {
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := f.service.database.backfillChangeSource(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted backfill: %v", err)
			}
			if framedCount() != 1 {
				t.Fatal("interrupted backfill changed another source")
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, config)
			ctx = f.service.runDetailContext(t.Context())
			stopMaintenance()
		}
	}
	if usage() >= initialUsage {
		t.Fatal("backfill did not save storage")
	}
	finalUsage := usage()
	if more, err := f.service.database.backfillChangeSource(ctx); err != nil || more || usage() != finalUsage {
		t.Fatalf("completed backfill was not idempotent: more=%v err=%v", more, err)
	}
	var binaryStored []byte
	if err := f.service.database.reader.QueryRowContext(ctx, "SELECT bundle FROM change_sources WHERE version_id=?", sources[2].version.ID).Scan(&binaryStored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binaryStored, append([]byte(changeSourceMagic+"\x01\x00"), sources[2].bundle...)) {
		t.Fatal("incompressible source did not retain its readable stored form")
	}
	restoreLegacy()
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	f.service.maintainChangeSources(canceledCtx)
	if framedCount() != 0 {
		t.Fatal("canceled maintenance changed a source")
	}
	f.service.maintainChangeSources(ctx)
	if got := framedCount(); got != len(sources) {
		t.Fatalf("maintenance framed %d sources, want %d in one pass", got, len(sources))
	}
	if got := usage(); got != finalUsage {
		t.Fatalf("maintenance usage=%d want=%d", got, finalUsage)
	}
	checkReaders()
	for _, source := range sources {
		version := source.version
		if _, err := f.service.database.db.ExecContext(ctx, "UPDATE change_sources SET bundle=?,object_key='',object_sha256='',object_bytes=0,content_bytes=0 WHERE version_id=?", []byte(changeSourceMagic+"\x01\x01invalid"), version.ID); err == nil {
			t.Fatal("framed source could be overwritten")
		}
	}
}

func (s *Service) maintainChangeSources(ctx context.Context) {
	for ctx.Err() == nil {
		more, err := s.database.backfillChangeSource(ctx)
		if err != nil || !more {
			return
		}
	}
}

func (d *database) backfillChangeSource(ctx context.Context) (bool, error) {
	var versionID, changeID string
	var bundle []byte
	err := d.reader.QueryRowContext(ctx, `SELECT s.version_id,v.change_id,s.bundle FROM change_sources s
JOIN change_versions v ON v.id=s.version_id WHERE s.object_key='' AND substr(s.bundle,1,4)<>x'00444353' ORDER BY s.version_id LIMIT 1`).Scan(&versionID, &changeID, &bundle)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select legacy change source: %w", err)
	}
	version, err := readChangeVersionSnapshot(ctx, d.reader, changeID, versionID)
	if err != nil {
		return false, err
	}
	if version.Source == nil {
		return false, errors.New("legacy change source metadata is missing")
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle); err != nil {
		return false, err
	}
	stored, err := encodeChangeSource(bundle)
	if err != nil {
		return false, err
	}
	restored, err := decodeChangeSource(stored, version.Source.Bytes)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(restored, bundle) {
		return false, errors.New("change source compression changed source bytes")
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, restored); err != nil {
		return false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at,conversion_rowid,conversion_before_sha256,conversion_after_sha256)
SELECT c.organization_id,c.project_id,'sources',c.work_item_id,1,s.rowid,'',s.rowid,?,?
FROM change_sources s JOIN change_versions v ON v.id=s.version_id JOIN change_requests c ON c.id=v.change_id
WHERE s.version_id=? AND s.bundle=? AND s.object_key=''
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
conversion_rowid=excluded.conversion_rowid,conversion_before_sha256=excluded.conversion_before_sha256,conversion_after_sha256=excluded.conversion_after_sha256`, storageConversionDigest(bundle, "", "", int64(0), int64(0)), storageConversionDigest(stored, "", "", int64(0), int64(0)), versionID, bundle); err != nil {
		return false, fmt.Errorf("authorize compressed change source: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE change_sources SET bundle=? WHERE version_id=? AND bundle=?", stored, versionID, bundle); err != nil {
		return false, fmt.Errorf("store compressed change source: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE storage_conversion_progress SET conversion_rowid=0,conversion_before_sha256='',conversion_after_sha256='' WHERE family='sources' AND conversion_rowid=(SELECT rowid FROM change_sources WHERE version_id=?)`, versionID); err != nil {
		return false, err
	}
	if err := d.commit(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}
