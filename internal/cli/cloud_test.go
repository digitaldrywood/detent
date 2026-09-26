package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry"
)

func TestReadCloudConfig(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\n"
	for _, test := range []struct {
		name, body string
		env        map[string]string
		wantError  bool
	}{
		{name: "valid", body: base, env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}},
		{name: "missing signing key", body: base, env: map[string]string{"WORKOS_API_KEY": "sk_test"}, wantError: true},
		{name: "literal secret rejected", body: base + "signing_key: " + seed + "\n", env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}, wantError: true},
		{name: "invalid env name", body: strings.Replace(base, "issuer: detent-cloud", "issuer: detent-cloud\n  signing_key_env: bad-name", 1), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cloud.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := readCloudConfig(path, func(name string) string { return test.env[name] })
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if err == nil && (config.ListenAddress != "127.0.0.1:8017" || config.Issuer != "detent-cloud" || config.Provider == nil || len(config.SigningKey) == 0) {
				t.Fatalf("config = %+v", config)
			}
			if err != nil && strings.Contains(err.Error(), seed) {
				t.Fatal("error exposes the signing key")
			}
		})
	}
}

func TestCloudRegistryAndKeyCommands(t *testing.T) {
	t.Parallel()
	registry := filepath.Join(t.TempDir(), "registry.db")
	run := func(env map[string]string, args ...string) (string, error) {
		cmd := newCloudCommand(func(name string) string { return env[name] })
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return output.String(), err
	}
	register := []string{"registry", "register", "--registry", registry, "--organization", "org_example", "--provider-organization", "org_workos", "--name", "Example", "--endpoint", "unix:/run/detent/tenants/org_example.sock", "--generation", "1"}
	for _, want := range []string{`"changed":true`, `"changed":false`} {
		output, err := run(nil, register...)
		if err != nil || !strings.Contains(output, want) {
			t.Fatalf("register output = %q, %v; want %s", output, err, want)
		}
	}
	if _, err := run(nil, append(register[:len(register)-2:len(register)-2], "--generation", "0")...); err == nil {
		t.Fatal("register accepted generation zero")
	}
	output, err := run(nil, "registry", "list", "--registry", registry)
	if err != nil || !strings.Contains(output, `"id":"org_example"`) {
		t.Fatalf("list output = %q, %v", output, err)
	}
	output, err = run(nil, "assertion-key")
	if err != nil {
		t.Fatal(err)
	}
	var generated map[string]string
	if err := json.Unmarshal([]byte(output), &generated); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudassert.ParsePrivateKey(generated["signing_key"]); err != nil {
		t.Fatal(err)
	}
	output, err = run(map[string]string{"KEY": generated["signing_key"]}, "assertion-key", "--from-env", "KEY")
	if err != nil || !strings.Contains(output, generated["public_key"]) || strings.Contains(output, generated["signing_key"]) {
		t.Fatalf("public key output = %q, %v", output, err)
	}
}

