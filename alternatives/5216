package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo/v4"
)

const apiTokenEnv = "DETENT_API_TOKEN"

const uiAPICookieName = "detent_ui_api"

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

func (s *Server) trustLoopbackPeerRead() bool {
	if s == nil {
		return false
	}
	cfg := s.globalConfig
	if s.globalConfigSource != nil {
		cfg = s.globalConfigSource()
	}
	return cfg.TrustLoopbackPeerRead
}

func (s *Server) warnIfAPITokenMissingOnNonLoopback() {
	if s == nil || s.apiToken() != "" || serverAddressLoopback(s.serverAddr) {
		return
	}
	if s.trustLoopbackPeerRead() {
		s.logger.Warn("api_token is not configured; trusted loopback peers have read-only API access and all other API access without scoped API keys will fail closed", "addr", s.serverAddr)
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

func requestAPICredentialsSupplied(req *http.Request) bool {
	if req == nil {
		return false
	}
	return len(req.Header.Values("Authorization")) > 0 || len(req.Header.Values("X-API-Key")) > 0
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
	if requestAPICredentialsSupplied(c.Request()) {
		return
	}
	if strings.HasPrefix(c.Request().URL.Path, "/api/") || s.uiAPIToken() == "" {
		return
	}
	c.SetCookie(&http.Cookie{ // #nosec G124 -- HttpOnly and SameSiteLax are fixed below; Secure follows the request transport.
		Name:     uiAPICookieName,
		Value:    s.uiAPIToken(),
		Path:     "/api/v1/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.Request().TLS != nil,
	})
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
