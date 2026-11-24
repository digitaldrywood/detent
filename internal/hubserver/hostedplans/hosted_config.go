package hostedplans

import (
	"crypto/ed25519"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

type HostedDestination struct {
	OrganizationID       string `yaml:"organization_id"`
	WorkOSOrganizationID string `yaml:"workos_organization_id"`
	PublicURL            string `yaml:"public_url"`
}

type HostedConfig struct {
	EmailSender              auth.EmailSender
	Billing                  *HostedBillingConfig
	EntitlementAdministrator string
	EntitlementAdminToken    []byte
	Plans                    *HostedPlansConfig
	OrganizationID           string
	WorkOSOrganizationID     string
	BootstrapSubject         string
	PublicURL                string
	StaffEmails              []string
	SupportActors            []string
	Directory                []HostedDestination
	SharedEntry              *HostedSharedEntry
	Provider                 auth.HostedProvider
	PlanID                   string
	StorageQuotaBytes        int64
	EventQuota               int64
	Postgres                 *postgres.Config
}

type HostedSharedEntry struct {
	Issuer          string
	PublicKeys      []ed25519.PublicKey
	Generation      int64
	ResolveKey      entryauthority.Resolver
	ArchiveContents attachment.ArchiveContentStore
	Content         entryauthority.ContentHandler
	RunDetails      attachment.DetailStore
	Icons           attachment.IconStore
	ReadArchive     entryauthority.ArchiveReader

	Replays  *cloudassert.ReplayCache
	verifier *cloudassert.Verifier
}

func (e *HostedSharedEntry) Validate(organization string) error {
	if e == nil {
		return nil
	}
	if !SafeID(e.Issuer) || e.Generation < 1 || len(e.PublicKeys) == 0 || len(e.PublicKeys) > 4 {
		return errors.New("shared entry requires an issuer, allocation generation and one to four public keys")
	}
	for _, key := range e.PublicKeys {
		if len(key) != ed25519.PublicKeySize {
			return errors.New("shared entry public key is invalid")
		}
	}
	e.verifier = &cloudassert.Verifier{Replays: e.Replays, PublicKeys: e.PublicKeys, Issuer: e.Issuer, Audience: organization, Generation: e.Generation}
	return nil
}

func (e *HostedSharedEntry) Verifier() *cloudassert.Verifier {
	return e.verifier
}

func ValidateHostedConfig(c *HostedConfig) error {
	return c.validate()
}

func (c *HostedConfig) validate() error {
	if c == nil {
		return nil
	}
	if c.Plans.IsZero() {
		c.Plans = nil
	}
	if len(c.EntitlementAdminToken) > 0 && (len(c.EntitlementAdminToken) < 32 || !SafeID(c.EntitlementAdministrator)) || len(c.EntitlementAdminToken) == 0 && c.EntitlementAdministrator != "" {
		return errors.New("entitlement administration requires an actor ID and a separate token of at least 32 bytes")
	}
	if !strings.HasPrefix(c.OrganizationID, "org_") || len(c.OrganizationID) > 64 || !SafeID(c.OrganizationID) || c.Provider == nil {
		return errors.New("hosted organization identity and identity provider are required")
	}
	if c.WorkOSOrganizationID == "" && c.BootstrapSubject == "" {
		return errors.New("unallocated hosted Hub requires a bootstrap user ID")
	}
	if !hostedPublicURL(c.PublicURL) {
		return errors.New("hosted public URL must use HTTPS or loopback HTTP without a path, query or fragment")
	}
	if c.PlanID != "" && !SafeID(c.PlanID) || c.StorageQuotaBytes < 0 || c.EventQuota < 0 {
		return errors.New("hosted plan metadata is invalid")
	}
	if c.SharedEntry != nil && len(c.Directory) != 0 {
		return errors.New("shared-entry tenants use the shared organization chooser instead of a directory")
	}
	if err := c.SharedEntry.Validate(c.OrganizationID); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, destination := range c.Directory {
		if !SafeID(destination.OrganizationID) || !SafeID(destination.WorkOSOrganizationID) || !hostedPublicURL(destination.PublicURL) || seen[destination.OrganizationID] {
			return errors.New("hosted organization directory is invalid")
		}
		seen[destination.OrganizationID] = true
	}
	for _, actor := range c.SupportActors {
		if !EmailListed(c.StaffEmails, actor) {
			return errors.New("hosted support actors must also be declared staff identities")
		}
	}
	if c.Plans != nil {
		if c.PlanID != "" || c.StorageQuotaBytes != 0 || c.EventQuota != 0 {
			return errors.New("use entitlements instead of legacy plan metadata fields")
		}
		if err := c.Plans.Validate(); err != nil {
			return err
		}
	}
	plans := c.Plans
	if plans == nil {
		defaults := DefaultPlans(PlansInput(c))
		plans = &defaults
	}
	return c.Billing.Validate(plans)
}

func (c *HostedConfig) BindsSharedOrganization(id string) bool {
	return c != nil && c.SharedEntry != nil && id != "" && c.OrganizationID == id
}

func (c *HostedConfig) Deployment() (string, int64) {
	if c.SharedEntry != nil {
		return "shared", c.SharedEntry.Generation
	}
	return "origin", 0
}

func PlansInput(cfg *HostedConfig) *Hosted {
	if cfg == nil {
		return nil
	}
	return &Hosted{Billing: cfg.Billing, Plans: cfg.Plans, PlanID: cfg.PlanID, StorageQuotaBytes: cfg.StorageQuotaBytes, EventQuota: cfg.EventQuota}
}

func hostedPublicURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u != nil && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && (u.Scheme == "https" || u.Scheme == "http" && hubapi.ListenerAddressLoopback(u.Host))
}

func EmailListed(emails []string, value string) bool {
	return slices.ContainsFunc(emails, func(email string) bool { return strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(value)) })
}
