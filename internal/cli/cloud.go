package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cli/conversationconfig"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubgithub"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
)

type cloudFileConfig struct {
	Storage struct {
		Backend               string `yaml:"backend"`
		PostgresDSNEnv        string `yaml:"postgres_dsn_env"`
		MaxConnections        int    `yaml:"max_connections"`
		ServingConnections    int    `yaml:"serving_connections"`
		BackgroundConnections int    `yaml:"background_connections"`
	} `yaml:"storage"`
	TenantPostgres            *hubPostgresFileConfig    `yaml:"tenant_postgres"`
	PublicURL                 string                    `yaml:"public_url"`
	Listen                    string                    `yaml:"listen"`
	StateDirectory            string                    `yaml:"state_directory"`
	StaffEmails               []string                  `yaml:"staff_emails"`
	SupportActors             []string                  `yaml:"support_actors"`
	EntitlementAdministrators []string                  `yaml:"entitlement_administrators"`
	Platform                  cloudentry.PlatformConfig `yaml:"platform"`
	Assertion                 struct {
		Issuer        string `yaml:"issuer"`
		SigningKeyEnv string `yaml:"signing_key_env"`
	} `yaml:"assertion"`
	WorkOS struct {
		ClientID  string `yaml:"client_id"`
		APIKeyEnv string `yaml:"api_key_env"`
		APIURL    string `yaml:"api_url"`
		IssuerURL string `yaml:"issuer_url"`
	} `yaml:"workos"`
	Allocation  *cloudAllocationFileConfig `yaml:"allocation"`
	Billing     *cloudBillingFileConfig    `yaml:"billing"`
	Attachments *attachment.Config         `yaml:"attachments"`
}

