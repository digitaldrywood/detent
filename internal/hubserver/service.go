package hubserver

import (
	"context"
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
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
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
	changeColumnFamily      int
	changeColumnAfter       any
	changeSessionAfter      string
	dashboard               *dashboardDelivery
	releaseNotes            []releasenotes.Build
	activityDurations       activityDurationCache
	issuePages              nativeIssuePageCache
	writerStatsAt           atomic.Int64
	writerWaits             atomic.Int64
	writerWaited            atomic.Int64
	readerWaits             atomic.Int64
	readerWaited            atomic.Int64
	requestMetrics          *tenantRequestMetrics
	runnerPublishedReleases sync.Map
	tokenUse                sync.Map
	tokenUseWork            sync.WaitGroup
	mcpHTTP                 *mcp.HTTPHandler
	operatorChat            *chat.Service
	administration          *operatoradmin.Executor
	billing                 *hostedBillingWorker
	healthDetector          *healthDetector
	issueMailer             *issueMailer
	hostedMutationMu        sync.Mutex
	sourceVerification      chan struct{}
	runtimeMu               sync.Mutex
	hostedAuthLogger        *slog.Logger
	hostedSessions          *auth.Service
	echo                    *echo.Echo
	database                *database
	searchRefresh           searchstore.Refresh
	tracker                 tracker.Tracker
	config                  Config
	outbox                  *outboxWorker
	ready                   atomic.Bool
	wakeSpriteRunnersAfter  func(nativeScope, json.RawMessage)
	startSpriteLifecycle    func(nativeScope, string)
	startSpritePoolForQueue func(nativeScope)
	spriteWakeMu            sync.Mutex
	spriteWakes             map[spriteWakeKey]*spriteLifecyclePass
	spriteWakeWork          sync.WaitGroup
	secretRewrapOnce        sync.Once
	secretRewrapReady       chan struct{}
	secretRewrapDone        chan struct{}
	workerCancel            context.CancelFunc
	workerDone              chan struct{}
	boardRefresh            chan struct{}
	pageConversionFamily    atomic.Uint64
	activityConvertFamily   atomic.Uint64
	projectionCursors       sync.Map
	cleanupPollCursors      sync.Map
	boardRefreshCursor      string
	boardRetentionScope     nativeScope
	lastNativeCleanup       time.Time
	githubBatchCursor       tracker.ProjectID
	workerStopOnce          sync.Once
	reconcileCancel         context.CancelFunc
	reconcileDone           chan struct{}
	reconcileStopOnce       sync.Once
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
	Outbox                   OutboxHealth            `json:"outbox"`
	Repositories             RepositoryHealthSummary `json:"repositories"`
}

