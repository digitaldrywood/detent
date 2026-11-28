package hubserver

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/activation"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/releasenotes"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPIdleTimeout       = 2 * time.Minute
)

type Service struct {
	archiveEncoder          attachment.ArchiveEncoder
	changeColumnAfter       any
	dashboard               *dashboardDelivery
	releaseNotes            []releasenotes.Build
	boardAccess             boardAccessCache
	writerStatsAt           atomic.Int64
	writerWaits             atomic.Int64
	writerWaited            atomic.Int64
	requestMetrics          *tenantRequestMetrics
	maintenance             *hubmaintenance.Scheduler
	runnerPublishedReleases sync.Map
	tokenUseWork            sync.WaitGroup
	mcpHTTP                 *mcp.HTTPHandler
	operatorChat            *chat.Service
	administration          *operatoradmin.Executor
	billing                 *hostedBillingWorker
	issueMailer             *issueMailer
	sourceVerification      chan struct{}
	runtimeMu               sync.Mutex
	hostedAuthLogger        *slog.Logger
	hostedSessions          *auth.Service
	echo                    *echo.Echo
	database                *database
	config                  Config
	outbox                  *outboxWorker
	ready                   atomic.Bool
	wakeSpriteRunnersAfter  func(context.Context, hubscope.NativeScope, json.RawMessage)
	startSpriteLifecycle    func(context.Context, hubscope.NativeScope, string)
	startSpritePoolForQueue func(context.Context, hubscope.NativeScope)
	spriteWakeMu            sync.Mutex
	spriteWakes             map[spriteWakeKey]*spriteLifecyclePass
	spriteWakeWork          sync.WaitGroup
	secretRewrapOnce        sync.Once
	secretRewrapReady       chan struct{}
	secretRewrapDone        chan struct{}
	workerCancel            context.CancelFunc
	workerDone              chan struct{}
	boardRefresh            chan struct{}
	cleanupPollCursors      sync.Map
	boardRefreshCursor      string
	boardRetentionScope     hubscope.NativeScope
	workerStopOnce          sync.Once
	pullRequests            *pullRequestCache
	notifications           *notificationBroker
	conversations           *conversationService
	closeOnce               sync.Once
	closeErr                error
	clientBuild             appClientBuild
	localOrganization       string
	workspaces              *workspaceService
}

type healthResponse struct {
	Status                   string                  `json:"status"`
	MigrationsRunning        bool                    `json:"migrations_running"`
	SchemaVersion            int64                   `json:"schema_version,omitempty"`
	SchemaFingerprint        string                  `json:"schema_fingerprint,omitempty"`
	HubSchemaApplicable      bool                    `json:"hub_schema_applicable"`
	RegistrySchemaVersion    int64                   `json:"registry_schema_version"`
	RegistrySchemaApplicable bool                    `json:"registry_schema_applicable"`
	AuthSchemaVersion        int64                   `json:"auth_schema_version"`
	AuthSchemaApplicable     bool                    `json:"auth_schema_applicable"`
	PlatformSchemaVersion    int64                   `json:"platform_schema_version"`
	PlatformSchemaApplicable bool                    `json:"platform_schema_applicable"`
	SchemaFactsComplete      bool                    `json:"schema_facts_complete"`
	Version                  string                  `json:"version,omitempty"`
	Commit                   string                  `json:"commit,omitempty"`
	Repositories             RepositoryHealthSummary `json:"repositories"`
}

