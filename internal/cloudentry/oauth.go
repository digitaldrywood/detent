package cloudentry

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const oauthScope = "detent:mcp"
const oauthAccessPrefix = "oauth_access_"
const oauthAccessLifetime = time.Hour
const oauthRefreshLifetime = 30 * 24 * time.Hour

func (s *Service) registerOAuth() {
	s.echo.GET("/.well-known/oauth-protected-resource", s.oauthResourceMetadata)
	s.echo.GET("/.well-known/oauth-protected-resource/mcp", s.oauthResourceMetadata)
	s.echo.GET("/.well-known/oauth-authorization-server", s.oauthServerMetadata)
	s.echo.POST("/oauth/register", s.oauthRegister)
	s.echo.GET("/oauth/authorize", s.oauthAuthorize)
	s.echo.GET("/oauth/consent/:request", s.oauthConsent)
	s.echo.POST("/oauth/consent/:request", s.oauthApprove)
	s.echo.POST("/oauth/token", s.oauthToken)
	s.echo.GET("/api/cloud/account/oauth-grants", s.oauthGrantsJSON)
	s.echo.DELETE("/api/cloud/account/oauth-grants/:grant", s.oauthRevokeJSON)
}

func (s *Service) oauthResourceMetadata(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{"resource": s.config.PublicURL + "/mcp", "authorization_servers": []string{s.config.PublicURL}, "scopes_supported": []string{oauthScope}, "bearer_methods_supported": []string{"header"}})
}

func (s *Service) oauthServerMetadata(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"issuer": s.config.PublicURL, "authorization_endpoint": s.config.PublicURL + "/oauth/authorize", "token_endpoint": s.config.PublicURL + "/oauth/token", "registration_endpoint": s.config.PublicURL + "/oauth/register",
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"}, "scopes_supported": []string{oauthScope}, "authorization_response_iss_parameter_supported": true,
	})
}

func oauthError(c echo.Context, status int, code string) error {
	return c.JSON(status, map[string]string{"error": code})
}

func (s *Service) oauthChallenge(c echo.Context) error {
	c.Response().Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.config.PublicURL+`/.well-known/oauth-protected-resource", scope="`+oauthScope+`"`)
	return oauthError(c, http.StatusUnauthorized, "invalid_token")
}

type oauthClient struct {
	ID            string   `json:"client_id"`
	Name          string   `json:"client_name"`
	RedirectURIs  []string `json:"redirect_uris"`
	AuthMethod    string   `json:"token_endpoint_auth_method"`
	Secret        string   `json:"client_secret,omitempty"`
	GrantTypes    []string `json:"grant_types"`
	ResponseTypes []string `json:"response_types"`
}

func validOAuthRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func (s *Service) oauthRegister(c echo.Context) error {
	var client oauthClient
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10))
	if err := decoder.Decode(&client); err != nil {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata")
	}
	if client.Name == "" {
		client.Name = "MCP assistant"
	}
	if len(client.Name) > 128 || len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 20 {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata")
	}
	for _, uri := range client.RedirectURIs {
		if !validOAuthRedirect(uri) {
			return oauthError(c, http.StatusBadRequest, "invalid_redirect_uri")
		}
	}
	if client.AuthMethod == "" {
		client.AuthMethod = "none"
	}
	if !slices.Contains([]string{"none", "client_secret_post", "client_secret_basic"}, client.AuthMethod) {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata")
	}
	for _, grant := range client.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			return oauthError(c, http.StatusBadRequest, "invalid_client_metadata")
		}
	}
	if len(client.ResponseTypes) > 0 && (len(client.ResponseTypes) != 1 || client.ResponseTypes[0] != "code") {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata")
	}
	id, err := s.config.generateToken()
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	client.ID = "oauth_client_" + id
	client.Secret = ""
	if client.AuthMethod != "none" {
		client.Secret, err = s.config.generateToken()
		if err != nil {
			return oauthError(c, 503, "server_error")
		}
	}
	tx, err := s.auth.store.db.BeginTx(c.Request().Context(), nil)
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(c.Request().Context(), "INSERT INTO oauth_clients(id,name,auth_method,secret_hash,created_at) VALUES(?,?,?,?,?)", client.ID, client.Name, client.AuthMethod, apikey.HashToken(client.Secret), formatTime(s.config.now()))
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	for _, uri := range slices.Compact(slices.Sorted(slices.Values(client.RedirectURIs))) {
		if _, err = tx.ExecContext(c.Request().Context(), "INSERT INTO oauth_redirect_uris(client_id,uri) VALUES(?,?)", client.ID, uri); err != nil {
			return oauthError(c, 503, "server_error")
		}
	}
	if err = tx.Commit(); err != nil {
		return oauthError(c, 503, "server_error")
	}
	client.GrantTypes = []string{"authorization_code", "refresh_token"}
	client.ResponseTypes = []string{"code"}
	return c.JSON(http.StatusCreated, client)
}

