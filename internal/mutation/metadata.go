// Package mutation carries trusted application mutation context across adapters.
// Operation records remain owned by the application, never by MCP transports.
package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var (
	ErrConflict  = errors.New("mutation retry key has different content")
	ErrUncertain = errors.New("mutation outcome is pending or uncertain; inspect the resource before retrying")
)

// Metadata contains identifiers and hashes only. Never put raw arguments,
// credentials, business keys, or rendered application errors in audit records.
type Metadata struct {
	PrincipalID    string `json:"principal_id"`
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id,omitempty"`
	ResourceID     string `json:"resource_id,omitempty"`
	Action         string `json:"action"`
	Source         string `json:"source"`
	Confirmation   string `json:"confirmation"`
	CorrelationID  string `json:"correlation_id"`
	RetryIdentity  string `json:"retry_identity,omitempty"`
	InputHash      string `json:"input_hash,omitempty"`
}

func digest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Bind uses an explicit business key, independently of transport request IDs
// and connection/session IDs. Changed input conflicts within this identity.
func (m Metadata) Bind(key string, input any) (Metadata, error) {
	if m.PrincipalID == "" || m.OrganizationID == "" || m.Action == "" || key == "" || len(key) > 128 {
		return m, errors.New("invalid mutation identity")
	}
	var err error
	m.RetryIdentity, err = digest([]string{m.PrincipalID, m.OrganizationID, m.ProjectID, m.Action, key})
	if err != nil {
		return m, err
	}
	m.InputHash, err = digest(input)
	return m, err
}

type contextKey struct{}

func WithContext(ctx context.Context, m Metadata) context.Context {
	return context.WithValue(ctx, contextKey{}, m)
}
func FromContext(ctx context.Context) (Metadata, bool) {
	m, ok := ctx.Value(contextKey{}).(Metadata)
	return m, ok
}

// ErrorText preserves dashboard diagnostics while excluding provider payloads
// and credentials from logs emitted during an MCP application mutation.
func ErrorText(ctx context.Context, err error) string {
	if m, ok := FromContext(ctx); ok && m.Source == "mcp" {
		return "application mutation unavailable"
	}
	return err.Error()
}
