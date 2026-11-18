package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDetailStoreKeepsContentPerOrganization(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	store := repository.DetailStore(func() time.Time { return now })
	organizations := []tracker.OrganizationID{"org_detail_first", "org_detail_second"}
	data := []byte(`{"attempt_id":"attempt_example","outcome":"succeeded"}`)
	refs := make(map[tracker.OrganizationID]attachment.ArchiveReference)
	for _, org := range organizations {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			ref, err := store.StoreDetail(t.Context(), string(org), data)
			if err != nil {
				t.Fatal(err)
			}
			if previous, ok := refs[org]; ok && previous != ref {
				t.Fatalf("replay changed reference: %+v != %+v", ref, previous)
			}
			refs[org] = ref
		}
	}
	if refs[organizations[0]].Key == refs[organizations[1]].Key {
		t.Fatal("organizations share one object key")
	}
	for index, org := range organizations {
		ref := refs[org]
		got, err := store.ReadDetail(t.Context(), string(org), ref)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s read=%q err=%v", org, got, err)
		}
		canonical, err := store.ReadCanonicalDetail(t.Context(), string(org), "prj_example", ref.Key)
		if err != nil || !bytes.Equal(canonical, data) {
			t.Fatalf("%s canonical read=%q err=%v", org, canonical, err)
		}
		foreign := organizations[1-index]
		if _, err := store.ReadDetail(t.Context(), string(foreign), ref); !errors.Is(err, attachment.ErrInvalid) {
			t.Fatalf("foreign organization read another tenant's reference: %v", err)
		}
		forged := refs[foreign]
		forged.Key, forged.SHA256 = ref.Key, ref.SHA256
		if _, err := store.ReadDetail(t.Context(), string(foreign), forged); err == nil {
			t.Fatal("foreign organization read another tenant's object key")
		}
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hub_detail_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3 AND compressed_bytes=$4 AND content_bytes=$5`, org, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s stored rows=%d err=%v", org, count, err)
		}
	}
	missing, _, err := attachment.PrepareArchive(string(organizations[0]), 1, bytes.NewReader([]byte("never stored")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadDetail(t.Context(), string(organizations[0]), missing); !errors.Is(err, artifact.ErrMissing) {
		t.Fatalf("missing object read=%v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE hub_detail_objects SET content=$3 WHERE organization_id=$1 AND object_key=$2`, organizations[0], refs[organizations[0]].Key, bytes.Repeat([]byte{1}, int(refs[organizations[0]].CompressedSize))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadDetail(t.Context(), string(organizations[0]), refs[organizations[0]]); !errors.Is(err, artifact.ErrIntegrity) {
		t.Fatalf("corrupted object read=%v", err)
	}
	if _, err := store.StoreDetail(t.Context(), string(organizations[0]), data); !errors.Is(err, artifact.ErrIntegrity) {
		t.Fatalf("replay over corrupted object=%v", err)
	}
	if got, err := store.ReadDetail(t.Context(), string(organizations[1]), refs[organizations[1]]); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("corruption in one organization changed the other: %q %v", got, err)
	}
}

