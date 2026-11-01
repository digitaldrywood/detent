package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func conversionFamilies() []storageconversion.Family {
	return []storageconversion.Family{storageconversion.Runs, storageconversion.Versions, storageconversion.Evidence}
}

func conversionScope(org tracker.OrganizationID, family storageconversion.Family, second bool) storageconversion.Scope {
	aggregate := "wi_first_1"
	if second {
		aggregate = "wi_first_2"
	}
	if family != storageconversion.Runs {
		aggregate = "change_" + aggregate
	}
	return storageconversion.Scope{Organization: org, Project: "prj_first", Family: family, AggregateID: aggregate}
}

func conversionParent(t *testing.T, repository *postgres.Repository, objects *historyObjects, scope storageconversion.Scope) {
	t.Helper()
	if scope.Family == storageconversion.Runs {
		return
	}
	ref, err := objects.StoreDetail(t.Context(), string(scope.Organization), []byte(`{"body":"Example change"}`))
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteRecords(t.Context(), scope.Organization, corestore.Serving, func(w eventstore.Writer) error {
		_, err := w.CreateChange(t.Context(), eventstore.Change{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: "wi_first_1", ID: scope.AggregateID, Title: "Example change", PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func conversionInsert(ctx context.Context, w eventstore.Writer, scope storageconversion.Scope, ref attachment.ArchiveReference, id string, sequence, version int64) error {
	switch scope.Family {
	case storageconversion.Runs:
		_, err := w.AppendEvent(ctx, eventstore.Event{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(scope.AggregateID), ID: id, Sequence: sequence, Type: "run.started", SchemaVersion: 1, ActorKind: "human", ActorPrincipalID: "example", RecordedAt: historyAt(), PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize, PayloadFormatVersion: version})
		return err
	case storageconversion.Versions:
		_, err := w.CreateVersion(ctx, eventstore.Version{OrganizationID: scope.Organization, ID: id, ChangeID: scope.AggregateID, Number: sequence, HeadSHA: "head", PolicyRevisionID: "policy", ActorKind: "human", ActorPrincipalID: "example", CreatedAt: historyAt(), PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize, PayloadFormatVersion: version})
		return err
	case storageconversion.Evidence:
		_, err := w.AppendEvidence(ctx, eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: scope.AggregateID, Sequence: sequence, Kind: "discussion", SourceKey: new(id), ActorKind: "human", ActorPrincipalID: "example", CreatedAt: historyAt(), PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize, PayloadFormatVersion: version})
		return err
	default:
		return storageconversion.ErrInvalid
	}
}

func seedConversion(t *testing.T, repository *postgres.Repository, objects *historyObjects, scope storageconversion.Scope, id string, sequence, version int64) attachment.ArchiveReference {
	t.Helper()
	ref, err := objects.StoreDetail(t.Context(), string(scope.Organization), []byte(fmt.Sprintf(`{"id":%q,"body":"Example historical payload"}`, id)))
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteRecords(t.Context(), scope.Organization, corestore.Serving, func(w eventstore.Writer) error {
		return conversionInsert(t.Context(), w, scope, ref, id, sequence, version)
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func convertPayload(_ context.Context, _ storageconversion.Candidate, raw []byte) ([]byte, error) {
	return append(bytes.Clone(raw), '\n'), nil
}

func conversionRecords(t *testing.T, r *postgres.Repository, scope storageconversion.Scope) any {
	t.Helper()
	var records any
	err := r.ReadRecords(t.Context(), scope.Organization, corestore.Serving, func(reader eventstore.Reader) error {
		var err error
		switch scope.Family {
		case storageconversion.Runs:
			records, err = reader.Events(t.Context(), scope.Project, tracker.NativeWorkItemID(scope.AggregateID), 0, 200)
		case storageconversion.Versions:
			records, err = reader.Versions(t.Context(), scope.AggregateID, 0, 200)
		case storageconversion.Evidence:
			records, err = reader.Evidence(t.Context(), scope.AggregateID, 0, 200)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func assertConversionProgress(t *testing.T, r *postgres.Repository, scope storageconversion.Scope, rows, size int64) {
	t.Helper()
	got, err := r.ConversionProgress(t.Context(), scope)
	if err != nil || got != (storageconversion.Progress{Rows: rows, Bytes: size}) {
		t.Fatalf("conversion progress = %+v, %v; want %d rows and %d bytes", got, err, rows, size)
	}
}

func assertConvertedRecords(t *testing.T, objects *historyObjects, scope storageconversion.Scope, before, after any) {
	t.Helper()
	oldRows, newRows := reflect.ValueOf(before), reflect.ValueOf(after)
	if oldRows.Len() != newRows.Len() {
		t.Fatalf("record count changed from %d to %d", oldRows.Len(), newRows.Len())
	}
	for i := range oldRows.Len() {
		oldRow, newRow := oldRows.Index(i), newRows.Index(i)
		oldVersion, newVersion := oldRow.FieldByName("PayloadFormatVersion").Int(), newRow.FieldByName("PayloadFormatVersion").Int()
		if newVersion != storageconversion.CurrentVersion {
			t.Fatalf("record %d has format version %d", i, newVersion)
		}
		if oldVersion == newVersion {
			if !reflect.DeepEqual(oldRow.Interface(), newRow.Interface()) {
				t.Fatal("current-format record was changed")
			}
			continue
		}
		refFor := func(row reflect.Value) attachment.ArchiveReference {
			return eventstore.Reference(row.FieldByName("PayloadObjectKey").String(), row.FieldByName("PayloadSHA256").String(), row.FieldByName("PayloadBytes").Int(), row.FieldByName("PayloadContentBytes").Int())
		}
		original, err := objects.ReadDetail(t.Context(), string(scope.Organization), refFor(oldRow))
		if err != nil {
			t.Fatalf("original object became unreadable: %v", err)
		}
		converted, err := objects.ReadDetail(t.Context(), string(scope.Organization), refFor(newRow))
		if err != nil || !bytes.Equal(converted, append(bytes.Clone(original), '\n')) {
			t.Fatalf("converted content mismatch: %q, %v", converted, err)
		}
		for field := range oldRow.NumField() {
			name := oldRow.Type().Field(field).Name
			switch name {
			case "PayloadFormatVersion", "PayloadObjectKey", "PayloadSHA256", "PayloadBytes", "PayloadContentBytes":
			default:
				if !reflect.DeepEqual(oldRow.Field(field).Interface(), newRow.Field(field).Interface()) {
					t.Fatalf("conversion changed immutable field %s", name)
				}
			}
		}
	}
}

func TestStorageConversionTenantRecovery(t *testing.T) {
	for _, family := range conversionFamilies() {
		t.Run(string(family), func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			objects := newHistoryObjects()
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedRepository(t, r, org)
				for _, second := range []bool{false, true} {
					scope := conversionScope(org, family, second)
					conversionParent(t, r, objects, scope)
					for sequence := int64(1); sequence <= 3; sequence++ {
						version := int64(1)
						if sequence == 3 {
							version = 0
						}
						seedConversion(t, r, objects, scope, fmt.Sprintf("%s_%d", scope.AggregateID, sequence), sequence, version)
					}
				}
			}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				for _, second := range []bool{false, true} {
					scope := conversionScope(org, family, second)
					before := conversionRecords(t, r, scope)
					assertConversionProgress(t, r, scope, 0, 0)
					page, err := r.ConversionPage(t.Context(), scope, storageconversion.Cursor{}, 100)
					if err != nil || len(page.Items) != 2 {
						t.Fatalf("pending page = %+v, %v", page, err)
					}
					var cursor storageconversion.Cursor
					for turn := range 2 {
						owner := storageconversion.New(r, objects, convertPayload)
						result, err := owner.Batch(t.Context(), scope, cursor, 1)
						if err != nil || result.Converted != 1 || result.Visited != 1 {
							t.Fatalf("turn %d = %+v, %v", turn, result, err)
						}
						cursor = result.Next
					}
					assertConversionProgress(t, r, scope, 2, page.Items[0].Source.UncompressedSize+page.Items[1].Source.UncompressedSize)
					assertConvertedRecords(t, objects, scope, before, conversionRecords(t, r, scope))
					replay, err := storageconversion.New(r, objects, convertPayload).Batch(t.Context(), scope, storageconversion.Cursor{}, 100)
					if err != nil || replay.Converted != 0 || replay.Next != (storageconversion.Cursor{}) {
						t.Fatalf("replay = %+v, %v", replay, err)
					}
				}
			}
		})
	}
}

type conversionObjects struct {
	base  *historyObjects
	read  func(context.Context, string, attachment.ArchiveReference) ([]byte, error)
	store func(context.Context, string, []byte) (attachment.ArchiveReference, error)
}

func (s *conversionObjects) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.read != nil {
		return s.read(ctx, org, ref)
	}
	return s.base.ReadDetail(ctx, org, ref)
}

func (s *conversionObjects) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	if s.store != nil {
		return s.store(ctx, org, raw)
	}
	return s.base.StoreDetail(ctx, org, raw)
}

type conversionRepository struct {
	*postgres.Repository
	commit func(context.Context, storageconversion.Candidate, attachment.ArchiveReference) (bool, error)
}

func (r conversionRepository) CommitConversion(ctx context.Context, candidate storageconversion.Candidate, target attachment.ArchiveReference) (bool, error) {
	return r.commit(ctx, candidate, target)
}

func TestStorageConversionCrashPoints(t *testing.T) {
	for _, family := range conversionFamilies() {
		for _, point := range []string{"before upload", "uncertain upload", "after upload", "before commit", "uncertain commit", "corrupt source", "corrupt target", "transform failure"} {
			t.Run(string(family)+"/"+point, func(t *testing.T) {
				r, _ := repositoryFor(t, template.Database(t), smallPool())
				seedRepository(t, r, "org_first")
				scope := conversionScope("org_first", family, false)
				objects := newHistoryObjects()
				conversionParent(t, r, objects, scope)
				ref := seedConversion(t, r, objects, scope, "legacy", 1, 1)
				injected := errors.New("injected conversion failure")
				storage := &conversionObjects{base: objects}
				uploads := 0
				storage.store = func(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
					uploads++
					if point == "before upload" {
						return attachment.ArchiveReference{}, injected
					}
					stored, err := objects.StoreDetail(ctx, org, raw)
					if point == "uncertain upload" {
						return attachment.ArchiveReference{}, injected
					}
					return stored, err
				}
				storage.read = func(ctx context.Context, org string, target attachment.ArchiveReference) ([]byte, error) {
					if point == "after upload" && target.Key != ref.Key {
						return nil, injected
					}
					if point == "corrupt source" && target.Key == ref.Key {
						return nil, artifact.ErrIntegrity
					}
					if point == "corrupt target" && target.Key != ref.Key && uploads > 0 {
						return []byte("corrupt"), nil
					}
					return objects.ReadDetail(ctx, org, target)
				}
				repository := conversionRepository{Repository: r, commit: func(ctx context.Context, candidate storageconversion.Candidate, target attachment.ArchiveReference) (bool, error) {
					if point == "before commit" {
						return false, injected
					}
					committed, err := r.CommitConversion(ctx, candidate, target)
					if point == "uncertain commit" && err == nil {
						return false, injected
					}
					return committed, err
				}}
				transform := convertPayload
				if point == "transform failure" {
					transform = func(context.Context, storageconversion.Candidate, []byte) ([]byte, error) { return nil, injected }
				}
				result, err := storageconversion.New(repository, storage, transform).Batch(t.Context(), scope, storageconversion.Cursor{}, 100)
				wantRows := int64(0)
				if point == "uncertain upload" || point == "uncertain commit" {
					wantRows = 1
				}
				if point == "uncertain upload" {
					if err != nil || result.Converted != 1 {
						t.Fatalf("uncertain upload did not reconcile: %+v, %v", result, err)
					}
				} else if err == nil {
					t.Fatal("injected failure was not returned")
				}
				assertConversionProgress(t, r, scope, wantRows, wantRows*ref.UncompressedSize)
				retryUploads := 0
				retryStorage := &conversionObjects{base: objects, store: func(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
					retryUploads++
					return objects.StoreDetail(ctx, org, raw)
				}}
				retry, err := storageconversion.New(r, retryStorage, convertPayload).Batch(t.Context(), scope, storageconversion.Cursor{}, 100)
				if err != nil || retry.Converted != 1-int(wantRows) {
					t.Fatalf("restart = %+v, %v", retry, err)
				}
				assertConversionProgress(t, r, scope, 1, ref.UncompressedSize)
				wantUploads := 0
				if point == "before upload" || point == "corrupt source" || point == "transform failure" {
					wantUploads = 1
				}
				if retryUploads != wantUploads {
					t.Fatalf("restart uploaded %d objects; want %d", retryUploads, wantUploads)
				}
				objects.mu.Lock()
				objectCount := len(objects.objects)
				objects.mu.Unlock()
				wantObjects := 2
				if family != storageconversion.Runs {
					wantObjects++
				}
				if objectCount != wantObjects {
					t.Fatalf("restart produced %d objects; want %d stable keys", objectCount, wantObjects)
				}
			})
		}
	}
}

func TestStorageConversionConcurrentRuns(t *testing.T) {
	for _, family := range conversionFamilies() {
		t.Run(string(family), func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedRepository(t, r, "org_first")
			scope := conversionScope("org_first", family, false)
			objects := newHistoryObjects()
			conversionParent(t, r, objects, scope)
			ref := seedConversion(t, r, objects, scope, "legacy", 1, 1)
			arrived, release := make(chan struct{}, 2), make(chan struct{})
			var unblock sync.Once
			var workers sync.WaitGroup
			t.Cleanup(func() { unblock.Do(func() { close(release) }); workers.Wait() })
			type outcome struct {
				result storageconversion.Result
				err    error
			}
			finished := make(chan outcome, 2)
			for range 2 {
				workers.Go(func() {
					owner := storageconversion.New(r, objects, func(ctx context.Context, candidate storageconversion.Candidate, raw []byte) ([]byte, error) {
						arrived <- struct{}{}
						select {
						case <-release:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						if err := r.Read(ctx, scope.Organization, corestore.Background, func(reader corestore.Reader) error {
							_, err := reader.Project(ctx, scope.Project)
							return err
						}); err != nil {
							return nil, err
						}
						return convertPayload(ctx, candidate, raw)
					})
					result, err := owner.Batch(t.Context(), scope, storageconversion.Cursor{}, 100)
					finished <- outcome{result: result, err: err}
				})
			}
			for range 2 {
				select {
				case <-arrived:
				case outcome := <-finished:
					t.Fatalf("conversion ended before barrier: %+v, %v", outcome.result, outcome.err)
				}
			}
			unblock.Do(func() { close(release) })
			converted := 0
			for range 2 {
				outcome := <-finished
				if outcome.err != nil {
					t.Fatal(outcome.err)
				}
				converted += outcome.result.Converted
			}
			if converted != 1 {
				t.Fatalf("concurrent runs converted %d rows, want one", converted)
			}
			assertConversionProgress(t, r, scope, 1, ref.UncompressedSize)
		})
	}
}

func TestStorageConversionLateCommit(t *testing.T) {
	for _, family := range conversionFamilies() {
		t.Run(string(family), func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedRepository(t, r, "org_first")
			scope := conversionScope("org_first", family, false)
			objects := newHistoryObjects()
			conversionParent(t, r, objects, scope)
			first := seedConversion(t, r, objects, scope, "middle", 10, 1)
			late, err := objects.StoreDetail(t.Context(), "org_first", []byte(`{"id":"early","body":"Late commit"}`))
			if err != nil {
				t.Fatal(err)
			}
			ready, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var unblock sync.Once
			var worker sync.WaitGroup
			var signal sync.Once
			t.Cleanup(func() { unblock.Do(func() { close(release) }); worker.Wait() })
			worker.Go(func() {
				defer signal.Do(func() { close(ready) })
				finished <- r.WriteRecords(t.Context(), scope.Organization, corestore.Serving, func(w eventstore.Writer) error {
					if err := conversionInsert(t.Context(), w, scope, late, "early", 1, 1); err != nil {
						signal.Do(func() { close(ready) })
						return err
					}
					signal.Do(func() { close(ready) })
					select {
					case <-release:
						return nil
					case <-t.Context().Done():
						return t.Context().Err()
					}
				})
			})
			<-ready
			page, err := r.ConversionPage(t.Context(), scope, storageconversion.Cursor{}, 1)
			if err != nil || len(page.Items) != 1 || page.Items[0].Source != first {
				t.Fatalf("visible page = %+v, %v", page, err)
			}
			unblock.Do(func() { close(release) })
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			beyond := seedConversion(t, r, objects, scope, "newest", 20, 1)
			end, err := r.ConversionPage(t.Context(), scope, page.Next, 1)
			if err != nil || len(end.Items) != 0 || end.Next != (storageconversion.Cursor{}) {
				t.Fatalf("bounded sweep did not end = %+v, %v", end, err)
			}
			owner := storageconversion.New(r, objects, convertPayload)
			result, err := owner.Batch(t.Context(), scope, end.Next, 100)
			if err != nil || result.Converted != 3 {
				t.Fatalf("next sweep missed committed records = %+v, %v", result, err)
			}
			assertConversionProgress(t, r, scope, 3, first.UncompressedSize+late.UncompressedSize+beyond.UncompressedSize)
		})
	}
}

func TestStorageConversionCompareAndSwap(t *testing.T) {
	for _, family := range conversionFamilies() {
		t.Run(string(family), func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedRepository(t, r, "org_first")
			scope := conversionScope("org_first", family, false)
			objects := newHistoryObjects()
			conversionParent(t, r, objects, scope)
			ref := seedConversion(t, r, objects, scope, "legacy", 1, 1)
			page, err := r.ConversionPage(t.Context(), scope, storageconversion.Cursor{}, 1)
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("page = %+v, %v", page, err)
			}
			target, err := objects.StoreDetail(t.Context(), "org_first", []byte(`{"converted":true}`))
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name   string
				change func(*storageconversion.Candidate)
			}{
				{"digest", func(c *storageconversion.Candidate) { c.Source = target }},
				{"format version", func(c *storageconversion.Candidate) { c.FormatVersion = 2 }},
				{"compressed size", func(c *storageconversion.Candidate) { c.Source.CompressedSize++ }},
				{"content size", func(c *storageconversion.Candidate) { c.Source.UncompressedSize++ }},
				{"record ID", func(c *storageconversion.Candidate) { c.Position.ID = "missing" }},
				{"sequence", func(c *storageconversion.Candidate) { c.Position.Sequence++ }},
				{"project", func(c *storageconversion.Candidate) { c.Scope.Project = "prj_second" }},
				{"aggregate", func(c *storageconversion.Candidate) { c.Scope.AggregateID = "missing" }},
				{"tenant", func(c *storageconversion.Candidate) { c.Scope.Organization = "org_second" }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					candidate := page.Items[0]
					tc.change(&candidate)
					committed, err := r.CommitConversion(t.Context(), candidate, target)
					if committed || err != nil && !errors.Is(err, storageconversion.ErrInvalid) {
						t.Fatalf("stale candidate committed = %v, %v", committed, err)
					}
					assertConversionProgress(t, r, scope, 0, 0)
				})
			}
			committed, err := r.CommitConversion(t.Context(), page.Items[0], target)
			if err != nil || !committed {
				t.Fatalf("valid candidate committed = %v, %v", committed, err)
			}
			committed, err = r.CommitConversion(t.Context(), page.Items[0], target)
			if err != nil || committed {
				t.Fatalf("duplicate candidate committed = %v, %v", committed, err)
			}
			assertConversionProgress(t, r, scope, 1, ref.UncompressedSize)
		})
	}
}

func TestStorageConversionRollbackAndAuthority(t *testing.T) {
	for _, family := range conversionFamilies() {
		t.Run(string(family), func(t *testing.T) {
			admin := template.Database(t)
			r, pool := repositoryFor(t, admin, smallPool())
			objects := newHistoryObjects()
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedRepository(t, r, org)
				for _, second := range []bool{false, true} {
					scope := conversionScope(org, family, second)
					conversionParent(t, r, objects, scope)
					seedConversion(t, r, objects, scope, scope.AggregateID+"_legacy", 1, 1)
				}
			}
			scope := conversionScope("org_first", family, false)
			page, err := r.ConversionPage(t.Context(), scope, storageconversion.Cursor{}, 1)
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("page = %+v, %v", page, err)
			}
			candidate := page.Items[0]
			target, err := objects.StoreDetail(t.Context(), "org_first", []byte(`{"converted":true}`))
			if err != nil {
				t.Fatal(err)
			}
			runtime := runtimeFor(t, pool, pool)
			for _, boundOrg := range []string{"", "org_second", "org_unknown"} {
				tx := tenantTx(t, runtime, pool, boundOrg)
				changed, err := pgquery.New(tx).ConversionCommit(t.Context(), pgquery.ConversionCommitParams{OrganizationID: "org_first", ProjectID: string(scope.Project), Family: string(family), AggregateID: scope.AggregateID, RecordID: candidate.Position.ID, RecordSequence: candidate.Position.Sequence, ExpectedVersion: 1, SourceKey: candidate.Source.Key, SourceDigest: candidate.Source.SHA256, SourceBytes: candidate.Source.CompressedSize, SourceContentBytes: candidate.Source.UncompressedSize, TargetKey: target.Key, TargetDigest: target.SHA256, TargetBytes: target.CompressedSize, TargetContentBytes: target.UncompressedSize})
				if err != nil || changed {
					t.Fatalf("scope %q bypassed tenant authority: %v, %v", boundOrg, changed, err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			table := map[storageconversion.Family]string{storageconversion.Runs: "collaboration_events", storageconversion.Versions: "change_versions", storageconversion.Evidence: "change_evidence"}[family]
			for _, statement := range []string{
				"UPDATE " + table + " SET payload_format_version=2 WHERE payload_format_version=1",
				"UPDATE " + table + " SET actor_principal_id='changed' WHERE payload_format_version=1",
				"DELETE FROM " + table + " WHERE payload_format_version=1",
				"UPDATE " + table + " SET payload_object_key=$1,payload_sha256=$2,payload_bytes=$3,payload_content_bytes=$4,payload_format_version=2 WHERE payload_format_version=1",
			} {
				tx := tenantTx(t, runtime, pool, "org_first")
				var err error
				if bytes.Contains([]byte(statement), []byte("$1")) {
					_, err = tx.ExecContext(t.Context(), statement, target.Key, target.SHA256, target.CompressedSize, target.UncompressedSize)
				} else {
					_, err = tx.ExecContext(t.Context(), statement)
				}
				requireSQLState(t, err, "23514")
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			before := conversionRecords(t, r, scope)
			if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_conversion_progress() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='23514',MESSAGE='injected progress failure'; END; $$;
CREATE TRIGGER reject_conversion_progress BEFORE INSERT OR UPDATE ON storage_conversion_progress FOR EACH ROW EXECUTE FUNCTION reject_conversion_progress()`); err != nil {
				t.Fatal(err)
			}
			owner := storageconversion.New(r, objects, convertPayload)
			result, err := owner.Batch(t.Context(), scope, storageconversion.Cursor{}, 100)
			if err == nil || result.Converted != 0 {
				t.Fatalf("rejected progress write committed: %+v, %v", result, err)
			}
			if !reflect.DeepEqual(before, conversionRecords(t, r, scope)) {
				t.Fatal("rejected progress write changed the object reference")
			}
			assertConversionProgress(t, r, scope, 0, 0)
			if _, err := admin.ExecContext(t.Context(), "ALTER TABLE storage_conversion_progress DISABLE TRIGGER reject_conversion_progress"); err != nil {
				t.Fatal(err)
			}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				for _, second := range []bool{false, true} {
					result, err := owner.Batch(t.Context(), conversionScope(org, family, second), storageconversion.Cursor{}, 100)
					if err != nil || result.Converted != 1 {
						t.Fatalf("recovery = %+v, %v", result, err)
					}
				}
			}
			for _, bound := range []struct {
				org  string
				want int
			}{{"", 0}, {"org_first", 2}, {"org_second", 2}, {"org_unknown", 0}} {
				tx := tenantTx(t, runtime, pool, bound.org)
				var count int
				if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM storage_conversion_progress").Scan(&count); err != nil || count != bound.want {
					t.Fatalf("scope %q progress rows = %d, %v; want %d", bound.org, count, err, bound.want)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			tx := tenantTx(t, runtime, pool, "org_first")
			_, err = tx.ExecContext(t.Context(), "UPDATE storage_conversion_progress SET converted_rows=1000")
			requireSQLState(t, err, "42501")
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStorageConversionBudgets(t *testing.T) {
	for _, family := range conversionFamilies() {
		for _, size := range []int{400_000, 1_100_000} {
			t.Run(fmt.Sprintf("%s/%d", family, size), func(t *testing.T) {
				r, _ := repositoryFor(t, template.Database(t), smallPool())
				seedRepository(t, r, "org_first")
				scope := conversionScope("org_first", family, false)
				objects := newHistoryObjects()
				conversionParent(t, r, objects, scope)
				ref, err := objects.StoreDetail(t.Context(), "org_first", bytes.Repeat([]byte("x"), size))
				if err != nil {
					t.Fatal(err)
				}
				for sequence := int64(1); sequence <= 3; sequence++ {
					if err := r.WriteRecords(t.Context(), "org_first", corestore.Serving, func(w eventstore.Writer) error {
						return conversionInsert(t.Context(), w, scope, ref, fmt.Sprintf("record_%d", sequence), sequence, 1)
					}); err != nil {
						t.Fatal(err)
					}
				}
				owner := storageconversion.New(r, objects, convertPayload)
				var cursor storageconversion.Cursor
				for turn := range 3 {
					result, err := owner.Batch(t.Context(), scope, cursor, 100)
					if err != nil || result.Converted != 1 || result.Visited != 1 {
						t.Fatalf("budget turn %d = %+v, %v", turn, result, err)
					}
					cursor = result.Next
				}
				assertConversionProgress(t, r, scope, 3, 3*ref.UncompressedSize)
			})
		}
	}
}

func TestStorageConversionValidationAndFailedRowProgress(t *testing.T) {
	for _, family := range conversionFamilies() {
		t.Run(string(family), func(t *testing.T) {
			r, _ := repositoryFor(t, template.Database(t), smallPool())
			seedRepository(t, r, "org_first")
			scope := conversionScope("org_first", family, false)
			objects := newHistoryObjects()
			conversionParent(t, r, objects, scope)
			ref := seedConversion(t, r, objects, scope, "first", 1, 1)
			seedConversion(t, r, objects, scope, "second", 2, 1)
			page, err := r.ConversionPage(t.Context(), scope, storageconversion.Cursor{}, 1)
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("page = %+v, %v", page, err)
			}
			for _, tc := range []struct {
				name   string
				change func(*storageconversion.Scope, *storageconversion.Cursor, *int)
			}{
				{"zero limit", func(_ *storageconversion.Scope, _ *storageconversion.Cursor, limit *int) { *limit = 0 }},
				{"oversize limit", func(_ *storageconversion.Scope, _ *storageconversion.Cursor, limit *int) { *limit = 101 }},
				{"missing organization", func(s *storageconversion.Scope, _ *storageconversion.Cursor, _ *int) { s.Organization = "" }},
				{"unsupported family", func(s *storageconversion.Scope, _ *storageconversion.Cursor, _ *int) { s.Family = "attempts" }},
				{"organization reuse", func(_ *storageconversion.Scope, c *storageconversion.Cursor, _ *int) {
					c.Scope.Organization = "org_second"
				}},
				{"project reuse", func(_ *storageconversion.Scope, c *storageconversion.Cursor, _ *int) { c.Scope.Project = "prj_second" }},
				{"aggregate reuse", func(_ *storageconversion.Scope, c *storageconversion.Cursor, _ *int) { c.Scope.AggregateID = "another" }},
				{"family reuse", func(_ *storageconversion.Scope, c *storageconversion.Cursor, _ *int) { c.Scope.Family = "unsupported" }},
				{"negative sequence", func(_ *storageconversion.Scope, c *storageconversion.Cursor, _ *int) { c.After.Sequence = -1 }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					requested, cursor, limit := scope, page.Next, 1
					tc.change(&requested, &cursor, &limit)
					if _, err := r.ConversionPage(t.Context(), requested, cursor, limit); !errors.Is(err, storageconversion.ErrInvalid) {
						t.Fatalf("invalid page error = %v", err)
					}
				})
			}
			for _, version := range []int64{-1, 3} {
				err := r.WriteRecords(t.Context(), scope.Organization, corestore.Serving, func(w eventstore.Writer) error {
					return conversionInsert(t.Context(), w, scope, ref, "invalid", 20, version)
				})
				if !errors.Is(err, eventstore.ErrInvalidRecord) {
					t.Fatalf("invalid format version %d error = %v", version, err)
				}
			}
			injected := errors.New("failed first record")
			owner := storageconversion.New(r, objects, func(ctx context.Context, candidate storageconversion.Candidate, raw []byte) ([]byte, error) {
				if candidate.Position.Sequence == 1 {
					return nil, injected
				}
				return convertPayload(ctx, candidate, raw)
			})
			failed, err := owner.Batch(t.Context(), scope, storageconversion.Cursor{}, 1)
			if !errors.Is(err, injected) || failed.Converted != 0 || failed.Visited != 1 {
				t.Fatalf("failed record = %+v, %v", failed, err)
			}
			continued, err := owner.Batch(t.Context(), scope, failed.Next, 1)
			if err != nil || continued.Converted != 1 {
				t.Fatalf("failure starved the next record = %+v, %v", continued, err)
			}
			recovered, err := storageconversion.New(r, objects, convertPayload).Batch(t.Context(), scope, storageconversion.Cursor{}, 100)
			if err != nil || recovered.Converted != 1 {
				t.Fatalf("failed record did not recover = %+v, %v", recovered, err)
			}
		})
	}
}
