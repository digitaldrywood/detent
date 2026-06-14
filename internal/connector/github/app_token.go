package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	jwtBackdate        = 60 * time.Second
	jwtLifetime        = 10 * time.Minute
	tokenRefreshWindow = 5 * time.Minute
)

type InstallationTokenConfig struct {
	Endpoint       string
	AppID          string
	InstallationID string
	PrivateKey     string
	PrivateKeyPath string
	HTTPClient     HTTPClient
	Now            func() time.Time
	LookupEnv      func(string) string
}

type InstallationTokenSource struct {
	endpoint       string
	appID          string
	installationID string
	privateKey     string
	httpClient     HTTPClient
	now            func() time.Time
	mu             sync.Mutex
	cachedToken    string
	expiresAt      time.Time
	details        InstallationTokenDetails
}

type InstallationTokenDetails struct {
	Token               string
	ExpiresAt           time.Time
	Permissions         map[string]string
	RepositorySelection string
	Repositories        []InstallationRepository
}

type InstallationRepository struct {
	FullName string
}

func NewInstallationTokenSource(cfg InstallationTokenConfig) (*InstallationTokenSource, error) {
	lookupEnv := cfg.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultGraphQLEndpoint
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}

	appID := resolveSecretValue(cfg.AppID, lookupEnv)
	installationID := resolveSecretValue(cfg.InstallationID, lookupEnv)
	privateKey, err := privateKeyFromConfig(cfg, lookupEnv)
	if err != nil {
		return nil, err
	}
	if appID == "" || installationID == "" || privateKey == "" {
		return nil, ErrMissingAppConfig
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = NewPooledHTTPClient(HTTPTransportConfig{})
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &InstallationTokenSource{
		endpoint:       endpoint,
		appID:          appID,
		installationID: installationID,
		privateKey:     normalizePrivateKey(privateKey),
		httpClient:     httpClient,
		now:            now,
	}, nil
}

func (s *InstallationTokenSource) Token(ctx context.Context) (string, error) {
	details, err := s.TokenDetails(ctx)
	if err != nil {
		return "", err
	}
	return details.Token, nil
}

func (s *InstallationTokenSource) TokenDetails(ctx context.Context) (InstallationTokenDetails, error) {
	if err := ctx.Err(); err != nil {
		return InstallationTokenDetails{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.cachedToken != "" && s.expiresAt.After(now.Add(tokenRefreshWindow)) {
		return s.details, nil
	}

	jwt, err := s.jwt(now)
	if err != nil {
		return InstallationTokenDetails{}, err
	}

	details, err := s.requestInstallationToken(ctx, jwt)
	if err != nil {
		return InstallationTokenDetails{}, err
	}
	s.cachedToken = details.Token
	s.expiresAt = details.ExpiresAt
	s.details = details

	return details, nil
}

func (s *InstallationTokenSource) jwt(now time.Time) (string, error) {
	header, err := jsonSegment(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := jsonSegment(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": s.appID,
	})
	if err != nil {
		return "", err
	}

	signingInput := header + "." + payload
	signature, err := signJWT(signingInput, s.privateKey)
	if err != nil {
		return "", err
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *InstallationTokenSource) requestInstallationToken(ctx context.Context, jwt string) (InstallationTokenDetails, error) {
	endpoint, err := installationTokenURL(s.endpoint, s.installationID)
	if err != nil {
		return InstallationTokenDetails{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return InstallationTokenDetails{}, fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return InstallationTokenDetails{}, ctxErr
		}
		return InstallationTokenDetails{}, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() {
		if err := drainAndClose(resp.Body); err != nil {
			return
		}
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return InstallationTokenDetails{}, fmt.Errorf("%w: read response: %w", ErrTransient, err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return InstallationTokenDetails{}, classifyStatus(resp.StatusCode, resp.Header, raw)
	}

	var decoded struct {
		Token               string            `json:"token"`
		ExpiresAt           time.Time         `json:"expires_at"`
		Permissions         map[string]string `json:"permissions"`
		RepositorySelection string            `json:"repository_selection"`
		Repositories        []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return InstallationTokenDetails{}, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	if strings.TrimSpace(decoded.Token) == "" || decoded.ExpiresAt.IsZero() {
		return InstallationTokenDetails{}, ErrInvalidResponse
	}

	details := InstallationTokenDetails{
		Token:               strings.TrimSpace(decoded.Token),
		ExpiresAt:           decoded.ExpiresAt,
		Permissions:         make(map[string]string, len(decoded.Permissions)),
		RepositorySelection: strings.TrimSpace(decoded.RepositorySelection),
		Repositories:        make([]InstallationRepository, 0, len(decoded.Repositories)),
	}
	for key, value := range decoded.Permissions {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			details.Permissions[key] = value
		}
	}
	for _, repo := range decoded.Repositories {
		fullName := strings.TrimSpace(repo.FullName)
		if fullName != "" {
			details.Repositories = append(details.Repositories, InstallationRepository{FullName: fullName})
		}
	}
	return details, nil
}

func privateKeyFromConfig(cfg InstallationTokenConfig, lookupEnv func(string) string) (string, error) {
	if privateKey := resolveSecretValue(cfg.PrivateKey, lookupEnv); privateKey != "" {
		return privateKey, nil
	}

	path := resolveSecretValue(cfg.PrivateKeyPath, lookupEnv)
	if path == "" {
		return "", ErrMissingAppConfig
	}

	expanded, err := expandPath(path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(expanded)
	if err != nil {
		return "", fmt.Errorf("%w: read private key: %w", ErrMissingAppConfig, err)
	}
	return string(raw), nil
}

func expandPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}

func normalizePrivateKey(privateKey string) string {
	return strings.ReplaceAll(privateKey, `\n`, "\n")
}

func installationTokenURL(endpoint string, installationID string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}

	basePath := strings.TrimRight(parsed.Path, "/")
	if parsed.Host == "api.github.com" {
		basePath = ""
	} else if strings.HasSuffix(basePath, "/api/graphql") {
		basePath = strings.TrimSuffix(basePath, "/api/graphql") + "/api/v3"
	} else if strings.HasSuffix(basePath, "/graphql") {
		basePath = strings.TrimSuffix(basePath, "/graphql")
	}

	parsed.Path = strings.TrimRight(basePath, "/") + "/app/installations/" + url.PathEscape(installationID) + "/access_tokens"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func jsonSegment(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func signJWT(signingInput string, privateKey string) ([]byte, error) {
	block, _ := pem.Decode([]byte(privateKey))
	if block == nil {
		return nil, ErrInvalidPrivateKey
	}

	key, err := parseRSAPrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPrivateKey, err)
	}
	return signature, nil
}

func parseRSAPrivateKey(raw []byte) (*rsa.PrivateKey, error) {
	key, err := x509.ParsePKCS1PrivateKey(raw)
	if err == nil {
		return key, nil
	}

	parsed, err := x509.ParsePKCS8PrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPrivateKey, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, ErrInvalidPrivateKey
	}
	return key, nil
}
