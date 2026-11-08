package hubserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSpritePromotionPreservesTenantOverridesAfterReopen(t *testing.T) {
	var poolConfig *pgxpool.Config
	repository, _ := projectRecordPostgresWithAdmin(t, 3, func(cfg *pgxpool.Config) { poolConfig = cfg.Copy() })
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	keys := secretTestKeys(t, "1", "1")
	services := map[tracker.OrganizationID]*Service{}
	scopes := map[tracker.OrganizationID]nativeScope{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		actor := "admin_" + string(organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: string(organization), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: actor, Hash: actor, Name: "Administrator", Kind: "service", Scope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for rank, project := range []tracker.ProjectID{"first", "second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native", SchedulingRank: int64(rank)}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				envelope, err := keys.Seal([]byte(string(organization)+":"+string(project)), accessstore.SecretAAD(organization, project, flySpritesToken))
				if err != nil {
					return err
				}
				if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, ProjectID: project, Kind: flySpritesToken, OrganizationSlug: "example", Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, ProjectID: project, Actor: actor, Kind: flySpritesToken, Event: "set", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		services[organization] = &Service{config: Config{TenantRepository: repository, AccessRepository: repository, SecretKeys: keys, Hosted: &HostedConfig{OrganizationID: string(organization)}, now: func() time.Time { return now }}}
		scopes[organization] = nativeScope{organization: organization, credential: apiCredential{ID: actor, Hash: actor, Scope: apiScopeAdmin}}
	}
	first, _, _, err := services["org_first"].firstProjectSpritesTokenRecord(t.Context(), scopes["org_first"])
	if err != nil {
		t.Fatal(err)
	}
	if err := services["org_second"].promoteSpritesTokenRecord(t.Context(), scopes["org_second"], first); err == nil {
		t.Fatal("foreign preview promoted another tenant token")
	}
	foreign := scopes["org_first"]
	foreign.organization = "org_second"
	if _, _, _, err := services["org_first"].firstProjectSpritesTokenRecord(t.Context(), foreign); err == nil {
		t.Fatal("foreign tenant preview was authorized")
	}
	for organization, service := range services {
		change, _, _, err := service.firstProjectSpritesTokenRecord(t.Context(), scopes[organization])
		if err != nil {
			t.Fatal(err)
		}
		if change.ProjectID != "first" {
			t.Fatalf("selected source=%s", change.ProjectID)
		}
		if err := service.promoteSpritesTokenRecord(t.Context(), scopes[organization], change); err != nil {
			t.Fatal(err)
		}
		if err := service.promoteSpritesTokenRecord(t.Context(), scopes[organization], change); err == nil {
			t.Fatal("promotion overwrote its existing organization token")
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reopened, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := reopened.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, project := range []tracker.ProjectID{"", "first", "second"} {
				secret, err := r.AccessSecret(t.Context(), project, flySpritesToken)
				if err != nil {
					return err
				}
				plain, err := keys.Open(secret.Envelope, accessstore.SecretAAD(organization, project, flySpritesToken))
				if err != nil {
					return err
				}
				want := string(organization) + ":" + string(project)
				if project == "" {
					want = string(organization) + ":first"
				}
				matches := string(plain) == want
				clear(plain)
				if !matches {
					t.Fatal("promotion lost a tenant token or project override")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPromoteSpritesToken(t *testing.T) {
	for _, test := range []struct {
		name                                       string
		changed, existing, ranked, revoked, rewrap bool
	}{
		{name: "confirmed token"}, {name: "rewrapped after preview", rewrap: true}, {name: "changed source", changed: true}, {name: "existing organization token", existing: true},
		{name: "first token follows project rank", ranked: true},
		{name: "authority revoked after preview", revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSpriteNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1")})
			storeSpriteWakeSecret(t, f, spritesSecretSentinel)
			promotedToken := spritesSecretSentinel
			sourceProject := f.project.ID
			if test.ranked {
				second := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
				storeSpriteWakeSecret(t, second, "ranked-token")
				promotedToken, sourceProject = "ranked-token", second.project.ID
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					projects, err := w.Projects(t.Context(), "", 100)
					if err != nil {
						return err
					}
					for _, project := range projects {
						project.SchedulingRank = 2
						if project.ID == sourceProject {
							project.SchedulingRank = 0
						}
						if _, err := w.UpdateProject(t.Context(), project); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if err != nil || status != 200 {
				t.Fatalf("fixture authority=%d %v", status, err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, credential: credential}
			f.service.config.Hosted = &HostedConfig{OrganizationID: string(scope.organization)}
			change, _, _, err := f.service.firstProjectSpritesTokenRecord(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if change.ProjectID != sourceProject {
				t.Fatalf("selected token project=%s want=%s", change.ProjectID, sourceProject)
			}
			raw, err := json.Marshal(change)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), spritesSecretSentinel) {
				t.Fatal("preview exposed token")
			}
			if test.rewrap {
				f.service.signalSecretRewrap()
				<-f.service.secretRewrapDone
				f.service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
				f.service.rewrapSecrets(t.Context())
				current, _, envelope, err := f.service.firstProjectSpritesTokenRecord(t.Context(), scope)
				if err != nil || current.Fingerprint != change.Fingerprint || envelope.Version != 2 {
					t.Fatalf("preview changed after rewrap: %v", err)
				}
			}
			if test.changed {
				storeSpriteWakeSecret(t, f, "replacement")
			}
			if test.existing {
				storeOrganizationSpritesSecret(t, f, "existing")
			}
			if test.revoked {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					return w.RevokeAccessCredential(t.Context(), scope.credential.ID, f.service.config.now())
				}); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.WithValue(t.Context(), nativeOperatorScopeKey{}, func(context.Context) (nativeScope, error) { return scope, nil })
			result, err := f.service.executePromoteSpritesToken(ctx, chat.Action{Kind: "promote_sprites_token", ProjectID: string(f.project.ID), Arguments: raw})
			if test.changed || test.existing || test.revoked {
				if err == nil {
					t.Fatal("stale or overwriting promotion accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(result.Message, spritesSecretSentinel) {
					t.Fatal("result exposed token")
				}
				resolved, _, envelope, err := f.service.resolveSpritesSecret(t.Context(), nil, scope)
				if err != nil {
					t.Fatal(err)
				}
				token, err := f.service.config.SecretKeys.Open(envelope, secretAAD(string(resolved.organization), "", flySpritesToken))
				if err != nil {
					t.Fatal(err)
				}
				if string(token) != promotedToken {
					t.Fatal("promoted token changed")
				}
				clear(token)
			}
			projectScope := scope
			projectScope.project = f.project.ID
			_, _, envelope, err := f.service.resolveSpritesSecret(t.Context(), nil, projectScope)
			if err != nil {
				t.Fatal(err)
			}
			token, err := f.service.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(f.project.ID), flySpritesToken))
			if err != nil {
				t.Fatal(err)
			}
			expected := spritesSecretSentinel
			if test.changed {
				expected = "replacement"
			}
			if string(token) != expected {
				t.Fatal("project override was changed")
			}
			clear(token)
		})
	}
}
