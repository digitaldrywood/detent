package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/cloudentry/integration/registrationtest"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestRegistrationOnlyTenantConfiguration(t *testing.T) {
	t.Parallel()
	body := clihangCloudPostgres + "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion: {issuer: entry}\nworkos: {client_id: client_example}\nattachments: {endpoint: https://storage.example.test, region: test, bucket: private}\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "cloud.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="}
	entry, err := readCloudConfig(path, clihangCloudLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	legacy := "organization_id: org_example\nworkos_organization_id: provider_example\nbootstrap_subject: user_original\nconversation: {enabled: false}\nusage: {currency: EUR, prices: {example: {input: 3}}}\n"
	if err := os.WriteFile(filepath.Join(dir, "tenant.yaml"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	spec := cloudentry.TenantSpec{Directory: dir, Organization: cloudentry.Organization{ID: "org_example", ProviderID: "provider_example", State: "ready", Generation: 2}, PublicURL: entry.PublicURL, Issuer: entry.Issuer, PublicKey: cloudassert.PublicKeyOf(entry.SigningKey)}
	raw, err := entry.SharedHub.Configure(spec)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := entry.SharedHub.TenantConfig(raw, spec)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hosted.BootstrapSubject != "user_original" || cfg.Conversation != nil || cfg.Usage == nil || cfg.Usage.Currency != "EUR" || cfg.Usage.Prices["example"].Input != 3 {
		t.Fatal("registration lost hosted bootstrap identity, conversation policy or pricing")
	}
	const legacyBilling = "billing: {mode: test, account_id: acct_legacy, customer_id: cus_registered, portal_configuration_id: bpc_legacy, api_key_env: EXAMPLE_STRIPE_KEY, webhook_secret_env: EXAMPLE_STRIPE_WEBHOOK, grace_seconds: 900, reconcile_seconds: 180, credit_cost_multiplier: 2, prices: [{price_id: price_legacy, label: Team, plan: {id: team, version: 1}}]}\n"
	env["EXAMPLE_STRIPE_KEY"] = "sk_test_example_legacy"
	env["EXAMPLE_STRIPE_WEBHOOK"] = "whsec_example_legacy_webhook"
	t.Run("legacy billing references survive registration and reopen", func(t *testing.T) {
		const plans = "entitlements: {base: {id: free, version: 1}, window_seconds: 3600, retention_windows: 24, connected_seconds: 90, invitation_seconds: 86400, plans: [{id: free, version: 1, features: [collaboration], allowances: {projects: 3}}, {id: team, version: 1, features: [collaboration], allowances: {projects: 30}}]}\n"
		spec := spec
		spec.ImportedConfiguration = []byte(legacy + plans + legacyBilling)
		for range 2 {
			data, err := entry.SharedHub.Configure(spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := entry.SharedHub.TenantConfig(data, spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := hostedplans.ValidateHostedConfig(got.Hosted); err != nil {
				t.Fatal(err)
			}
			b := got.Hosted.Billing
			if b == nil || b.Provider == nil || b.AccountID != "acct_legacy" || b.CustomerID != "cus_registered" || b.ReconcileSeconds != 180 || len(b.Prices) != 1 || b.Prices[0].PriceID != "price_legacy" || b.CreditCostMultiplier != 2 || string(b.WebhookSecret) != env["EXAMPLE_STRIPE_WEBHOOK"] {
				t.Fatal("registered billing configuration changed")
			}
			if bytes.Contains(data, []byte(env["EXAMPLE_STRIPE_KEY"])) || bytes.Contains(data, b.WebhookSecret) {
				t.Fatal("billing credential value reached persisted configuration")
			}
			spec.ImportedConfiguration = data
		}
	})
	t.Run("open persisted registration", func(t *testing.T) {
		registrationtest.VerifyUpgrade(t, func(prior cloudentry.Config, runtime postgres.Config, plans *hostedplans.HostedPlansConfig, customers map[string]string) cloudentry.Config {
			var file cloudFileConfig
			if err := yaml.Unmarshal([]byte(body), &file); err != nil {
				t.Fatal(err)
			}
			c := runtime.Connection
			file.TenantPostgres.Host, file.TenantPostgres.Port = c.Host, int(c.Port)
			file.TenantPostgres.Database, file.TenantPostgres.User, file.TenantPostgres.SSLMode = c.Database, c.User, c.SSLMode
			file.PublicURL, file.Listen, file.StateDirectory = prior.PublicURL, prior.ListenAddress, prior.StateDir
			file.Platform, file.StaffEmails = prior.Platform, prior.StaffEmails
			env[file.TenantPostgres.PasswordEnv] = c.Password
			env[file.Storage.PostgresDSNEnv] = prior.Storage.PostgresPool.Config().ConnString()
			env["DETENT_CLOUD_ASSERTION_KEY"] = base64.StdEncoding.EncodeToString(prior.SigningKey.Seed())
			for _, key := range []string{"DETENT_HUB_SECRET_KEYS", "DETENT_HUB_SECRET_KEY_VERSION"} {
				env[key] = prior.LookupEnv(key)
			}
			originalPlans, err := yaml.Marshal(struct {
				Plans *hostedplans.HostedPlansConfig `yaml:"entitlements"`
			}{plans})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"alpha", "beta"} {
				path := filepath.Join(prior.StateDir, "tenants", "org_"+name)
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				source := strings.NewReplacer("org_example", "org_"+name, "provider_example", "porg_"+name, "user_original", "user_alice").Replace(legacy)
				source += string(originalPlans) + strings.NewReplacer("cus_registered", customers["org_"+name], "id: team", "id: "+plans.Plans[1].ID).Replace(legacyBilling)
				if err := os.WriteFile(filepath.Join(path, "tenant.yaml"), []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := yaml.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			pgPath := filepath.Join(dir, "upgraded.yaml")
			if err := os.WriteFile(pgPath, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := readCloudConfig(pgPath, clihangCloudLookup(env))
			if err != nil {
				t.Fatal(err)
			}
			return loaded
		})
	})
}

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

func TestCloudTenantConfigurationUsesHostDatabase(t *testing.T) {
	t.Parallel()
	config := cloudFileConfig{Allocation: &cloudAllocationFileConfig{}, TenantPostgres: &hubPostgresFileConfig{
		Host: "db.example.test", Port: 5432, Database: "detent", User: "detent_runtime", PasswordEnv: "TENANT_DATABASE_PASSWORD", SSLMode: "verify-full",
		MaxConnections: 8, ServingConnections: 4, BackgroundConnections: 2,
	}}
	config.PublicURL = "https://hub.example.test"
	config.WorkOS.APIKeyEnv = "WORKOS_API_KEY"
	config.WorkOS.ClientID = "client_example"
	config.Assertion.Issuer = "detent-cloud"
	config.Storage.Backend = "postgres"
	spec := cloudentry.TenantSpec{
		Organization: cloudentry.Organization{ID: "org_example", ProviderID: "provider_example", Generation: 1},
		PublicURL:    config.PublicURL, Issuer: config.Assertion.Issuer, PublicKey: "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=",
	}
	raw, err := tenantConfiguration(config)(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-database-password") || strings.Contains(string(raw), "TENANT_DATABASE_PASSWORD") {
		t.Fatalf("generated hosted configuration contains an invalid secret reference: %s", raw)
	}
	values := map[string]string{"WORKOS_API_KEY": "workos-fixture", "TENANT_DATABASE_PASSWORD": "private-database-password"}
	hosted, err := parseHostedConfig(bytes.NewReader(raw), func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if err := hostedplans.ValidateHostedConfig(hosted); err != nil {
		t.Fatal(err)
	}
	if hosted.Postgres != nil {
		t.Fatal("generated tenant settings include a database connection")
	}
}

func TestTenantEnvironmentPassesCredentialsOnlyWhenPresent(t *testing.T) {
	config := cloudFileConfig{Allocation: &cloudAllocationFileConfig{}}
	config.WorkOS.APIKeyEnv = "WORKOS_API_KEY"
	for _, test := range []struct {
		name   string
		keys   string
		want   []string
		github map[string]string
	}{
		{name: "missing", want: []string{"WORKOS_API_KEY=workos-test-key"}},
		{name: "present", keys: `{"1":"a2V5"}`, want: []string{"WORKOS_API_KEY=workos-test-key", `DETENT_HUB_SECRET_KEYS={"1":"a2V5"}`, "DETENT_HUB_SECRET_KEY_VERSION=1"}},
		{name: "GitHub App", github: map[string]string{"DETENT_HUB_GITHUB_APP_ID": "123", "DETENT_HUB_GITHUB_APP_PRIVATE_KEY": "private-key", "DETENT_HUB_GITHUB_WEBHOOK_SECRET": "webhook-secret"}, want: []string{"WORKOS_API_KEY=workos-test-key", "DETENT_HUB_GITHUB_APP_ID=123", "DETENT_HUB_GITHUB_APP_PRIVATE_KEY=private-key", "DETENT_HUB_GITHUB_WEBHOOK_SECRET=webhook-secret"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := tenantEnvironment(config, func(name string) string {
				switch name {
				case "WORKOS_API_KEY":
					return "workos-test-key"
				case "DETENT_HUB_SECRET_KEYS":
					return test.keys
				case "DETENT_HUB_SECRET_KEY_VERSION":
					return "1"
				}
				return test.github[name]
			})
			if strings.Join(env, "\n") != strings.Join(test.want, "\n") {
				t.Fatalf("tenant environment = %q, want %q", env, test.want)
			}
		})
	}
}

func TestReadCloudConfig(t *testing.T) {
	t.Parallel()
	seed := "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	base := "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\n"
	postgresStorage := "storage:\n  backend: postgres\n  postgres_dsn_env: EXAMPLE_PLATFORM_DSN\nattachments:\n  endpoint: https://storage.example.test\n  region: test\n  bucket: private\n"
	_, tenantDatabase, _ := strings.Cut(clihangCloudPostgres, "tenant_postgres:")
	tenantDatabase = "tenant_postgres:" + tenantDatabase
	for _, test := range []struct {
		name, body string
		env        map[string]string
		wantError  bool
	}{
		{name: "missing shared runtime role", body: base + postgresStorage, env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "EXAMPLE_PLATFORM_DSN": "postgres://example:example@localhost/example"}, wantError: true},
		{name: "valid", body: base + tenantDatabase + postgresStorage, env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "EXAMPLE_PLATFORM_DSN": "postgres://example:example@localhost/example"}},
		{name: "unknown entry backend", body: base + "storage:\n  backend: other\n", env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}, wantError: true},
		{name: "postgres needs explicit settings", body: base + "storage:\n  backend: postgres\n", env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}, wantError: true},
		{name: "postgres DSN stays in environment", body: base + "storage:\n  backend: postgres\n  postgres_dsn: forbidden\n", env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed}, wantError: true},
		{name: "postgres", body: base + tenantDatabase + "storage:\n  backend: postgres\n  postgres_dsn_env: EXAMPLE_PLATFORM_DSN\n  max_connections: 5\n  serving_connections: 3\n  background_connections: 1\nattachments:\n  endpoint: https://storage.example.test\n  region: test\n  bucket: private\n", env: map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "EXAMPLE_PLATFORM_DSN": "postgres://example:example@localhost/example"}},
		{name: "product App webhook", body: base + tenantDatabase + postgresStorage, env: map[string]string{"EXAMPLE_PLATFORM_DSN": "postgres://example:example@localhost/example", "WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "DETENT_HUB_GITHUB_WEBHOOK_SECRET": "webhook-secret", "DETENT_HUB_GITHUB_APP_ID": "123", "DETENT_HUB_GITHUB_APP_PRIVATE_KEY": "app-key", "DETENT_HUB_GITHUB_APP_CLIENT_ID": "app-client", "DETENT_HUB_GITHUB_APP_CLIENT_SECRET": "app-secret"}},
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
			config, err := readCloudConfig(path, clihangCloudLookup(test.env))
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if err == nil && (config.ListenAddress != "127.0.0.1:8017" || config.Issuer != "detent-cloud" || config.Provider == nil || len(config.SigningKey) == 0) {
				t.Fatalf("config = %+v", config)
			}
			if err == nil && string(config.GitHubWebhookSecret) != test.env["DETENT_HUB_GITHUB_WEBHOOK_SECRET"] {
				t.Fatal("entry webhook secret was not read from the environment")
			}
			if err == nil && test.env["DETENT_HUB_GITHUB_APP_ID"] != "" && (config.GitHubApp == nil || config.GitHubApp.AppID != "123" || config.GitHubApp.PrivateKey != "app-key") {
				t.Fatal("entry App credentials were not read from the environment")
			}
			if err == nil && (config.GitHubUserClientID != test.env["DETENT_HUB_GITHUB_APP_CLIENT_ID"] || config.GitHubUserClientSecret != test.env["DETENT_HUB_GITHUB_APP_CLIENT_SECRET"]) {
				t.Fatal("entry user authorization credentials were not read from the environment")
			}
			if err == nil && (config.Allocation != nil || config.SharedHub == nil) {
				t.Fatal("registration-only entry must serve through the shared Hub with signup disabled")
			}
			if err == nil && test.name == "valid" && (config.Storage.Pool.MaxConnections != 6 || config.Storage.Pool.ServingConnections != 4 || config.Storage.Pool.BackgroundConnections != 1) {
				t.Fatal("omitted platform budgets did not select 6/4/1")
			}
			if err == nil && test.name == "postgres" {
				if config.Storage.Backend != "postgres" || config.Storage.PostgresDSN != test.env["EXAMPLE_PLATFORM_DSN"] {
					t.Fatal("entry storage selection was not loaded")
				}
				if strings.Contains(string(config.RestartSettings), test.env["EXAMPLE_PLATFORM_DSN"]) {
					t.Fatal("restart settings contain the PostgreSQL credential")
				}
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
	base := clihangCloudPostgres + clihangCloudAttachments + "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nplatform:\n  bootstrap_admin_email: operator@example.test\nstaff_emails: [ops@example.test, Plans@Example.test]\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\n"
	allocation := "allocation:\n  tenant_root: /var/lib/detent/tenants\n  socket_root: /run/detent/tenants\n  binary: /usr/local/bin/detent\n  max_tenants: 4\n  entitlement_administrator: pilot-operator\n  entitlement_admin_token_env: DETENT_ENTITLEMENT_ADMIN_TOKEN\n"
	for _, test := range []struct {
		name, body, wantError string
	}{
		{"staff administrator", base + "entitlement_administrators: [plans@example.test]\n" + allocation, ""},
		{"no administrators", base + allocation, ""},
		{"administrator outside staff", base + "entitlement_administrators: [finance@example.test]\n" + allocation, ""},
		{"administrator without tenant credential", base + "entitlement_administrators: [plans@example.test]\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "cloud.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "DETENT_ENTITLEMENT_ADMIN_TOKEN": token}
			config, err := readCloudConfig(path, clihangCloudLookup(env))
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if config.Platform.BootstrapAdminEmail != "operator@example.test" {
				t.Fatalf("platform = %+v", config.Platform)
			}
			if test.name != "administrator without tenant credential" && (config.Allocation == nil || string(config.Allocation.EntitlementAdminToken) != token) {
				t.Fatalf("allocation = %+v", config.Allocation)
			}
			if strings.Contains(test.body, "entitlement_administrators") && strings.Join(config.EntitlementAdministrators, ",") != map[bool]string{true: "finance@example.test", false: "plans@example.test"}[test.name == "administrator outside staff"] {
				t.Fatalf("administrators = %v", config.EntitlementAdministrators)
			}
		})
	}
}

func TestCloudAssertionKeyCommands(t *testing.T) {
	t.Parallel()
	run := func(env map[string]string, args ...string) (string, error) {
		cmd := newCloudCommand(options{lookupEnv: func(name string) string { return env[name] }})
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return output.String(), err
	}
	output, err := run(nil, "assertion-key")
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
	body := clihangCloudPostgres + "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nstaff_emails: [support@example.test]\nsupport_actors: [support@example.test]\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation: {}\n"
	body += "attachments:\n  endpoint: https://nyc3.digitaloceanspaces.com\n  region: nyc3\n  bucket: detent-private-attachments\n"
	path := filepath.Join(t.TempDir(), "cloud.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": seed, "DETENT_ATTACHMENTS_ACCESS_KEY_ID": "spaces-key", "DETENT_ATTACHMENTS_SECRET_ACCESS_KEY": "spaces-secret"}
	config, err := readCloudConfig(path, clihangCloudLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	allocation := config.Allocation
	if allocation.Launcher != nil {
		t.Fatal("hosted allocation launches a Hub process per organization")
	}
	if config.Attachments == nil || config.Attachments.AccessKeyID != env["DETENT_ATTACHMENTS_ACCESS_KEY_ID"] || config.Attachments.SecretAccessKey != env["DETENT_ATTACHMENTS_SECRET_ACCESS_KEY"] {
		t.Fatal("entry attachment credentials were not loaded from the environment")
	}
	if allocation == nil || config.SharedHub == nil || allocation.MaxConcurrent != 1 || allocation.RetryLimit != 5 {
		t.Fatalf("allocation = %+v", allocation)
	}
	launcher := config.SharedHub
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
	tenantPath := filepath.Join(t.TempDir(), "tenant.yaml")
	if err := os.WriteFile(tenantPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tenant, enabled, err := readHostedConfig(tenantPath, clihangCloudLookup(env))
	if err != nil {
		t.Fatalf("generated tenant configuration is invalid: %v\n%s", err, raw)
	}
	if !enabled || tenant == nil {
		t.Fatal("generated tenant configuration did not enable hosting")
	}
	if tenant.OrganizationID != "org_tenant" || tenant.WorkOSOrganizationID != "org_workos" || tenant.SharedEntry == nil || tenant.SharedEntry.Generation != 1 || tenant.BootstrapSubject != "" {
		t.Fatalf("tenant = %+v", tenant)
	}
	t.Run("missing legacy source", func(t *testing.T) {
		spec := cloudentry.TenantSpec{Directory: t.TempDir(), Organization: cloudentry.Organization{ID: "org_example", ProviderID: "porg_example", State: "ready", Managed: true}}
		if _, err := launcher.Configure(spec); err == nil {
			t.Fatal("ready organization lost workspace policy when its original config disappeared")
		}
		spec.Organization.WorkspacePolicyKey = new(string)
		if _, err := launcher.Configure(spec); err == nil {
			t.Fatal("workspace marker hid missing hosted configuration")
		}
		spec.Organization.HostedConfigurationKey = new(string)
		if _, err := launcher.Configure(spec); err != nil {
			t.Fatalf("known disabled workspace policy requires a legacy file: %v", err)
		}
	})
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
	base := clihangCloudPostgres + clihangCloudAttachments + "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\n"
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
			config, err := readCloudConfig(path, clihangCloudLookup(env))
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if err != nil {
				return
			}
			if config.Billing == nil || config.Billing.Mode != "test" || string(config.Billing.WebhookSecret) != env["DETENT_STRIPE_TEST_WEBHOOK_SECRET"] {
				t.Fatalf("billing = %+v", config.Billing)
			}
			launcher := config.SharedHub
			raw, err := launcher.Configure(cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 1}, PublicURL: "https://hub.example.test", Issuer: "detent-cloud", PublicKey: "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="})
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"sk_test_fixture_value", "whsec_fixture_secret_value"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("tenant configuration contains a secret")
				}
			}
			tenantPath := filepath.Join(t.TempDir(), "tenant.yaml")
			if err := os.WriteFile(tenantPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			tenant, _, err := readHostedConfig(tenantPath, clihangCloudLookup(env))
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
	base := clihangCloudPostgres + clihangCloudAttachments + "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation:\n  tenant_root: /var/lib/detent/tenants\n  socket_root: /run/detent/tenants\n  binary: /usr/local/bin/detent\n  max_tenants: 4\n  entitlements:\n    base: {id: pilot_free, version: 1}\n    window_seconds: 3600\n    retention_windows: 24\n    connected_seconds: 90\n    invitation_seconds: 86400\n    plans:\n      - {id: pilot_free, version: 1, features: [collaboration], allowances: {projects: 1}}\n"
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
			config, err := readCloudConfig(path, clihangCloudLookup(env))
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, want error %v", err, test.wantError)
			}
			if err != nil {
				if strings.Contains(err.Error(), token) {
					t.Fatal("error exposes the entitlement token")
				}
				return
			}
			launcher := config.SharedHub
			if launcher == nil {
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
			tenant, err := launcher.TenantConfig(raw, cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 1}, Issuer: "detent-cloud"})
			if err != nil {
				t.Fatalf("generated tenant configuration is invalid: %v\n%s", err, raw)
			}
			if got := string(tenant.Hosted.EntitlementAdminToken) == token && tenant.Hosted.EntitlementAdministrator == "pilot-operator"; got != test.wantGrants {
				t.Fatalf("tenant entitlement administration = %v, want %v", got, test.wantGrants)
			}
			if tenant.Hosted.Plans == nil || tenant.Hosted.Billing != nil {
				t.Fatalf("tenant plans = %v billing = %v", tenant.Hosted.Plans, tenant.Hosted.Billing)
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
	base := clihangCloudPostgres + clihangCloudAttachments + "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation:\n  tenant_root: /t\n  socket_root: /s\n  binary: /bin/detent\n  max_tenants: 2\n"
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
			config, err := readCloudConfig(path, clihangCloudLookup(env))
			if err != nil {
				t.Fatalf("readCloudConfig() error = %v", err)
			}
			launcher := config.SharedHub
			raw, err := launcher.Configure(cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_tenant", ProviderID: "org_workos", Generation: 1}, PublicURL: "https://hub.example.test", Issuer: "detent-cloud", PublicKey: seed})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "\nentitlements: null\n") {
				t.Fatalf("generated tenant configuration overrides the default catalog:\n%s", raw)
			}
			tenant, err := parseHostedConfig(bytes.NewReader(raw), clihangCloudLookup(env))
			if err != nil {
				t.Fatal(err)
			}
			if err := hostedplans.ValidateHostedConfig(tenant); err != nil || tenant.Plans != nil {
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

func TestCloudReloadValidatesFileAndPreservesLaunchIdentity(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "cloud.yaml")
	body := clihangCloudPostgres + clihangCloudAttachments + "public_url: https://hub.example.test\nstate_directory: /var/lib/detent/cloud\nassertion:\n  issuer: detent-cloud\nworkos:\n  client_id: client_example\nallocation:\n  tenant_root: /var/lib/detent/tenants\n  socket_root: /run/detent/tenants\n  binary: /usr/local/bin/detent\n  max_tenants: 4\n  entitlement_administrator: test-operator\n  entitlement_admin_token_env: ENTITLEMENT_TOKEN\n  entitlements:\n    base: {id: free_example, version: 1}\n    window_seconds: 3600\n    retention_windows: 24\n    connected_seconds: 90\n    invitation_seconds: 86400\n    plans:\n      - {id: free_example, version: 1, features: [collaboration], allowances: {projects: 1}}\n"
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(body)
	env := map[string]string{"WORKOS_API_KEY": "sk_test", "DETENT_CLOUD_ASSERTION_KEY": "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=", "ENTITLEMENT_TOKEN": "entitlement-admin-token-0123456789abcdef"}
	config, err := readCloudConfig(path, clihangCloudLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	write(strings.Replace(body, "allocation:\n", "allocation:\n  allowed_emails: [member@example.test]\n  allowed_domains: [example.org]\n", 1))
	allowlisted, err := config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(config.RestartSettings, allowlisted.RestartSettings) ||
		!reflect.DeepEqual(allowlisted.Allocation.AllowedEmails, []string{"member@example.test"}) ||
		!reflect.DeepEqual(allowlisted.Allocation.AllowedDomains, []string{"example.org"}) {
		t.Fatal("allowlist reload lost values or changed restart-only settings")
	}
	for _, invalid := range []string{body + "unknown_key: true\n", body + "---\npublic_url: https://other.example.test\n", strings.Replace(body, "connected_seconds: 90", "connected_seconds: 0", 1), strings.Replace(body, "client_id: client_example", "client_id: ''", 1)} {
		write(invalid)
		if _, err := config.Reload(); err == nil {
			t.Fatal("invalid complete cloud file accepted")
		}
	}
	write(strings.Replace(strings.Replace(body, "hub.example.test", "changed.example.test", 1), "projects: 1", "projects: 50", 1))
	next, err := config.Reload()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.ConfigureRuntime(*next.Runtime)(cloudentry.TenantSpec{Organization: cloudentry.Organization{ID: "org_example", ProviderID: "porg_example", Generation: 1}, PublicURL: config.PublicURL, Issuer: config.Issuer, PublicKey: cloudassert.PublicKeyOf(config.SigningKey)})
	if err != nil {
		t.Fatal(err)
	}
	var tenant hostedFileConfig
	if err := yaml.Unmarshal(raw, &tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.PublicURL != "https://hub.example.test" || tenant.Plans.Plans[0].Allowances["projects"] != 50 {
		t.Fatalf("reloaded tenant=%+v", tenant)
	}
}