func resolveLocalOrganization(ctx context.Context, repository *postgres.Repository, cfg Config) (tracker.OrganizationID, error) {
	organization, err := repository.DesignatedLocalOrganization(ctx)
	if errors.Is(err, corestore.ErrNotFound) {
		token := strings.TrimSpace(string(cfg.InitialAdminToken))
		if token == "" {
			return "", err
		}
		organization, err = bootstrapLocalOrganization(ctx, repository, cfg, token)
		if err != nil {
			return "", fmt.Errorf("bootstrap local organization: %w", err)
		}
	}
	if err != nil {
		return "", err
	}
	err = repository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		row, err := reader.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		if row.OrganizationID != organization || !row.Local {
			return corestore.ErrTenantMismatch
		}
		identity, err := reader.HubIdentity(ctx)
		if err != nil {
			return fmt.Errorf("read local Hub identity: %w", err)
		}
		if identity.OrganizationID != organization || identity.ID == "" || len(identity.CursorKey) != 32 {
			return corestore.ErrTenantMismatch
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return organization, nil
}

func bootstrapLocalOrganization(ctx context.Context, repository *postgres.Repository, cfg Config, token string) (tracker.OrganizationID, error) {
	organization := tracker.OrganizationID(hubapi.NewID("org"))
	now := cfg.Clock().UTC()
	hash := apikey.HashToken(token)
	cursorKey := make([]byte, 32)
	if _, err := rand.Read(cursorKey); err != nil {
		return "", fmt.Errorf("generate Hub identity: %w", err)
	}
	secret, audit, err := newNativeCursorSecret(cfg, organization)
	if err != nil {
		return "", err
	}
	return repository.BootstrapLocalOrganization(ctx,
		accessstore.Organization{OrganizationID: organization, Name: "Local organization", Slug: "local-organization", Local: true, CreatedAt: now},
		accessstore.Credential{
			OrganizationID: organization, ID: bootstrapTokenID, Name: "Bootstrap administrator",
			Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: string(hubapi.APIScopeAdmin),
			KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now,
		},
		accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: cursorKey},
		secret, audit,
	)
}

func Open(ctx context.Context, cfg Config) (*Service, error) {
	return openService(ctx, cfg, nil)
}

