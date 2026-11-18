package postgres_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestArtifactCatalogTrafficRejectsUnboundedReports(t *testing.T) {
	t.Parallel()
	_, repository, _, now := artifactCatalogFixture(t)
	org := tracker.OrganizationID("org_first")
	service := fmt.Sprintf("service_%032x", 2)
	projects := []tracker.ProjectID{"prj_first", "prj_second"}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, project := range projects {
			if err := w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: service, Origin: "https://example.invalid", Mode: "hosted", HostedOptIn: true, PublisherTokenID: "first"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	record := func(project tracker.ProjectID, id string, upload, download, requests int64) error {
		return repository.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
			return w.RecordCatalogTrafficNow(t.Context(), postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project, ServiceID: service, ID: id, UploadBytes: upload, DownloadBytes: download, Requests: requests}, now)
		})
	}
	for _, test := range []struct {
		name                       string
		upload, download, requests int64
	}{
		{"oversized upload", artifact.MaxArtifactBytes + 1, 0, 1},
		{"oversized download", 0, artifact.MaxArtifactBytes + 1, 1},
		{"overflowing upload", math.MaxInt64, 0, 1},
		{"overflowing download", 0, math.MaxInt64, 1},
		{"overflowing requests", 0, 0, math.MaxInt64},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := record(projects[0], strings.ReplaceAll(test.name, " ", "-"), test.upload, test.download, test.requests); !errors.Is(err, artifact.ErrInvalid) {
				t.Fatalf("report error = %v, want %v; an unbounded report can overflow organization traffic accounting", err, artifact.ErrInvalid)
			}
		})
	}
	for _, project := range projects {
		if err := record(project, "bounded-"+string(project), artifact.MaxArtifactBytes, artifact.MaxArtifactBytes, 1); err != nil {
			t.Fatalf("%s bounded report: %v", project, err)
		}
	}
	if err := repository.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
		usage, err := r.CatalogServiceTraffic(t.Context(), postgres.CatalogServiceTrafficInput{OrganizationID: org, Minute: now.Truncate(time.Minute), Minute_2: now.Add(time.Minute)})
		if err != nil {
			return err
		}
		if want := int64(4 * artifact.MaxArtifactBytes); usage.RelayBytes != want || usage.Requests != 2 {
			t.Fatalf("organization traffic = %d bytes, %d requests, want %d bytes, 2 requests", usage.RelayBytes, usage.Requests, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactCatalogHostedRelayExcludesCustomerTraffic(t *testing.T) {
	t.Parallel()
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
