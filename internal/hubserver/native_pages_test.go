package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeOrganizationCursorPaging(t *testing.T) {
	service := newClockedPostgresNativeService(t, Config{})
	var previous string
	for _, test := range []struct {
		name   string
		legacy bool
	}{{"First example", false}, {"Second example", false}, {"First legacy", true}, {"Second legacy", true}} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": test.name})
			requireNativeStatus(t, response, http.StatusCreated)
			var organization nativeOrganization
			decodeHubResponse(t, response, &organization)
			f := newNativeFixture(t, service, organization.ID, "Example project")
			if test.legacy {
				if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), `DELETE FROM organization_secrets WHERE organization_id=$1 AND kind=$2`, organization.ID, accessstore.NativeCursorKeyKind); err != nil {
					t.Fatal(err)
				}
			}
			item := f.create(t, "First item")
			second := f.create(t, "Second item")
			comments := f.base + "/work-items/" + string(item.WorkItemID) + "/comments"
			for _, body := range []string{"First comment", "Second comment"} {
				response := performHubAPIRequest(t, service, http.MethodPost, comments, f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: body}, Body: body})
				requireNativeStatus(t, response, http.StatusOK)
			}
			for _, issue := range []tracker.NativeIssue{item, second} {
				if err := service.refreshBoardRecord(t.Context(), organization.ID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: issue.WorkItemID}, service.config.now()); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{f.base + "/work-items", comments, f.base + "/work-items/" + string(item.WorkItemID) + "/history"} {
				response := performHubAPIRequest(t, service, http.MethodGet, path+"?limit=1", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.Page[json.RawMessage]
				decodeHubResponse(t, response, &page)
				if len(page.Items) != 1 || page.NextCursor == "" {
					t.Fatalf("first page at %s: items=%d cursor present=%v", path, len(page.Items), page.NextCursor != "")
				}
				response = performHubAPIRequest(t, service, http.MethodGet, path+"?limit=1&cursor="+url.QueryEscape(page.NextCursor), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var next tracker.Page[json.RawMessage]
				decodeHubResponse(t, response, &next)
				if len(next.Items) != 1 {
					t.Fatalf("second page at %s: items=%d", path, len(next.Items))
				}
				if bytes.Equal(page.Items[0], next.Items[0]) {
					t.Fatalf("cursor repeated the first record at %s", path)
				}
				if path == f.base+"/work-items" {
					if previous != "" {
						response = performHubAPIRequest(t, service, http.MethodGet, path+"?cursor="+url.QueryEscape(previous), f.token, nil)
						requireNativeStatus(t, response, http.StatusUnprocessableEntity)
					}
					previous = page.NextCursor
				}
			}
		})
	}
}

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

func TestPostgresNativeCursorKeyLazyCreation(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	service := &Service{config: Config{AccessRepository: repository, SecretKeys: secretTestKeys(t, "1", "1"), now: time.Now}}
	var previous []byte
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: time.Now().UTC()})
			}); err != nil {
				t.Fatal(err)
			}
			service.config.SecretKeys = nil
			if _, err := service.readNativeCursorKey(t.Context(), org); err == nil {
				t.Fatal("created cursor key without its keyring")
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.AccessSecret(t.Context(), "", accessstore.NativeCursorKeyKind)
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("failed creation persisted a cursor key: %v", err)
			}
			service.config.SecretKeys = secretTestKeys(t, "1", "1")
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
					key, err := service.readNativeCursorKey(t.Context(), org)
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
			service.config.SecretKeys = nil
			if _, err := service.readNativeCursorKey(t.Context(), org); err == nil {
				t.Fatal("cursor key opened without its keyring")
			}
			service.config.SecretKeys = secretTestKeys(t, "1", "1")
			again, err := service.readNativeCursorKey(t.Context(), org)
			if err != nil || !bytes.Equal(first, again) {
				t.Fatalf("existing key changed after failed open: %v", err)
			}
		})
	}
	if _, err := service.readNativeCursorKey(t.Context(), "org_missing"); !errors.Is(err, corestore.ErrNotFound) {
		t.Fatalf("missing organization cursor key: %v", err)
	}
}