func openService(ctx context.Context, cfg Config, startup *startupHealth) (*Service, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.Normalized()
	if err := validateCredentialMaintenance(cfg); err != nil {
		return nil, err
	}
	if err := hostedplans.ValidateHostedConfig(cfg.Hosted); err != nil {
		return nil, err
	}
	notes, err := releasenotes.Read(detent.StaticFS())
	if err != nil {
		return nil, err
	}
	hostedAuthLogger := cfg.Logger
	if cfg.Hosted != nil {
		cfg.Logger = slog.New(hostedLogHandler{output: cfg.Logger.Handler()})
	}
	database, err := openDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if startup != nil {
		startup.schema.Store(&MigrationHealth{SchemaVersion: database.schemaVersion, SchemaFingerprint: database.schemaFingerprint})
	}
	database.now = cfg.Clock
	database.newLeaseID = cfg.Deps().NewLeaseID
	var localOrganization tracker.OrganizationID
	if database.postgres != nil {
		database.logger = cfg.Logger
		cfg.TenantRepository = database.postgres
		cfg.AccessRepository = database.postgres
		cfg.BillingRepository = database.postgres
		database.billingRepository = database.postgres
		if cfg.Hosted != nil {
			database.hostedOrganization = tracker.OrganizationID(cfg.Hosted.OrganizationID)
			database.linkedSourceBase = cfg.Hosted.PublicURL
			if cfg.Hosted.SharedEntry != nil {
				database.linkedSourceBase += "/organizations/" + cfg.Hosted.OrganizationID
			}
			database.runtimeBilling = cfg.Hosted.Billing
			if cfg.Hosted.EmailSender != nil {
				return nil, errors.Join(hubapi.ErrPostgresFeatureUnavailable, database.Close())
			}
		} else {
			localOrganization, err = resolveLocalOrganization(ctx, database.postgres, cfg)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("resolve local organization: %w", err), database.Close())
			}
			database.hostedOrganization = localOrganization
			if database.detailObjects = database.postgres.DetailStore(cfg.Clock); cfg.RunDetails == nil {
				cfg.RunDetails = database.detailObjects
			}
		}
		cfg = cfg.Normalized()
	}
	ctx = database.policyContext(ctx)
	if database.postgres != nil && cfg.Hosted != nil {
		if err := (&Service{config: cfg}).validateAccessHostedBinding(ctx); err != nil {
			return nil, errors.Join(err, database.Close())
		}
	}
	if err := (&Service{config: cfg, localOrganization: string(localOrganization)}).checkAccessSecretKeys(ctx); err != nil {
		return nil, errors.Join(err, database.Close())
	}
	if database.postgres != nil && cfg.Hosted != nil && !cfg.CredentialMaintenance {
		if err := hubapi.FromFeatureError(database.plansService().ConfigurePlans(ctx, hostedplans.PlansInput(cfg.Hosted))); err != nil {
			return nil, errors.Join(err, database.Close())
		}
		if err := hubapi.FromFeatureError(database.plansService().ConfigureBilling(ctx, hostedplans.PlansInput(cfg.Hosted))); err != nil {
			return nil, errors.Join(err, database.Close())
		}
	}
	cfg.InitialAdminToken = nil

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Server.ReadHeaderTimeout = defaultHTTPReadHeaderTimeout
	e.Server.IdleTimeout = defaultHTTPIdleTimeout
	workerContext, workerCancel := context.WithCancel(context.Background())
	service := &Service{
		releaseNotes:       notes,
		echo:               e,
		notifications:      newNotificationBroker(),
		hostedAuthLogger:   hostedAuthLogger,
		database:           database,
		sourceVerification: make(chan struct{}, 1),
		config:             cfg,
		localOrganization:  string(localOrganization),
		spriteWakes:        make(map[spriteWakeKey]*spriteLifecyclePass),
		workerCancel:       workerCancel,
		workerDone:         make(chan struct{}),
		secretRewrapReady:  make(chan struct{}),
		secretRewrapDone:   make(chan struct{}),
		boardRefresh:       make(chan struct{}, 1),
		clientBuild:        conversationClientIdentity(staticFiles(cfg.StaticFS), cfg.Version, cfg.Clock()),
		pullRequests:       newPullRequestCache(),
	}
	if database.postgres != nil && !cfg.CredentialMaintenance {
		service.maintenance, err = service.newReadModelMaintenance()
		if err != nil {
			workerCancel()
			return nil, errors.Join(err, database.Close())
		}
	}
	if service.config.NativeRunEvents == nil && service.config.TenantRepository != nil {
		service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	}
	if service.config.ConversationDispatch == nil && service.config.TenantRepository != nil {
		service.config.ConversationDispatch = tenantNativeRunEventParticipant{service: service}
	}
	if service.config.ProjectParticipants == nil && service.config.TenantRepository != nil && service.config.AccessRepository != nil {
		service.config.ProjectParticipants = tenantProjectParticipants{service: service}
	}
	service.configureSpriteLifecycle(workerContext)
	if database.postgres == nil || cfg.OutboxBackend != nil {
		service.outbox = newOutboxWorker(service)
	}
	if cfg.Hosted != nil {
		service.hostedSessions, err = auth.NewSessionService(auth.SessionConfig{SessionTTL: 30 * 24 * time.Hour, PublicURL: cfg.Hosted.PublicURL}, service)
		if err != nil {
			workerCancel()
			return nil, errors.Join(err, database.Close())
		}
	}
	if cfg.Conversation != nil && cfg.Conversation.Enabled && !cfg.CredentialMaintenance {
		service.conversations = newConversationService(service, cfg.Conversation.Normalized())
	}
	closeStartupDatabase := func(err error) error {
		if service.conversations != nil {
			service.conversations.Stop()
		}
		return errors.Join(err, database.Close())
	}
	if cfg.Workspace != nil && cfg.Workspace.Enabled && !cfg.CredentialMaintenance {
		if err := cfg.Workspace.Validate(); err != nil {
			workerCancel()
			return nil, closeStartupDatabase(err)
		}
		service.workspaces = newWorkspaceService(service, *cfg.Workspace)
		if err := service.workspaces.start(ctx); err != nil {
			workerCancel()
			return nil, closeStartupDatabase(err)
		}
	}
	e.Use(service.measureRequests, service.policyCacheMiddleware, service.changeContentMiddleware, service.runDetailMiddleware, service.requestDeadline)
	service.registerRoutes(e)
	if service.conversations != nil {
		if err := service.conversations.Start(ctx); err != nil {
			workerCancel()
			return nil, closeStartupDatabase(err)
		}
	}
	if cfg.Hosted != nil && !cfg.CredentialMaintenance {
		service.requestMetrics, err = database.openRequestMetrics(ctx, tracker.OrganizationID(cfg.Hosted.OrganizationID))
		if err != nil {
			workerCancel()
			if service.workspaces != nil {
				service.workspaces.StopContext(ctx)
			}
			var mcpErr error
			if service.mcpHTTP != nil {
				mcpErr = service.mcpHTTP.Shutdown(ctx)
			}
			return nil, closeStartupDatabase(errors.Join(err, mcpErr))
		}
	}
	if cfg.SecretKeys != nil && !cfg.CredentialMaintenance {
		go service.runSecretRewrap(workerContext)
	} else {
		close(service.secretRewrapDone)
	}
	if cfg.CredentialMaintenance {
		workerCancel()
		close(service.workerDone)
		service.ready.Store(true)
		if startup != nil {
			startup.auth.Store(service)
		}
		return service, nil
	}
	go service.runPostgresMaintenance(service.runDetailContext(workerContext))
	service.ready.Store(true)
	if service.outbox != nil {
		service.outbox.start(ctx)
	}
	service.startHostedBilling()
	service.startIssueMailer()
	service.dashboard = newDashboardDelivery(service)
	service.dashboard.start()
	if startup != nil {
		startup.auth.Store(service)
	}
	return service, nil
}