func Open(ctx context.Context, cfg Config) (*Service, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalized()
	if err := validateCredentialMaintenance(cfg); err != nil {
		return nil, err
	}
	if err := cfg.Hosted.validate(); err != nil {
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
	database.now = cfg.now
	database.newLeaseID = cfg.newLeaseID
	if database.postgres != nil {
		database.logger = cfg.Logger
		cfg.TenantRepository = database.postgres
		cfg.AccessRepository = database.postgres
		cfg.BillingRepository = database.postgres
		database.billingRepository = database.postgres
		database.hostedOrganization = tracker.OrganizationID(cfg.Hosted.OrganizationID)
		database.linkedSourceBase = cfg.Hosted.PublicURL
		if cfg.Hosted.SharedEntry != nil {
			database.linkedSourceBase += "/organizations/" + cfg.Hosted.OrganizationID
		}
		database.runtimeBilling = cfg.Hosted.Billing
		cfg = cfg.normalized()
		if cfg.Hosted.EmailSender != nil {
			return nil, errors.Join(ErrPostgresFeatureUnavailable, database.Close())
		}
	}
	ctx = database.policyContext(ctx)
	if database.postgres == nil {
		database.configureRuntimePublication(cfg)
	}
	var secretKeysErr error
	if cfg.AccessRepository != nil {
		if cfg.Hosted == nil {
			return nil, errors.Join(auth.ErrHostedIdentity, database.Close())
		}
		secretKeysErr = (&Service{config: cfg}).checkAccessSecretKeys(ctx)
	} else {
		secretKeysErr = database.checkProjectSecretKeys(ctx, cfg.SecretKeys)
	}
	if err := secretKeysErr; err != nil {
		return nil, errors.Join(err, database.Close())
	}
	if database.postgres != nil {
		if err := database.configureHostedPlans(ctx, cfg.Hosted); err != nil {
			return nil, errors.Join(err, database.Close())
		}
		if err := database.configureHostedBilling(ctx, cfg.Hosted); err != nil {
			return nil, errors.Join(err, database.Close())
		}
	}
	if !cfg.CredentialMaintenance && database.postgres == nil {
		if err := database.ensureInitialAdminToken(ctx, cfg.InitialAdminToken); err != nil {
			return nil, errors.Join(err, database.Close())
		}
	}
	cfg.InitialAdminToken = nil
	var workTracker tracker.Tracker
	if database.postgres == nil {
		workTracker, err = tracker.NewStore(database)
		if err != nil {
			return nil, errors.Join(err, database.Close())
		}
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Server.ReadHeaderTimeout = defaultHTTPReadHeaderTimeout
	e.Server.IdleTimeout = defaultHTTPIdleTimeout
	workerContext, workerCancel := context.WithCancel(context.Background())
	reconcileContext, reconcileCancel := context.WithCancel(context.Background())
	service := &Service{
		releaseNotes:       notes,
		echo:               e,
		notifications:      newNotificationBroker(),
		hostedAuthLogger:   hostedAuthLogger,
		database:           database,
		searchRefresh:      database.backfillNativeSearch,
		sourceVerification: make(chan struct{}, 1),
		tracker:            workTracker,
		config:             cfg,
		spriteWakes:        make(map[spriteWakeKey]*spriteLifecyclePass),
		workerCancel:       workerCancel,
		workerDone:         make(chan struct{}),
		secretRewrapReady:  make(chan struct{}),
		secretRewrapDone:   make(chan struct{}),
		boardRefresh:       make(chan struct{}, 1),
		reconcileCancel:    reconcileCancel,
		reconcileDone:      make(chan struct{}),
		clientBuild:        conversationClientIdentity(conversationClientFS, cfg.Version, cfg.now()),
		pullRequests:       newPullRequestCache(),
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
	if cfg.Hosted == nil {
		if err := database.reader.QueryRowContext(ctx, "SELECT id FROM organizations WHERE local=1").Scan(&service.localOrganization); err != nil {
			workerCancel()
			reconcileCancel()
			return nil, errors.Join(err, database.Close())
		}
	}
	service.wakeSpriteRunnersAfter = func(scope nativeScope, result json.RawMessage) {
		service.scheduleSpriteWake(workerContext, scope, result)
	}
	service.startSpriteLifecycle = func(scope nativeScope, state string) {
		service.scheduleSpriteLifecycle(workerContext, scope, state)
	}
	service.startSpritePoolForQueue = func(scope nativeScope) {
		service.scheduleSpritePoolForQueue(workerContext, scope)
	}
	if database.postgres == nil {
		service.outbox = newOutboxWorker(service)
	}
	if cfg.Hosted != nil {
		service.hostedSessions, err = auth.NewSessionService(auth.SessionConfig{SessionTTL: 30 * 24 * time.Hour, PublicURL: cfg.Hosted.PublicURL}, service)
		if err != nil {
			workerCancel()
			reconcileCancel()
			return nil, errors.Join(err, database.Close())
		}
	}
	if cfg.Conversation != nil && cfg.Conversation.Enabled && !cfg.CredentialMaintenance {
		service.conversations = newConversationService(service, cfg.Conversation.normalized())
	}
	if cfg.Workspace != nil && cfg.Workspace.Enabled && database.postgres == nil && !cfg.CredentialMaintenance {
		if err := cfg.Workspace.validate(); err != nil {
			workerCancel()
			reconcileCancel()
			return nil, errors.Join(err, database.Close())
		}
		service.workspaces = newWorkspaceService(service, *cfg.Workspace)
		if err := service.workspaces.start(ctx); err != nil {
			workerCancel()
			reconcileCancel()
			return nil, errors.Join(err, database.Close())
		}
	}
	e.Use(service.measureRequests, service.policyCacheMiddleware, service.changeContentMiddleware, service.runDetailMiddleware)
	service.registerRoutes(e)
	if service.conversations != nil {
		if err := service.conversations.start(ctx); err != nil {
			workerCancel()
			reconcileCancel()
			return nil, errors.Join(err, database.Close())
		}
	}
	if cfg.Hosted != nil && !cfg.CredentialMaintenance {
		service.requestMetrics, err = database.openRequestMetrics(ctx, tracker.OrganizationID(cfg.Hosted.OrganizationID))
		if err != nil {
			workerCancel()
			reconcileCancel()
			if service.conversations != nil {
				service.conversations.stop()
			}
			if service.workspaces != nil {
				service.workspaces.StopContext(ctx)
			}
			var mcpErr error
			if service.mcpHTTP != nil {
				mcpErr = service.mcpHTTP.Shutdown(ctx)
			}
			return nil, errors.Join(err, mcpErr, database.Close())
		}
	}
	if cfg.SecretKeys != nil && !cfg.CredentialMaintenance {
		go service.runSecretRewrap(workerContext)
	} else {
		close(service.secretRewrapDone)
	}
	if cfg.CredentialMaintenance {
		workerCancel()
		reconcileCancel()
		close(service.workerDone)
		close(service.reconcileDone)
		service.ready.Store(true)
		return service, nil
	}
	if database.postgres != nil {
		close(service.reconcileDone)
		go service.runPostgresMaintenance(service.runDetailContext(workerContext))
	} else {
		service.maintainGitHubWebhooks(ctx)
		go service.runGitHubWebhookMaintenance(service.runDetailContext(workerContext))
		go service.runGitHubReconciliation(service.runDetailContext(reconcileContext))
	}
	service.ready.Store(true)
	if service.outbox != nil {
		service.outbox.start(ctx)
	}
	service.startHostedBilling()
	if database.postgres == nil {
		service.startHealthDetector()
	}
	service.startIssueMailer()
	service.dashboard = newDashboardDelivery(service)
	service.dashboard.start()
	return service, nil
}

func (s *Service) Tracker() tracker.Tracker {
	return s.tracker
}

func Run(ctx context.Context, cfg Config) (resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = cfg.normalized()
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
	listener, err := activation.Listen(ctx, network, address, cfg.listen)
	if err != nil {
		return fmt.Errorf("listen for hub requests: %w", err)
	}
	if cfg.CredentialMaintenance && !listenerAddressLoopback(listener.Addr().String()) {
		return errors.Join(ErrInsecureListener, listener.Close())
	}
	if network == "unix" {
		if err := os.Chmod(address, 0o600); err != nil {
			return errors.Join(fmt.Errorf("restrict hub socket: %w", err), listener.Close())
		}
	}
	if err := cfg.Hosted.validate(); err != nil {
		return errors.Join(err, listener.Close())
	}
	progress := &startupHealth{config: cfg}
	if cfg.Hosted != nil && cfg.Hosted.SharedEntry != nil {
		progress.verifier = cfg.Hosted.SharedEntry.verifier
	}
	progress.running.Store(true)
	cfg.startup = progress
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
	service, err = Open(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil
		}
		return err
	}
	progress.service.Store(service)
	progress.running.Store(false)
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
	if _, unix := listenerUnixPath(cfg.ListenAddress); unix || listenerAddressLoopback(cfg.ListenAddress) || certFile != "" || cfg.TrustedProxy {
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

func listenerAddressLoopback(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.signalSecretRewrap()
		s.echo.ServeHTTP(w, r)
	})
}

func (s *Service) Serve(listener net.Listener) error {
	if s.config.CredentialMaintenance && listener != nil && !listenerAddressLoopback(listener.Addr().String()) {
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
	if s.config.CredentialMaintenance && listener != nil && !listenerAddressLoopback(listener.Addr().String()) {
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
		s.conversations.stop()
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
	s.stopHealthDetector()
	return errors.Join(mcpErr, httpErr, s.stopGitHubWebhookMaintenance(), s.stopGitHubReconciliation())
}

func (s *Service) Backup(ctx context.Context, destination string) error {
	if !s.ready.Load() {
		return ErrNotReady
	}
	return s.database.backup(ctx, destination)
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
		reconcileErr := s.stopGitHubReconciliation()
		s.stopHostedBilling()
		s.stopHealthDetector()
		s.stopIssueMailer()
		if s.outbox != nil {
			s.outbox.stop()
		}
		if s.conversations != nil {
			s.conversations.stop()
		}
		if s.workspaces != nil {
			s.workspaces.StopContext(ctx)
		}
		s.tokenUseWork.Wait()
		var metricsErr error
		if s.requestMetrics != nil {
			metricsErr = errors.Join(s.requestMetrics.drain(ctx, s.config.now()), s.requestMetrics.close())
		}
		s.closeErr = errors.Join(mcpErr, httpErr, webhookErr, reconcileErr, metricsErr, s.database.Close())
	})
	return s.closeErr
}

func (s *Service) runGitHubWebhookMaintenance(ctx context.Context) {
	ctx = s.runDetailContext(ctx)
	defer close(s.workerDone)
	s.maintainNativeBoard(ctx)
	ticker := time.NewTicker(s.config.WebhookMaintenanceInterval)
	defer ticker.Stop()
	lastOptimize := s.config.now()
	var lastAnalyticsRetry time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.boardRefresh:
			s.maintainNativeBoard(ctx)
		case <-ticker.C:
			s.maintainNativeBoard(ctx)
			s.maintainNativeSearch(ctx)
			now := s.config.now()
			retryAnalytics := now.Sub(lastAnalyticsRetry) >= time.Hour
			s.maintainAnalyticsRollups(ctx, now, retryAnalytics)
			if retryAnalytics {
				lastAnalyticsRetry = now
			}
			if now.Sub(lastOptimize) >= time.Hour {
				if err := s.database.optimize(ctx, "PRAGMA optimize"); err != nil && !errors.Is(err, context.Canceled) {
					s.config.Logger.Warn("optimize hub database", "error", err)
				}
				lastOptimize = now
			}
			if err := s.convertLegacyStorage(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.Warn("convert legacy storage", "error", err)
			}
			if err := s.expireGoTestReceipts(ctx, now, s.config.now); err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.Warn("expire go test receipts", "error", err)
			}
			s.maintainGitHubWebhooks(ctx)
			if err := s.maintainGitHubBatches(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.Error("process GitHub intake batches", "error", err)
			}
		}
	}
}

