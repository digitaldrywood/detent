package hubserver

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type secretPostgresFixture struct {
	service    *Service
	repository tenantstore.Repository
	admin      *sql.DB
	originals  []accessstore.Secret
}

func newSecretPostgresFixture(t *testing.T) secretPostgresFixture {
	t.Helper()
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service := &Service{config: Config{AccessRepository: repository, TenantRepository: repository, SecretKeys: secretTestKeys(t, "1", "1"), Logger: discardLogger(), now: func() time.Time { return now }}}
	fixture := secretPostgresFixture{service: service, repository: repository, admin: admin}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"", "project"} {
				envelope, err := service.config.SecretKeys.Seal([]byte(spritesSecretSentinel), accessstore.SecretAAD(org, project, flySpritesToken))
				if err != nil {
					return err
				}
				secret := accessstore.Secret{OrganizationID: org, ProjectID: project, Kind: flySpritesToken, Envelope: envelope, UpdatedAt: now}
				if err := w.StoreAccessSecret(t.Context(), secret, accessstore.SecretAudit{OrganizationID: org, ProjectID: project, Kind: secret.Kind, Actor: "fixture", Event: "set", KeyVersion: 1, RecordedAt: now}); err != nil {
					return err
				}
				fixture.originals = append(fixture.originals, secret)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

type secretMutationAfterRead struct {
	tenantstore.Repository
	mutate func() error
}

func (r *secretMutationAfterRead) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	if err := r.Repository.ReadTenant(ctx, org, workload, read); err != nil {
		return err
	}
	if r.mutate == nil {
		return nil
	}
	mutate := r.mutate
	r.mutate = nil
	return mutate()
}
