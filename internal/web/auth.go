package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo/v4"
)

const apiTokenEnv = "DETENT_API_TOKEN"

const uiAPICookieName = "detent_ui_api"

type apiAuthOptions struct {
	mutating      bool
	allowUICookie bool
}

func (s *Server) apiAuth(mutating bool) echo.MiddlewareFunc {
	return s.apiAuthWithOptions(apiAuthOptions{mutating: mutating})
}

func (s *Server) apiAuthWithOptions(opts apiAuthOptions) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			authorized, err := s.authorizeAPIRequest(c, opts)
			if !authorized || err != nil {
				return err
			}
			return next(c)
		}
	}
}

func (s *Server) authorizeAPIRequest(c echo.Context, opts apiAuthOptions) (bool, error) {
	token := s.apiToken()
	if token != "" {
		for _, candidate := range requestAPITokens(c.Request()) {
			if constantTimeTokenEqual(candidate, token) {
				return true, nil
			}
		}
		if opts.allowUICookie && s.authorizeUIAPICookie(c) {
			return true, nil
		}
		return false, c.JSON(http.StatusUnauthorized, errorResponse("unauthorized", "Valid API token is required"))
	}
	if serverAddressLoopback(s.serverAddr) {
		return true, nil
	}
	message := "configure api_token or DETENT_API_TOKEN to enable API access on non-loopback hosts"
	if opts.mutating {
		message = "configure api_token or DETENT_API_TOKEN to enable API mutations on non-loopback hosts"
	}
	return false, c.JSON(http.StatusForbidden, errorResponse("api_token_required", message))
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
	s.logger.Warn("api_token is not configured; API routes will fail closed on non-loopback hosts", "addr", s.serverAddr)
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

func constantTimeTokenEqual(candidate string, token string) bool {
	candidate = strings.TrimSpace(candidate)
	token = strings.TrimSpace(token)
	if candidate == "" || token == "" {
		return false
	}
	candidateHash := sha256.Sum256([]byte(candidate))
	tokenHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(candidateHash[:], tokenHash[:]) == 1
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
	return constantTimeTokenEqual(cookie.Value, s.uiAPIToken())
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
