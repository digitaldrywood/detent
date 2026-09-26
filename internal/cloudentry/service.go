package cloudentry

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"errors"
	"fmt"
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
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	assertionLifetime = 20 * time.Second
	sessionLifetime   = 30 * 24 * time.Hour
	contentSecurity   = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self' blob:; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://checkout.stripe.com https://billing.stripe.com"
)

type Config struct {
	PublicURL     string
	Issuer        string
	SigningKey    ed25519.PrivateKey
	Provider      auth.HostedProvider
	StaffEmails   []string
	SupportActors []string
	StateDir      string
	ListenAddress string
	Logger        *slog.Logger
	Allocation    *AllocationConfig

	now           func() time.Time
	generateToken func() (string, error)
	transport     func(Organization) (http.RoundTripper, error)
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
	if err := c.Allocation.validate(); err != nil {
		return err
	}
	if !safeID(c.Issuer) || len(c.SigningKey) != ed25519.PrivateKeySize || c.Provider == nil || strings.TrimSpace(c.StateDir) == "" {
		return errors.New("shared entry requires an issuer, signing key, identity provider and state directory")
	}
	return nil
}

type Service struct {
	config     Config
	registry   *Registry
	auth       *authStore
	echo       *echo.Echo
	secure     bool
	transports sync.Map
	mutationMu sync.Mutex

	stopAllocator context.CancelFunc
	allocatorDone chan struct{}
	wake          chan struct{}
}

func Open(ctx context.Context, cfg Config) (*Service, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.generateToken == nil {
		cfg.generateToken = apikey.GenerateToken
	}
	registry, err := OpenRegistry(ctx, filepath.Join(cfg.StateDir, "registry.db"))
	if err != nil {
		return nil, err
	}
	registry.now = cfg.now
	authStorage, err := openStore(ctx, filepath.Join(cfg.StateDir, "auth.db"), authApplicationID, "auth")
	if err != nil {
		return nil, errors.Join(err, registry.Close())
	}
	service := &Service{config: cfg, registry: registry, auth: &authStore{store: authStorage, now: cfg.now}, secure: strings.HasPrefix(cfg.PublicURL, "https://")}
	if cfg.transport == nil {
		service.config.transport = service.tenantTransport
	}
	service.echo = echo.New()
	service.echo.HideBanner, service.echo.HidePort = true, true
	service.echo.Server.ReadHeaderTimeout = 5 * time.Second
	service.routes()
	service.startAllocator(ctx)
	return service, nil
}

func (s *Service) Handler() http.Handler {
	return s.echo
}

func (s *Service) Registry() *Registry {
	return s.registry
}

func (s *Service) Close() error {
	return errors.Join(s.closeAllocator(), s.registry.Close(), s.auth.store.Close())
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
	service.config.Logger.Info("shared entry serving", "address", listener.Addr().String())
	result := make(chan error, 1)
	go func() {
		err := service.echo.Server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(service.echo.Shutdown(shutdown), <-result)
	}
}

func (s *Service) routes() {
	e := s.echo
	e.Pre(s.boundary)
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", http.FileServerFS(detent.StaticFS()))))
	e.GET("/health", func(c echo.Context) error { return c.JSON(http.StatusOK, map[string]string{"status": "ok"}) })
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
	e.POST("/logout", s.logout)
	e.POST("/organizations/:organization/logout", s.logout)
	e.GET("/support", s.supportPage)
	e.POST("/support/start", s.startSupport)
	e.GET("/invite", s.startInvitation)
	e.GET("/invitations/join", s.joinPage)
	e.POST("/invitations/join", s.joinInvitation)
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

func (s *Service) session(c echo.Context) (accountSession, error) {
	cookie, err := c.Cookie(s.cookieName("session"))
	if err != nil || cookie.Value == "" || len(cookie.Value) > 256 {
		return accountSession{}, errNoSession
	}
	session, err := s.auth.session(c.Request().Context(), apikey.HashToken(cookie.Value))
	if err != nil {
		return accountSession{}, err
	}
	current, err := s.config.Provider.CurrentSession(c.Request().Context(), session.Identity)
	if err != nil || current.Subject != session.Subject || !current.ExpiresAt.After(s.config.now()) {
		return accountSession{}, errNoSession
	}
	return session, nil
}

func (s *Service) supportActor(email string) bool {
	return s.staff(email) && listed(s.config.SupportActors, email)
}

func (s *Service) staff(email string) bool {
	return listed(s.config.StaffEmails, email)
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

func (s *Service) home(c echo.Context) error {
	if _, err := s.session(c); err == nil {
		return c.Redirect(http.StatusSeeOther, "/organizations")
	}
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "login", Title: "Sign in"})
}

func (s *Service) sameOrigin(c echo.Context) bool {
	origin := c.Request().Header.Get("Origin")
	return origin != "" && origin == strings.TrimRight(s.config.PublicURL, "/")
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