func Run(ctx context.Context, cfg Config) (resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.Normalized()
	if err := validateListenerSecurity(cfg); err != nil {
		return err
	}
	if err := validateCredentialMaintenance(cfg); err != nil {
		return err
	}

	network, address := "tcp", cfg.ListenAddress
	if path, ok := listenerUnixPath(cfg.ListenAddress); ok {
		if err := prepareUnixListener(ctx, path); err != nil {
			return err
		}
		network, address = "unix", path
	}
	listener, err := activation.Listen(ctx, network, address, cfg.Deps().Listen)
	if err != nil {
		return fmt.Errorf("listen for hub requests: %w", err)
	}
	if cfg.CredentialMaintenance && !hubapi.ListenerAddressLoopback(listener.Addr().String()) {
		return errors.Join(ErrInsecureListener, listener.Close())
	}
	if network == "unix" {
		if err := os.Chmod(address, 0o600); err != nil {
			return errors.Join(fmt.Errorf("restrict hub socket: %w", err), listener.Close())
		}
	}
	if err := hostedplans.ValidateHostedConfig(cfg.Hosted); err != nil {
		return errors.Join(err, listener.Close())
	}
	progress := &startupHealth{config: cfg}
	if cfg.Hosted != nil && cfg.Hosted.SharedEntry != nil {
		progress.verifier = cfg.Hosted.SharedEntry.Verifier()
	}
	if cfg.Postgres != nil {
		postgresConfig := *cfg.Postgres
		postgresConfig.ObserveSchemaValidation = progress.running.Store
		cfg.Postgres = &postgresConfig
	}
	var service *Service
	server := &http.Server{Handler: progress, ReadHeaderTimeout: defaultHTTPReadHeaderTimeout, IdleTimeout: defaultHTTPIdleTimeout}
	serveDone := make(chan struct{})
	var serveErr error
	ctx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	go func() {
		defer close(serveDone)
		defer cancelStartup()
		if cfg.TLSCertFile != "" {
			serveErr = server.ServeTLS(listener, cfg.TLSCertFile, cfg.TLSKeyFile)
			return
		}
		serveErr = server.Serve(listener)
	}()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		shutdownErr := server.Shutdown(cleanup)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
		<-serveDone
		err := serveErr
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		resultErr = errors.Join(resultErr, shutdownErr, err)
		if service != nil {
			resultErr = errors.Join(resultErr, service.CloseContext(cleanup))
		}
	}()
	service, err = openService(ctx, cfg, progress)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil
		}
		return err
	}
	progress.service.Store(service)
	service.signalSecretRewrap()
	cfg.Logger.Info("hub serving", "address", listener.Addr().String())
	select {
	case <-ctx.Done():
	case <-serveDone:
	}
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	return shutdownServiceAndServer(shutdownContext, service.Shutdown, server.Shutdown)
}

