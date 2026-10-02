package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestTenantEnvironmentPassesOpenAIKeyOnlyWhenPresent(t *testing.T) {
	config := cloudFileConfig{Allocation: &cloudAllocationFileConfig{}}
	config.WorkOS.APIKeyEnv = "WORKOS_API_KEY"
	for _, test := range []struct {
		name string
		key  string
		want int
	}{
		{name: "missing", want: 1},
		{name: "present", key: "test-key", want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := tenantEnvironment(config, func(name string) string {
				if name == "OPENAI_API_KEY" {
					return test.key
				}
				if name == "WORKOS_API_KEY" {
					return "workos-test-key"
				}
				return ""
			})
			if len(env) != test.want {
				t.Fatalf("tenant environment has %d entries, want %d", len(env), test.want)
			}
			if test.key != "" && env[1] != "OPENAI_API_KEY="+test.key {
				t.Fatal("OpenAI key was not passed to the tenant")
			}
		})
	}
}

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
		{name: "attachment secret in YAML rejected", body: base + "attachments:\n  endpoint: https://nyc3.digitaloceanspaces.com\n  region: nyc3\n  bucket: private\n  access_key_id: forbidden\n", env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}, wantError: true},
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

func TestCloudEntitlementAdministrators(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	token := "entitlement-admin-token-0123456789abcdef"
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nstaff_emails: [ops@example.test, Plans@Example.test]\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\n"
	allocation := "allocation:\n  tenant_root: /var/lib/detent/tenants\n  socket_root: /run/detent/tenants\n  binary: /usr/local/bin/detent\n  max_tenants: 4\n  entitlement_administrator: pilot-operator\n  entitlement_admin_token_env: DETENT_ENTITLEMENT_ADMIN_TOKEN\n"
	for _, test := range []struct {
		name, body, wantError string
	}{
		{"staff administrator", base + "entitlement_administrators: [plans@example.test]\n" + allocation, ""},
		{"no administrators", base + allocation, ""},
		{"administrator outside staff", base + "entitlement_administrators: [plans@example.test, finance@example.test]\n" + allocation, `"finance@example.test" is not listed in staff_emails`},
		{"administrator without tenant credential", base + "entitlement_administrators: [plans@example.test]\n", "requires allocation.entitlement_admin_token_env"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cloud.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "DETENT_ENTITLEMENT_ADMIN_TOKEN": token}
			config, err := readCloudConfig(path, func(name string) string { return env[name] })
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.Allocation == nil || string(config.Allocation.EntitlementAdminToken) != token {
				t.Fatalf("allocation = %+v", config.Allocation)
			}
			if strings.Contains(test.body, "entitlement_administrators") && strings.Join(config.EntitlementAdministrators, ",") != "plans@example.test" {
				t.Fatalf("administrators = %v", config.EntitlementAdministrators)
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
	register := []string{"registry", "register", "--registry", registry, "--organization", "org_example", "--provider-organization", "org_workos", "--name", "Example", "--endpoint", "unix:" + filepath.Join(t.TempDir(), "org_example.sock"), "--generation", "1"}
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
	body += "attachments:\n  endpoint: https://nyc3.digitaloceanspaces.com\n  region: nyc3\n  bucket: detent-private-attachments\n"
	path := filepath.Join(t.TempDir(), "cloud.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "DETENT_ATTACHMENTS_ACCESS_KEY_ID": "spaces-key", "DETENT_ATTACHMENTS_SECRET_ACCESS_KEY": "spaces-secret"}
	config, err := readCloudConfig(path, func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	allocation := config.Allocation
	if config.Attachments == nil || config.Attachments.AccessKeyID != env["DETENT_ATTACHMENTS_ACCESS_KEY_ID"] || config.Attachments.SecretAccessKey != env["DETENT_ATTACHMENTS_SECRET_ACCESS_KEY"] {
		t.Fatal("entry attachment credentials were not loaded from the environment")
	}
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
	if strings.Contains(string(raw), "attachments") || strings.Contains(string(raw), "spaces-key") || strings.Contains(string(raw), "spaces-secret") {
		t.Fatal("tenant configuration contains attachment storage configuration or credentials")
	}
	for _, value := range launcher.Environment {
		if strings.HasPrefix(value, "DETENT_ATTACHMENTS_") || strings.Contains(value, "spaces-key") || strings.Contains(value, "spaces-secret") {
			t.Fatal("tenant environment contains Spaces credentials")
		}
	}
	tenantPath := filepath.Join(t.TempDir(), "tenant.yaml")
	if err := os.WriteFile(tenantPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tenant, enabled, err := readHostedConfig(tenantPath, func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("generated tenant configuration is invalid: %v\n%s", err, raw)
	}
	if !enabled || tenant == nil {
		t.Fatal("generated tenant configuration did not enable hosting")
	}
	if tenant.OrganizationID != "org_tenant" || tenant.WorkOSOrganizationID != "org_workos" || tenant.SharedEntry == nil || tenant.SharedEntry.Generation != 1 || tenant.BootstrapSubject != "" {
		t.Fatalf("tenant = %+v", tenant)
	}
	for _, test := range []struct {
		name, policy      string
		wrongOrganization bool
	}{
		{name: "absent"},
		{name: "disabled", policy: "enabled: false\nterminal: {enabled: false, isolation: sandbox, record: false}"},
		{name: "files only", policy: "enabled: true\nterminal: {enabled: false, isolation: sandbox}"},
		{name: "terminal policy", policy: "enabled: true\nrequest_timeout: 3m\nretain_after_run: 10m\nidle_timeout: 15m\nmax_lifetime: 2h\nperson_max_open: 2\nplan: {max_open: 7}\nrelay: {memory: 64MB}\nfiles: {deny: [private/**]}\nterminal: {enabled: true, isolation: container, record: false}"},
		{name: "wrong organization", policy: "enabled: true", wrongOrganization: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var prior hostedFileConfig
			if err := yaml.Unmarshal(raw, &prior); err != nil {
				t.Fatal(err)
			}
			if test.policy != "" {
				prior.Workspaces = &hostedWorkspaceFileConfig{}
				if err := yaml.Unmarshal([]byte(test.policy), prior.Workspaces); err != nil {
					t.Fatal(err)
				}
			}
			if test.wrongOrganization {
				prior.OrganizationID = "org_other"
			}
			directory := t.TempDir()
			spec := cloudentry.TenantSpec{Directory: directory, Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 2}, PublicURL: "https://hub.example.test", Issuer: "detent-cloud", PublicKey: cloudassert.PublicKeyOf(key)}
			for generation := int64(2); generation <= 3; generation++ {
				encoded, err := yaml.Marshal(prior)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "tenant.yaml"), encoded, 0o600); err != nil {
					t.Fatal(err)
				}
				spec.Organization.Generation = generation
				generated, err := launcher.Configure(spec)
				if test.wrongOrganization {
					if err == nil {
						t.Fatal("another organization's workspace policy was accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var next hostedFileConfig
				if err := yaml.Unmarshal(generated, &next); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(next.Workspaces, prior.Workspaces) {
					t.Fatal("tenant workspace policy changed during generation")
				}
				next.Workspaces, prior.Workspaces = nil, nil
				prior.SharedEntry.AllocationGeneration = generation
				if !reflect.DeepEqual(next, prior) {
					t.Fatal("unrelated tenant settings changed")
				}
				if err := yaml.Unmarshal(generated, &prior); err != nil {
					t.Fatal(err)
				}
			}
			other, err := launcher.Configure(cloudentry.TenantSpec{Directory: t.TempDir(), Organization: cloudentry.Organization{ID: "org_other", ProviderID: "org_other", Generation: 1}})
			if err != nil {
				t.Fatal(err)
			}
			var unrelated hostedFileConfig
			if err := yaml.Unmarshal(other, &unrelated); err != nil || unrelated.Workspaces != nil {
				t.Fatal("workspace policy propagated to another tenant")
			}
		})
	}
}

func TestCloudBillingConfiguration(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\n"
	allocation := "allocation:\n  tenant_root: /t\n  socket_root: /s\n  binary: /bin/detent\n  max_tenants: 2\n  entitlements:\n    base: {id: free, version: 1}\n    window_seconds: 3600\n    retention_windows: 24\n    connected_seconds: 90\n    invitation_seconds: 86400\n    plans:\n      - {id: free, version: 1, features: [collaboration], allowances: {projects: 3}}\n      - {id: team, version: 1, features: [collaboration], allowances: {projects: 30}}\n  billing:\n    mode: MODE\n    account_id: acct_fixture\n    portal_configuration_id: bpc_fixture\n    api_key_env: DETENT_STRIPE_TEST_KEY\n    webhook_secret_env: DETENT_STRIPE_TEST_WEBHOOK_SECRET\n    grace_seconds: 3600\n    reconcile_seconds: 120\n    credit_cost_multiplier: 2\n    prices:\n      - {price_id: price_team, label: Team, plan: {id: team, version: 1}}\n"
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
			if err != nil || tenant.Billing == nil || tenant.Billing.Mode != "test" || tenant.Billing.CustomerID != "" || tenant.Billing.CreditCostMultiplier != 2 {
				t.Fatalf("tenant billing = %+v, %v\n%s", tenant, err, raw)
			}
		})
	}
}
func TestCloudAllocationPassesEntitlementAdministration(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	token := "entitlement-operator-token-0123456789abcdef"
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation:\n  tenant_root: /var/lib/detent/tenants\n  socket_root: /run/detent/tenants\n  binary: /usr/local/bin/detent\n  max_tenants: 4\n  entitlements:\n    base: {id: pilot_free, version: 1}\n    window_seconds: 3600\n    retention_windows: 24\n    connected_seconds: 90\n    invitation_seconds: 86400\n    plans:\n      - {id: pilot_free, version: 1, features: [collaboration], allowances: {projects: 1}}\n"
	administration := "  entitlement_administrator: pilot-operator\n  entitlement_admin_token_env: DETENT_ENTITLEMENT_TOKEN\n"
	for _, test := range []struct {
		name, body string
		env        map[string]string
		wantError  bool
		wantGrants bool
	}{
		{name: "without entitlement administration", body: base},
		{name: "operator grants", body: base + administration, env: map[string]string{"DETENT_ENTITLEMENT_TOKEN": token}, wantGrants: true},
		{name: "missing token", body: base + administration, wantError: true},
		{name: "short token", body: base + administration, env: map[string]string{"DETENT_ENTITLEMENT_TOKEN": "short"}, wantError: true},
		{name: "missing administrator", body: base + "  entitlement_admin_token_env: DETENT_ENTITLEMENT_TOKEN\n", env: map[string]string{"DETENT_ENTITLEMENT_TOKEN": token}, wantError: true},
		{name: "administrator without token environment", body: base + "  entitlement_administrator: pilot-operator\n", env: map[string]string{"DETENT_ENTITLEMENT_TOKEN": token}, wantError: true},
		{name: "administrator with spaces", body: base + "  entitlement_administrator: pilot operator\n  entitlement_admin_token_env: DETENT_ENTITLEMENT_TOKEN\n", env: map[string]string{"DETENT_ENTITLEMENT_TOKEN": token}, wantError: true},
		{name: "reuses tenant admin token", body: base + "  entitlement_administrator: pilot-operator\n  entitlement_admin_token_env: DETENT_HUB_ADMIN_TOKEN\n", env: map[string]string{"DETENT_HUB_ADMIN_TOKEN": token}, wantError: true},
		{name: "reuses provider secret", body: base + "  entitlement_administrator: pilot-operator\n  entitlement_admin_token_env: WORKOS_API_KEY\n", env: map[string]string{"WORKOS_API_KEY": token}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}
			for key, value := range test.env {
				env[key] = value
			}
			path := filepath.Join(t.TempDir(), "cloud.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := readCloudConfig(path, func(name string) string { return env[name] })
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if err != nil {
				if strings.Contains(err.Error(), token) {
					t.Fatal("error exposes the entitlement token")
				}
				return
			}
			launcher, ok := config.Allocation.Launcher.(*cloudentry.ExecLauncher)
			if !ok {
				t.Fatalf("launcher = %T", config.Allocation.Launcher)
			}
			key, err := cloudassert.ParsePrivateKey(seed)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := launcher.Configure(cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 1}, PublicURL: "https://hub.example.test", Issuer: "detent-cloud", PublicKey: cloudassert.PublicKeyOf(key)})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), token) {
				t.Fatal("tenant configuration contains the entitlement token")
			}
			tenantEnv := map[string]string{}
			for _, entry := range launcher.Environment {
				name, value, _ := strings.Cut(entry, "=")
				tenantEnv[name] = value
			}
			tenantPath := filepath.Join(t.TempDir(), "tenant.yaml")
			if err := os.WriteFile(tenantPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			tenant, _, err := readHostedConfig(tenantPath, func(name string) string { return tenantEnv[name] })
			if err != nil {
				t.Fatalf("generated tenant configuration is invalid: %v\n%s", err, raw)
			}
			if got := string(tenant.EntitlementAdminToken) == token && tenant.EntitlementAdministrator == "pilot-operator"; got != test.wantGrants {
				t.Fatalf("tenant entitlement administration = %v, want %v", got, test.wantGrants)
			}
			if tenant.Plans == nil || tenant.Billing != nil {
				t.Fatalf("tenant plans = %v billing = %v", tenant.Plans, tenant.Billing)
			}
		})
	}
}

