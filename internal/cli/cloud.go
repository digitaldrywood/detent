package cli

import (
	"bytes"
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

	"github.com/digitaldrywood/detent/internal/buildinfo"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type cloudFileConfig struct {
	PublicURL                 string   `yaml:"public_url"`
	Listen                    string   `yaml:"listen"`
	StateDirectory            string   `yaml:"state_directory"`
	StaffEmails               []string `yaml:"staff_emails"`
	SupportActors             []string `yaml:"support_actors"`
	EntitlementAdministrators []string `yaml:"entitlement_administrators"`
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
	TenantRoot               string                       `yaml:"tenant_root"`
	SocketRoot               string                       `yaml:"socket_root"`
	Binary                   string                       `yaml:"binary"`
	MaxTenants               int                          `yaml:"max_tenants"`
	MaxConcurrentProvisions  int                          `yaml:"max_concurrent_provisions"`
	MaxPerIdentity           int                          `yaml:"max_organizations_per_identity"`
	RetryLimit               int                          `yaml:"retry_limit"`
	MinFreeDiskBytes         uint64                       `yaml:"min_free_disk_bytes"`
	MinAvailableMemoryBytes  uint64                       `yaml:"min_available_memory_bytes"`
	AllowedEmails            []string                     `yaml:"allowed_emails"`
	AllowedDomains           []string                     `yaml:"allowed_domains"`
	Entitlements             *hubserver.HostedPlansConfig `yaml:"entitlements"`
	Billing                  *hostedBillingFileConfig     `yaml:"billing"`
	EntitlementAdministrator string                       `yaml:"entitlement_administrator"`
	EntitlementAdminTokenEnv string                       `yaml:"entitlement_admin_token_env"`
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
		tenant := hostedFileConfig{
			OrganizationID: spec.Organization.ID, WorkOSOrganizationID: spec.Organization.ProviderID, PublicURL: spec.PublicURL,
			StaffEmails: config.StaffEmails, SupportActors: config.SupportActors, Plans: config.Allocation.Entitlements, Billing: config.Allocation.Billing,
			Conversation:             &hostedConversationFileConfig{Enabled: true},
			Workspaces:               preserved.Workspaces,
			EntitlementAdministrator: config.Allocation.EntitlementAdministrator, EntitlementAdminTokenEnv: config.Allocation.EntitlementAdminTokenEnv,
			SharedEntry: &hostedSharedEntryFileConfig{Issuer: spec.Issuer, PublicKeys: []string{spec.PublicKey}, AllocationGeneration: spec.Organization.Generation},
		}
		tenant.WorkOS.ClientID, tenant.WorkOS.APIKeyEnv, tenant.WorkOS.APIURL, tenant.WorkOS.IssuerURL = config.WorkOS.ClientID, config.WorkOS.APIKeyEnv, config.WorkOS.APIURL, config.WorkOS.IssuerURL
		return yaml.Marshal(tenant)
	}
}

