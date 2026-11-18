package spritepromotion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"bytes"
	"encoding/base64"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
			repository := hubtest.Repository(t, hubtest.WithMaxConnections(4))
			service := New(Deps{Keys: keys, Now: time.Now, Decode: operatortool.DecodeArguments})
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Name: "Example source"})
					project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
					credential := hubtest.Credential(t, repository, organization, hubtest.CredentialOptions{Scope: "admin", AllProject: true})
					scope := hubtest.Scope(t, hubtest.ScopeFacts{Credential: credential, Admin: true})
					service.deps.Organization = organization
					service.deps.Write = func(ctx context.Context, write func(tenantstore.Writer) error) error {
						return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
							if err := scope.Authorize(ctx, w, true); err != nil {
								return err
							}
							return write(w)
						})
					}
					service.deps.Audit = func(event string, version int) accessstore.SecretAudit {
						return accessstore.SecretAudit{OrganizationID: organization, Actor: credential.ID, Kind: secretKind, Event: event, KeyVersion: version, RecordedAt: service.deps.Now()}
					}
					storeSecret := func(ctx context.Context, project tracker.ProjectID, token string) {
						t.Helper()
						envelope, err := keys.Seal([]byte(token), accessstore.SecretAAD(organization, project, secretKind))
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.StoreAccessSecret(ctx, accessstore.Secret{OrganizationID: organization, ProjectID: project, Kind: secretKind, OrganizationSlug: "example-org", Envelope: envelope, UpdatedAt: service.deps.Now()}, accessstore.SecretAudit{OrganizationID: organization, ProjectID: project, Kind: secretKind, Actor: "fixture", Event: "set", KeyVersion: envelope.Version, RecordedAt: service.deps.Now()})
						}); err != nil {
							t.Fatal(err)
						}
					}
					storeSecret(t.Context(), project.ID, spritesSecretSentinel)
					promotedToken := spritesSecretSentinel
					sourceProject := project.ID
					if test.ranked {
						second := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{Name: "Example ranked source"})
						storeSecret(t.Context(), second.ID, "ranked-token")
						promotedToken, sourceProject = "ranked-token", second.ID
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							ranks := w.(projectstore.OrganizationWriter)
							revision, err := ranks.OrganizationSchedulingRevision(t.Context())
							if err != nil {
								return err
							}
							return ranks.SetOrganizationProjectRanks(t.Context(), revision, []tracker.ProjectID{sourceProject, project.ID})
						}); err != nil {
							t.Fatal(err)
						}
					}
					var change Change
					var source accessstore.Secret
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						credential, err := r.CredentialByHash(t.Context(), credential.Hash)
						if err != nil {
							return err
						}
						scope.CredentialID = credential.ID
						change, source, err = firstProjectToken(t.Context(), r)
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
						service.deps.Keys = secretTestKeys(t, "2", "1", "2")
						envelope, err := service.deps.Keys.Rewrap(source.Envelope, accessstore.SecretAAD(organization, source.ProjectID, secretKind))
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.RewrapAccessSecret(t.Context(), source, envelope, accessstore.SecretAudit{OrganizationID: organization, ProjectID: source.ProjectID, Kind: secretKind, Actor: "fixture", Event: "rotate", KeyVersion: envelope.Version, RecordedAt: service.deps.Now()})
						}); err != nil {
							t.Fatal(err)
						}
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							current, secret, err := firstProjectToken(t.Context(), r)
							if err != nil || current.Fingerprint != change.Fingerprint || secret.Envelope.Version != 2 {
								t.Fatalf("preview changed after rewrap: %v", err)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					if test.changed {
						storeSecret(t.Context(), project.ID, "replacement")
					}
					if test.existing {
						storeSecret(t.Context(), "", "existing")
					}
					if test.revoked {
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.RevokeAccessCredential(t.Context(), scope.CredentialID, service.deps.Now())
						}); err != nil {
							t.Fatal(err)
						}
					}
					ctx := t.Context()
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
					result, err := service.Execute(ctx, raw)
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
							var refusal *hubapi.Error
							if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || refusal.Status != http.StatusUnprocessableEntity || !refusal.Public || refusal.Message != message {
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
						secret, err := r.AccessSecret(t.Context(), "", secretKind)
						if test.changed || test.revoked {
							if !errors.Is(err, corestore.ErrNotFound) {
								t.Fatalf("refused promotion wrote organization secret: %v", err)
							}
							return nil
						}
						if err != nil {
							return err
						}
						token, err := service.deps.Keys.Open(secret.Envelope, accessstore.SecretAAD(organization, "", secretKind))
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
						secret, err := r.AccessSecret(t.Context(), project.ID, secretKind)
						if err != nil {
							return err
						}
						token, err := service.deps.Keys.Open(secret.Envelope, accessstore.SecretAAD(organization, project.ID, secretKind))
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

func TestMain(m *testing.M) { hubtest.Main(m) }

const spritesSecretSentinel = "detent-test/org-id/token-id/SPRITES-VALUE-SENTINEL"

func secretTestKeys(t *testing.T, active string, versions ...string) *hubsecrets.Keyring {
	t.Helper()
	encoded := map[string]string{}
	for _, v := range versions {
		encoded[v] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte(v), 32))
	}
	raw, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return string(raw)
		}
		return active
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
