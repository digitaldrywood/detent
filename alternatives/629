package cloudentry

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const githubConnectionBase = "/api/cloud/account/github"

var setupReturnPath = regexp.MustCompile(`^(/[A-Za-z0-9_-]+)*/projects/[A-Za-z0-9_-]+/setup$`)

var errGitHubUserRevoked = errors.New("GitHub user authorization revoked")

type githubConnectionState struct {
	Session  string    `json:"session"`
	Nonce    string    `json:"nonce"`
	ReturnTo string    `json:"return_to"`
	Verifier string    `json:"verifier"`
	Phase    string    `json:"phase"`
	Expires  time.Time `json:"expires"`
}

type githubRepositoryList struct {
	Available    bool                    `json:"available"`
	Connected    bool                    `json:"connected"`
	Reconnect    bool                    `json:"reconnect"`
	Repositories []runnerauth.Repository `json:"repositories"`
}

func (s *Service) registerGitHubConnection() {
	s.echo.POST(githubConnectionBase+"/connect", s.connectGitHub)
	s.echo.GET(githubConnectionBase+"/repositories", s.githubRepositories)
	s.echo.GET("/auth/github/callback", s.completeGitHubConnection)
}

func (s *Service) githubConnectionAvailable() bool {
	return s.config.GitHubApp != nil && s.config.GitHubUserClientID != "" && s.config.GitHubUserClientSecret != ""
}

