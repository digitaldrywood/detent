package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type cloudFileConfig struct {
	PublicURL      string   `yaml:"public_url"`
	Listen         string   `yaml:"listen"`
	StateDirectory string   `yaml:"state_directory"`
	StaffEmails    []string `yaml:"staff_emails"`
	SupportActors  []string `yaml:"support_actors"`
	Assertion      struct {
		Issuer        string `yaml:"issuer"`
		SigningKeyEnv string `yaml:"signing_key_env"`
	} `yaml:"assertion"`
	WorkOS struct {
		ClientID  string `yaml:"client_id"`
		APIKeyEnv string `yaml:"api_key_env"`
		APIURL    string `yaml:"api_url"`
		IssuerURL string `yaml:"issuer_url"`
	} `yaml:"workos"`
	Allocation *cloudAllocationFileConfig `yaml:"allocation"`
}

type cloudAllocationFileConfig struct {
	TenantRoot              string                       `yaml:"tenant_root"`
	SocketRoot              string                       `yaml:"socket_root"`
	Binary                  string                       `yaml:"binary"`
	MaxTenants              int                          `yaml:"max_tenants"`
	MaxConcurrentProvisions int                          `yaml:"max_concurrent_provisions"`
	MaxPerIdentity          int                          `yaml:"max_organizations_per_identity"`
	RetryLimit              int                          `yaml:"retry_limit"`
	MinFreeDiskBytes        uint64                       `yaml:"min_free_disk_bytes"`
	MinAvailableMemoryBytes uint64                       `yaml:"min_available_memory_bytes"`
	AllowedEmails           []string                     `yaml:"allowed_emails"`
	AllowedDomains          []string                     `yaml:"allowed_domains"`
	Entitlements            *hubserver.HostedPlansConfig `yaml:"entitlements"`
}

func tenantConfiguration(config cloudFileConfig) func(cloudentry.TenantSpec) ([]byte, error) {
	return func(spec cloudentry.TenantSpec) ([]byte, error) {
		tenant := hostedFileConfig{
			OrganizationID: spec.Organization.ID, WorkOSOrganizationID: spec.Organization.ProviderID, PublicURL: spec.PublicURL,
			StaffEmails: config.StaffEmails, SupportActors: config.SupportActors, Plans: config.Allocation.Entitlements,
			SharedEntry: &hostedSharedEntryFileConfig{Issuer: spec.Issuer, PublicKeys: []string{spec.PublicKey}, AllocationGeneration: spec.Organization.Generation},
		}
		tenant.WorkOS.ClientID, tenant.WorkOS.APIKeyEnv, tenant.WorkOS.APIURL, tenant.WorkOS.IssuerURL = config.WorkOS.ClientID, config.WorkOS.APIKeyEnv, config.WorkOS.APIURL, config.WorkOS.IssuerURL
		return yaml.Marshal(tenant)
	}
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
	result := cloudentry.Config{
		PublicURL: config.PublicURL, Issuer: config.Assertion.Issuer, SigningKey: key, Provider: provider,
		StaffEmails: config.StaffEmails, SupportActors: config.SupportActors, StateDir: config.StateDirectory, ListenAddress: config.Listen, Logger: slog.Default(),
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
		result.Allocation = &cloudentry.AllocationConfig{
			TenantRoot: allocation.TenantRoot, SocketRoot: allocation.SocketRoot, MaxTenants: allocation.MaxTenants, MaxConcurrent: allocation.MaxConcurrentProvisions,
			MaxPerIdentity: allocation.MaxPerIdentity, RetryLimit: allocation.RetryLimit, MinFreeDiskBytes: allocation.MinFreeDiskBytes, MinAvailableMemoryBytes: allocation.MinAvailableMemoryBytes,
			AllowedEmails: allocation.AllowedEmails, AllowedDomains: allocation.AllowedDomains,
			Launcher: &cloudentry.ExecLauncher{Binary: binary, Environment: []string{config.WorkOS.APIKeyEnv + "=" + lookupEnv(config.WorkOS.APIKeyEnv)}, Configure: tenantConfiguration(config), Logger: slog.Default()},
		}
	}
	return result, nil
}

func newCloudCommand(lookupEnv func(string) string) *cobra.Command {
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
	cmd.AddCommand(newCloudServeCommand(lookupEnv), newCloudRegistryCommand(), newCloudAssertionKeyCommand(lookupEnv))
	return cmd
}

func newCloudServeCommand(lookupEnv func(string) string) *cobra.Command {
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
			config, err := readCloudConfig(configPath, lookupEnv)
			if err != nil {
				return err
			}
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
