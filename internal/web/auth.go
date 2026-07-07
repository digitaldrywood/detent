package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/store"
)

const apiTokenEnv = "DETENT_API_TOKEN"

const uiAPICookieName = "detent_ui_api"
const apiKeyDashboardCookieName = "detent_api_key_dashboard"
const apiKeyDashboardHeader = "X-Detent-API-Key-Dashboard"
const apiKeyDashboardTokenPayload = "detent-api-key-dashboard-v1"

type apiCredentialContextKey struct{}

type apiAuthOptions struct {
	mutating                        bool
	allowUICookie                   bool
	allowDashboardHTMX              bool
	requireDashboardManagementToken bool
}

func (s *Server) apiAuth(mutating bool) echo.MiddlewareFunc {
	return s.apiAuthWithOptions(apiAuthOptions{mutating: mutating})
}

func (s *Server) apiAuthWithOptions(opts apiAuthOptions) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			credential, err := s.authorizeAPIRequest(c, opts)
			if err != nil {
				return err
			}
			if err := s.applyAPIKeyRateLimit(c, credential); err != nil {
				s.recordAPIUsage(c, credential, start, err)
				return err
			}
			s.markAPIKeyLastUsed(credential)
			err = next(c)
			s.recordAPIUsage(c, credential, start, err)
			return err
		}
	}
}

func (s *Server) authorizeAPIRequest(c echo.Context, opts apiAuthOptions) (apikey.Credential, error) {
	if err := s.applyPreAuthIPRateLimit(c); err != nil {
		return apikey.Credential{}, err
	}

	token := s.apiToken()
	candidates := requestAPITokens(c.Request())
	if len(candidates) > 0 {
		var lastAuthErr *apikey.AuthError
		for _, candidate := range candidates {
			credential, err := s.authenticateCandidate(c.Request().Context(), candidate, token)
			if err == nil {
				s.setAPICredential(c, credential)
				return credential, nil
			}
			var authErr *apikey.AuthError
			if errors.As(err, &authErr) {
				if authErr.Code == "token_expired" || authErr.Code == "token_revoked" {
					return apikey.Credential{}, writeAPIAuthError(c, http.StatusUnauthorized, authErr.Code, authErr.Message)
				}
				lastAuthErr = authErr
				continue
			}
			s.logger.Warn("api key lookup failed", "error", err)
			return apikey.Credential{}, writeAPIAuthError(c, http.StatusServiceUnavailable, "service_unavailable", "API key authentication temporarily unavailable")
		}
		if lastAuthErr != nil {
			return apikey.Credential{}, writeAPIAuthError(c, http.StatusUnauthorized, lastAuthErr.Code, lastAuthErr.Message)
		}
		return apikey.Credential{}, writeAPIAuthError(c, http.StatusUnauthorized, "unauthorized", "Valid API token is required")
	}

	if opts.allowUICookie && s.authorizeUIAPICookie(c) {
		credential := apikey.StaticCredential()
		s.setAPICredential(c, credential)
		return credential, nil
	}

	if opts.allowDashboardHTMX && token == "" && authorizeDashboardHTMX(c) {
		if opts.requireDashboardManagementToken && !s.authorizeAPIKeyDashboardToken(c) {
			return apikey.Credential{}, writeAPIAuthError(c, http.StatusForbidden, "dashboard_token_required", "open the API Keys page from the dashboard before managing API keys")
		}
		credential := apikey.StaticCredential()
		s.setAPICredential(c, credential)
		return credential, nil
	}

	if token != "" {
		return apikey.Credential{}, writeAPIAuthError(c, http.StatusUnauthorized, "unauthorized", "Valid API token is required")
	}
	if serverAddressLoopback(s.serverAddr) {
		credential := apikey.StaticCredential()
		s.setAPICredential(c, credential)
		return credential, nil
	}
	message := "configure api_token or use an API key to enable API access on non-loopback hosts"
	if opts.mutating {
		message = "configure api_token or use an API key to enable API mutations on non-loopback hosts"
	}
	return apikey.Credential{}, writeAPIAuthError(c, http.StatusForbidden, "api_token_required", message)
}

