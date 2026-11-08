package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactCatalogHostedRelayExcludesCustomerTraffic(t *testing.T) {
	_, repository, _, now := artifactCatalogFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, mode := range []string{"customer", "hosted"} {
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				service := fmt.Sprintf("service_%032x", 2)
				if mode == "hosted" {
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: service, Origin: "https://example.invalid", Mode: "hosted", HostedOptIn: true, PublisherTokenID: "first"})
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
					return w.RecordCatalogTrafficNow(t.Context(), postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project, ServiceID: service, ID: mode, UploadBytes: 10, DownloadBytes: 20, Requests: 1}, now)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
				usage, err := r.CatalogServiceTraffic(t.Context(), postgres.CatalogServiceTrafficInput{OrganizationID: org, Minute: now.Truncate(time.Minute), Minute_2: now.Add(time.Minute)})
				if err != nil {
					return err
				}
				want := int64(0)
				if mode == "hosted" {
					want = 60
				}
				if usage.RelayBytes != want {
					t.Fatalf("%s relay usage = %d, want %d; customer storage consumed hosted relay allowance", mode, usage.RelayBytes, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