func shutdownServiceAndServer(ctx context.Context, shutdownService, shutdownServer func(context.Context) error) error {
	serviceErr := shutdownService(ctx)
	serverErr := shutdownServer(ctx)
	return errors.Join(serviceErr, serverErr)
}

func validateListenerSecurity(cfg Config) error {
	certFile := strings.TrimSpace(cfg.TLSCertFile)
	keyFile := strings.TrimSpace(cfg.TLSKeyFile)
	if (certFile == "") != (keyFile == "") {
		return errors.New("hub TLS certificate and key must be configured together")
	}
	if _, unix := listenerUnixPath(cfg.ListenAddress); unix || hubapi.ListenerAddressLoopback(cfg.ListenAddress) || certFile != "" || cfg.TrustedProxy {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInsecureListener, cfg.ListenAddress)
}

func listenerUnixPath(address string) (string, bool) {
	path, ok := strings.CutPrefix(strings.TrimSpace(address), "unix:")
	return path, ok && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func prepareUnixListener(ctx context.Context, path string) error {
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("hub Unix socket directory must exist and be private to the service user (mode 0700)")
	}
	existing, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || existing.Mode()&os.ModeSocket == 0 {
		return errors.New("hub Unix socket path exists and is not a socket")
	}
	dialer := net.Dialer{Timeout: time.Second}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err == nil {
		return errors.Join(errors.New("another process is serving the hub Unix socket"), connection.Close())
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("hub Unix socket may still be in use; remove it after confirming no Hub owns it")
	}
	return os.Remove(path)
}

func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.signalSecretRewrap()
		s.echo.ServeHTTP(w, r)
	})
}

func (s *Service) Serve(listener net.Listener) error {
	if s.config.CredentialMaintenance && listener != nil && !hubapi.ListenerAddressLoopback(listener.Addr().String()) {
		return ErrInsecureListener
	}
	if listener == nil {
		return errors.New("hub listener is required")
	}
	err := s.echo.Server.Serve(secretRewrapListener{Listener: listener, serving: s.signalSecretRewrap})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve hub requests: %w", err)
	}
	return nil
}

func (s *Service) ServeTLS(listener net.Listener, certificateFile string, keyFile string) error {
	if s.config.CredentialMaintenance && listener != nil && !hubapi.ListenerAddressLoopback(listener.Addr().String()) {
		return ErrInsecureListener
	}
	if listener == nil {
		return errors.New("hub listener is required")
	}
	err := s.echo.Server.ServeTLS(secretRewrapListener{Listener: listener, serving: s.signalSecretRewrap}, strings.TrimSpace(certificateFile), strings.TrimSpace(keyFile))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve TLS hub requests: %w", err)
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.ready.Store(false)
	s.notifications.closeAll()
	if s.dashboard != nil {
		s.dashboard.stop()
	}
	if s.conversations != nil {
		s.conversations.Stop()
	}
	var mcpErr error
	if s.mcpHTTP != nil {
		mcpErr = s.mcpHTTP.Shutdown(ctx)
	}
	httpErr := s.echo.Shutdown(ctx)
	if errors.Is(httpErr, http.ErrServerClosed) {
		httpErr = nil
	}
	if httpErr != nil {
		httpErr = fmt.Errorf("shut down hub server: %w", httpErr)
	}
	s.stopSpriteRunners()
	s.stopHostedBilling()
	return errors.Join(mcpErr, httpErr, s.stopGitHubWebhookMaintenance())
}