func (s *Service) runPostgresMaintenance(ctx context.Context) {
	defer close(s.workerDone)
	s.maintainNativeBoard(ctx)
	s.maintainNativeSearch(ctx)
	ticker := time.NewTicker(s.config.WebhookMaintenanceInterval)
	defer ticker.Stop()
	lastCleanup := s.config.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.boardRefresh:
			s.maintainNativeBoard(ctx)
		case <-ticker.C:
			s.maintainNativeBoard(ctx)
			s.maintainNativeSearch(ctx)
			now := s.config.now()
			if err := s.expireGoTestReceipts(ctx, now, s.config.now); err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.Warn("expire PostgreSQL Go test receipts", "error", err)
			}
			if err := s.expireRequestMetrics(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.Warn("expire PostgreSQL request metrics", "error", err)
			}
			if s.requestMetrics != nil {
				if err := s.requestMetrics.flush(ctx, s.config.now()); err != nil && !errors.Is(err, context.Canceled) {
					s.config.Logger.WarnContext(ctx, "flush tenant request metrics", "error", err)
				}
			}
			if now.Sub(lastCleanup) >= nativeCleanupInterval {
				if err := s.retainCommandPayloads(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
					s.config.Logger.WarnContext(ctx, "expire PostgreSQL command receipts", "error", err)
				}
				if err := s.prunePostgresRunnerMetrics(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
					s.config.Logger.WarnContext(ctx, "expire PostgreSQL runner host metrics", "error", err)
				}
				lastCleanup = now
			}
		}
	}
}

