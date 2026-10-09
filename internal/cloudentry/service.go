package cloudentry

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	assertionLifetime = 20 * time.Second
	sessionLifetime   = 30 * 24 * time.Hour
	contentSecurity   = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self' blob:; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://checkout.stripe.com https://billing.stripe.com"
)

type Config struct {
	Build                     buildinfo.Info
	PublicURL                 string
	Issuer                    string
	SigningKey                ed25519.PrivateKey
	Provider                  auth.HostedProvider
	Platform                  PlatformConfig
	StaffEmails               []string
	SupportActors             []string
	EntitlementAdministrators []string
	StateDir                  string
	ListenAddress             string
	Logger                    *slog.Logger
	Allocation                *AllocationConfig
	Billing                   *BillingConfig
	ConfigPath                string
	Attachments               *attachment.Config
	GitHubWebhookSecret       []byte
	GitHubApp                 *github.InstallationTokenConfig

	now                 func() time.Time
	generateToken       func() (string, error)
	transport           func(Organization) (http.RoundTripper, error)
	clientFS            fs.FS
	attachmentTransport http.RoundTripper

	tenantStartTimeout time.Duration
	platformDeadline   time.Duration
}

func (c Config) validate() error {
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("shared entry public URL must be an origin without a path")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return errors.New("shared entry public URL must use HTTPS except on loopback")
		}
	}
	host, _, err := net.SplitHostPort(c.ListenAddress)
	if ip := net.ParseIP(strings.Trim(host, "[]")); err != nil || host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("shared entry must listen on a loopback address behind the TLS proxy")
	}
	if err := c.Billing.validate(); err != nil {
		return err
	}
	if err := c.Allocation.validate(); err != nil {
		return err
	}
	if !safeID(c.Issuer) || len(c.SigningKey) != ed25519.PrivateKeySize || c.Provider == nil || strings.TrimSpace(c.StateDir) == "" {
		return errors.New("shared entry requires an issuer, signing key, identity provider and state directory")
	}
	return nil
}

type Service struct {
	administration    *operatoradmin.Executor
	config            Config
	registry          *Registry
	auth              *authStore
	echo              *echo.Echo
	secure            bool
	transports        sync.Map
	mutationMu        sync.Mutex
	verified          sessionVerifications
	refreshes         refreshLocks
	attachments       attachment.Storage
	attachmentMu      sync.RWMutex
	attachmentSweepAt time.Time
	diffBodySweepAt   time.Time
	githubRoutes      githubRoutes

	stopAllocator     context.CancelFunc
	allocatorDone     chan struct{}
	wake              chan struct{}
	stopBilling       context.CancelFunc
	billingDone       chan struct{}
	closeDevelopState func() error
	closePlatformMCP  func() error
}