func (s *Server) authenticateCandidate(ctx context.Context, candidate string, staticToken string) (apikey.Credential, error) {
	if s.apiKeys == nil {
		if strings.TrimSpace(staticToken) == "" {
			return apikey.Credential{}, &apikey.AuthError{Code: "unauthorized", Message: "Valid API token is required"}
		}
		if apiStaticTokenEqual(candidate, staticToken) {
			return apikey.StaticCredential(), nil
		}
		return apikey.Credential{}, &apikey.AuthError{Code: "unauthorized", Message: "Valid API token is required"}
	}
	return s.apiKeys.Authenticate(ctx, candidate, staticToken)
}

func (s *Server) setAPICredential(c echo.Context, credential apikey.Credential) {
	ctx := context.WithValue(c.Request().Context(), apiCredentialContextKey{}, credential)
	c.SetRequest(c.Request().WithContext(ctx))
}

func apiCredentialFromContext(ctx context.Context) (apikey.Credential, bool) {
	credential, ok := ctx.Value(apiCredentialContextKey{}).(apikey.Credential)
	return credential, ok
}

func (s *Server) requireScope(scope apikey.Scope) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, ok := apiCredentialFromContext(c.Request().Context())
			if !ok {
				return c.JSON(http.StatusForbidden, errorResponse("forbidden", "API key context missing"))
			}
			if !apikey.HasScope(credential.Scopes, scope) {
				return c.JSON(http.StatusForbidden, errorResponse("forbidden", "insufficient scope: "+string(scope)))
			}
			return next(c)
		}
	}
}

func (s *Server) requireProjectScope(scope apikey.Scope, projectParam string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			credential, ok := apiCredentialFromContext(c.Request().Context())
			if !ok {
				return c.JSON(http.StatusForbidden, errorResponse("forbidden", "API key context missing"))
			}
			if !apikey.HasScope(credential.Scopes, scope) {
				return c.JSON(http.StatusForbidden, errorResponse("forbidden", "insufficient scope: "+string(scope)))
			}
			projectID := requestProjectID(c, projectParam)
			if scope == apikey.ScopeWrite && !apikey.AllowsProject(credential.ProjectIDs, projectID) {
				return c.JSON(http.StatusForbidden, errorResponse("forbidden", "API key is not allowed for project "+projectID))
			}
			return next(c)
		}
	}
}

func requestProjectID(c echo.Context, param string) string {
	if value := strings.TrimSpace(c.Param(param)); value != "" {
		return value
	}
	if value := strings.TrimSpace(c.FormValue(param)); value != "" {
		return value
	}
	if value := strings.TrimSpace(c.QueryParam(param)); value != "" {
		return value
	}
	return ""
}

func (s *Server) apiToken() string {
	if s == nil {
		return ""
	}
	if token := strings.TrimSpace(s.lookupEnv(apiTokenEnv)); token != "" {
		return token
	}
	cfg := s.globalConfig
	if s.globalConfigSource != nil {
		cfg = s.globalConfigSource()
	}
	return strings.TrimSpace(cfg.APIToken)
}

func (s *Server) warnIfAPITokenMissingOnNonLoopback() {
	if s == nil || s.apiToken() != "" || serverAddressLoopback(s.serverAddr) {
		return
	}
	s.logger.Warn("api_token is not configured; API routes without scoped API keys will fail closed on non-loopback hosts", "addr", s.serverAddr)
}

func requestAPITokens(req *http.Request) []string {
	if req == nil {
		return nil
	}
	tokens := []string{}
	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	if bearer, ok := strings.CutPrefix(auth, "Bearer "); ok {
		if bearer = strings.TrimSpace(bearer); bearer != "" {
			tokens = append(tokens, bearer)
		}
	}
	if apiKey := strings.TrimSpace(req.Header.Get("X-API-Key")); apiKey != "" {
		tokens = append(tokens, apiKey)
	}
	return tokens
}