func TestSelfHostedIdentityNeverEnablesBilling(t *testing.T) {
	t.Parallel()
	env := map[string]string{"WORKOS_API_KEY": "sk_test_identity", "STRIPE_SECRET_KEY": "sk_test_billing", "STRIPE_WEBHOOK_SECRET": "whsec_0123456789abcdef"}
	for _, test := range []struct{ name, body string }{
		{name: "workos identity", body: "organization_id: org_self\nbootstrap_subject: user_owner\npublic_url: https://detent.example.test\nworkos:\n  client_id: client_example\n"},
		{name: "workos identity with entitlements", body: "organization_id: org_self\nbootstrap_subject: user_owner\npublic_url: https://detent.example.test\nworkos:\n  client_id: client_example\nentitlements:\n  base: {id: team, version: 1}\n  window_seconds: 3600\n  retention_windows: 24\n  connected_seconds: 90\n  invitation_seconds: 86400\n  plans:\n    - {id: team, version: 1, features: [collaboration], allowances: {projects: 100}}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "hosted.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			config, ok, err := readHostedConfig(path, func(name string) string { return env[name] })
			if err != nil || !ok {
				t.Fatalf("readHostedConfig = %v %v", ok, err)
			}
			if config.Billing != nil {
				t.Fatal("billing enabled without an explicit billing section")
			}
		})
	}
}

