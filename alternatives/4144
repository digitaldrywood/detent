package workpage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }
func newRepository(t *testing.T) (tenantstore.Repository, *sql.DB) {
	admin := hubtest.Database(t)
	return hubtest.RepositoryOn(t, admin), admin
}
func readPage(s *Service, ctx context.Context, caller Caller, path string, params url.Values) (int, Cursor, []byte, error) {
	return s.ReadLimit(ctx, caller, path, params, 200, 50)
}
func secretKeys(t *testing.T) *hubsecrets.Keyring {
	t.Helper()
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
func TestPostgresNativeCursorsUseTenantSecret(t *testing.T) {
	repository := hubtest.Repository(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service := New(Deps{Repository: repository, Keys: secretKeys(t), Now: func() time.Time { return now }})
	var previous string
	for i, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		scope := Caller{Organization: org, CredentialID: "key_example"}
		key := bytes.Repeat([]byte{byte(i + 1)}, 32)
		envelope, err := service.deps.Keys.Seal(key, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		_, cursor, actualKey, err := readPage(service, t.Context(), scope, "/items", url.Values{})
		if err != nil || !bytes.Equal(actualKey, key) {
			t.Fatalf("selected cursor key: error=%v", err)
		}
		cursor.After = "item_next"
		token, err := Encode(cursor, actualKey)
		if err != nil {
			t.Fatal(err)
		}
		_, decoded, _, err := readPage(service, t.Context(), scope, "/items", url.Values{"cursor": {token}})
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
			if _, _, _, err := readPage(service, t.Context(), scope, tc.path, url.Values{"cursor": {tc.token}}); err == nil {
				t.Fatalf("accepted %s cursor", tc.name)
			}
		}
		previous = token
	}
}

func TestPostgresNativeCursorKeyLazyCreation(t *testing.T) {
	repository, admin := newRepository(t)
	service := New(Deps{Repository: repository, Keys: secretKeys(t), Now: time.Now})
	var previous []byte
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: time.Now().UTC()})
			}); err != nil {
				t.Fatal(err)
			}
			service.deps.Keys = nil
			if _, err := service.ReadKey(t.Context(), org); err == nil {
				t.Fatal("created cursor key without its keyring")
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("failed creation persisted a cursor key: %v", err)
			}
			service.deps.Keys = secretKeys(t)
			type result struct {
				key []byte
				err error
			}
			results := make(chan result, 8)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for range cap(results) {
				workers.Go(func() {
					<-start
					key, err := service.ReadKey(t.Context(), org)
					results <- result{key, err}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			var first []byte
			for got := range results {
				if got.err != nil {
					t.Fatal(got.err)
				}
				if len(got.key) != 32 {
					t.Fatalf("cursor key length=%d", len(got.key))
				}
				if first == nil {
					first = got.key
				} else if !bytes.Equal(first, got.key) {
					t.Fatal("concurrent readers received different cursor keys")
				}
			}
			if bytes.Equal(previous, first) {
				t.Fatal("organizations received the same cursor key")
			}
			previous = first
			var count int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM organization_secret_audit WHERE organization_id=$1 AND kind=$2 AND event='create'`, org, accessstore.NativeCursorKeyKind).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("cursor key creation audit count=%d, want 1", count)
			}
			service.deps.Keys = nil
			if _, err := service.ReadKey(t.Context(), org); err == nil {
				t.Fatal("cursor key opened without its keyring")
			}
			service.deps.Keys = secretKeys(t)
			again, err := service.ReadKey(t.Context(), org)
			if err != nil || !bytes.Equal(first, again) {
				t.Fatalf("existing key changed after failed open: %v", err)
			}
		})
	}
	if _, err := service.ReadKey(t.Context(), "org_missing"); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("missing organization cursor key: %v", err)
	}
}
