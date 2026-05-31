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
		httpClient = &http.Client{Timeout: 30 * time.Second}
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
	if err := ctx.Err(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.cachedToken != "" && s.expiresAt.After(now.Add(tokenRefreshWindow)) {
		return s.cachedToken, nil
	}

	jwt, err := s.jwt(now)
	if err != nil {
		return "", err
	}

	token, expiresAt, err := s.requestInstallationToken(ctx, jwt)
	if err != nil {
		return "", err
	}
	s.cachedToken = token
	s.expiresAt = expiresAt

	return token, nil
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

func (s *InstallationTokenSource) requestInstallationToken(ctx context.Context, jwt string) (string, time.Time, error) {
	endpoint, err := installationTokenURL(s.endpoint, s.installationID)
	if err != nil {
		return "", time.Time{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", time.Time{}, ctxErr
		}
		return "", time.Time{}, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: read response: %w", ErrTransient, err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, classifyStatus(resp.StatusCode, resp.Header, raw)
	}

	var decoded struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	if strings.TrimSpace(decoded.Token) == "" || decoded.ExpiresAt.IsZero() {
		return "", time.Time{}, ErrInvalidResponse
	}

	return decoded.Token, decoded.ExpiresAt, nil
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

	basePath := parsed.Path
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
