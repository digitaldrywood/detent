package hubserver

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
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
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT bundle FROM change_sources WHERE version_id=?", version.ID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(stored, []byte(changeSourceMagic)) {
			t.Fatal("publication did not frame the source")
		}
		sources[index].version, sources[index].bundle, expected = version, bundle, version.ID
	}
	var trigger string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='change_sources_no_update'").Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	restoreLegacy := func() {
		t.Helper()
		if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER change_sources_no_update"); err != nil {
			t.Fatal(err)
		}
		for _, source := range sources {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_sources SET bundle=? WHERE version_id=?", source.bundle, source.version.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), trigger); err != nil {
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
			stored, err := nativeChangeSourceStored(t.Context(), f.service.database.reader, version)
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
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_sources WHERE substr(bundle,1,4)=?", []byte(changeSourceMagic)).Scan(&count); err != nil {
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
		ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
			if strings.HasPrefix(query, "UPDATE change_sources") {
				updates++
			}
		}})
		more, err := f.service.database.backfillChangeSource(ctx)
		if err != nil || !more || updates != 1 || framedCount() != processed {
			t.Fatalf("one-row transaction: more=%v err=%v updates=%d framed=%d want=%d", more, err, updates, framedCount(), processed)
		}
		var retainedBytes int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT sum(length(bundle)) FROM change_sources").Scan(&retainedBytes); err != nil {
			t.Fatal(err)
		}
		if got, want := usage(), initialUsage-int64(initialBytes-retainedBytes); got != want {
			t.Fatalf("usage=%d want=%d after compression", got, want)
		}
		checkReaders()
		var authorized int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM storage_conversion_progress WHERE conversion_rowid<>0 OR conversion_before_sha256<>'' OR conversion_after_sha256<>''").Scan(&authorized); err != nil || authorized != 0 {
			t.Fatalf("encoding permissions=%d: %v", authorized, err)
		}
		if processed == 1 {
			ctx, cancel := context.WithCancel(t.Context())
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
			stopMaintenance()
		}
	}
	if usage() >= initialUsage {
		t.Fatal("backfill did not save storage")
	}
	finalUsage := usage()
	if more, err := f.service.database.backfillChangeSource(t.Context()); err != nil || more || usage() != finalUsage {
		t.Fatalf("completed backfill was not idempotent: more=%v err=%v", more, err)
	}
	var binaryStored []byte
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT bundle FROM change_sources WHERE version_id=?", sources[2].version.ID).Scan(&binaryStored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binaryStored, append([]byte(changeSourceMagic+"\x01\x00"), sources[2].bundle...)) {
		t.Fatal("incompressible source did not retain its readable stored form")
	}
	restoreLegacy()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.service.maintainChangeSources(ctx)
	if framedCount() != 0 {
		t.Fatal("canceled maintenance changed a source")
	}
	f.service.maintainChangeSources(t.Context())
	if got := framedCount(); got != len(sources) {
		t.Fatalf("maintenance framed %d sources, want %d in one pass", got, len(sources))
	}
	if got := usage(); got != finalUsage {
		t.Fatalf("maintenance usage=%d want=%d", got, finalUsage)
	}
	checkReaders()
	for _, source := range sources {
		version := source.version
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_sources SET bundle=? WHERE version_id=?", []byte(changeSourceMagic+"\x01\x01invalid"), version.ID); err == nil {
			t.Fatal("framed source could be overwritten")
		}
	}
}
