package hubserver

import (
	"bytes"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresNativeCursorsUseTenantSecret(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service := &Service{config: Config{AccessRepository: repository, SecretKeys: secretTestKeys(t, "1", "1"), now: func() time.Time { return now }}}
	var previous string
	for i, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: org, credential: apiCredential{ID: "key_example", Scope: apiScopeAdmin}}
		if _, _, _, err := service.readNativePage(t.Context(), scope, "/items", url.Values{}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("missing selected cursor secret error=%v", err)
		}
		key := bytes.Repeat([]byte{byte(i + 1)}, 32)
		envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		_, cursor, actualKey, err := service.readNativePage(t.Context(), scope, "/items", url.Values{})
		if err != nil || !bytes.Equal(actualKey, key) {
			t.Fatalf("selected cursor key: error=%v", err)
		}
		cursor.After = "item_next"
		token, err := encodeNativeCursor(cursor, actualKey)
		if err != nil {
			t.Fatal(err)
		}
		_, decoded, _, err := service.readNativePage(t.Context(), scope, "/items", url.Values{"cursor": {token}})
		if err != nil || decoded.After != cursor.After {
			t.Fatalf("cursor continuation=%q error=%v", decoded.After, err)
		}
		for _, tc := range []struct{ name, path, token string }{
			{"different path", "/other", token},
			{"different tenant", "/items", previous},
		} {
			if tc.token == "" {
				continue
			}
			if _, _, _, err := service.readNativePage(t.Context(), scope, tc.path, url.Values{"cursor": {tc.token}}); err == nil {
				t.Fatalf("accepted %s cursor", tc.name)
			}
		}
		previous = token
	}
}
