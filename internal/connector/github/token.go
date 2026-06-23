package github

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type TokenSource interface {
	Token(context.Context) (string, error)
}

type RefreshableTokenSource interface {
	TokenSource
	RefreshToken(context.Context) (string, error)
}

type TokenRefreshFunc func(context.Context) (string, error)

type staticTokenSource string

func StaticTokenSource(token string) TokenSource {
	return staticTokenSource(token)
}

func (s staticTokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(s))
	if token == "" {
		return "", ErrMissingToken
	}
	return token, nil
}

type refreshableTokenSource struct {
	mu      sync.Mutex
	token   string
	refresh TokenRefreshFunc
}

func NewRefreshableTokenSource(token string, refresh TokenRefreshFunc) TokenSource {
	return &refreshableTokenSource{
		token:   strings.TrimSpace(token),
		refresh: refresh,
	}
}

func (s *refreshableTokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token := strings.TrimSpace(s.token)
	if token == "" {
		return "", ErrMissingToken
	}
	return token, nil
}

func (s *refreshableTokenSource) RefreshToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.refresh == nil {
		return "", ErrMissingToken
	}
	token, err := s.refresh(ctx)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrMissingToken
	}
	s.mu.Lock()
	s.token = token
	s.mu.Unlock()
	return token, nil
}

type GHTokenFunc func(context.Context, string) (string, error)

type TokenResolverConfig struct {
	Endpoint                string
	APIKey                  string
	GitHubAppID             string
	GitHubAppPrivateKey     string
	GitHubAppPrivateKeyPath string
	GitHubAppInstallationID string
	HTTPClient              HTTPClient
	Now                     func() time.Time
	LookupEnv               func(string) string
	GHToken                 GHTokenFunc
}

type TokenResolver struct {
	cfg       TokenResolverConfig
	lookupEnv func(string) string
	mu        sync.Mutex
	app       *InstallationTokenSource
}

func NewTokenResolver(cfg TokenResolverConfig) *TokenResolver {
	lookupEnv := cfg.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	return &TokenResolver{
		cfg:       cfg,
		lookupEnv: lookupEnv,
	}
}

func (r *TokenResolver) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var appErr error
	if r.hasGitHubAppCredentials() {
		token, err := r.gitHubAppToken(ctx)
		if err == nil && strings.TrimSpace(token) != "" {
			return strings.TrimSpace(token), nil
		}
		appErr = err
	}

	if token := r.resolveSecret(r.cfg.APIKey); token != "" {
		return token, nil
	}
	if token := strings.TrimSpace(r.lookupEnv("GITHUB_TOKEN")); token != "" {
		return token, nil
	}
	if r.cfg.GHToken != nil {
		token, err := r.cfg.GHToken(ctx, r.cfg.Endpoint)
		if err == nil && strings.TrimSpace(token) != "" {
			return strings.TrimSpace(token), nil
		}
	}

	if appErr != nil {
		return "", appErr
	}
	return "", ErrMissingToken
}

func (r *TokenResolver) gitHubAppToken(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.app == nil {
		app, err := NewInstallationTokenSource(InstallationTokenConfig{
			Endpoint:       r.cfg.Endpoint,
			AppID:          r.cfg.GitHubAppID,
			InstallationID: r.cfg.GitHubAppInstallationID,
			PrivateKey:     r.cfg.GitHubAppPrivateKey,
			PrivateKeyPath: r.cfg.GitHubAppPrivateKeyPath,
			HTTPClient:     r.cfg.HTTPClient,
			Now:            r.cfg.Now,
			LookupEnv:      r.lookupEnv,
		})
		if err != nil {
			return "", err
		}
		r.app = app
	}

	return r.app.Token(ctx)
}

func (r *TokenResolver) hasGitHubAppCredentials() bool {
	return r.resolveSecret(r.cfg.GitHubAppID) != "" &&
		r.resolveSecret(r.cfg.GitHubAppInstallationID) != "" &&
		(r.resolveSecret(r.cfg.GitHubAppPrivateKey) != "" || r.resolveSecret(r.cfg.GitHubAppPrivateKeyPath) != "")
}

func (r *TokenResolver) resolveSecret(value string) string {
	return resolveSecretValue(value, r.lookupEnv)
}

func resolveSecretValue(value string, lookupEnv func(string) string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if after, ok := strings.CutPrefix(value, "$"); ok {
		name := after
		if envNamePattern.MatchString(name) {
			return strings.TrimSpace(lookupEnv(name))
		}
	}
	return value
}
