package oauth

import "time"

const Scope = "detent:mcp"

const AccessPrefix = "oauth_access_"

const AccessLifetime = time.Hour

const RefreshLifetime = 30 * 24 * time.Hour

type Client struct {
	ID              string   `json:"client_id"`
	Name            string   `json:"client_name"`
	RedirectURIs    []string `json:"redirect_uris"`
	AuthMethod      string   `json:"token_endpoint_auth_method"`
	Secret          string   `json:"client_secret,omitempty"`
	SecretExpiresAt *int64   `json:"client_secret_expires_at,omitempty"`
	GrantTypes      []string `json:"grant_types"`
	ResponseTypes   []string `json:"response_types"`
}

type Registration struct {
	Name         string   `json:"n"`
	RedirectURIs []string `json:"r"`
	AuthMethod   string   `json:"a"`
	SecretHash   string   `json:"s"`
}

type Grant struct {
	ID             string  `json:"id"`
	ClientID       string  `json:"client_id"`
	ClientName     string  `json:"client_name"`
	OrganizationID string  `json:"organization_id"`
	ConnectedAt    string  `json:"connected_at"`
	LastUsedAt     *string `json:"last_used_at"`
	RevokedAt      *string `json:"revoked_at"`
}

type GrantPage struct {
	Grants     []Grant `json:"grants"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}