type cloudBillingFileConfig struct {
	Mode             string `yaml:"mode"`
	AccountID        string `yaml:"account_id"`
	APIKeyEnv        string `yaml:"api_key_env"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
}

func (b *cloudBillingFileConfig) defaults() {
	if b.Mode == "" {
		b.Mode = billing.ModeTest
	}
	if b.APIKeyEnv == "" {
		b.APIKeyEnv = "DETENT_STRIPE_" + strings.ToUpper(b.Mode) + "_KEY"
	}
	if b.WebhookSecretEnv == "" {
		b.WebhookSecretEnv = "DETENT_STRIPE_" + strings.ToUpper(b.Mode) + "_WEBHOOK_SECRET"
	}
}

type cloudAllocationFileConfig struct {
	TenantRoot               string                         `yaml:"tenant_root"`
	SocketRoot               string                         `yaml:"socket_root"`
	LegacyBinary             string                         `yaml:"binary"`
	LegacyMaxTenants         int                            `yaml:"max_tenants"`
	MaxConcurrentProvisions  int                            `yaml:"max_concurrent_provisions"`
	RetryLimit               int                            `yaml:"retry_limit"`
	MinFreeDiskBytes         uint64                         `yaml:"min_free_disk_bytes"`
	MinAvailableMemoryBytes  uint64                         `yaml:"min_available_memory_bytes"`
	AllowedEmails            []string                       `yaml:"allowed_emails"`
	AllowedDomains           []string                       `yaml:"allowed_domains"`
	Entitlements             *hostedplans.HostedPlansConfig `yaml:"entitlements"`
	Billing                  *hostedBillingFileConfig       `yaml:"billing"`
	EntitlementAdministrator string                         `yaml:"entitlement_administrator"`
	EntitlementAdminTokenEnv string                         `yaml:"entitlement_admin_token_env"`
}

func tenantEnvironment(config cloudFileConfig, lookupEnv func(string) string) []string {
	names := []string{config.WorkOS.APIKeyEnv}
	for _, name := range []string{"DETENT_HUB_GITHUB_APP_ID", "DETENT_HUB_GITHUB_APP_PRIVATE_KEY", "DETENT_HUB_GITHUB_WEBHOOK_SECRET"} {
		if lookupEnv(name) != "" {
			names = append(names, name)
		}
	}
	if lookupEnv("OPENAI_API_KEY") != "" {
		names = append(names, "OPENAI_API_KEY")
	}
	// Tenant Hubs encrypt project provider secrets with the operator's master
	// keys; without them a hosted project cannot store a Sprites token.
	if lookupEnv("DETENT_HUB_SECRET_KEYS") != "" {
		names = append(names, "DETENT_HUB_SECRET_KEYS", "DETENT_HUB_SECRET_KEY_VERSION")
	}
	if billing := config.Billing; billing != nil {
		names = append(names, billing.APIKeyEnv, billing.WebhookSecretEnv)
	}
	if tenantBilling := config.Allocation.Billing; tenantBilling != nil {
		names = append(names, tenantBilling.APIKeyEnv, tenantBilling.WebhookSecretEnv)
	}
	if name := config.Allocation.EntitlementAdminTokenEnv; name != "" {
		names = append(names, name)
	}
	var environment []string
	for _, name := range names {
		if validEnvName(name) && name != "DETENT_ATTACHMENTS_ACCESS_KEY_ID" && name != "DETENT_ATTACHMENTS_SECRET_ACCESS_KEY" {
			environment = append(environment, name+"="+lookupEnv(name))
		}
	}
	if level := lookupEnv("LOG_LEVEL"); level != "" {
		environment = append(environment, "LOG_LEVEL="+level)
	}
	return environment
}

func tenantConfiguration(config cloudFileConfig) func(cloudentry.TenantSpec) ([]byte, error) {
	return func(spec cloudentry.TenantSpec) ([]byte, error) {
		preserved, err := preservedTenantConfiguration(spec)
		if err != nil {
			return nil, err
		}
		tenant := preserved
		if tenant.OrganizationID == "" {
			tenant.Conversation = &conversationconfig.Section{Enabled: true}
		}
		tenant.OrganizationID, tenant.WorkOSOrganizationID = spec.Organization.ID, spec.Organization.ProviderID
		tenant.PublicURL = spec.PublicURL
		tenant.Postgres = nil
		tenant.SharedEntry = &hostedSharedEntryFileConfig{Issuer: spec.Issuer, PublicKeys: []string{spec.PublicKey}, AllocationGeneration: spec.Organization.Generation}
		if allocation := config.Allocation; allocation != nil && (spec.Organization.Managed || preserved.OrganizationID == "") {
			tenant.Plans, tenant.Billing = allocation.Entitlements, allocation.Billing
			tenant.EntitlementAdministrator, tenant.EntitlementAdminTokenEnv = allocation.EntitlementAdministrator, allocation.EntitlementAdminTokenEnv
		}
		if spec.Organization.CheckoutPrice != "" {
			tenant.Plans = hostedplans.CheckoutHostedPlans(tenant.Plans)
		}
		tenant.WorkOS.ClientID, tenant.WorkOS.APIKeyEnv, tenant.WorkOS.APIURL, tenant.WorkOS.IssuerURL = config.WorkOS.ClientID, config.WorkOS.APIKeyEnv, config.WorkOS.APIURL, config.WorkOS.IssuerURL
		return yaml.Marshal(tenant)
	}
}

func sharedTenantConfig(raw []byte, spec cloudentry.TenantSpec, lookup, lookupBilling func(string) string) (hubserver.Config, error) {
	hosted, err := parseHostedConfigWithBilling(bytes.NewReader(raw), lookup, lookupBilling)
	if err != nil {
		return hubserver.Config{}, err
	}
	if !hosted.BindsSharedOrganization(spec.Organization.ID) || hosted.WorkOSOrganizationID != spec.Organization.ProviderID || hosted.SharedEntry.Generation != spec.Organization.Generation || hosted.SharedEntry.Issuer != spec.Issuer {
		return hubserver.Config{}, hubserver.ErrHostedDatabaseBinding
	}
	authority := hosted.SharedEntry
	authority.ResolveKey, authority.Content, authority.ArchiveContents = spec.ResolveKey, spec.Content, spec.ArchiveContents
	authority.RunDetails, authority.ReadArchive, authority.Icons = spec.RunDetails, spec.ReadArchive, spec.Icons
	keys, err := hubsecrets.FromEnvironment(lookup)
	if err != nil {
		return hubserver.Config{}, err
	}
	cfg := hubserver.Config{Hosted: hosted, DatabaseBackend: hubserver.DatabasePostgres, SecretKeys: keys, GitHubWebhookSecret: []byte(strings.TrimSpace(lookup("DETENT_HUB_GITHUB_WEBHOOK_SECRET")))}
	var file hostedFileConfig
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return hubserver.Config{}, err
	}
	usage, priced, err := convertUsageConfig(file.Usage)
	if err != nil {
		return hubserver.Config{}, err
	}
	if priced {
		cfg.Usage = &usage
	}
	conversation, enabled, err := file.Conversation.Build(buildCoordinatorCodexBackend, os.Stat, lookup)
	if err != nil {
		return hubserver.Config{}, err
	}
	if enabled {
		cfg.Conversation = &conversation
	}
	workspace, enabled, err := readWorkspaceConfig(file.Workspaces)
	if err != nil {
		return hubserver.Config{}, err
	}
	if enabled {
		cfg.Workspace = &workspace
	}
	if app, key := lookup("DETENT_HUB_GITHUB_APP_ID"), lookup("DETENT_HUB_GITHUB_APP_PRIVATE_KEY"); app != "" && key != "" {
		transport := hubgithub.NewAppTransport(connectorgithub.InstallationTokenConfig{AppID: app, PrivateKey: key, LookupEnv: lookup})
		cfg.GitHubAppInstallation = transport.AppInstallation
		cfg.OutboxBackend = hubgithub.NewWriter(transport)
		cfg.ReconcileBackend = hubgithub.NewReconciler(transport)
		cfg.ImportBackend = hubgithub.NewImporter(transport)
		cfg.GitHubRequestCounts = transport.Counts
	}
	return cfg, nil
}

func preservedTenantConfiguration(spec cloudentry.TenantSpec) (hostedFileConfig, error) {
	data, err := spec.ConfigurationSource()
	if err != nil || data == nil {
		return hostedFileConfig{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var tenant hostedFileConfig
	var trailing any
	if err := decoder.Decode(&tenant); err != nil || !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return hostedFileConfig{}, errors.New("original hosted configuration is invalid")
	}
	if tenant.OrganizationID != spec.Organization.ID || tenant.WorkOSOrganizationID != spec.Organization.ProviderID {
		return hostedFileConfig{}, errors.New("original hosted configuration belongs to another organization")
	}
	if _, _, err := readWorkspaceConfig(tenant.Workspaces); err != nil {
		return hostedFileConfig{}, err
	}
	return tenant, nil
}

func validateTenantConfiguration(config cloudFileConfig, key ed25519.PrivateKey, lookupEnv func(string) string) error {
	spec := cloudentry.TenantSpec{
		Organization: cloudentry.Organization{ID: "org_startup_validation", ProviderID: "org_startup_validation", Generation: 1},
		PublicURL:    config.PublicURL, Issuer: config.Assertion.Issuer, PublicKey: cloudassert.PublicKeyOf(key),
	}
	raw, err := tenantConfiguration(config)(spec)
	if err != nil {
		return fmt.Errorf("allocated tenant configuration cannot be generated: %w", err)
	}
	environment := make(map[string]string)
	for _, entry := range tenantEnvironment(config, lookupEnv) {
		name, value, _ := strings.Cut(entry, "=")
		environment[name] = value
	}
	tenant, err := parseHostedConfig(bytes.NewReader(raw), func(name string) string { return environment[name] })
	if err == nil {
		err = hostedplans.ValidateHostedConfig(tenant)
	}
	if err != nil {
		return fmt.Errorf("allocated tenant configuration is invalid; every tenant Hub would refuse to start: %w", err)
	}
	return nil
}

func entitlementActorValid(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) == -1
}

func validateEntitlementAdministration(config cloudFileConfig, lookupEnv func(string) string) error {
	allocation := config.Allocation
	name := allocation.EntitlementAdminTokenEnv
	if name == "" && allocation.EntitlementAdministrator == "" {
		return nil
	}
	reserved := []string{config.WorkOS.APIKeyEnv, config.Assertion.SigningKeyEnv, "DETENT_HUB_ADMIN_TOKEN", "PATH", "HOME", "TMPDIR", "LANG", "TZ"}
	if config.Billing != nil {
		reserved = append(reserved, config.Billing.APIKeyEnv, config.Billing.WebhookSecretEnv)
	}
	if allocation.Billing != nil {
		reserved = append(reserved, allocation.Billing.APIKeyEnv, allocation.Billing.WebhookSecretEnv)
	}
	if !validEnvName(name) || slices.Contains(reserved, name) {
		return errors.New("entitlement token environment name is invalid")
	}
	if len(lookupEnv(name)) < 32 || !entitlementActorValid(allocation.EntitlementAdministrator) {
		return errors.New("entitlement administration requires an administrator ID and a token of at least 32 bytes")
	}
	return nil
}

func readCloudConfig(path string, lookupEnv func(string) string) (cloudentry.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return cloudentry.Config{}, errors.New("shared entry configuration could not be opened")
	}
	if len(raw) > 64*1024 {
		return cloudentry.Config{}, errors.New("shared entry configuration is too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var config cloudFileConfig
	if err := decoder.Decode(&config); err != nil {
		return cloudentry.Config{}, errors.New("shared entry configuration is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return cloudentry.Config{}, errors.New("shared entry configuration must contain one document")
	}
	if config.WorkOS.APIKeyEnv == "" {
		config.WorkOS.APIKeyEnv = "WORKOS_API_KEY"
	}
	if config.Assertion.SigningKeyEnv == "" {
		config.Assertion.SigningKeyEnv = "DETENT_CLOUD_ASSERTION_KEY"
	}
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8017"
	}
	if !validEnvName(config.WorkOS.APIKeyEnv) || !validEnvName(config.Assertion.SigningKeyEnv) {
		return cloudentry.Config{}, errors.New("shared entry secret environment variable names are invalid")
	}
	key, err := cloudassert.ParsePrivateKey(lookupEnv(config.Assertion.SigningKeyEnv))
	if err != nil {
		return cloudentry.Config{}, errors.New("shared entry assertion signing key is unavailable or invalid")
	}
	provider, err := auth.NewHostedProvider(auth.IdentityProviderWorkOS, auth.WorkOSConfig{
		APIURL: config.WorkOS.APIURL, IssuerURL: config.WorkOS.IssuerURL, ClientID: config.WorkOS.ClientID,
		APIKey: lookupEnv(config.WorkOS.APIKeyEnv), RedirectURL: strings.TrimRight(config.PublicURL, "/") + "/auth/oidc/callback",
	})
	if err != nil {
		return cloudentry.Config{}, err
	}
	if config.Billing != nil {
		config.Billing.defaults()
	}
	if config.Allocation != nil && config.Allocation.Billing != nil {
		if config.Allocation.Billing.Mode == "" {
			config.Allocation.Billing.Mode = billing.ModeTest
		}
	}
	if config.Storage.MaxConnections == 0 && config.Storage.ServingConnections == 0 && config.Storage.BackgroundConnections == 0 {
		config.Storage.MaxConnections = 6
		config.Storage.ServingConnections = 4
		config.Storage.BackgroundConnections = 1
	}
	if config.Storage.PostgresDSNEnv != "" && !validEnvName(config.Storage.PostgresDSNEnv) {
		return cloudentry.Config{}, errors.New("entry PostgreSQL DSN environment variable name is invalid")
	}
	if config.TenantPostgres != nil && config.Storage.Backend != "postgres" {
		return cloudentry.Config{}, errors.New("tenant PostgreSQL requires PostgreSQL platform storage")
	}
	if config.Storage.Backend == "postgres" && config.TenantPostgres == nil {
		return cloudentry.Config{}, errors.New("cloud serve requires tenant_postgres with the shared database runtime role and pool budgets")
	}
	result := cloudentry.Config{
		Storage:                cloudentry.StorageConfig{Backend: config.Storage.Backend, PostgresDSN: lookupEnv(config.Storage.PostgresDSNEnv), Pool: platformstore.PoolConfig{MaxConnections: config.Storage.MaxConnections, ServingConnections: config.Storage.ServingConnections, BackgroundConnections: config.Storage.BackgroundConnections}},
		LookupEnv:              lookupEnv,
		GitHubUserClientID:     lookupEnv("DETENT_HUB_GITHUB_APP_CLIENT_ID"),
		GitHubUserClientSecret: lookupEnv("DETENT_HUB_GITHUB_APP_CLIENT_SECRET"),
		GitHubWebhookSecret:    []byte(strings.TrimSpace(lookupEnv("DETENT_HUB_GITHUB_WEBHOOK_SECRET"))),
		Platform:               config.Platform,
		PublicURL:              config.PublicURL, Issuer: config.Assertion.Issuer, SigningKey: key, Provider: provider,
		StaffEmails: config.StaffEmails, SupportActors: config.SupportActors, EntitlementAdministrators: config.EntitlementAdministrators, StateDir: config.StateDirectory, ListenAddress: config.Listen, Logger: slog.Default(), ConfigPath: path,
	}
	appID, privateKey := lookupEnv("DETENT_HUB_GITHUB_APP_ID"), lookupEnv("DETENT_HUB_GITHUB_APP_PRIVATE_KEY")
	if appID != "" && privateKey != "" {
		result.GitHubApp = &connectorgithub.InstallationTokenConfig{AppID: appID, PrivateKey: privateKey, LookupEnv: lookupEnv}
	}
	result.ArchiveCredentials = func(_ context.Context, organization, reference string) (attachment.Credentials, error) {
		return attachment.CredentialsFromEnvironment(organization, reference, lookupEnv)
	}
	if config.Attachments != nil {
		config.Attachments.AccessKeyID = lookupEnv("DETENT_ATTACHMENTS_ACCESS_KEY_ID")
		config.Attachments.SecretAccessKey = lookupEnv("DETENT_ATTACHMENTS_SECRET_ACCESS_KEY")
		result.Attachments = config.Attachments
	}
	if allocation := config.Allocation; allocation != nil {
		if allocation.MaxConcurrentProvisions == 0 {
			allocation.MaxConcurrentProvisions = 1
		}
		if allocation.RetryLimit == 0 {
			allocation.RetryLimit = 5
		}
		if err := validateEntitlementAdministration(config, lookupEnv); err != nil {
			return cloudentry.Config{}, err
		}
		if allocation.Entitlements.IsZero() {
			allocation.Entitlements = nil
		}
		if err := validateTenantConfiguration(config, key, lookupEnv); err != nil {
			return cloudentry.Config{}, err
		}
		if allocation.TenantRoot == "" {
			allocation.TenantRoot = filepath.Join(result.StateDir, "tenants")
		}
		result.Allocation = &cloudentry.AllocationConfig{
			TenantRoot: allocation.TenantRoot, SocketRoot: allocation.SocketRoot, MaxConcurrent: allocation.MaxConcurrentProvisions,
			RetryLimit: allocation.RetryLimit, MinFreeDiskBytes: allocation.MinFreeDiskBytes, MinAvailableMemoryBytes: allocation.MinAvailableMemoryBytes,
			AllowedEmails: allocation.AllowedEmails, AllowedDomains: allocation.AllowedDomains,
		}
		if name := allocation.EntitlementAdminTokenEnv; name != "" {
			result.Allocation.EntitlementAdminToken = []byte(lookupEnv(name))
		}
		if tenantBilling := allocation.Billing; tenantBilling != nil {
			if !tenantBilling.CheckoutDisabled {
				result.Allocation.Prices = tenantBilling.Prices
			}
			if tenantBilling.CustomerID != "" || config.Billing == nil || tenantBilling.Mode != config.Billing.Mode || tenantBilling.AccountID != config.Billing.AccountID {
				return cloudentry.Config{}, errors.New("allocated tenant billing must match the entry billing mode and account and must not name a customer")
			}
			if _, err := readHostedBillingConfig(tenantBilling, lookupEnv); err != nil {
				return cloudentry.Config{}, err
			}
		}

	}
	if config.TenantPostgres != nil {
		hostConfig := config
		if hostConfig.Allocation == nil {
			hostConfig.Allocation = &cloudAllocationFileConfig{}
		}
		pg, err := hostConfig.TenantPostgres.runtime(lookupEnv)
		if err != nil {
			return cloudentry.Config{}, err
		}
		environment := tenantEnvironment(hostConfig, lookupEnv)
		tenantSecrets := make(map[string]string, len(environment))
		for _, pair := range environment {
			name, value, _ := strings.Cut(pair, "=")
			tenantSecrets[name] = value
		}
		tenantLookup := func(name string) string { return tenantSecrets[name] }
		result.SharedHub = &cloudentry.SharedHubConfig{Postgres: *pg, Configure: tenantConfiguration(config), TenantConfig: func(raw []byte, spec cloudentry.TenantSpec) (hubserver.Config, error) {
			billingLookup := tenantLookup
			if config.Allocation == nil {
				original, err := preservedTenantConfiguration(spec)
				if err != nil {
					return hubserver.Config{}, err
				}
				if b := original.Billing; b != nil {
					billingLookup = func(name string) string {
						if name == b.APIKeyEnv || name == b.WebhookSecretEnv {
							return lookupEnv(name)
						}
						return tenantLookup(name)
					}
				}
			}
			return sharedTenantConfig(raw, spec, tenantLookup, billingLookup)
		}}
	}
	if billingConfig := config.Billing; billingConfig != nil {
		if !validEnvName(billingConfig.APIKeyEnv) || !validEnvName(billingConfig.WebhookSecretEnv) {
			return cloudentry.Config{}, errors.New("shared billing secret environment variable names are invalid")
		}
		provider, err := billing.NewStripe(billing.StripeConfig{APIKey: lookupEnv(billingConfig.APIKeyEnv), Mode: billingConfig.Mode})
		if err != nil {
			return cloudentry.Config{}, err
		}
		customers, ok := provider.(billing.CustomerProvider)
		if !ok {
			return cloudentry.Config{}, errors.New("stripe provider cannot resolve customers")
		}
		secret := lookupEnv(billingConfig.WebhookSecretEnv)
		if len(secret) < 16 || !strings.HasPrefix(secret, "whsec_") {
			return cloudentry.Config{}, errors.New("shared billing webhook secret is unavailable or invalid")
		}
		result.Billing = &cloudentry.BillingConfig{Mode: billingConfig.Mode, AccountID: billingConfig.AccountID, WebhookSecret: []byte(secret), Provider: customers}
	}
	if config.Allocation != nil {
		initial := config
		result.ConfigureRuntime = func(update hostedplans.HostedRuntimeUpdate) func(cloudentry.TenantSpec) ([]byte, error) {
			next := initial
			allocation := *initial.Allocation
			allocation.Entitlements = update.Plans
			if allocation.Billing != nil {
				nextBilling := *allocation.Billing
				nextBilling.Prices = update.Prices
				allocation.Billing = &nextBilling
			}
			next.Allocation = &allocation
			return tenantConfiguration(next)
		}
		result.Runtime = &hostedplans.HostedRuntimeUpdate{Plans: config.Allocation.Entitlements}
		if config.Allocation.Billing != nil {
			result.Runtime.Prices = config.Allocation.Billing.Prices
		}
		restartAllocation := *config.Allocation
		restartAllocation.Entitlements = nil
		restartAllocation.AllowedEmails = nil
		restartAllocation.AllowedDomains = nil
		if restartAllocation.Billing != nil {
			restartBilling := *restartAllocation.Billing
			restartBilling.Prices = nil
			restartAllocation.Billing = &restartBilling
		}
		config.Allocation = &restartAllocation
	}
	result.RestartSettings, err = yaml.Marshal(config)
	if err != nil {
		return cloudentry.Config{}, err
	}
	result.Reload = func() (cloudentry.Config, error) { return readCloudConfig(path, lookupEnv) }
	if err := cloudentry.ValidateConfig(result); err != nil {
		return cloudentry.Config{}, err
	}
	return result, nil
}

func newCloudCommand(opts options) *cobra.Command {
	lookupEnv := opts.lookupEnv
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	cmd := &cobra.Command{
		Use:     "cloud",
		Short:   "Run the operator-hosted shared entry for Detent Cloud",
		Example: "detent cloud serve --entry-config /etc/detent/cloud.yaml",
		Long:    "Opt-in operator-hosted functionality: one shared public origin for sign-in, organization selection and authenticated routing to one shared Hub. Self-hosted Detent never needs it.",
		Args:    NoArgs,
	}
	cmd.AddCommand(newCloudServeCommand(lookupEnv, opts.build), newCloudAssertionKeyCommand(lookupEnv))
	return cmd
}

func newCloudServeCommand(lookupEnv func(string) string, build buildinfo.Info) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve the shared entry",
		Example:      "detent cloud serve --entry-config /etc/detent/cloud.yaml",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := OutputForCommand(cmd); err != nil {
				return err
			}
			logger, _, err := serveLogger(cmd, lookupEnv, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			slog.SetDefault(logger)
			config, err := readCloudConfig(configPath, lookupEnv)
			if err != nil {
				return err
			}
			config.Build = build
			return cloudentry.Run(cmd.Context(), config)
		},
	}
	cmd.Flags().StringVar(&configPath, "entry-config", "", "shared entry configuration file")
	if err := cmd.MarkFlagRequired("entry-config"); err != nil {
		panic(err)
	}
	return cmd
}

func newCloudAssertionKeyCommand(lookupEnv func(string) string) *cobra.Command {
	var keyEnv string
	cmd := &cobra.Command{
		Use:          "assertion-key",
		Example:      "detent cloud assertion-key --from-env DETENT_CLOUD_ASSERTION_KEY",
		Short:        "Generate an entry signing key, or print the public key for the configured one",
		Long:         "Without --from-env, prints a new base64 Ed25519 signing seed and its public key as JSON; store the seed in the entry's secret environment and list the public key in each tenant's shared_entry.public_keys. With --from-env, prints only the public key for the seed in that variable.",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keyEnv != "" {
				if !validEnvName(keyEnv) {
					return errors.New("invalid environment variable name")
				}
				key, err := cloudassert.ParsePrivateKey(lookupEnv(keyEnv))
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"public_key": cloudassert.PublicKeyOf(key)})
			}
			seed := make([]byte, ed25519.SeedSize)
			if _, err := rand.Read(seed); err != nil {
				return err
			}
			key := ed25519.NewKeyFromSeed(seed)
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"signing_key": base64.StdEncoding.EncodeToString(seed), "public_key": cloudassert.PublicKeyOf(key)})
		},
	}
	cmd.Flags().StringVar(&keyEnv, "from-env", "", "environment variable holding an existing signing seed")
	return cmd
}