func (s *Service) oauthAuthorize(c echo.Context) error {
	q := c.QueryParams()
	for _, key := range []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method", "resource", "scope"} {
		if len(q[key]) > 1 {
			return oauthError(c, 400, "invalid_request")
		}
	}
	var name string
	err := s.auth.store.reader.QueryRowContext(c.Request().Context(), "SELECT c.name FROM oauth_clients c JOIN oauth_redirect_uris r ON r.client_id=c.id WHERE c.id=? AND r.uri=?", q.Get("client_id"), q.Get("redirect_uri")).Scan(&name)
	if err != nil {
		return oauthError(c, 400, "invalid_request")
	}
	if q.Get("response_type") != "code" || q.Get("resource") != s.config.PublicURL+"/mcp" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) > 2048 {
		return oauthError(c, 400, "invalid_request")
	}
	challenge, err := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if err != nil || len(challenge) != sha256.Size || len(q.Get("code_challenge")) != 43 {
		return oauthError(c, 400, "invalid_request")
	}
	if q.Get("scope") != "" && q.Get("scope") != oauthScope {
		return oauthError(c, 400, "invalid_scope")
	}
	id, err := s.config.generateToken()
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	browser, err := s.config.generateToken()
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	expires := s.config.now().Add(10 * time.Minute)
	_, err = s.auth.store.db.ExecContext(c.Request().Context(), `INSERT INTO oauth_requests(id,client_id,redirect_uri,state,challenge,resource,browser_hash,expires_at) VALUES(?,?,?,?,?,?,?,?)`, id, q.Get("client_id"), q.Get("redirect_uri"), q.Get("state"), q.Get("code_challenge"), q.Get("resource"), apikey.HashToken(browser), formatTime(expires))
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	s.setCookie(c, "oauth", browser, expires)
	return c.Redirect(http.StatusSeeOther, "/oauth/consent/"+id)
}

type oauthRequest struct{ ID, Client, Name, Redirect, State, Challenge, Resource, BrowserHash string }

func (s *Service) oauthBrowserRequest(c echo.Context) (oauthRequest, error) {
	var r oauthRequest
	var expires string
	err := s.auth.store.reader.QueryRowContext(c.Request().Context(), `SELECT r.id,r.client_id,c.name,r.redirect_uri,r.state,r.challenge,r.resource,r.browser_hash,r.expires_at FROM oauth_requests r JOIN oauth_clients c ON c.id=r.client_id WHERE r.id=? AND r.code_hash IS NULL AND r.consumed_at IS NULL`, c.Param("request")).Scan(&r.ID, &r.Client, &r.Name, &r.Redirect, &r.State, &r.Challenge, &r.Resource, &r.BrowserHash, &expires)
	if err != nil {
		return r, err
	}
	cookie, err := c.Cookie(s.cookieName("oauth"))
	if err != nil {
		return r, err
	}
	at, err := parseTime(expires)
	if err != nil || !at.After(s.config.now()) || subtle.ConstantTimeCompare([]byte(apikey.HashToken(cookie.Value)), []byte(r.BrowserHash)) != 1 {
		return r, errNoSession
	}
	return r, nil
}

func (s *Service) oauthConsent(c echo.Context) error {
	r, err := s.oauthBrowserRequest(c)
	if err != nil {
		return oauthError(c, 400, "invalid_request")
	}
	session, err := s.accountKeySession(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?"+url.Values{"return": {"/oauth/consent/" + r.ID}}.Encode())
	}
	choices, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	data := templates.HostedPageData{Mode: "oauth", Title: "Connect assistant", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""), OAuthClient: r.Name, OAuthRequest: r.ID, OAuthRedirect: r.Redirect}
	for _, choice := range choices {
		data.Organizations = append(data.Organizations, templates.HostedOrganizationChoice{ID: choice.ID, Name: choice.Name})
	}
	return s.render(c, http.StatusOK, data)
}