func preservedTenantConfiguration(spec cloudentry.TenantSpec) (hostedFileConfig, error) {
	if spec.Directory == "" {
		return hostedFileConfig{}, nil
	}
	file, err := os.Open(filepath.Join(spec.Directory, "tenant.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return hostedFileConfig{}, nil
	}
	if err != nil {
		return hostedFileConfig{}, errors.New("tenant workspace policy could not be opened")
	}
	decoder := yaml.NewDecoder(io.LimitReader(file, 128*1024))
	decoder.KnownFields(true)
	var tenant hostedFileConfig
	decodeErr := decoder.Decode(&tenant)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return hostedFileConfig{}, errors.New("tenant workspace policy could not be read")
	}
	if tenant.OrganizationID != spec.Organization.ID || tenant.WorkOSOrganizationID != spec.Organization.ProviderID {
		return hostedFileConfig{}, errors.New("tenant workspace policy belongs to another organization")
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
		err = hubserver.ValidateHostedConfig(tenant)
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

func validateEntitlementAdministrators(config cloudFileConfig) error {
	for _, email := range config.EntitlementAdministrators {
		if !slices.ContainsFunc(config.StaffEmails, func(staff string) bool { return strings.EqualFold(strings.TrimSpace(staff), strings.TrimSpace(email)) }) {
			return fmt.Errorf("entitlement_administrators entry %q is not listed in staff_emails; every entitlement administrator must be staff", email)
		}
	}
	if len(config.EntitlementAdministrators) > 0 && (config.Allocation == nil || config.Allocation.EntitlementAdminTokenEnv == "") {
		return errors.New("entitlement_administrators requires allocation.entitlement_admin_token_env so the entry can reach tenant entitlements")
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
	if config.WorkOS.APIKeyEnv == "" {
		config.WorkOS.APIKeyEnv = "WORKOS_API_KEY"
	}
	if config.Assertion.SigningKeyEnv == "" {
		config.Assertion.SigningKeyEnv = "DETENT_CLOUD_ASSERTION_KEY"
	}
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8017"
	}
	if err := validateEntitlementAdministrators(config); err != nil {
		return cloudentry.Config{}, err
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
	result := cloudentry.Config{
		GitHubWebhookSecret: []byte(strings.TrimSpace(lookupEnv("DETENT_HUB_GITHUB_WEBHOOK_SECRET"))),
		PublicURL:           config.PublicURL, Issuer: config.Assertion.Issuer, SigningKey: key, Provider: provider,
		StaffEmails: config.StaffEmails, SupportActors: config.SupportActors, EntitlementAdministrators: config.EntitlementAdministrators, StateDir: config.StateDirectory, ListenAddress: config.Listen, Logger: slog.Default(), ConfigPath: path,
	}
	if config.Attachments != nil {
		config.Attachments.AccessKeyID = lookupEnv("DETENT_ATTACHMENTS_ACCESS_KEY_ID")
		config.Attachments.SecretAccessKey = lookupEnv("DETENT_ATTACHMENTS_SECRET_ACCESS_KEY")
		result.Attachments = config.Attachments
	}
	if allocation := config.Allocation; allocation != nil {
		binary := allocation.Binary
		if binary == "" {
			if binary, err = os.Executable(); err != nil {
				return cloudentry.Config{}, err
			}
		}
		if allocation.MaxConcurrentProvisions == 0 {
			allocation.MaxConcurrentProvisions = 1
		}
		if allocation.MaxPerIdentity == 0 {
			allocation.MaxPerIdentity = 1
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
		result.Allocation = &cloudentry.AllocationConfig{
			TenantRoot: allocation.TenantRoot, SocketRoot: allocation.SocketRoot, MaxTenants: allocation.MaxTenants, MaxConcurrent: allocation.MaxConcurrentProvisions,
			MaxPerIdentity: allocation.MaxPerIdentity, RetryLimit: allocation.RetryLimit, MinFreeDiskBytes: allocation.MinFreeDiskBytes, MinAvailableMemoryBytes: allocation.MinAvailableMemoryBytes,
			AllowedEmails: allocation.AllowedEmails, AllowedDomains: allocation.AllowedDomains,
			Launcher: &cloudentry.ExecLauncher{Binary: binary, Environment: tenantEnvironment(config, lookupEnv), Configure: tenantConfiguration(config), Logger: slog.Default(), RestartLimit: allocation.RetryLimit},
		}
		if name := allocation.EntitlementAdminTokenEnv; name != "" {
			result.Allocation.EntitlementAdminToken = []byte(lookupEnv(name))
		}
		if tenantBilling := allocation.Billing; tenantBilling != nil {
			if tenantBilling.CustomerID != "" || config.Billing == nil || tenantBilling.Mode != config.Billing.Mode || tenantBilling.AccountID != config.Billing.AccountID {
				return cloudentry.Config{}, errors.New("allocated tenant billing must match the entry billing mode and account and must not name a customer")
			}
			if _, err := readHostedBillingConfig(tenantBilling, lookupEnv); err != nil {
				return cloudentry.Config{}, err
			}
		}
		if err := validateTenantConfiguration(config, key, lookupEnv); err != nil {
			return cloudentry.Config{}, err
		}
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
		Long:    "Opt-in operator-hosted functionality: one shared public origin for sign-in, organization selection and authenticated routing to dedicated tenant Hubs. Self-hosted Detent never needs it.",
		Args:    NoArgs,
	}
	cmd.AddCommand(newCloudServeCommand(lookupEnv, opts.build), newCloudRegistryCommand(), newCloudAssertionKeyCommand(lookupEnv))
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
			logger, level, err := serveLogger(cmd, lookupEnv, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			slog.SetDefault(logger)
			// Tenant Hubs run at the level this process resolved, not at
			// whatever LOG_LEVEL the environment carried under a --log-level
			// override.
			tenantLookup := func(name string) string {
				if name == "LOG_LEVEL" {
					return level
				}
				return lookupEnv(name)
			}
			config, err := readCloudConfig(configPath, tenantLookup)
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

func newCloudRegistryCommand() *cobra.Command {
	var registryPath string
	var organization cloudentry.Organization
	var disabled bool
	cmd := &cobra.Command{
		Use:     "registry",
		Short:   "Administer the stopped shared entry's organization registry",
		Example: "detent cloud registry list --registry /var/lib/detent/cloud/registry.db",
		Args:    NoArgs,
	}
	cmd.PersistentFlags().StringVar(&registryPath, "registry", "", "registry database (STATE_DIRECTORY/registry.db); the shared entry must be stopped")
	add := &cobra.Command{
		Use:          "register",
		Short:        "Register or update an organization allocation idempotently",
		Example:      "detent cloud registry register --registry /var/lib/detent/cloud/registry.db --organization org_example --provider-organization org_workos --name Example --endpoint unix:/run/detent/tenants/org_example.sock --generation 1",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			registry, err := cloudentry.OpenRegistry(cmd.Context(), registryPath)
			if err != nil {
				return err
			}
			if disabled {
				organization.State = "disabled"
			}
			changed, registerErr := registry.Register(cmd.Context(), organization)
			if err := errors.Join(registerErr, registry.Close()); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"organization": organization.ID, "changed": changed})
		},
	}
	add.Flags().StringVar(&organization.ID, "organization", "", "immutable Detent organization ID (org_...)")
	add.Flags().StringVar(&organization.ProviderID, "provider-organization", "", "WorkOS organization ID")
	add.Flags().StringVar(&organization.Name, "name", "", "organization name shown in the chooser")
	add.Flags().StringVar(&organization.Endpoint, "endpoint", "", "private tenant endpoint: unix:/absolute/socket in a directory private to the service user")
	add.Flags().Int64Var(&organization.Generation, "generation", 0, "allocation generation matching the tenant's shared_entry configuration")
	add.Flags().BoolVar(&disabled, "disabled", false, "register the organization without routing traffic to it")
	list := &cobra.Command{
		Use:          "list",
		Short:        "List registered organizations",
		Example:      "detent cloud registry list --registry /var/lib/detent/cloud/registry.db",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			registry, err := cloudentry.OpenRegistry(cmd.Context(), registryPath)
			if err != nil {
				return err
			}
			organizations, listErr := registry.List(cmd.Context())
			if err := errors.Join(listErr, registry.Close()); err != nil {
				return err
			}
			if organizations == nil {
				organizations = []cloudentry.Organization{}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(organizations)
		},
	}
	cmd.AddCommand(add, list)
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
