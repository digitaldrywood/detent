package orgsettings_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestSecretStatusLiveDatabase(t *testing.T) {
	t.Parallel()
	const secretKind = "slack_webhook"
	oldKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	activeKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + oldKey + `","2":"` + activeKey + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "2"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	h := hubfixture.OpenHosted(t, hubfixture.Config{
		SecretKeys: keys,
		Hosted:     &hostedplans.HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: hubfixture.NewHostedProvider()},
	})
	organization := h.Primary
	envelope, err := keys.Seal([]byte("known-live-token"), accessstore.SecretAAD(organization, "", secretKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		now := h.Now()
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: secretKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: secretKind, Actor: "fixture", Event: "set", KeyVersion: 2, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	admin := h.Admin
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
		if err := h.Repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			secret, err = r.AccessSecret(t.Context(), "", secretKind)
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
	if _, err := tx.ExecContext(t.Context(), "UPDATE organization_secrets SET master_key_version=1 WHERE organization_id=$1 AND kind=$2", organization, secretKind); err != nil {
		t.Fatal(err)
	}
	response := h.Hub.Request(t, http.MethodGet, hubfixture.OrganizationBase(organization)+"/secrets/status?after_kind="+accessstore.NativeCursorKeyKind, h.AdminToken(), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	type secretVersion struct {
		Kind             string `json:"kind"`
		MasterKeyVersion int    `json:"master_key_version"`
	}
	var status struct {
		OrganizationID string            `json:"organization_id"`
		Counts         []secretVersion   `json:"counts"`
		Problems       []json.RawMessage `json:"problems"`
	}
	hubfixture.Decode(t, response, &status)
	committedSecret := slices.ContainsFunc(status.Counts, func(group secretVersion) bool {
		return group.Kind == secretKind && group.MasterKeyVersion == 2
	})
	if status.OrganizationID != string(organization) || len(status.Problems) != 0 || !committedSecret {
		t.Fatalf("live status=%+v", status)
	}
	if after := readSecret(); !reflect.DeepEqual(after, before) {
		t.Fatal("status modified stored secret")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if after := readVersion(); after != version {
		t.Fatalf("schema version changed from %d to %d", version, after)
	}
}