func (s *Service) Close() error {
	return s.CloseContext(context.Background())
}

func (s *Service) CloseContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.ready.Store(false)
		s.notifications.closeAll()
		if s.dashboard != nil {
			s.dashboard.stop()
		}
		var mcpErr error
		if s.mcpHTTP != nil {
			mcpErr = s.mcpHTTP.Shutdown(ctx)
		}
		httpErr := s.echo.Close()
		if errors.Is(httpErr, http.ErrServerClosed) {
			httpErr = nil
		}
		s.stopSpriteRunners()
		webhookErr := s.stopGitHubWebhookMaintenance()
		s.stopHostedBilling()
		s.stopIssueMailer()
		if s.outbox != nil {
			s.outbox.stop()
		}
		if s.conversations != nil {
			s.conversations.Stop()
		}
		if s.workspaces != nil {
			s.workspaces.StopContext(ctx)
		}
		s.tokenUseWork.Wait()
		var metricsErr error
		if s.requestMetrics != nil {
			metricsErr = errors.Join(s.requestMetrics.Drain(ctx, s.config.Clock()), s.requestMetrics.Close())
		}
		s.closeErr = errors.Join(mcpErr, httpErr, webhookErr, metricsErr, s.database.Close())
	})
	return s.closeErr
}

type hostedMaintenanceTenant struct{ hubmaintenance.Tenant }

func (t hostedMaintenanceTenant) Page(ctx context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || t.Organization <= after {
		return nil, nil
	}
	return []hubmaintenance.Tenant{t.Tenant}, nil
}

func (s *Service) newReadModelMaintenance() (*hubmaintenance.Scheduler, error) {
	return hubmaintenance.NewScoped(hostedMaintenanceTenant{s.MaintenanceTenant()}, s.ReadModelMaintenanceFeatures(), s.database.postgres.WithProjectWorkflows, s.config.WebhookMaintenanceInterval)
}

func (s *Service) MaintenanceTenant() hubmaintenance.Tenant {
	tenant := hubmaintenance.Tenant{Organization: s.appOrganizationID(), Generation: 1}
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		tenant.Generation = s.config.Hosted.SharedEntry.Generation
	}
	return tenant
}

func (s *Service) ReadModelMaintenanceFeatures() []hubmaintenance.Feature {
	return []hubmaintenance.Feature{
		s.boardFeature("board-dirty", hubmaintenance.ByteLimit/hubmaintenance.TenantLimit, true),
		s.WorkItemPageMaintenanceFeature(),
		s.IntegrationPageMaintenanceFeature(),
		s.OnboardingPageMaintenanceFeature(),
		s.HealthPageMaintenanceFeature(),
		s.ProjectPageMaintenanceFeature(),
		s.UsageMaintenanceFeature(),
		{Name: "github-intake", Bytes: 64 << 20, Step: func(ctx context.Context, selected hubmaintenance.Tenant, _ hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			if err := hubmaintenance.RequireTenant(ctx, selected); err != nil {
				return hubmaintenance.Outcome{}, err
			}
			return hubmaintenance.Outcome{Progress: s.config.ImportBackend != nil, Items: 1}, s.maintainGitHubBatches(ctx)
		}},
		s.GitHubReconciliationMaintenanceFeature(),
		{Name: "github-webhooks", Bytes: 4 << 20, Step: func(ctx context.Context, selected hubmaintenance.Tenant, _ hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			if err := hubmaintenance.RequireTenant(ctx, selected); err != nil {
				return hubmaintenance.Outcome{}, err
			}
			return hubmaintenance.Outcome{Progress: true, Items: 1}, s.maintainGitHubWebhookRecords(ctx)
		}},
		s.boardFeature("board", BoardMaintenanceBytes, false),
	}
}

