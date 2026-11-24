package registrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entryfixture"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/harness"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func Close(ctx context.Context) error { return harness.Postgres.Close(ctx) }

func VerifyUpgrade(t *testing.T, load func(cloudentry.Config, postgres.Config, *hostedplans.HostedPlansConfig, map[string]string) cloudentry.Config) {
	t.Helper()
	var registered *cloudentry.Config
	objects := harness.NewSpaces(t, false)
	storageConfig := objects.Config()
	storageConfig.Endpoint = "https://storage.example.test"
	storage, err := attachment.NewStorage(t.Context(), storageConfig, objects.Transport)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := billing.NewStripe(billing.StripeConfig{APIKey: "sk_test_registration_fixture", Client: &http.Client{Transport: entrytest.HandlerTransport{Handler: http.NotFoundHandler()}}})
	if err != nil {
		t.Fatal(err)
	}
	contents := attachment.ArchiveContents{Storage: storage.(attachment.ArchiveStorage)}
	f := entryfixture.NewWithConfig(t, slog.New(slog.DiscardHandler), []func(*cloudentry.Config){func(cfg *cloudentry.Config) {
		cfg.Attachments, cfg.AttachmentTransport = &storageConfig, objects.Transport
		cfg.LookupEnv = harness.SecretEnvironment(1, 1)
		registered = cfg
	}}, func(cfg *hubserver.Config) {
		cfg.RunDetails = harness.TenantDetails{ArchiveContentStore: contents}
		cfg.Hosted.Billing = &hostedplans.HostedBillingConfig{
			Mode: "test", AccountID: "acct_legacy", CustomerID: hubapi.NewID("cus"),
			PortalConfigurationID: "bpc_legacy", WebhookSecret: []byte("whsec_example_legacy_webhook"), Provider: provider,
			ReconcileSeconds: 180, GraceSeconds: 900, CreditCostMultiplier: 2,
			Prices: []hostedplans.HostedBillingPrice{{PriceID: "price_legacy", Label: "Team", Plan: cfg.Hosted.Plans.Plans[1].PlanReference}},
		}
	})
	browser := harness.NewBrowser(t, f.Service.Handler())
	browser.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	var key, scoped struct{ Token string }
	harness.CreateAccessKey(t, browser, "global", nil, "", &key, "read")
	harness.CreateAccessKey(t, browser, "selected", []harness.KeyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "all"}}}, "", &scoped, "read")
	original := f.Fixtures["org_alpha"].Config
	customers := make(map[string]string, len(f.Fixtures))
	for id, tenant := range f.Fixtures {
		customers[id] = tenant.Config.Hosted.Billing.CustomerID
	}
	cfg := load(*registered, *original.Postgres, original.Hosted.Plans, customers)
	if err := f.Service.Close(); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range f.Fixtures {
		if err := fixture.Service.Close(); err != nil {
			t.Fatal(err)
		}
	}
	priorRequests := objects.Requests()
	historical := make(map[string]bool)
	for _, request := range priorRequests {
		if key, ok := strings.CutPrefix(request, "PUT "); ok {
			historical[key] = true
		}
	}
	f.Provider.Users["user_staff"] = "staff@example.test"
	f.Provider.Users["user_admin"] = registered.Platform.BootstrapAdminEmail
	cfg.Provider, cfg.AttachmentTransport = f.Provider, registered.AttachmentTransport
	parse := cfg.SharedHub.TenantConfig
	cfg.SharedHub.TenantConfig = func(data []byte, spec cloudentry.TenantSpec) (hubserver.Config, error) {
		tenant, err := parse(data, spec)
		if err == nil {
			tenant.Hosted.Provider = f.Provider
			if tenant.Hosted.Billing == nil {
				return hubserver.Config{}, errors.New("missing registered billing configuration")
			}
			tenant.Hosted.Billing.Provider = provider
		}
		return tenant, err
	}
	entry, err := cloudentry.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := entry.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct{ name, token, id, own, other string }{
		{"global alpha", key.Token, "org_alpha", "Alpha secret project", "Beta secret project"},
		{"global beta", key.Token, "org_beta", "Beta secret project", "Alpha secret project"},
		{"scoped alpha", scoped.Token, "org_alpha", "Alpha secret project", "Beta secret project"},
		{"scoped beta refused", scoped.Token, "org_beta", "", "Beta secret project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := harness.NewBrowser(t, entry.Handler())
			resp := harness.Request(t, client, http.MethodGet, "/api/v2/projects", nil, map[string]string{"Authorization": "Bearer " + tc.token, "X-Detent-Organization": tc.id})
			want := http.StatusOK
			if tc.own == "" {
				want = http.StatusForbidden
			}
			if resp.Code != want || !strings.Contains(resp.Body.String(), tc.own) || strings.Contains(resp.Body.String(), tc.other) {
				t.Fatalf("pre-upgrade key could not isolate projects for %s (HTTP %d): %s", tc.id, resp.Code, resp.Body.String())
			}
		})
	}
	t.Run("registered billing accounts remain scheduled", func(t *testing.T) {
		for _, name := range []string{"alpha", "beta"} {
			var customer string
			var seconds int
			err := f.DB.QueryRowContext(t.Context(), `SELECT a.customer_id,o.billing_reconcile_seconds
				FROM public.hosted_billing_accounts a JOIN platform.organizations o ON a.organization_id=o.id
				WHERE o.id=$1 AND a.account_id='acct_legacy' AND a.mode='test' AND o.billing_next_due_at IS NOT NULL`, "org_"+name).Scan(&customer, &seconds)
			if err != nil || customer != customers["org_"+name] || seconds != 180 {
				t.Fatalf("registered %s customer=%q seconds=%d error=%v", name, customer, seconds, err)
			}
		}
	})
	t.Run("staff bootstrap", func(t *testing.T) {
		client := harness.NewBrowser(t, entry.Handler())
		if landing := client.Login("/auth/oidc/start", "user_staff:"); landing != "/platform" {
			t.Fatalf("staff landing = %q", landing)
		}
		resp, raw := client.Get("/api/cloud/platform/health")
		var health struct {
			Hubs []struct {
				ID      string
				Running bool
			} `json:"tenant_hubs"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &health) != nil || len(health.Hubs) != 1 || health.Hubs[0].ID != "shared" || !health.Hubs[0].Running {
			t.Fatalf("staff health did not identify one running shared Hub (HTTP %d)", resp.StatusCode)
		}
	})
	t.Run("bootstrap administrator", func(t *testing.T) {
		client := harness.NewBrowser(t, entry.Handler())
		client.Login("/auth/oidc/start", "user_admin:")
		resp, raw := client.Get("/api/cloud/platform/members")
		var members struct{ Self struct{ Email, Role string } }
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(raw), &members) != nil || members.Self.Email != registered.Platform.BootstrapAdminEmail || members.Self.Role != "admin" {
			t.Fatalf("bootstrap administrator membership = %+v (HTTP %d)", members.Self, resp.StatusCode)
		}
	})
	t.Run("historical archives", func(t *testing.T) {
		read := make(map[string]bool)
		for _, request := range objects.Requests()[len(priorRequests):] {
			key, ok := strings.CutPrefix(request, "GET ")
			if ok && historical[key] {
				read[key] = true
			}
		}
		for _, org := range []string{"org_alpha", "org_beta"} {
			ok := false
			for key := range read {
				ok = ok || strings.HasPrefix(key, "orgs/"+org+"/blobs/sha256/")
			}
			if !ok {
				t.Errorf("shared entry did not read a pre-upgrade archive for %s", org)
			}
		}
	})
}