func TestCloudEmptyEntitlementsUseDefaultCatalog(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation:\n  tenant_root: /t\n  socket_root: /s\n  binary: /bin/detent\n  max_tenants: 2\n"
	for _, test := range []struct{ name, body string }{
		{name: "absent", body: base},
		{name: "empty mapping", body: base + "  entitlements: {}\n"},
		{name: "empty block", body: base + "  entitlements:\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}
			path := filepath.Join(t.TempDir(), "cloud.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := readCloudConfig(path, func(name string) string { return env[name] })
			if err != nil {
				t.Fatalf("readCloudConfig() error = %v", err)
			}
			launcher := config.Allocation.Launcher.(*cloudentry.ExecLauncher)
			raw, err := launcher.Configure(cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 1}, PublicURL: "https://hub.example.test", Issuer: "detent-cloud", PublicKey: seed})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "\nentitlements: null\n") {
				t.Fatalf("generated tenant configuration overrides the default catalog:\n%s", raw)
			}
			tenant, err := parseHostedConfig(bytes.NewReader(raw), func(name string) string { return env[name] })
			if err != nil {
				t.Fatal(err)
			}
			if err := hubserver.ValidateHostedConfig(tenant); err != nil || tenant.Plans != nil {
				t.Fatalf("tenant plans = %+v, %v; want default catalog", tenant.Plans, err)
			}
		})
	}
}

