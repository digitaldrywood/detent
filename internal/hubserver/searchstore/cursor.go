package searchstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	cursorVersion = 5
	cursorKeyInfo = "detent search cursor v5"
)

var ErrCursorUnavailable = errors.New("search cursor sealer unavailable")

type CursorKey func(context.Context, tracker.OrganizationID) ([]byte, error)

type CursorSealer struct{ key CursorKey }

func NewCursorSealer(key CursorKey) *CursorSealer { return &CursorSealer{key: key} }

func (s *CursorSealer) available() bool { return s != nil && s.key != nil }

func (s *CursorSealer) aead(ctx context.Context, organization tracker.OrganizationID) (cipher.AEAD, error) {
	if !s.available() {
		return nil, ErrCursorUnavailable
	}
	root, err := s.key(ctx, organization)
	if err != nil {
		return nil, fmt.Errorf("read search cursor key: %w", err)
	}
	if len(root) != sha256.Size {
		return nil, fmt.Errorf("read search cursor key: %w", ErrCursorUnavailable)
	}
	derived, err := hkdf.Key(sha256.New, root, nil, cursorKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive search cursor key: %w", err)
	}
	defer clear(derived)
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("derive search cursor key: %w", err)
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func cursorAAD(organization tracker.OrganizationID, fingerprint [32]byte) []byte {
	return append(append([]byte{cursorVersion}, organization...), fingerprint[:]...)
}

func (s *CursorSealer) seal(ctx context.Context, organization tracker.OrganizationID, position pagePosition) (string, error) {
	plaintext, err := json.Marshal(position)
	if err != nil {
		return "", ErrInvalid
	}
	aead, err := s.aead(ctx, organization)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal([]byte{cursorVersion}, nil, plaintext, cursorAAD(organization, position.Fingerprint))), nil
}

func (s *CursorSealer) open(ctx context.Context, organization tracker.OrganizationID, fingerprint [32]byte, token string) ([]byte, error) {
	if !s.available() {
		return nil, ErrCursorUnavailable
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) == 0 || raw[0] != cursorVersion {
		return nil, corestore.ErrInvalidCursor
	}
	aead, err := s.aead(ctx, organization)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nil, raw[1:], cursorAAD(organization, fingerprint))
	if err != nil {
		return nil, corestore.ErrInvalidCursor
	}
	return plaintext, nil
}
