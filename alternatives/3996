package hubserver

import (
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSecretStatusLiveDatabase(t *testing.T) {
	t.Parallel()
	keys := secretTestKeys(t, "2", "1", "2")
	service := openTestService(t, Config{SecretKeys: keys, Hosted: &hostedplans.HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}})
	organization := tracker.OrganizationID(service.config.Hosted.OrganizationID)
	envelope, err := keys.Seal([]byte("known-live-token"), accessstore.SecretAAD(organization, "", slackWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := service.config.Clock()
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: slackWebhookSecret, Actor: "fixture", Event: "set", KeyVersion: 2, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	admin := usagePostgresAdmin(t, service)
	readVersion := func() int64 {
		t.Helper()
		var version int64
		if err := admin.QueryRowContext(t.Context(), "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	readSecret := func() accessstore.Secret {
		t.Helper()
		var secret accessstore.Secret
		if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			secret, err = r.AccessSecret(t.Context(), "", slackWebhookSecret)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return secret
	}
	version, before := readVersion(), readSecret()
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	if _, err := tx.ExecContext(t.Context(), "UPDATE organization_secrets SET master_key_version=1 WHERE organization_id=$1 AND kind=$2", organization, slackWebhookSecret); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/"+string(organization)+"/secrets/status?after_kind="+accessstore.NativeCursorKeyKind, testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var status HostedSecretStatus
	decodeHubResponse(t, response, &status)
	if status.OrganizationID != organization || len(status.Problems) != 0 || len(status.Counts) != 1 || status.Counts[0].Kind != slackWebhookSecret || status.Counts[0].MasterKeyVersion != 2 || status.Counts[0].Count != 1 {
		t.Fatalf("live status=%+v", status)
	}
	if after := readSecret(); !reflect.DeepEqual(after, before) {
		t.Fatal("status modified stored secret")
	}
	if after := readVersion(); after != version {
		t.Fatalf("schema version changed from %d to %d", version, after)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}
