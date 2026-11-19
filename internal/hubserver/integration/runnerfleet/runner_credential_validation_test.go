package runnerfleet_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCredentialExpiryBoundaries(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	clock := hubtest.NewClock(start)
	service := hubfixture.Open(t, hubfixture.Config{Clock: clock.Now})
	type renewal struct {
		organization tracker.OrganizationID
		runner       string
		expires      time.Time
	}
	var renewed []renewal
	for _, test := range []struct {
		name      string
		offset    time.Duration
		statement string
		want      int
		wantRenew int
	}{
		{"before expiry", -time.Microsecond, "", http.StatusOK, http.StatusOK},
		{"at expiry", 0, "", http.StatusUnauthorized, http.StatusOK},

		{"vacation", 30 * 24 * time.Hour, "", http.StatusUnauthorized, http.StatusOK},
		{"before creation", -25 * time.Hour, "", http.StatusUnauthorized, http.StatusUnauthorized},
		{"invalid creation", 0, "UPDATE api_tokens SET created_at = 'invalid' WHERE organization_id=$1 AND id=$2", http.StatusUnauthorized, http.StatusUnauthorized},
		{"invalid expiry", 0, "UPDATE api_tokens SET expires_at = 'invalid' WHERE organization_id=$1 AND id=$2", http.StatusUnauthorized, http.StatusUnauthorized},
		{"empty interval", 0, "UPDATE api_tokens SET expires_at = created_at WHERE organization_id=$1 AND id=$2", http.StatusUnauthorized, http.StatusUnauthorized},
		{"missing expiry", 0, "UPDATE api_tokens SET expires_at = NULL WHERE organization_id=$1 AND id=$2", http.StatusUnauthorized, http.StatusUnauthorized},
		{"revoked and expired", 0, "UPDATE api_tokens SET revoked_at = created_at WHERE organization_id=$1 AND id=$2", http.StatusUnauthorized, http.StatusUnauthorized},
		{"rotated and expired", 0, "UPDATE api_tokens SET token_hash = md5(id)||md5(organization_id||id) WHERE organization_id=$1 AND id=$2", http.StatusUnauthorized, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock.Set(start)
			organization := service.CreateOrganization(t, test.name)
			f := hubfixture.NewNative(t, service, organization, "Expiry project")
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read)
			r.Enroll(t)
			now := r.Identity.ExpiresAt.Add(test.offset)
			clock.Set(now)
			if test.statement != "" {
				_, err := service.Admin.ExecContext(t.Context(), test.statement, organization, r.Binding.RunnerID)
				if test.name == "invalid creation" || test.name == "invalid expiry" {
					var failure interface{ SQLState() string }
					if !errors.As(err, &failure) || failure.SQLState() != "22007" {
						t.Fatalf("invalid timestamp write error = %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			hubfixture.RequireStatus(t, service.Request(t, http.MethodGet, r.IdentityPath(), r.Redemption.Credential, nil), test.want)
			if test.statement == "" && test.offset == 0 {
				for _, path := range []string{
					r.IdentityPath() + "/rotate",
					r.OrgBase + "/runners/" + runnerauth.NewBinding().RunnerID + "/renew",
					"/api/v2/organizations/other/runners/" + r.Binding.RunnerID + "/renew",
				} {
					hubfixture.RequireStatus(t, service.Request(t, http.MethodPost, path, r.Redemption.Credential, struct{}{}), http.StatusUnauthorized)
				}
			}
			hubfixture.RequireStatus(t, service.Request(t, http.MethodPost, r.IdentityPath()+"/renew", r.Redemption.Credential, struct{}{}), test.wantRenew)
			if test.wantRenew == http.StatusOK {
				hubfixture.RequireStatus(t, service.Request(t, http.MethodGet, r.IdentityPath(), r.Redemption.Credential, nil), http.StatusOK)
				renewed = append(renewed, renewal{organization: organization, runner: r.Binding.RunnerID, expires: now.Add(runnerauth.CredentialTTL)})
			}
		})
	}
	if err := service.Service.Close(); err != nil {
		t.Fatal(err)
	}
	service = service.Reopen(t)
	for _, runner := range renewed {
		if err := service.Repository.ReadTenant(t.Context(), runner.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			identity, err := reader.RunnerIdentity(t.Context(), runner.runner)
			if err != nil {
				return err
			}
			if identity.OrganizationID != runner.organization || identity.TokenID != runner.runner || !identity.ExpiresAt.Equal(runner.expires) {
				return errors.New("renewed runner interval changed after reopen")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerCredentialsAndRejectedProviderDataAreRedacted(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.Enroll(t)
	const provider = "example-provider-key-not-for-hub"
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{"display_name": "safe", "capacity": 1, "version": "test", "provider_key": provider})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	if err := f.Hub.Service.Close(); err != nil {
		t.Fatal(err)
	}
	admin := f.Hub.Admin
	rows, err := admin.QueryContext(t.Context(), "SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname='public' ORDER BY tablename")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{provider, r.Enrollment.Token, r.Redemption.Credential} {
		for _, table := range tables {
			var persisted bool
			query := `SELECT EXISTS(SELECT 1 FROM public."` + strings.ReplaceAll(table, `"`, `""`) + `" t WHERE strpos(t::text,$1)>0 OR strpos(t::text,encode(convert_to($1,'UTF8'),'hex'))>0)`
			if err := admin.QueryRowContext(t.Context(), query, value).Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			if persisted {
				t.Fatalf("private value escaped into table %s", table)
			}
		}
		if bytes.Contains(logs.Bytes(), []byte(value)) || bytes.Contains(response.Body.Bytes(), []byte(value)) {
			t.Fatal("private value escaped into logs or errors")
		}
	}
}