func TestCloudRejectsInvalidTenantConfigurationAtStartup(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation:\n  tenant_root: /t\n  socket_root: /s\n  binary: /bin/detent\n  max_tenants: 2\n"
	windows := "    window_seconds: 3600\n    retention_windows: 24\n    connected_seconds: 90\n    invitation_seconds: 86400\n"
	plan := "    plans:\n      - {id: free, version: 1, features: [collaboration], allowances: {projects: 3}}\n"
	for _, test := range []struct{ name, entitlements string }{
		{name: "plans without windows", entitlements: "    base: {id: free, version: 1}\n" + plan},
		{name: "windows without plans", entitlements: "    base: {id: free, version: 1}\n" + windows},
		{name: "base only", entitlements: "    base: {id: free, version: 1}\n"},
		{name: "explicitly empty plan list", entitlements: "    plans: []\n"},
		{name: "missing base", entitlements: windows + plan},
		{name: "base not configured", entitlements: "    base: {id: team, version: 1}\n" + windows + plan},
		{name: "unknown feature", entitlements: "    base: {id: free, version: 1}\n" + windows + "    plans:\n      - {id: free, version: 1, features: [admin_bypass]}\n"},
		{name: "unknown allowance", entitlements: "    base: {id: free, version: 1}\n" + windows + "    plans:\n      - {id: free, version: 1, allowances: {unlimited: 1}}\n"},
		{name: "negative allowance", entitlements: "    base: {id: free, version: 1}\n" + windows + "    plans:\n      - {id: free, version: 1, allowances: {projects: -1}}\n"},
		{name: "duplicate plan", entitlements: "    base: {id: free, version: 1}\n" + windows + plan + "      - {id: free, version: 1}\n"},
		{name: "unbounded retention", entitlements: "    base: {id: free, version: 1}\n    window_seconds: 3600\n    retention_windows: 721\n    connected_seconds: 90\n    invitation_seconds: 86400\n" + plan},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}
			path := filepath.Join(t.TempDir(), "cloud.yaml")
			if err := os.WriteFile(path, []byte(base+"  entitlements:\n"+test.entitlements), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := readCloudConfig(path, func(name string) string { return env[name] })
			if err == nil || !strings.Contains(err.Error(), "allocated tenant configuration is invalid") {
				t.Fatalf("readCloudConfig() error = %v, want tenant configuration rejection", err)
			}
		})
	}
}
