package cli

import (
	"errors"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/profiling"
)

type hostedFileConfig struct {
	SMTP                     *hostedSMTPFileConfig         `yaml:"smtp"`
	Profiling                profiling.Config              `yaml:"profiling,omitempty"`
	Billing                  *hostedBillingFileConfig      `yaml:"billing"`
	EntitlementAdministrator string                        `yaml:"entitlement_administrator"`
	EntitlementAdminTokenEnv string                        `yaml:"entitlement_admin_token_env"`
	Plans                    *hubserver.HostedPlansConfig  `yaml:"entitlements"`
	OrganizationID           string                        `yaml:"organization_id"`
	WorkOSOrganizationID     string                        `yaml:"workos_organization_id"`
	BootstrapSubject         string                        `yaml:"bootstrap_subject"`
	PublicURL                string                        `yaml:"public_url"`
	StaffEmails              []string                      `yaml:"staff_emails"`
	SupportActors            []string                      `yaml:"support_actors"`
	PlanID                   string                        `yaml:"plan_id"`
	StorageQuotaBytes        int64                         `yaml:"storage_quota_bytes"`
	EventQuota               int64                         `yaml:"event_quota"`
	Directory                []hubserver.HostedDestination `yaml:"directory"`
	SharedEntry              *hostedSharedEntryFileConfig  `yaml:"shared_entry"`
	Conversation             *hostedConversationFileConfig `yaml:"conversation"`
	Usage                    *hostedUsageFileConfig        `yaml:"usage"`
	Workspaces               *hostedWorkspaceFileConfig    `yaml:"workspaces"`
	WorkOS                   struct {
		ClientID  string `yaml:"client_id"`
		APIKeyEnv string `yaml:"api_key_env"`
		APIURL    string `yaml:"api_url"`
		IssuerURL string `yaml:"issuer_url"`
	} `yaml:"workos"`
}

type hostedSMTPFileConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"password_env"`
	From        string `yaml:"from"`
}

func readHostedSMTPSender(config *hostedSMTPFileConfig, lookupEnv func(string) string) (auth.EmailSender, error) {
	if config == nil {
		return nil, errors.New("SMTP is not configured")
	}
	var password string
	if config.PasswordEnv != "" {
		if !validEnvName(config.PasswordEnv) {
			return nil, errors.New("SMTP password environment variable name is invalid")
		}
		password = lookupEnv(config.PasswordEnv)
		if password == "" {
			return nil, errors.New("SMTP password is unavailable")
		}
	}
	port := config.Port
	if port == 0 {
		port = 587
	}
	return auth.NewSMTPSender(auth.SMTPConfig{Host: config.Host, Port: port, Username: config.Username, Password: password, From: config.From})
}

type hostedSharedEntryFileConfig struct {
	Issuer               string   `yaml:"issuer"`
	PublicKeys           []string `yaml:"public_keys"`
	AllocationGeneration int64    `yaml:"allocation_generation"`
}

func readHostedSharedEntry(config *hostedSharedEntryFileConfig) (*hubserver.HostedSharedEntry, error) {
	entry := &hubserver.HostedSharedEntry{Issuer: config.Issuer, Generation: config.AllocationGeneration}
	for _, value := range config.PublicKeys {
		key, err := cloudassert.ParsePublicKey(value)
		if err != nil {
			return nil, err
		}
		entry.PublicKeys = append(entry.PublicKeys, key)
	}
	return entry, nil
}

type hostedBillingFileConfig struct {
	Mode                  string                         `yaml:"mode,omitempty"`
	CheckoutDisabled      bool                           `yaml:"checkout_disabled,omitempty"`
	AccountID             string                         `yaml:"account_id"`
	CustomerID            string                         `yaml:"customer_id"`
	PortalConfigurationID string                         `yaml:"portal_configuration_id"`
	APIKeyEnv             string                         `yaml:"api_key_env"`
	WebhookSecretEnv      string                         `yaml:"webhook_secret_env"`
	GraceSeconds          int64                          `yaml:"grace_seconds"`
	ReconcileSeconds      int64                          `yaml:"reconcile_seconds"`
	CreditCostMultiplier  float64                        `yaml:"credit_cost_multiplier,omitempty"`
	CreditPacks           []hubserver.HostedCreditPack   `yaml:"credit_packs"`
	Prices                []hubserver.HostedBillingPrice `yaml:"prices"`
}

func readHostedBillingConfig(config *hostedBillingFileConfig, lookupEnv func(string) string) (*hubserver.HostedBillingConfig, error) {
	if config == nil {
		return nil, errors.New("billing configuration is required")
	}
	if !validEnvName(config.APIKeyEnv) || !validEnvName(config.WebhookSecretEnv) {
		return nil, errors.New("billing secret environment variable names are required and must be valid")
	}
	provider, err := billing.NewStripe(billing.StripeConfig{APIKey: lookupEnv(config.APIKeyEnv), Mode: config.Mode})
	if err != nil {
		return nil, err
	}
	secret := lookupEnv(config.WebhookSecretEnv)
	if len(secret) < 16 || !strings.HasPrefix(secret, "whsec_") {
		return nil, errors.New("billing webhook secret is unavailable or invalid")
	}
	return &hubserver.HostedBillingConfig{
		Mode: config.Mode, CheckoutDisabled: config.CheckoutDisabled, AccountID: config.AccountID, CustomerID: config.CustomerID, PortalConfigurationID: config.PortalConfigurationID,
		WebhookSecret: []byte(secret), GraceSeconds: config.GraceSeconds, ReconcileSeconds: config.ReconcileSeconds,
		Prices: config.Prices, CreditPacks: config.CreditPacks, CreditCostMultiplier: config.CreditCostMultiplier, Provider: provider,
	}, nil
}

func readHostedConfig(path string, lookupEnv func(string) string) (result *hubserver.HostedConfig, enabled bool, resultErr error) {
	if strings.TrimSpace(path) == "" {
		return nil, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, errors.New("hosted configuration could not be opened")
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("hosted configuration could not be closed"))
		}
	}()
	result, err = parseHostedConfig(file, lookupEnv)
	if err != nil {
		return nil, false, err
	}
	return result, true, nil
}

func parseHostedConfig(reader io.Reader, lookupEnv func(string) string) (*hubserver.HostedConfig, error) {
	decoder := yaml.NewDecoder(io.LimitReader(reader, 128*1024))
	decoder.KnownFields(true)
	var config hostedFileConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, errors.New("hosted configuration is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("hosted configuration must contain one document")
	}
	if config.WorkOS.APIKeyEnv == "" {
		config.WorkOS.APIKeyEnv = "WORKOS_API_KEY"
	}
	if !validEnvName(config.WorkOS.APIKeyEnv) {
		return nil, errors.New("hosted API key environment variable name is invalid")
	}
	provider, err := auth.NewHostedProvider(auth.IdentityProviderWorkOS, auth.WorkOSConfig{
		APIURL: config.WorkOS.APIURL, IssuerURL: config.WorkOS.IssuerURL, ClientID: config.WorkOS.ClientID,
		APIKey: lookupEnv(config.WorkOS.APIKeyEnv), RedirectURL: config.PublicURL + "/auth/oidc/callback",
	})
	if err != nil {
		return nil, err
	}
	var entitlementToken []byte
	if config.EntitlementAdminTokenEnv != "" {
		if !validEnvName(config.EntitlementAdminTokenEnv) {
			return nil, errors.New("entitlement token environment name is invalid")
		}
		entitlementToken = []byte(lookupEnv(config.EntitlementAdminTokenEnv))
		if len(entitlementToken) < 32 {
			return nil, errors.New("entitlement administration token is unavailable or too short")
		}
	}
	var sharedEntry *hubserver.HostedSharedEntry
	if config.SharedEntry != nil {
		if sharedEntry, err = readHostedSharedEntry(config.SharedEntry); err != nil {
			return nil, err
		}
	}
	var billingConfig *hubserver.HostedBillingConfig
	if config.Billing != nil {
		billingConfig, err = readHostedBillingConfig(config.Billing, lookupEnv)
		if err != nil {
			return nil, err
		}
	}
	var sender auth.EmailSender
	if config.SMTP != nil {
		sender, err = readHostedSMTPSender(config.SMTP, lookupEnv)
		if err != nil {
			return nil, err
		}
	}
	return &hubserver.HostedConfig{
		EmailSender:              sender,
		Billing:                  billingConfig,
		EntitlementAdministrator: config.EntitlementAdministrator, EntitlementAdminToken: entitlementToken,
		Plans:          config.Plans,
		OrganizationID: config.OrganizationID, WorkOSOrganizationID: config.WorkOSOrganizationID,
		BootstrapSubject: config.BootstrapSubject, PublicURL: config.PublicURL,
		StaffEmails: config.StaffEmails, SupportActors: config.SupportActors, Directory: config.Directory, SharedEntry: sharedEntry, Provider: provider,
		PlanID: config.PlanID, StorageQuotaBytes: config.StorageQuotaBytes, EventQuota: config.EventQuota,
	}, nil
}