func TestDetailStoreProgressesInsideSaturatedTransactions(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	store := repository.DetailStore(func() time.Time { return now })
	const org = tracker.OrganizationID("org_detail_saturated")
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.StoreDetail(t.Context(), string(org), []byte(`{"existing":true}`))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	background := make(chan error, 1)
	go func() {
		background <- repository.ReadTenant(t.Context(), org, corestore.Background, func(tenantstore.Reader) error {
			close(entered)
			<-release
			return nil
		})
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	<-entered
	guard, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for _, workload := range []string{"read", "write"} {
		run := func(fn func(tenantstore.Reader) error) error {
			if workload == "read" {
				return repository.ReadTenant(guard, org, corestore.Serving, fn)
			}
			return repository.WriteTenant(guard, org, corestore.Serving, func(w tenantstore.Writer) error { return fn(w) })
		}
		if err := run(func(tenantstore.Reader) error {
			ref, err := store.StoreDetail(guard, string(org), []byte(`{"nested":"`+workload+`"}`))
			if err != nil {
				return err
			}
			if got, err := store.ReadDetail(guard, string(org), ref); err != nil || !bytes.Contains(got, []byte(workload)) {
				return errors.Join(err, errors.New("nested read returned other content"))
			}
			_, err = store.ReadCanonicalDetail(guard, string(org), "prj_example", stored.Key)
			return err
		}); err != nil {
			t.Fatalf("%s transaction with every serving and background slot held: %v", workload, err)
		}
	}
	close(release)
	if err := <-background; err != nil {
		t.Fatal(err)
	}
}

func TestDetailStoreReclaimsOnlyUnreferencedExpiredObjects(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := start
	store := repository.DetailStore(func() time.Time { return now })
	organizations := []tracker.OrganizationID{"org_reclaim_first", "org_reclaim_second"}
	type objects struct{ referenced, abandoned, touched, recent attachment.ArchiveReference }
	stored := map[tracker.OrganizationID]*objects{}
	put := func(org tracker.OrganizationID, body string) attachment.ArchiveReference {
		t.Helper()
		ref, err := store.StoreDetail(t.Context(), string(org), []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	present := func(org tracker.OrganizationID, ref attachment.ArchiveReference) bool {
		t.Helper()
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hub_detail_objects WHERE organization_id=$1 AND object_key=$2`, org, ref.Key).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 1
	}
	for _, org := range organizations {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: start})
		}); err != nil {
			t.Fatal(err)
		}
		set := &objects{referenced: put(org, `{"kind":"referenced"}`), abandoned: put(org, `{"kind":"abandoned"}`), touched: put(org, `{"kind":"touched"}`)}
		ref := set.referenced
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO archive_objects(organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,created_at) VALUES($1,'archive_reclaim',$2,$3,$4,$5,$6,$7,$8,$9,$10)`, org, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, start); err != nil {
			t.Fatal(err)
		}
		stored[org] = set
	}
	now = start.Add(8 * 24 * time.Hour)
	for _, org := range organizations {
		stored[org].recent = put(org, `{"kind":"recent"}`)
		if touched := put(org, `{"kind":"touched"}`); touched != stored[org].touched {
			t.Fatalf("re-store changed reference: %+v", touched)
		}
	}
	cutoff := now.Add(-7 * 24 * time.Hour)
	reclaimed, err := store.Reclaim(t.Context(), organizations[0], cutoff)
	if err != nil || reclaimed != 1 {
		t.Fatalf("first reclaim=%d err=%v", reclaimed, err)
	}
	first, second := stored[organizations[0]], stored[organizations[1]]
	if present(organizations[0], first.abandoned) || !present(organizations[0], first.referenced) || !present(organizations[0], first.touched) || !present(organizations[0], first.recent) {
		t.Fatal("reclaim removed a referenced, re-stored or recent object, or kept the abandoned one")
	}
	if !present(organizations[1], second.abandoned) {
		t.Fatal("reclaim of one organization removed another organization's object")
	}
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM archive_objects WHERE organization_id=$1 AND id='archive_reclaim'`, organizations[0]); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := store.Reclaim(t.Context(), organizations[0], cutoff); err != nil || reclaimed != 1 || present(organizations[0], first.referenced) {
		t.Fatalf("expired reference kept its bytes: reclaimed=%d err=%v", reclaimed, err)
	}
	budget := postgres.DetailReclaimPage * postgres.DetailReclaimPages
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO hub_detail_objects(organization_id,object_key,sha256,compressed_bytes,content_bytes,content,created_at)
SELECT $1,'orgs/'||$1||'/blobs/sha256/aa/'||lpad(to_hex(n),64,'0'),lpad(to_hex(n),64,'0'),1,1,'\x00'::bytea,$2 FROM generate_series(1,$3::int) AS n`, organizations[1], start, budget+1); err != nil {
		t.Fatal(err)
	}
	firstPass, err := store.Reclaim(t.Context(), organizations[1], cutoff)
	if err != nil || firstPass == 0 || firstPass > budget {
		t.Fatalf("first bounded reclaim=%d err=%v, want 1..%d", firstPass, err, budget)
	}
	if secondPass, err := store.Reclaim(t.Context(), organizations[1], cutoff); err != nil || firstPass+secondPass != budget+2 {
		t.Fatalf("continued reclaim=%d+%d err=%v, want %d abandoned objects", firstPass, secondPass, err, budget+2)
	}
	if !present(organizations[1], second.referenced) || !present(organizations[1], second.touched) || !present(organizations[1], second.recent) {
		t.Fatal("bounded reclaim removed a live object")
	}
}
