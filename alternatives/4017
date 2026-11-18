package sprites

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

var ErrValidation = errors.New("sprites rejected the token or could not validate its organization")

var ErrTokenRejected = errors.New("sprites rejected the organization token; replace it through the Sprites connector")
var ErrBilling = errors.New("the Fly organization needs Sprites billing enabled; open the Sprites account page, enable billing, then retry")

func ResponseError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrTokenRejected
	case http.StatusPaymentRequired:
		return ErrBilling
	default:
		return ErrValidation
	}
}

// ValidateToken uses only the fixed Sprites origin. Redirects are refused
// and neither provider bodies nor transport errors are returned or logged.
func ValidateToken(ctx context.Context, configured *http.Client, token []byte) (string, error) {
	if len(token) == 0 || len(token) > 8192 {
		return "", ErrValidation
	}
	for _, b := range token {
		if b <= ' ' || b >= 127 {
			return "", ErrValidation
		}
	}
	// Sprites organization tokens carry org-slug/org-id/token-id/token-value.
	// A successful authenticated list validates that prefix even for an empty org.
	parts := strings.Split(string(token), "/")
	if len(parts) != 4 {
		return "", ErrValidation
	}
	for _, part := range parts {
		if part == "" {
			return "", ErrValidation
		}
	}
	slug := strings.Clone(parts[0])
	if !ValidSlug(slug) {
		return "", ErrValidation
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.sprites.dev/v1/sprites", nil)
	if err != nil {
		return "", ErrValidation
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
		return "", ErrValidation
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", ResponseError(response.StatusCode)
	}
	var result struct {
		Sprites []struct {
			OrganizationSlug string `json:"org_slug"`
			Organization     string `json:"organization"`
		} `json:"sprites"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil || result.Sprites == nil {
		return "", ErrValidation
	}
	// Never trust a provider response that returns a different organization.
	for _, sprite := range result.Sprites {
		if sprite.OrganizationSlug == "" && sprite.Organization == "" || sprite.OrganizationSlug != "" && sprite.OrganizationSlug != slug || sprite.Organization != "" && sprite.Organization != slug {
			return "", ErrValidation
		}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", ErrValidation
	}
	return slug, nil
}

func ValidSlug(slug string) bool {
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
