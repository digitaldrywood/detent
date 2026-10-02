package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type catalogPage struct {
	Tools      []operatortool.Definition `json:"tools"`
	NextCursor string                    `json:"nextCursor,omitempty"`
}

type catalogCursor struct {
	Offset int    `json:"offset"`
	Digest string `json:"digest"`
}

func (s *session) catalog(ctx context.Context, cursor string) (catalogPage, error) {
	definitions := operatortool.Catalog()
	if lister, ok := s.executor.(interface {
		ListTools(context.Context) ([]operatortool.Definition, error)
	}); ok {
		var err error
		definitions, err = lister.ListTools(ctx)
		if err != nil {
			return catalogPage{}, operatortool.ErrAccessDenied
		}
	}
	// Only the same typed registry that tools/call accepts can be advertised.
	if len(definitions) > len(operatortool.Registry()) {
		return catalogPage{}, errors.New("invalid catalog")
	}
	seen := make(map[string]bool, len(definitions))
	canonical := make([]operatortool.Definition, 0, len(definitions))
	for _, definition := range definitions {
		_, ok := operatortool.Lookup(definition.Name)
		if !ok || seen[definition.Name] {
			return catalogPage{}, errors.New("invalid catalog")
		}
		seen[definition.Name] = true
	}
	for _, shared := range operatortool.Registry() {
		if seen[shared.Name] {
			canonical = append(canonical, shared)
		}
	}
	definitions = canonical
	offset := 0
	if cursor != "" {
		if len(cursor) > 256 {
			return catalogPage{}, operatortool.ErrInvalidArguments
		}
		raw, err := json.Marshal(struct {
			Identity operatortool.Identity
			Tools    []operatortool.Definition
		}{operatortool.ConnectionIdentity(ctx), definitions})
		if err != nil {
			return catalogPage{}, err
		}
		sum := sha256.Sum256(raw)
		digest := base64.RawURLEncoding.EncodeToString(sum[:])
		raw, err = base64.RawURLEncoding.DecodeString(cursor)
		var position catalogCursor
		if err != nil || decodeObject(raw, &position, true) != nil || position.Digest != digest || position.Offset <= 0 || position.Offset >= len(definitions) {
			return catalogPage{}, operatortool.ErrInvalidArguments
		}
		offset = position.Offset
	}
	return catalogPage{Tools: append([]operatortool.Definition{}, definitions[offset:]...)}, nil
}