func Open(ctx context.Context, cfg Config) (result *Service, resultErr error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.tenantStartTimeout == 0 {
		cfg.tenantStartTimeout = 30 * time.Second
	}
	if cfg.generateToken == nil {
		cfg.generateToken = apikey.GenerateToken
	}
	cfg, closeDevelopState, err := prepareDevelopState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil && closeDevelopState != nil {
			resultErr = errors.Join(resultErr, closeDevelopState())
		}
	}()
	var attachments attachment.Storage
	if cfg.Attachments != nil {
		var err error
		attachments, err = attachment.NewStorage(ctx, *cfg.Attachments, cfg.attachmentTransport)
		if err != nil {
			return nil, fmt.Errorf("configure attachments: %w", err)
		}
	}
	registry, err := OpenRegistry(ctx, filepath.Join(cfg.StateDir, "registry.db"))
	if err != nil {
		return nil, err
	}
	registry.now = cfg.now
	if err := registry.seedPlatformMembers(ctx, cfg); err != nil {
		return nil, errors.Join(err, registry.Close())
	}
	if len(cfg.StaffEmails)+len(cfg.SupportActors)+len(cfg.EntitlementAdministrators) > 0 {
		cfg.Logger.Warn("legacy platform email lists are deprecated and used only to seed an empty registry")
	}
	authStorage, err := openStore(ctx, filepath.Join(cfg.StateDir, "auth.db"), authApplicationID, "auth")
	if err != nil {
		return nil, errors.Join(err, registry.Close())
	}
	seal, err := newTokenSeal(cfg.SigningKey)
	if err != nil {
		return nil, errors.Join(err, authStorage.Close(), registry.Close())
	}
	service := &Service{config: cfg, registry: registry, auth: &authStore{store: authStorage, now: cfg.now, seal: seal}, secure: strings.HasPrefix(cfg.PublicURL, "https://"), closeDevelopState: closeDevelopState}
	service.attachments = attachments
	if cfg.transport == nil {
		service.config.transport = service.tenantTransport
	}
	service.echo = echo.New()
	service.echo.HideBanner, service.echo.HidePort = true, true
	service.echo.Server.ReadHeaderTimeout = 5 * time.Second
	service.routes()
	service.registerPlatformCredits(ctx)
	if err := service.routeDevelopTenants(ctx); err != nil {
		return nil, errors.Join(err, service.Close())
	}
	service.startAllocator(ctx)
	if err := service.initializeGitHubRoutes(ctx); err != nil {
		return nil, errors.Join(err, service.Close())
	}
	service.startBillingWorker(ctx)
	return service, nil
}

func (s *Service) Handler() http.Handler {
	return s.echo
}

func (s *Service) Registry() *Registry {
	return s.registry
}

func (s *Service) Close() error {
	s.closeBillingWorker()
	var mcpErr error
	if s.closePlatformMCP != nil {
		mcpErr = s.closePlatformMCP()
	}
	err := errors.Join(mcpErr, s.closeAllocator(), s.registry.Close(), s.auth.store.Close())
	if s.closeDevelopState != nil {
		err = errors.Join(err, s.closeDevelopState())
	}
	return err
}

func Run(ctx context.Context, cfg Config) (resultErr error) {
	service, err := Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, service.Close())
	}()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for shared entry requests: %w", err)
	}
	return service.serve(ctx, listener)
}

func (s *Service) serve(ctx context.Context, listener net.Listener) error {
	since, before := s.githubReplayWindow(ctx)
	s.config.Logger.Info("shared entry serving", "address", listener.Addr().String())
	result := make(chan error, 1)
	go func() {
		err := s.echo.Server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}()
	replayCtx, cancelReplay := context.WithTimeout(ctx, 5*time.Minute)
	replayDone := make(chan struct{})
	go func() {
		defer close(replayDone)
		s.replayGitHubDeliveries(replayCtx, since, before)
	}()
	defer func() {
		cancelReplay()
		<-replayDone
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(s.echo.Shutdown(shutdown), <-result)
	}
}