func (s *Service) runPostgresMaintenance(ctx context.Context) {
	defer close(s.workerDone)
	var maintenance sync.WaitGroup
	maintenance.Go(func() {
		if err := s.config.Deps().RunMaintenance(ctx, s.maintenance, func(err error) {
			s.config.Logger.WarnContext(ctx, "maintain hosted read models", "error", err)
		}); err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.WarnContext(ctx, "stop hosted read-model maintenance", "error", err)
		}
	})
	defer maintenance.Wait()
	interval := s.config.WebhookMaintenanceInterval
	runPass := func(run func(context.Context)) {
		passCtx, cancel := context.WithTimeout(ctx, interval)
		defer cancel()
		run(passCtx)
	}
	runPass(s.refreshPostgresProjections)
	searchRefresh := s.searchRefreshTimer()
	defer searchRefresh.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastCleanup := s.config.Clock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.boardRefresh:
			runPass(s.refreshPostgresProjections)
		case <-searchRefresh.C:
			runPass(s.maintainNativeSearch)
			searchRefresh.Reset(interval)
		case <-ticker.C:
			runPass(func(ctx context.Context) {
				lastCleanup = s.runPostgresMaintenanceTick(ctx, lastCleanup)
			})
		}
	}
}

func (s *Service) refreshPostgresProjections(ctx context.Context) {
	if err := s.maintainTeamUnread(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "refresh team unread summary", "error", err)
	}
	if err := s.populateChangeColumns(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.WarnContext(ctx, "populate PostgreSQL current Change reviews", "error", err)
	}
}

func (s *Service) runPostgresMaintenanceTick(ctx context.Context, lastCleanup time.Time) time.Time {
	s.refreshPostgresProjections(ctx)
	now := s.config.Clock()
	if err := s.expireGoTestReceipts(ctx, now, s.config.Clock); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.Warn("expire PostgreSQL Go test receipts", "error", err)
	}
	if err := s.expireRequestMetrics(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.Warn("expire PostgreSQL request metrics", "error", err)
	}
	if s.requestMetrics != nil {
		if err := s.requestMetrics.Flush(ctx, s.config.Clock()); err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.WarnContext(ctx, "flush tenant request metrics", "error", err)
		}
	}
	if ctx.Err() == nil && now.Sub(lastCleanup) >= nativeCleanupInterval {
		if err := s.maintainNativeRetention(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.WarnContext(ctx, "maintain PostgreSQL native retention", "error", err)
		}
		return now
	}
	return lastCleanup
}

func (s *Service) stopGitHubWebhookMaintenance() error {
	if s == nil || s.workerCancel == nil || s.workerDone == nil {
		return nil
	}
	s.workerStopOnce.Do(func() {
		s.workerCancel()
		<-s.workerDone
		<-s.secretRewrapDone
	})
	return nil
}

func (s *Service) health(c echo.Context) error {
	response, status := s.readInstanceHealth(c.Request().Context())
	return c.JSON(status, response)
}

func (s *Service) readInstanceHealth(ctx context.Context) (healthResponse, int) {
	if !s.ready.Load() || s.database.health(ctx) != nil {
		return healthResponse{Status: "unavailable"}, http.StatusServiceUnavailable
	}
	return healthResponse{
		Status:              "ok",
		SchemaVersion:       s.database.schemaVersion,
		SchemaFingerprint:   s.database.schemaFingerprint,
		HubSchemaApplicable: true,
		SchemaFactsComplete: true,
		Version:             s.config.Version,
		Commit:              s.config.Commit,
	}, http.StatusOK
}
