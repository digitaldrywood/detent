package postgres_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOnboardingProgressUsesOneTenantScopedRead(t *testing.T) {
	repository, pool := repositoryFor(t, template.Database(t), smallPool())
	counts := new(pageWireCounts)
	measured := pageMeasuredRepository(t, pool, counts)
	for tenant, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedRepository(t, repository, org)
		want := onboarding.Progress{Revision: 1, Repository: "existing", Doctor: tenant == 0, Provider: tenant == 1, Artifacts: "local", UpdatedAt: historyAt().Format("2006-01-02T15:04:05.999999999Z07:00")}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SaveProjectOnboardingProgress(t.Context(), "prj_first", want, 0)
		}); err != nil {
			t.Fatal(err)
		}
		seeded := 0
		for _, size := range []int{2, 50} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for seeded < size {
					id := tracker.ProjectID(fmt.Sprintf("prj_extra_%d_%d", tenant, seeded))
					if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native"}); err != nil {
						return err
					}
					if err := w.SaveProjectOnboardingProgress(t.Context(), id, want, 0); err != nil {
						return err
					}
					seeded++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name    string
				project tracker.ProjectID
				want    onboarding.Progress
				missing bool
			}{
				{name: "saved", project: "prj_first", want: want},
				{name: "unsaved", project: "prj_second"},
				{name: "missing", project: "prj_missing", missing: true},
				{name: "other_tenant", project: tracker.ProjectID(fmt.Sprintf("prj_extra_%d_0", 1-tenant)), missing: true},
			} {
				t.Run(fmt.Sprintf("%s/%d/%s", org, size, test.name), func(t *testing.T) {
					counts.reset()
					if err := measured.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						got, err := r.ProjectOnboardingProgress(t.Context(), test.project)
						if test.missing {
							if !errors.Is(err, corestore.ErrNotFound) {
								t.Fatalf("missing project read=%v, want not found", err)
							}
							return nil
						}
						if err != nil {
							return err
						}
						if got != test.want {
							t.Fatalf("progress=%+v, want %+v", got, test.want)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					statements, exchanges := counts.read()
					checkoutSQL, checkoutRTT := counts.checkout()
					t.Logf("statements=%d exchanges=%d checkout=%d/%d", statements, exchanges, checkoutSQL, checkoutRTT)
					if statements != 4 || exchanges != 3 || checkoutSQL != 1 || checkoutRTT != 1 {
						t.Fatalf("one indexed progress read: statements=%d exchanges=%d checkout=%d/%d, want 4/3 + 1/1", statements, exchanges, checkoutSQL, checkoutRTT)
					}
				})
			}
		}
	}
}