func (s *Service) routes() {
	s.registerAdministration()
	e := s.echo
	e.Pre(s.boundary)
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", http.FileServerFS(detent.StaticFS()))))
	e.GET("/health", s.health)
	e.GET("/", s.home)
	e.GET("/auth/oidc/start", s.startLogin)
	e.GET("/auth/oidc/callback", s.completeLogin)
	e.GET("/organizations", s.chooser)
	e.GET("/organizations/new", s.newOrganizationPage)
	e.POST("/organizations", s.createOrganization)
	e.GET("/organizations/:organization/provisioning", s.provisioningPage)
	e.POST("/organizations/:organization/provisioning/resume", s.resumeProvisioning)
	e.GET("/api/cloud/organizations/:organization/provisioning", s.provisioningJSON)
	e.GET("/organizations/:organization/delete", s.deleteOrganizationPage)
	e.POST("/organizations/:organization/delete", s.deleteOrganization)
	e.GET("/api/cloud/organizations", s.organizationsJSON)
	e.GET("/api/cloud/session", s.sessionJSON)
	e.POST("/logout", s.logout)
	e.POST("/organizations/:organization/logout", s.logout)
	e.GET(platformPath, s.platformPage)
	e.GET(platformPath+"/*", s.platformPage)
	e.GET("/api/cloud/platform/organizations", s.platformOrganizationsJSON)
	e.GET("/api/cloud/platform/organizations/:organization", s.platformOrganizationJSON)
	e.POST("/api/cloud/platform/organizations/:organization/resume", s.resumePlatformProvisioning)
	e.GET("/api/cloud/platform/allowlist", s.platformAllowlistJSON)
	e.GET("/api/cloud/platform/health", s.platformHealthJSON)
	e.GET("/api/cloud/platform/users", s.platformUsersJSON)
	e.GET("/api/cloud/platform/audit", s.platformAuditJSON)
	e.GET("/api/cloud/platform/organizations/:organization/entitlements", s.platformEntitlementsJSON)
	e.POST("/api/cloud/platform/organizations/:organization/entitlements", s.changePlatformEntitlement)
	e.GET("/api/cloud/platform/members", s.platformMembersJSON)
	e.POST("/api/cloud/platform/members", s.changePlatformMember)
	e.PATCH("/api/cloud/platform/members/:email", s.changePlatformMember)
	e.DELETE("/api/cloud/platform/members/:email", s.changePlatformMember)
	e.GET("/support", s.supportPage)
	e.POST("/support/start", s.startSupport)
	e.POST("/webhooks/stripe/:mode", s.stripeWebhook)
	e.POST("/api/v1/webhooks/github", s.githubWebhook)
	e.GET("/invite", s.startInvitation)
	s.registerAttachmentRoutes(e)
	e.Any("/organizations/:organization", s.proxy)
	e.Any("/organizations/:organization/*", s.proxy)
	e.Any("/api/v2/organizations/:organization/*", s.proxy)
}

func (s *Service) boundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		header := c.Response().Header()
		header.Set("Cache-Control", "no-store")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Content-Security-Policy", contentSecurity)
		if s.secure {
			header.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !cloudassert.CanonicalPath(c.Request().URL) {
			return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "Resource was not found"})
		}
		if s.redirectLegacyNavigation(c) {
			return c.Redirect(http.StatusTemporaryRedirect, s.config.PublicURL+c.Request().URL.RequestURI())
		}
		return next(c)
	}
}

func (s *Service) cookieName(name string) string {
	if s.secure {
		return "__Host-detent_" + name
	}
	return "detent_entry_" + name
}