func (s *Service) githubUserClient() *http.Client {
	client := http.Client{Timeout: 20 * time.Second}
	if s.config.GitHubApp != nil {
		if configured, ok := s.config.GitHubApp.HTTPClient.(*http.Client); ok {
			client = *configured
			client.Timeout = 20 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (s *Service) githubOAuthConfig() oauth2.Config {
	return oauth2.Config{ClientID: s.config.GitHubUserClientID, ClientSecret: s.config.GitHubUserClientSecret,
		RedirectURL: s.config.PublicURL + "/auth/github/callback",
		Endpoint:    oauth2.Endpoint{AuthURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token", AuthStyle: oauth2.AuthStyleInParams}}
}

func (s *Service) connectGitHub(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return c.NoContent(http.StatusUnauthorized)
	}
	if !s.githubConnectionAvailable() {
		return c.NoContent(http.StatusNotFound)
	}
	var input struct {
		ReturnTo string `json:"return_to"`
	}
	if err := json.NewDecoder(io.LimitReader(c.Request().Body, 4096)).Decode(&input); err != nil || !validSetupReturn(input.ReturnTo) {
		return oauthError(c, http.StatusBadRequest, "invalid_request")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()
	source, err := github.NewAppTokenSource(*s.config.GitHubApp)
	if err != nil {
		return oauthError(c, http.StatusServiceUnavailable, "github_unavailable")
	}
	token, err := source.Token(ctx)
	if err != nil {
		return oauthError(c, http.StatusServiceUnavailable, "github_unavailable")
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := s.githubUserGET(ctx, token, "/app", &app); err != nil || !safeID(app.Slug) {
		return oauthError(c, http.StatusServiceUnavailable, "github_unavailable")
	}
	nonce, err := s.config.generateToken()
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	state := githubConnectionState{Session: session.Hash, Nonce: nonce, ReturnTo: input.ReturnTo, Phase: "install", Expires: s.config.now().Add(10 * time.Minute)}
	if err := s.saveGitHubState(c, state); err != nil {
		return oauthError(c, 503, "server_error")
	}
	return c.JSON(http.StatusOK, map[string]string{"url": "https://github.com/apps/" + url.PathEscape(app.Slug) + "/installations/new?state=" + url.QueryEscape(nonce)})
}

func validSetupReturn(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u == nil {
		return false
	}
	return len(value) <= 2048 && u.Scheme == "" && u.Host == "" && u.User == nil && u.Fragment == "" && !strings.ContainsAny(value, "\r\n\\") && setupReturnPath.MatchString(u.Path)
}

func (s *Service) saveGitHubState(c echo.Context, state githubConnectionState) error {
	sealed, err := s.sealOAuth("github-connect", state)
	if err != nil {
		return err
	}
	s.setCookie(c, "github-connect", sealed, state.Expires)
	return nil
}

func (s *Service) completeGitHubConnection(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return c.NoContent(http.StatusUnauthorized)
	}
	cookie, err := c.Cookie(s.cookieName("github-connect"))
	if err != nil && c.QueryParam("installation_id") != "" {
		return c.Redirect(http.StatusSeeOther, "/organizations")
	}
	if err != nil || !s.githubConnectionAvailable() {
		return oauthError(c, 400, "invalid_request")
	}
	var state githubConnectionState
	if s.openOAuth("github-connect", cookie.Value, &state) != nil || state.Session != session.Hash || !state.Expires.After(s.config.now()) || subtle.ConstantTimeCompare([]byte(state.Nonce), []byte(c.QueryParam("state"))) != 1 {
		return oauthError(c, 400, "invalid_request")
	}
	s.setCookie(c, "github-connect", "", time.Unix(1, 0))
	if state.Phase == "install" {
		if c.QueryParam("installation_id") == "" {
			return c.Redirect(http.StatusSeeOther, state.ReturnTo)
		}
		state.Phase, state.Verifier = "authorize", oauth2.GenerateVerifier()
		state.Nonce, err = s.config.generateToken()
		if err != nil {
			return oauthError(c, 503, "server_error")
		}
		if err := s.saveGitHubState(c, state); err != nil {
			return oauthError(c, 503, "server_error")
		}
		config := s.githubOAuthConfig()
		return c.Redirect(http.StatusSeeOther, config.AuthCodeURL(state.Nonce, oauth2.S256ChallengeOption(state.Verifier), oauth2.SetAuthURLParam("prompt", "select_account")))
	}
	if state.Phase != "authorize" || c.QueryParam("code") == "" {
		return c.Redirect(http.StatusSeeOther, state.ReturnTo)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()
	config := s.githubOAuthConfig()
	token, err := config.Exchange(context.WithValue(ctx, oauth2.HTTPClient, s.githubUserClient()), c.QueryParam("code"), oauth2.VerifierOption(state.Verifier))
	if err != nil || token.AccessToken == "" {
		return c.Redirect(http.StatusSeeOther, state.ReturnTo)
	}
	if err := s.saveGitHubConnection(ctx, session.Subject, token); err != nil {
		return oauthError(c, 503, "server_error")
	}
	return c.Redirect(http.StatusSeeOther, state.ReturnTo)
}

func (s *Service) saveGitHubConnection(ctx context.Context, owner string, token *oauth2.Token) error {
	sealed, err := s.auth.sealToken("github-user-access", owner, token.AccessToken)
	if err != nil {
		return err
	}
	id, err := s.config.generateToken()
	if err != nil {
		return err
	}
	var expires *string
	if !token.Expiry.IsZero() {
		value := formatTime(token.Expiry)
		expires = &value
	}
	_, err = s.auth.store.db.ExecContext(ctx, `INSERT INTO github_user_connections(owner_subject,id,access_token,expires_at,created_at) VALUES(?,?,?,?,?) ON CONFLICT(owner_subject) DO UPDATE SET access_token=excluded.access_token,expires_at=excluded.expires_at,created_at=excluded.created_at,revoked_at=NULL`, owner, "github_"+id, sealed, expires, formatTime(s.config.now()))
	return err
}

func (s *Service) githubRepositories(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return c.NoContent(http.StatusUnauthorized)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	result := githubRepositoryList{Available: s.githubConnectionAvailable(), Repositories: []runnerauth.Repository{}}
	if !result.Available {
		return c.JSON(http.StatusOK, result)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()
	var sealed string
	var expires, revoked sql.NullString
	err = s.auth.store.reader.QueryRowContext(ctx, `SELECT access_token,expires_at,revoked_at FROM github_user_connections WHERE owner_subject=?`, session.Subject).Scan(&sealed, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return c.JSON(http.StatusOK, result)
	}
	if err != nil {
		return oauthError(c, 503, "server_error")
	}
	result.Reconnect = true
	if revoked.Valid {
		return c.JSON(http.StatusOK, result)
	}
	if expires.Valid {
		expiry, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil || !expiry.After(s.config.now()) {
			return c.JSON(http.StatusOK, result)
		}
	}
	token, err := s.auth.openToken("github-user-access", session.Subject, sealed)
	if err != nil {
		return c.JSON(http.StatusOK, result)
	}
	repos, err := s.listGitHubUserRepositories(ctx, token)
	if err != nil {
		if errors.Is(err, errGitHubUserRevoked) {
			if _, err := s.auth.store.db.ExecContext(ctx, `UPDATE github_user_connections SET revoked_at=?,access_token='' WHERE owner_subject=? AND access_token=? AND revoked_at IS NULL`, formatTime(s.config.now()), session.Subject, sealed); err != nil {
				return oauthError(c, 503, "server_error")
			}
		}
		return c.JSON(http.StatusOK, result)
	}
	result.Connected, result.Reconnect, result.Repositories = true, false, repos
	return c.JSON(http.StatusOK, result)
}

func (s *Service) githubUserGET(ctx context.Context, token, path string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := s.githubUserClient().Do(request)
	if err != nil {
		return errors.New("GitHub listing unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return errGitHubUserRevoked
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("GitHub listing unavailable")
	}
	return json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(out)
}

func (s *Service) listGitHubUserRepositories(ctx context.Context, token string) ([]runnerauth.Repository, error) {
	ids := []int64{}
	for page := 1; ; page++ {
		var result struct {
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		if err := s.githubUserGET(ctx, token, "/user/installations?per_page=100&page="+strconv.Itoa(page), &result); err != nil {
			return nil, err
		}
		for _, installation := range result.Installations {
			ids = append(ids, installation.ID)
		}
		if len(result.Installations) < 100 {
			break
		}
	}
	repos := []runnerauth.Repository{}
	seen := make(map[string]bool)
	for _, id := range ids {
		for page := 1; ; page++ {
			var result struct {
				Repositories []struct {
					Name  string `json:"name"`
					Owner struct {
						Login string `json:"login"`
					} `json:"owner"`
					Private  bool       `json:"private"`
					PushedAt *time.Time `json:"pushed_at"`
				} `json:"repositories"`
			}
			path := fmt.Sprintf("/user/installations/%d/repositories?per_page=100&page=%d", id, page)
			if err := s.githubUserGET(ctx, token, path, &result); err != nil {
				return nil, err
			}
			for _, repo := range result.Repositories {
				key := strings.ToLower(repo.Owner.Login + "/" + repo.Name)
				if !seen[key] {
					repos = append(repos, runnerauth.Repository{Owner: repo.Owner.Login, Name: repo.Name, Private: repo.Private, PushedAt: repo.PushedAt})
					seen[key] = true
				}
			}
			if len(result.Repositories) < 100 {
				break
			}
		}
	}
	return repos, nil
}
