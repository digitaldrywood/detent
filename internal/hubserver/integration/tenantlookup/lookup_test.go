package tenantlookup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest/pgwire"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestSharedEntryLookups(t *testing.T) {
	admin := hubtest.Database(t)
	wire := new(pgwire.Wire)
	repository := pgwire.Repository(t, admin, hubtest.PostgresConfig(t, admin).Connection, wire)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: time.Now()})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ref, _, err := attachment.PrepareArchive("org_first", 1, strings.NewReader("example"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO archive_objects(organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, "org_first", "example", ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second", "org_first"} {
		wire.Reset()
		record, err := tenantstore.LookupOrganization(t.Context(), repository, org)
		if err != nil || record.OrganizationID != org {
			t.Fatalf("organization %s returned %s, error %v", org, record.OrganizationID, err)
		}
		exists, err := tenantstore.LookupAttachment(t.Context(), repository, org, "", ref.Key)
		if err != nil || exists != (org == "org_first") {
			t.Fatalf("organization %s attachment exists=%t, error %v", org, exists, err)
		}
		if exchanges, statements := wire.Stop(); exchanges != 2 || statements != 4 {
			t.Errorf("organization %s used %d statements and %d exchanges, want 4 and 2", org, statements, exchanges)
		}
	}
	err = repository.ReadRequest(t.Context(), "org_first", func(ctx context.Context) error {
		if _, err := tenantstore.LookupOrganization(ctx, repository, "org_second"); !errors.Is(err, corestore.ErrTenantMismatch) {
			t.Fatalf("foreign lookup inside request: %v", err)
		}
		wire.Reset()
		exists, err := tenantstore.LookupAttachment(ctx, repository, "org_first", "", ref.Key)
		if err != nil || !exists {
			t.Fatalf("request attachment exists=%t error=%v", exists, err)
		}
		if exchanges, statements := wire.Stop(); exchanges != 1 || statements != 1 {
			t.Errorf("lookup inside request used %d statements and %d exchanges, want 1 and 1", statements, exchanges)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
