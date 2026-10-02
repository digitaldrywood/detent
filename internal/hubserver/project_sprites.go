package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

var errSpritesValidation = errors.New("sprites rejected the token or could not validate its organization")

// validateSpritesToken uses only the fixed Sprites origin. Redirects are refused
// and neither provider bodies nor transport errors are returned or logged.
func validateSpritesToken(ctx context.Context, configured *http.Client, token []byte) (string, error) {
	if len(token) == 0 || len(token) > 8192 {
		return "", errSpritesValidation
	}
	for _, b := range token {
		if b <= ' ' || b >= 127 {
			return "", errSpritesValidation
		}
	}
	// Sprites organization tokens carry org-slug/org-id/token-id/token-value.
	// A successful authenticated list validates that prefix even for an empty org.
	parts := strings.Split(string(token), "/")
	if len(parts) != 4 {
		return "", errSpritesValidation
	}
	for _, part := range parts {
		if part == "" {
			return "", errSpritesValidation
		}
	}
	slug := strings.Clone(parts[0])
	if !validSpritesSlug(slug) {
		return "", errSpritesValidation
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.sprites.dev/v1/sprites", nil)
	if err != nil {
		return "", errSpritesValidation
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	defer request.Header.Del("Authorization")
	client := http.Client{Timeout: 15 * time.Second}
	if configured != nil {
		client = *configured
	}
	if client.Timeout <= 0 || client.Timeout > 15*time.Second {
		client.Timeout = 15 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", errSpritesValidation
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", errSpritesValidation
	}
	var result struct {
		Sprites []struct {
			OrganizationSlug string `json:"org_slug"`
			Organization     string `json:"organization"`
		} `json:"sprites"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil || result.Sprites == nil {
		return "", errSpritesValidation
	}
	// Never trust a provider response that returns a different organization.
	for _, sprite := range result.Sprites {
		if sprite.OrganizationSlug == "" && sprite.Organization == "" || sprite.OrganizationSlug != "" && sprite.OrganizationSlug != slug || sprite.Organization != "" && sprite.Organization != slug {
			return "", errSpritesValidation
		}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errSpritesValidation
	}
	return slug, nil
}

func validSpritesSlug(slug string) bool {
	if len(slug) == 0 || len(slug) > 63 || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return false
	}
	for _, c := range slug {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// checkProjectSprites is the internal use path: unseal for one fixed provider
// call, audit before the call, and clear the byte buffer on every exit. No API
// or coordinator tool exposes a plaintext reader or an arbitrary callback.
func (s *Service) checkProjectSprites(ctx context.Context, scope nativeScope) (string, error) {
	var envelope hubsecrets.Envelope
	err := s.database.db.QueryRowContext(ctx, `SELECT ciphertext, nonce, wrapped_data_key, master_key_version FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, scope.organization, scope.project, flySpritesToken).Scan(&envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err != nil {
		return "", err
	}
	if err := s.auditSecretUse(ctx, scope, envelope.Version); err != nil {
		return "", err
	}
	token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
	if err != nil {
		return "", err
	}
	defer clear(token)
	return validateSpritesToken(ctx, s.config.SpritesHTTPClient, token)
}
