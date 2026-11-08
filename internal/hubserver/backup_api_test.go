package hubserver

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAdminBackupBoundary(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, organization := range []tracker.OrganizationID{"org_security", "org_other"} {
		t.Run(string(organization), func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
					return err
				}
				return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "org_provider"})
			}); err != nil {
				t.Fatal(err)
			}
			f := accountPreferencesPostgresFixture(t, repository)
			f.service.config.Hosted.OrganizationID = string(organization)
			f.service.database.hostedOrganization = organization
			owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
			output := filepath.Join(t.TempDir(), "snapshot.db")
			requireNativeCode(t, f.request(t, owner, http.MethodPost, "/api/v2/admin/backup", map[string]string{"output": output}), http.StatusNotFound, "not_found")
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("hosted backup created output: %v", err)
			}
		})
	}
}
