package hubconfig

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/usagecosts"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/update"
)

const (
	DefaultListenAddress              = "127.0.0.1:7777"
	DefaultWebhookPayloadRetention    = 7 * 24 * time.Hour
	DefaultWebhookMaintenanceInterval = time.Minute
	defaultBusyTimeout                = 5 * time.Second
	defaultShutdownTime               = 5 * time.Second
	defaultOutboxPoll                 = time.Second
	defaultOutboxBase                 = 2 * time.Second
	defaultOutboxMax                  = 15 * time.Minute
	defaultOutboxStale                = 5 * time.Minute
	defaultOutboxAttempts             = 8
	DefaultReconcileInterval          = 10 * time.Minute
	DefaultFullRepairInterval         = 24 * time.Hour
)

type DatabaseBackend string

const (
	DatabasePostgres DatabaseBackend = "postgres"
)

var ErrDatabaseBackend = errors.New("hub database backend configuration is invalid")
var ErrNetworkFilesystem = errors.New("hub database requires a local filesystem")

type Config struct {
	TenantRepository            tenantstore.Repository
	ChangeVersionAuthority      hubrecords.ChangeVersionAuthority
	ClaimBillingReconciliation  func(context.Context) (bool, error)
	RecordBillingSchedule       func(context.Context, int64) error
	BillingRepository           tenantstore.Repository
	AccessRepository            tenantstore.Repository
	ProjectParticipants         tenantstore.ProjectParticipants
	ConversationDispatch        tenantstore.DispatchRequests
	ConversationWorkerAuthority conversations.ConversationWorkerAuthority
	NativeRunEvents             hubrecords.NativeRunEventParticipant
	Clock                       func() time.Time
	ArchiveReader               entryauthority.ArchiveReader
	StaticFS                    fs.FS
	Content                     entryauthority.ContentHandler
	RunDetails                  attachment.DetailStore
	Icons                       attachment.IconStore
	RunnerReleaseClient         update.ReleaseClient
	SecretKeys                  *hubsecrets.Keyring
	SpritesHTTPClient           *http.Client
	SlackHTTPClient             *http.Client
	CredentialMaintenance       bool
	Hosted                      *hostedplans.HostedConfig
	Conversation                *conversations.ConversationConfig
	Usage                       *usagecosts.Prices
	// Workspace enables workspace sessions and the relay.
	// Nil means the surfaces stay disabled with their reason.
	Workspace             *workspacehub.Config
	GitHubRequestCounts   func() []githubcontract.GitHubRequestCount
	GitHubAppInstallation func(context.Context, string) (githubcontract.GitHubAppInstallation, error)

	ImportBackend              githubcontract.ImportBackend
	DatabaseBackend            DatabaseBackend
	Postgres                   *postgres.Config
	ListenAddress              string
	TLSCertFile                string
	TLSKeyFile                 string
	TrustedProxy               bool
	InitialAdminToken          []byte
	BusyTimeout                time.Duration
	ShutdownTimeout            time.Duration
	GitHubWebhookSecret        []byte
	WebhookPayloadRetention    time.Duration
	WebhookMaintenanceInterval time.Duration
	Logger                     *slog.Logger
	Version                    string
	Commit                     string
	OutboxBackend              githubcontract.OutboxBackend
	OutboxPollInterval         time.Duration
	OutboxBaseBackoff          time.Duration
	OutboxMaxBackoff           time.Duration
	OutboxProcessingTimeout    time.Duration
	OutboxMaxAttempts          int
	ReconcileBackend           githubcontract.ReconcileBackend
	ReconcileInterval          time.Duration
	FullRepairInterval         time.Duration

	hostedBindingMigration     bool
	validateDatabaseFilesystem func(string) error
	deps                       hubdeps.Deps
}

func (c *Config) Deps() *hubdeps.Deps {
	return &c.deps
}

func (c Config) Normalized() Config {
	c.deps = c.deps.WithDefaults()
	if c.RunDetails == nil && c.Hosted != nil && c.Hosted.SharedEntry != nil {
		c.RunDetails = c.Hosted.SharedEntry.RunDetails
	}
	if c.DatabaseBackend == DatabasePostgres && c.RunDetails != nil {
		c.RunDetails = hubmaintenance.LimitDetails(c.RunDetails)
	}
	if c.RunnerReleaseClient == nil {
		c.RunnerReleaseClient = update.NewGitHubClient(update.GitHubClientConfig{})
	}
	if c.Usage != nil {
		normalized := c.Usage.Normalized()
		c.Usage = &normalized
	}
	if c.Workspace != nil {
		normalized := c.Workspace.Normalized()
		c.Workspace = &normalized
	}
	if c.CredentialMaintenance {
		c.ImportBackend, c.OutboxBackend, c.ReconcileBackend = nil, nil, nil
		c.GitHubWebhookSecret, c.GitHubRequestCounts = nil, nil
		c.GitHubAppInstallation = nil
	}
	if c.ListenAddress == "" {
		c.ListenAddress = DefaultListenAddress
	}
	if c.BusyTimeout <= 0 {
		c.BusyTimeout = defaultBusyTimeout
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTime
	}
	if c.WebhookPayloadRetention <= 0 {
		c.WebhookPayloadRetention = DefaultWebhookPayloadRetention
	}
	if c.WebhookMaintenanceInterval <= 0 {
		c.WebhookMaintenanceInterval = DefaultWebhookMaintenanceInterval
	}
	if c.OutboxPollInterval <= 0 {
		c.OutboxPollInterval = defaultOutboxPoll
	}
	if c.OutboxBaseBackoff <= 0 {
		c.OutboxBaseBackoff = defaultOutboxBase
	}
	if c.OutboxMaxBackoff <= 0 {
		c.OutboxMaxBackoff = defaultOutboxMax
	}
	if c.OutboxMaxBackoff < c.OutboxBaseBackoff {
		c.OutboxMaxBackoff = c.OutboxBaseBackoff
	}
	if c.OutboxProcessingTimeout <= 0 {
		c.OutboxProcessingTimeout = defaultOutboxStale
	}
	if c.OutboxMaxAttempts <= 0 {
		c.OutboxMaxAttempts = defaultOutboxAttempts
	}
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = DefaultReconcileInterval
	}
	if c.FullRepairInterval <= 0 {
		c.FullRepairInterval = DefaultFullRepairInterval
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.validateDatabaseFilesystem == nil {
		c.validateDatabaseFilesystem = validateLocalDatabaseFilesystem
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.TenantRepository != nil && c.ConversationWorkerAuthority == nil {
		c.ConversationWorkerAuthority = conversations.NewTenantWorkerAuthority(c.Clock)
	}
	c.GitHubWebhookSecret = append([]byte(nil), c.GitHubWebhookSecret...)
	c.InitialAdminToken = append([]byte(nil), c.InitialAdminToken...)
	return c
}

func (cfg Config) ValidateDatabaseBackend() error {
	return cfg.ValidateServingBackend(cfg.Postgres != nil)
}

func (cfg Config) ValidateServingBackend(hasRepository bool) error {
	if cfg.DatabaseBackend != DatabasePostgres || !hasRepository || cfg.hostedBindingMigration {
		return ErrDatabaseBackend
	}
	return nil
}