func TestCloudAllocationGeneratesTenantConfiguration(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	body := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nstaff_emails: [support@example.test]\nsupport_actors: [support@example.test]\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation:\n  tenant_root: /var/lib/detent/tenants\n  socket_root: /run/detent/tenants\n  binary: /usr/local/bin/detent\n  max_tenants: 4\n"
	path := filepath.Join(t.TempDir(), "cloud.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}
	config, err := readCloudConfig(path, func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	allocation := config.Allocation
	if allocation == nil || allocation.MaxTenants != 4 || allocation.MaxConcurrent != 1 || allocation.MaxPerIdentity != 1 || allocation.RetryLimit != 5 {
		t.Fatalf("allocation = %+v", allocation)
	}
	launcher, ok := allocation.Launcher.(*cloudentry.ExecLauncher)
	if !ok || launcher.Binary != "/usr/local/bin/detent" || len(launcher.Environment) != 1 || launcher.Environment[0] != "WORKOS_API_KEY=sk_test" {
		t.Fatalf("launcher = %+v", allocation.Launcher)
	}
	key, err := cloudassert.ParsePrivateKey(seed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := launcher.Configure(cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 1}, PublicURL: "https://hub.example.test", Issuer: "detent-cloud", PublicKey: cloudassert.PublicKeyOf(key)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk_test") || strings.Contains(string(raw), seed) {
		t.Fatal("tenant configuration contains a secret")
	}
	tenantPath := filepath.Join(t.TempDir(), "tenant.yaml")
	if err := os.WriteFile(tenantPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tenant, _, err := readHostedConfig(tenantPath, func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("generated tenant configuration is invalid: %v\n%s", err, raw)
	}
	if tenant.OrganizationID != "org_tenant" || tenant.WorkOSOrganizationID != "org_workos" || tenant.SharedEntry == nil || tenant.SharedEntry.Generation != 1 || tenant.BootstrapSubject != "" {
		t.Fatalf("tenant = %+v", tenant)
	}
}

func TestCloudBillingConfiguration(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\n"
	allocation := "allocation:\n  tenant_root: /t\n  socket_root: /s\n  binary: /bin/detent\n  max_tenants: 2\n  entitlements:\n    base: {id: free, version: 1}\n    window_seconds: 3600\n    retention_windows: 24\n    connected_seconds: 90\n    invitation_seconds: 86400\n    plans:\n      - {id: free, version: 1, features: [collaboration], allowances: {projects: 3}}\n      - {id: team, version: 1, features: [collaboration], allowances: {projects: 30}}\n  billing:\n    mode: MODE\n    account_id: acct_fixture\n    portal_configuration_id: bpc_fixture\n    api_key_env: DETENT_STRIPE_TEST_KEY\n    webhook_secret_env: DETENT_STRIPE_TEST_WEBHOOK_SECRET\n    grace_seconds: 3600\n    reconcile_seconds: 120\n    prices:\n      - {price_id: price_team, label: Team, plan: {id: team, version: 1}}\n"
	env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "DETENT_STRIPE_TEST_KEY": "sk_test_fixture_value", "DETENT_STRIPE_TEST_WEBHOOK_SECRET": "whsec_fixture_secret_value", "DETENT_STRIPE_LIVE_KEY": "sk_live_fixture_value"}
	for _, test := range []struct {
		name, body string
		wantError  bool
	}{
		{name: "test mode", body: base + "billing:\n  account_id: acct_fixture\n" + strings.Replace(allocation, "MODE", "test", 1)},
		{name: "tenant mode differs", body: base + "billing:\n  account_id: acct_fixture\n" + strings.Replace(allocation, "MODE", "live", 1), wantError: true},
		{name: "tenant billing without entry billing", body: base + strings.Replace(allocation, "MODE", "test", 1), wantError: true},
		{name: "live mode needs live webhook secret", body: base + "billing:\n  mode: live\n  account_id: acct_fixture\n", wantError: true},
		{name: "live key in test mode", body: base + "billing:\n  account_id: acct_fixture\n  api_key_env: DETENT_STRIPE_LIVE_KEY\n", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cloud.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := readCloudConfig(path, func(name string) string { return env[name] })
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if err != nil {
				return
			}
			if config.Billing == nil || config.Billing.Mode != "test" || string(config.Billing.WebhookSecret) != env["DETENT_STRIPE_TEST_WEBHOOK_SECRET"] {
				t.Fatalf("billing = %+v", config.Billing)
			}
			launcher := config.Allocation.Launcher.(*cloudentry.ExecLauncher)
			raw, err := launcher.Configure(cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 1}, PublicURL: "https://hub.example.test", Issuer: "detent-cloud", PublicKey: "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="})
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"sk_test_fixture_value", "whsec_fixture_secret_value"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("tenant configuration contains a secret")
				}
			}
			if len(launcher.Environment) != 3 {
				t.Fatalf("tenant environment = %d entries", len(launcher.Environment))
			}
			tenantPath := filepath.Join(t.TempDir(), "tenant.yaml")
			if err := os.WriteFile(tenantPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			tenant, _, err := readHostedConfig(tenantPath, func(name string) string { return env[name] })
			if err != nil || tenant.Billing == nil || tenant.Billing.Mode != "test" || tenant.Billing.CustomerID != "" {
				t.Fatalf("tenant billing = %+v, %v\n%s", tenant, err, raw)
			}
		})
	}
}
