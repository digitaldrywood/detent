package hubserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
)

func TestPromoteSpritesToken(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, test := range []struct {
		name                                       string
		changed, existing, ranked, revoked, rewrap bool
	}{
		{name: "confirmed token"}, {name: "rewrapped after preview", rewrap: true}, {name: "changed source", changed: true}, {name: "existing organization token", existing: true},
		{name: "first token follows project rank", ranked: true},
		{name: "authority revoked after preview", revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{SecretKeys: secretTestKeys(t, "1", "1")})
			storeSpriteWakeSecret(t, f, spritesSecretSentinel)
			promotedToken := spritesSecretSentinel
			sourceProject := f.project.ID
			if test.ranked {
				second := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
				storeSpriteWakeSecret(t, second, "ranked-token")
				promotedToken, sourceProject = "ranked-token", second.project.ID
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET scheduling_rank=CASE WHEN id=? THEN 0 ELSE 2 END WHERE organization_id=?", sourceProject, f.project.OrganizationID); err != nil {
					t.Fatal(err)
				}
			}
			scope := nativeScope{organization: f.project.OrganizationID, credential: apiCredential{Scope: apiScopeAdmin, Hash: apikey.HashToken(testHubAdminToken)}}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE token_hash=?", scope.credential.Hash).Scan(&scope.credential.ID); err != nil {
				t.Fatal(err)
			}
			change, _, _, err := firstProjectSpritesToken(t.Context(), f.service.database.db, scope)
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
				current, _, envelope, err := firstProjectSpritesToken(t.Context(), f.service.database.reader, scope)
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
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(f.service.config.now()), scope.credential.ID); err != nil {
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
				resolved, _, envelope, err := resolveSpritesSecret(t.Context(), f.service.database.db, scope)
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
			_, _, envelope, err := resolveSpritesSecret(t.Context(), f.service.database.db, projectScope)
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