func (s *Service) oauthApprove(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return oauthError(c, 403, "access_denied")
	}
	if !s.csrfValid(c, session, "") {
		return oauthError(c, 403, "access_denied")
	}
	r, err := s.oauthBrowserRequest(c)
	if err != nil {
		return oauthError(c, 400, "invalid_request")
	}
	redirect, err := url.Parse(r.Redirect)
	if err != nil {
		return oauthError(c, 400, "invalid_request")
	}
	query := redirect.Query()
	query.Set("state", r.State)
	query.Set("iss", s.config.PublicURL)
	if c.FormValue("decision") == "deny" {
		if _, err := s.auth.store.db.ExecContext(c.Request().Context(), "UPDATE oauth_requests SET consumed_at=? WHERE id=? AND consumed_at IS NULL", formatTime(s.config.now()), r.ID); err != nil {
			return oauthError(c, 503, "server_error")
		}
		s.setCookie(c, "oauth", "", time.Unix(1, 0))
		query.Set("error", "access_denied")
		redirect.RawQuery = query.Encode()
		return c.Redirect(http.StatusSeeOther, redirect.String())
	}
	if c.FormValue("decision") != "allow" {
		return oauthError(c, 400, "invalid_request")
	}
	org, err := s.readyOrganization(c.Request().Context(), c.FormValue("organization"))
	if err != nil {
		return oauthError(c, 403, "access_denied")
	}
	reach, err := s.ownerKeyReach(c.Request().Context(), session.Subject, session.Email, org)
	if err != nil {
		return oauthError(c, 403, "access_denied")
	}
	policy, err := s.personalKeyPolicy(c.Request().Context(), org.ID)
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	if policy != "allowed" {
		return oauthError(c, 403, "access_denied")
	}
	permission := apikey.ScopeWrite
	switch reach.Role {
	case "viewer":
		permission = apikey.ScopeRead
	case "owner", "admin":
		permission = apikey.ScopeAdmin
	}
	id, err := s.config.generateToken()
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	id = "oauth_grant_" + id
	code, err := s.config.generateToken()
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	ctx := c.Request().Context()
	tx, err := s.auth.store.db.BeginTx(ctx, nil)
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO oauth_grants(id,client_id,owner_subject,owner_email,organization_id,permission,resource,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, r.Client, session.Subject, session.Email, org.ID, permission, r.Resource, formatTime(s.config.now()))
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	result, err := tx.ExecContext(ctx, `UPDATE oauth_requests SET code_hash=?,code_expires_at=?,grant_id=? WHERE id=? AND code_hash IS NULL AND consumed_at IS NULL AND julianday(expires_at)>julianday(?)`, apikey.HashToken(code), formatTime(s.config.now().Add(5*time.Minute)), id, r.ID, formatTime(s.config.now()))
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return oauthError(c, 400, "invalid_request")
	}
	if err := tx.Commit(); err != nil {
		return oauthError(c, 503, "server_error")
	}
	s.setCookie(c, "oauth", "", time.Unix(1, 0))
	query.Set("code", code)
	redirect.RawQuery = query.Encode()
	return c.Redirect(http.StatusSeeOther, redirect.String())
}

func oauthPKCE(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func validOAuthVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-._~", r) {
			return false
		}
	}
	return true
}

func (s *Service) oauthClientAuthentication(c echo.Context, form url.Values) (string, error) {
	id, secret := form.Get("client_id"), form.Get("client_secret")
	method := "none"
	if secret != "" {
		method = "client_secret_post"
	}
	if c.Request().Header.Get("Authorization") != "" {
		user, password, ok := c.Request().BasicAuth()
		if !ok || secret != "" {
			return "", errNoSession
		}
		var err error
		user, err = url.QueryUnescape(user)
		if err != nil {
			return "", errNoSession
		}
		password, err = url.QueryUnescape(password)
		if err != nil {
			return "", errNoSession
		}
		if id != "" && id != user {
			return "", errNoSession
		}
		id, secret, method = user, password, "client_secret_basic"
	}
	var storedMethod, hash string
	if err := s.auth.store.reader.QueryRowContext(c.Request().Context(), "SELECT auth_method,secret_hash FROM oauth_clients WHERE id=?", id).Scan(&storedMethod, &hash); err != nil {
		return "", err
	}
	if storedMethod != method || method != "none" && subtle.ConstantTimeCompare([]byte(hash), []byte(apikey.HashToken(secret))) != 1 {
		return "", errNoSession
	}
	return id, nil
}

func (s *Service) oauthToken(c echo.Context) error {
	media, _, err := mime.ParseMediaType(c.Request().Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		return oauthError(c, 400, "invalid_request")
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 16*1024)
	if err := c.Request().ParseForm(); err != nil {
		return oauthError(c, 400, "invalid_request")
	}
	form := c.Request().PostForm
	for _, values := range form {
		if len(values) != 1 {
			return oauthError(c, 400, "invalid_request")
		}
	}
	client, err := s.oauthClientAuthentication(c, form)
	if err != nil {
		c.Response().Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
		return oauthError(c, 401, "invalid_client")
	}
	if form.Get("resource") != s.config.PublicURL+"/mcp" {
		return oauthError(c, 400, "invalid_target")
	}
	if form.Get("scope") != "" && form.Get("scope") != oauthScope {
		return oauthError(c, 400, "invalid_scope")
	}
	if form.Get("grant_type") != "authorization_code" && form.Get("grant_type") != "refresh_token" {
		return oauthError(c, 400, "unsupported_grant_type")
	}
	response, err := s.exchangeOAuthToken(c.Request().Context(), client, form)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errNoSession) {
			return oauthError(c, 400, "invalid_grant")
		}
		return oauthError(c, 503, "server_error")
	}
	return c.JSON(http.StatusOK, response)
}