func (s *Service) setCookie(c echo.Context, name, value string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if value == "" {
		maxAge = -1
	}
	cookie := &http.Cookie{Name: s.cookieName(name), Value: value, Path: "/", Expires: expires, MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if !s.secure {
		cookie.Secure = c.Request().TLS != nil
	}
	c.SetCookie(cookie)
}

// storedSession reads the account session from the entry's own store without
// asking the provider; proxied requests verify the organization's access
// token instead.
func (s *Service) storedSession(c echo.Context) (accountSession, error) {
	cookie, err := c.Cookie(s.cookieName("session"))
	if err != nil || cookie.Value == "" || len(cookie.Value) > 256 {
		return accountSession{}, errNoSession
	}
	return s.auth.session(c.Request().Context(), apikey.HashToken(cookie.Value))
}

func (s *Service) session(c echo.Context) (accountSession, error) {
	session, err := s.storedSession(c)
	if err != nil {
		return accountSession{}, err
	}
	now := s.config.now()
	method := c.Request().Method
	// Staff and support sessions reach across organizations, so they are
	// re-verified on every request rather than trusted for the recheck window.
	cacheable := s.platformRole(c.Request().Context(), session.Email) == "" && session.Identity.SupportActor == ""
	if cacheable && (method == http.MethodGet || method == http.MethodHead) && session.Identity.ExpiresAt.After(now) && s.verified.fresh(session.Hash, now) {
		return session, nil
	}
	current, err := s.config.Provider.CurrentSession(c.Request().Context(), session.Identity)
	if err != nil || current.Subject != session.Subject || !current.ExpiresAt.After(now) {
		s.verified.forget(session.Hash)
		reason := auth.HostedIdentityReason(err)
		if err == nil {
			reason = auth.HostedReasonSessionInvalid
		}
		s.config.Logger.InfoContext(c.Request().Context(), "hosted session revalidation failed", "reason", reason, "request_id", auth.HostedRequestID(c.Response(), c.Request()))
		return accountSession{}, errNoSession
	}
	s.verified.record(session.Hash, now)
	return session, nil
}

func (s *Service) supportActor(ctx context.Context, email string) bool {
	role := s.platformRole(ctx, email)
	return role == "admin" || role == "support"
}

func (s *Service) landing(ctx context.Context, email string, identity auth.HostedIdentity) string {
	if !s.platformIdentity(ctx, email, identity) {
		return "/organizations"
	}
	choices, err := s.organizationChoices(ctx, accountSession{Subject: identity.Subject, Email: email, Identity: identity})
	if err != nil || len(choices) > 0 {
		return "/organizations"
	}
	return platformPath
}

func listed(values []string, email string) bool {
	for _, staff := range values {
		if strings.EqualFold(strings.TrimSpace(staff), strings.TrimSpace(email)) {
			return true
		}
	}
	return false
}

func (s *Service) render(c echo.Context, status int, data templates.HostedPageData) error {
	data.SharedOrigin = true
	data.Assets.Favicon = "/static/img/detent-mark.svg"
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(status)
	return templates.HostedPage(data).Render(c.Request().Context(), c.Response())
}

func (s *Service) denied(c echo.Context, status int, message string) error {
	data := templates.HostedPageData{Mode: "denied", Title: "Access unavailable", Error: message}
	if session, err := s.session(c); err == nil {
		data.Email, data.CSRF = session.Email, cloudassert.CSRFToken(session.CSRFSecret, "")
	}
	return s.render(c, status, data)
}

func (s *Service) browserUnavailable(c echo.Context, status int) error {
	request := c.Request()
	organization := c.Param("organization")
	retry := "/organizations/" + organization + "/work"
	if (request.Method == http.MethodGet || request.Method == http.MethodHead) && validReturnPath(request.URL.Path, organization) {
		retry = request.URL.RequestURI()
	}
	data := templates.HostedPageData{Mode: "unavailable", Title: "Temporarily unavailable", Error: "We could not open this organization right now. Please try again shortly.", RetryURL: retry}
	if session, err := s.storedSession(c); err == nil {
		data.Email, data.CSRF = session.Email, cloudassert.CSRFToken(session.CSRFSecret, "")
	}
	return s.render(c, status, data)
}

func (s *Service) loginDenied(c echo.Context, status int, message string, denial auth.HostedDenial) error {
	denial.Status = status
	auth.LogHostedDenial(s.config.Logger, c.Response(), c.Request(), denial)
	return s.denied(c, status, message)
}

func (s *Service) home(c echo.Context) error {
	if session, err := s.session(c); err == nil {
		return c.Redirect(http.StatusSeeOther, s.landing(c.Request().Context(), session.Email, session.Identity))
	}
	if served, err := s.clientShell(c); served || err != nil {
		return err
	}
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "login", Title: "Sign in"})
}

func (s *Service) sameOrigin(c echo.Context) bool {
	header := c.Request().Header
	origin := header.Get("Origin")
	if origin == "null" {
		return header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return origin != "" && (origin == strings.TrimRight(s.config.PublicURL, "/") ||
		s.legacyHostedRequest(c.Request()) && origin == "https://"+c.Request().Host)
}

func (s *Service) csrfValid(c echo.Context, session accountSession, organization string) bool {
	if !s.sameOrigin(c) {
		return false
	}
	value := c.Request().Header.Get("X-CSRF-Token")
	if value == "" {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10)
		value = c.FormValue("csrf")
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(cloudassert.CSRFToken(session.CSRFSecret, organization))) == 1
}
