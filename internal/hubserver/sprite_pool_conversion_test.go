package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPromoteSpritesToken(t *testing.T) {
	for _, test := range []struct {
		name                                              string
		changed, existing, ranked, revoked, rewrap, raced bool
	}{
		{name: "confirmed token"}, {name: "rewrapped after preview", rewrap: true}, {name: "changed source", changed: true}, {name: "existing organization token", existing: true},
		{name: "first token follows project rank", ranked: true},
		{name: "authority revoked after preview", revoked: true},
		{name: "concurrent organization token", raced: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			keys := secretTestKeys(t, "1", "1")
			service := newPostgresNativeService(t, Config{SecretKeys: keys})
			repository := service.config.TenantRepository
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					f := newNativeFixture(t, service, organization, "Example source")
					storeSecret := func(ctx context.Context, project tracker.ProjectID, token string) {
						t.Helper()
						envelope, err := keys.Seal([]byte(token), accessstore.SecretAAD(organization, project, flySpritesToken))
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.StoreAccessSecret(ctx, accessstore.Secret{OrganizationID: organization, ProjectID: project, Kind: flySpritesToken, OrganizationSlug: "example-org", Envelope: envelope, UpdatedAt: service.config.now()}, accessstore.SecretAudit{OrganizationID: organization, ProjectID: project, Kind: flySpritesToken, Actor: "fixture", Event: "set", KeyVersion: envelope.Version, RecordedAt: service.config.now()})
						}); err != nil {
							t.Fatal(err)
						}
					}
					storeSecret(t.Context(), f.project.ID, spritesSecretSentinel)
					promotedToken := spritesSecretSentinel
					sourceProject := f.project.ID
					if test.ranked {
						second := newNativeFixture(t, service, organization, "Example ranked source")
						storeSecret(t.Context(), second.project.ID, "ranked-token")
						promotedToken, sourceProject = "ranked-token", second.project.ID
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							ranks := w.(projectstore.OrganizationWriter)
							revision, err := ranks.OrganizationSchedulingRevision(t.Context())
							if err != nil {
								return err
							}
							return ranks.SetOrganizationProjectRanks(t.Context(), revision, []tracker.ProjectID{sourceProject, f.project.ID})
						}); err != nil {
							t.Fatal(err)
						}
					}
					scope := nativeScope{organization: organization, credential: apiCredential{Scope: apiScopeAdmin, Hash: apikey.HashToken(testHubAdminToken)}}
					var change spriteTokenPromotion
					var source accessstore.Secret
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						credential, err := r.CredentialByHash(t.Context(), scope.credential.Hash)
						if err != nil {
							return err
						}
						scope.credential.ID = credential.ID
						change, source, err = firstProjectSpritesToken(t.Context(), r)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if change.ProjectID != sourceProject {
						t.Fatalf("selected token project=%s want=%s", change.ProjectID, sourceProject)
					}
					raw, err := json.Marshal(change)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(raw), spritesSecretSentinel) || strings.Contains(string(raw), promotedToken) {
						t.Fatal("preview exposed token")
					}
					if test.rewrap {
						service.config.SecretKeys = secretTestKeys(t, "2", "1", "2")
						envelope, err := service.config.SecretKeys.Rewrap(source.Envelope, accessstore.SecretAAD(organization, source.ProjectID, flySpritesToken))
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.RewrapAccessSecret(t.Context(), source, envelope, accessstore.SecretAudit{OrganizationID: organization, ProjectID: source.ProjectID, Kind: flySpritesToken, Actor: "fixture", Event: "rotate", KeyVersion: envelope.Version, RecordedAt: service.config.now()})
						}); err != nil {
							t.Fatal(err)
						}
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							current, secret, err := firstProjectSpritesToken(t.Context(), r)
							if err != nil || current.Fingerprint != change.Fingerprint || secret.Envelope.Version != 2 {
								t.Fatalf("preview changed after rewrap: %v", err)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					if test.changed {
						storeSecret(t.Context(), f.project.ID, "replacement")
					}
					if test.existing {
						storeSecret(t.Context(), "", "existing")
					}
					if test.revoked {
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.RevokeAccessCredential(t.Context(), scope.credential.ID, service.config.now())
						}); err != nil {
							t.Fatal(err)
						}
					}
					ctx := context.WithValue(t.Context(), nativeOperatorScopeKey{}, func(context.Context) (nativeScope, error) { return scope, nil })
					interleaved := false
					if test.raced {
						ctx = postgres.WithTransactionTiming(ctx, nil, func(_ time.Time, query string) {
							if !interleaved && strings.Contains(query, "FROM organization_secrets WHERE organization_id=$1 AND kind=$2") {
								interleaved = true
								writeCtx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
								defer cancel()
								storeSecret(writeCtx, "", "existing")
							}
						})
					}
					result, err := service.executePromoteSpritesToken(ctx, chat.Action{Kind: "promote_sprites_token", ProjectID: string(f.project.ID), Arguments: raw})
					if test.changed || test.existing || test.revoked || test.raced {
						if err == nil {
							t.Fatal("stale or overwriting promotion accepted")
						}
						if test.raced && !interleaved {
							t.Fatal("concurrent organization token was not inserted")
						}
						if test.changed || test.raced {
							message := "The source Sprites token changed; request a new preview"
							if test.raced {
								message = "An organization Sprites token is already configured"
							}
							var refusal *nativeError
							if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || refusal.status != http.StatusUnprocessableEntity || !refusal.publicMessage || refusal.Message != message {
								t.Fatalf("promotion refusal=%v want invalid_request: %s", err, message)
							}
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(result.Message, spritesSecretSentinel) || strings.Contains(result.Message, promotedToken) {
							t.Fatal("result exposed token")
						}
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						secret, err := r.AccessSecret(t.Context(), "", flySpritesToken)
						if test.changed || test.revoked {
							if !errors.Is(err, corestore.ErrNotFound) {
								t.Fatalf("refused promotion wrote organization secret: %v", err)
							}
							return nil
						}
						if err != nil {
							return err
						}
						token, err := service.config.SecretKeys.Open(secret.Envelope, accessstore.SecretAAD(organization, "", flySpritesToken))
						if err != nil {
							return err
						}
						defer clear(token)
						expected := promotedToken
						if test.existing || test.raced {
							expected = "existing"
						}
						if string(token) != expected {
							t.Fatal("promoted token changed")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						secret, err := r.AccessSecret(t.Context(), f.project.ID, flySpritesToken)
						if err != nil {
							return err
						}
						token, err := service.config.SecretKeys.Open(secret.Envelope, accessstore.SecretAAD(organization, f.project.ID, flySpritesToken))
						if err != nil {
							return err
						}
						defer clear(token)
						expected := spritesSecretSentinel
						if test.changed {
							expected = "replacement"
						}
						if string(token) != expected {
							t.Fatal("project override was changed")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