func serverAddressLoopback(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authorizeDashboardHTMX(c echo.Context) bool {
	if c == nil || c.Request() == nil || c.Request().Header.Get("HX-Request") != "true" {
		return false
	}
	req := c.Request()
	if !requestSameOriginDashboardSource(req) {
		return false
	}
	if requestHostLoopback(req.Host) {
		return requestRemoteAddrLoopback(req.RemoteAddr)
	}
	return requestDashboardHTMXTarget(req)
}

func requestHostLoopback(host string) bool {
	host = strings.TrimSpace(host)
	return host != "" && serverAddressLoopback(host)
}

func requestRemoteAddrLoopback(remoteAddr string) bool {
	remoteAddr = strings.TrimSpace(remoteAddr)
	return remoteAddr != "" && serverAddressLoopback(remoteAddr)
}

func requestDashboardHTMXTarget(req *http.Request) bool {
	if req == nil {
		return false
	}
	return strings.TrimSpace(req.Header.Get("HX-Target")) != ""
}

func requestSameOriginDashboardSource(req *http.Request) bool {
	if req == nil {
		return false
	}
	for _, value := range []string{
		req.Header.Get("HX-Current-URL"),
		req.Header.Get("Origin"),
		req.Header.Get("Referer"),
	} {
		if requestURLHostMatches(value, req.Host) {
			return true
		}
	}
	return false
}

func requestURLHostMatches(rawURL string, host string) bool {
	rawURL = strings.TrimSpace(rawURL)
	host = normalizeRequestHost(host)
	if rawURL == "" || host == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	return normalizeRequestHost(parsed.Host) == host
}

func normalizeRequestHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if parsedHost, port, err := net.SplitHostPort(host); err == nil {
		parsedHost = strings.Trim(strings.TrimSpace(parsedHost), "[]")
		return strings.ToLower(net.JoinHostPort(parsedHost, port))
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

func defaultLookupEnv(key string) string {
	return os.Getenv(key)
}

func (s *Server) uiAPICookie(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		s.setUIAPICookie(c)
		return next(c)
	}
}

func (s *Server) setUIAPICookie(c echo.Context) {
	if c == nil || c.Request() == nil || c.Request().Method != http.MethodGet {
		return
	}
	if strings.HasPrefix(c.Request().URL.Path, "/api/") || s.uiAPIToken() == "" {
		return
	}
	c.SetCookie(&http.Cookie{
		Name:     uiAPICookieName,
		Value:    s.uiAPIToken(),
		Path:     "/api/v1/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.Request().TLS != nil,
	})
}

func (s *Server) authorizeUIAPICookie(c echo.Context) bool {
	if c == nil || c.Request() == nil || c.Request().Header.Get("HX-Request") != "true" {
		return false
	}
	cookie, err := c.Cookie(uiAPICookieName)
	if err != nil {
		return false
	}
	return apiStaticTokenEqual(cookie.Value, s.uiAPIToken())
}

func (s *Server) uiAPIToken() string {
	token := s.apiToken()
	if token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("detent-ui-api-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func newDashboardAuthSecret() ([32]byte, error) {
	var secret [32]byte
	_, err := rand.Read(secret[:])
	return secret, err
}

func (s *Server) apiKeyDashboardToken() string {
	if s == nil {
		return ""
	}
	mac := hmac.New(sha256.New, s.dashboardAuthSecret[:])
	_, _ = mac.Write([]byte(apiKeyDashboardTokenPayload))
	return apiKeyDashboardTokenPayload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) setAPIKeyDashboardCookie(c echo.Context, token string) {
	if c == nil || c.Request() == nil || strings.TrimSpace(token) == "" {
		return
	}
	c.SetCookie(&http.Cookie{
		Name:     apiKeyDashboardCookieName,
		Value:    token,
		Path:     "/api/v1/keys",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   c.Request().TLS != nil,
	})
}

func (s *Server) authorizeAPIKeyDashboardToken(c echo.Context) bool {
	if c == nil || c.Request() == nil {
		return false
	}
	candidate := strings.TrimSpace(c.Request().Header.Get(apiKeyDashboardHeader))
	if candidate == "" || !apiStaticTokenEqual(candidate, s.apiKeyDashboardToken()) {
		return false
	}
	cookie, err := c.Cookie(apiKeyDashboardCookieName)
	if err != nil {
		return false
	}
	return apiStaticTokenEqual(candidate, cookie.Value)
}

func apiStaticTokenEqual(candidate string, token string) bool {
	candidate = strings.TrimSpace(candidate)
	token = strings.TrimSpace(token)
	if candidate == "" || token == "" {
		return false
	}
	candidateHash := sha256.Sum256([]byte(candidate))
	tokenHash := sha256.Sum256([]byte(token))
	return hmac.Equal(candidateHash[:], tokenHash[:])
}

func (s *Server) applyPreAuthIPRateLimit(c echo.Context) error {
	if s.ipLimiter == nil {
		return nil
	}
	allowed, remaining := s.ipLimiter.Allow(c.RealIP(), time.Now())
	s.setRateLimitHeaders(c, s.ipLimiter.Limit(), remaining)
	if allowed {
		return nil
	}
	c.Response().Header().Set("Retry-After", "60")
	return writeAPIAuthError(c, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded, retry after 60 seconds")
}

func (s *Server) applyAPIKeyRateLimit(c echo.Context, credential apikey.Credential) error {
	if s.keyLimiter == nil {
		return nil
	}
	key := strings.TrimSpace(credential.ID)
	if key == "" {
		key = apikey.StaticKeyID
	}
	allowed, remaining := s.keyLimiter.Allow(key, time.Now())
	s.setRateLimitHeaders(c, s.keyLimiter.Limit(), remaining)
	if allowed {
		return nil
	}
	c.Response().Header().Set("Retry-After", "60")
	return writeAPIAuthError(c, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded, retry after 60 seconds")
}

func writeAPIAuthError(c echo.Context, status int, code string, message string) error {
	if err := c.JSON(status, errorResponse(code, message)); err != nil {
		return err
	}
	return echo.NewHTTPError(status, message)
}

func (s *Server) setRateLimitHeaders(c echo.Context, limit int, remaining int) {
	c.Response().Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	c.Response().Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
}

func (s *Server) recordAPIUsage(c echo.Context, credential apikey.Credential, started time.Time, handlerErr error) {
	if s == nil || s.store == nil || credential.Static || strings.TrimSpace(credential.ID) == "" {
		return
	}
	status := c.Response().Status
	var httpErr *echo.HTTPError
	if errors.As(handlerErr, &httpErr) && httpErr.Code > 0 {
		status = httpErr.Code
	}
	if status == 0 {
		status = http.StatusOK
	}
	usage := store.APIUsageLog{
		APIKeyID:   credential.ID,
		Method:     c.Request().Method,
		Path:       c.Request().URL.Path,
		StatusCode: status,
		LatencyMS:  int(time.Since(started).Milliseconds()),
		IP:         c.RealIP(),
		UserAgent:  c.Request().UserAgent(),
		CreatedAt:  time.Now(),
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.RecordAPIUsageLog(ctx, usage); err != nil {
			s.logger.Warn("failed to log api usage", "error", err, "api_key_id", credential.ID, "key", credential.PrefixLast4)
		}
	}()
}

func (s *Server) markAPIKeyLastUsed(credential apikey.Credential) {
	if s == nil || s.store == nil || credential.Static || strings.TrimSpace(credential.ID) == "" {
		return
	}
	id := credential.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.store.MarkAPIKeyLastUsed(ctx, id, time.Now()); err != nil {
			s.logger.Warn("failed to update api key last used", "error", err, "api_key_id", id, "key", credential.PrefixLast4)
		}
	}()
}
