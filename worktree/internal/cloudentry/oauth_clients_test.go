package cloudentry

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
)

func TestOAuthClientMigrationAndBackfill(t *testing.T) {
	ctx := t.Context()
	admin := entryPostgresDatabase(t)
	var logs lockedBuffer
	cfg := secretEntryConfig(t, &logs)
	cfg.now = time.Now
	entryPostgresConfig(t, admin)(&cfg)
	registry, storage, err := openEntryStores(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(registry.Close(), storage.Close()); err != nil {
			t.Error(err)
		}
	})
	if _, err := registry.Register(ctx, Organization{ID: "org_example", ProviderID: "porg_example", Name: "Example", State: "ready", Endpoint: "unix:" + t.TempDir() + "/hub.sock", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	seal, err := newTokenSeal(cfg.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{auth: &authStore{store: storage, seal: seal}, config: cfg}
	db := storage.db
	registration := oauthRegistration{Name: "Assistant", AuthMethod: "client_secret_post", SecretHash: apikey.HashToken("client-secret"), RedirectURIs: []string{"https://assistant.example.test/callback", "http://localhost:12345/callback"}}
	ids := make([]string, 3)
	for i := range ids {
		sealed, err := s.sealOAuth("oauth-client", registration)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = "oauth_client_" + sealed
	}
	slices.Sort(ids)
	const broken = "oauth_client_!unreadable"
	const created = "2026-10-20T00:00:00Z"
	for _, id := range append(slices.Clone(ids), broken) {
		for _, statement := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO oauth_clients(id,name,auth_method,secret_hash,created_at) VALUES(?,?,?,?,?)", []any{id, registration.Name, registration.AuthMethod, registration.SecretHash, created}},
			{"INSERT INTO oauth_grants(id,client_id,owner_subject,owner_email,organization_id,permission,resource,created_at,access_hash,access_expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)", []any{"grant_" + id, id, "user_owner", "owner@example.test", "org_example", "write", testPublicURL + "/mcp", created, apikey.HashToken("access_" + id), "2026-11-01T00:00:00Z"}},
			{"INSERT INTO oauth_refresh_tokens(organization_id,token_hash,grant_id,expires_at) VALUES('org_example',?,?,?)", []any{apikey.HashToken("refresh_" + id), "grant_" + id, "2026-11-01T00:00:00Z"}},
		} {
			if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, id := range append(slices.Clone(ids), broken) {
		var accessHash, refreshHash string
		if err := storage.reader.QueryRowContext(ctx, `SELECT g.access_hash,t.token_hash FROM oauth_grants g JOIN oauth_refresh_tokens t ON t.grant_id=g.id WHERE g.client_id=? AND g.revoked_at IS NULL`, id).Scan(&accessHash, &refreshHash); err != nil || accessHash != apikey.HashToken("access_"+id) || refreshHash != apikey.HashToken("refresh_"+id) {
			t.Fatalf("live grant changed by migration: %v", err)
		}
	}
	if _, err := admin.ExecContext(ctx, `CREATE FUNCTION public.fail_second_client() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected insert failure'; END $$; CREATE TRIGGER fail_second_client BEFORE INSERT ON platform.oauth_client_redirect_uris FOR EACH ROW WHEN (NEW.client_id=`+sqlLiteral(ids[1])+` AND NEW.position=1) EXECUTE FUNCTION public.fail_second_client()`); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillOAuthClients(ctx); err == nil {
		t.Fatal("injected failure was ignored")
	}
	for i, id := range ids {
		want := 0
		if i == 0 {
			want = 2
		}
		var count int
		if err := storage.reader.QueryRowContext(ctx, "SELECT count(*) FROM oauth_client_redirect_uris WHERE client_id=?", id).Scan(&count); err != nil || count != want {
			t.Fatalf("client %d committed redirects=%d want=%d err=%v", i, count, want, err)
		}
	}
	if strings.Count(logs.String(), "backfill OAuth client unavailable") != 1 || !strings.Contains(logs.String(), created) || !strings.Contains(logs.String(), `"error":`) {
		t.Fatalf("unreadable client not logged once: %s", logs.String())
	}
	for _, value := range append(append(slices.Clone(ids), broken), registration.RedirectURIs...) {
		if strings.Contains(logs.String(), value) {
			t.Fatal("backfill log exposed client data")
		}
	}
	if _, err := admin.ExecContext(ctx, "DROP TRIGGER fail_second_client ON platform.oauth_client_redirect_uris"); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillOAuthClients(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		got, err := s.oauthRegistration(ctx, id)
		if err != nil || !reflect.DeepEqual(got, registration) {
			t.Fatalf("backfilled registration differs: %v", err)
		}
	}
	if _, err := admin.ExecContext(ctx, `CREATE TRIGGER refuse_backfill_rewrite BEFORE INSERT ON platform.oauth_client_redirect_uris FOR EACH ROW EXECUTE FUNCTION public.fail_second_client()`); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillOAuthClients(ctx); err != nil {
		t.Fatalf("completed backfill wrote again: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.backfillOAuthClients(cancelled); err == nil {
		t.Fatal("cancelled backfill continued")
	}
	var missing int
	if err := storage.reader.QueryRowContext(ctx, `SELECT count(*) FROM oauth_clients c WHERE NOT EXISTS(SELECT 1 FROM oauth_client_redirect_uris u WHERE u.client_id=c.id)`).Scan(&missing); err != nil || missing != 1 {
		t.Fatalf("missing redirects=%d err=%v", missing, err)
	}
	if _, err := s.oauthRegistration(ctx, broken); err == nil {
		t.Fatal("unreadable client accepted")
	}
}

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
