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