func (s *Service) prunePostgresRunnerMetrics(ctx context.Context, now time.Time) error {
	organization := tracker.OrganizationID(s.appOrganizationID())
	if organization == "" || s.config.TenantRepository == nil {
		return errors.New("PostgreSQL runner host metric retention requires an organization")
	}
	cutoff := now.UTC().Truncate(time.Hour).Add(-720 * time.Hour)
	return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		return w.PruneRunnerHostMetrics(ctx, cutoff, runnerstore.MaxRunnerHostPruneBatch)
	})
}

func (s *Service) maintainGitHubWebhooks(ctx context.Context) {
	now := s.config.now().UTC()
	if err := s.processPendingWebhooks(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.Error("process pending GitHub webhooks", "error", err)
	}
	if _, err := s.database.purgeWebhookPayloads(ctx, now, s.config.WebhookPayloadRetention); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.Warn("purge GitHub webhook payloads", "error", err)
	}
	s.maintainSpritePools(ctx)
	var retentionErr error
	if s.lastNativeCleanup.IsZero() || now.Sub(s.lastNativeCleanup) >= nativeCleanupInterval {
		s.lastNativeCleanup = now
		retentionErr = s.maintainNativeRetention(ctx, now)
	} else {
		retentionErr = s.maintainNativeBackfills(ctx)
	}
	if retentionErr != nil && !errors.Is(retentionErr, context.Canceled) {
		s.config.Logger.Warn("maintain native retention", "error", retentionErr)
	}
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
	if s.database.postgres != nil {
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
	outbox, err := s.OutboxHealth(ctx)
	if err != nil {
		return healthResponse{Status: "unavailable"}, http.StatusServiceUnavailable
	}
	repositories, err := s.database.repositoryFreshness(ctx, s.config.now().UTC(), s.config.ReconcileInterval)
	if err != nil {
		return healthResponse{Status: "unavailable"}, http.StatusServiceUnavailable
	}
	status := "ok"
	if repositories.Summary.Stale > 0 || repositories.Summary.Error > 0 {
		status = "degraded"
	}
	return healthResponse{
		Status:              status,
		SchemaVersion:       s.database.schemaVersion,
		SchemaFingerprint:   s.database.schemaFingerprint,
		HubSchemaApplicable: true,
		SchemaFactsComplete: true,
		Version:             s.config.Version,
		Commit:              s.config.Commit,
		Outbox:              outbox,
		Repositories:        repositories.Summary,
	}, http.StatusOK
}
