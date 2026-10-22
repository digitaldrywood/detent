package cloudentry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

func entrySecretAAD(purpose, binding string) []byte {
	return []byte("detent-cloud-entry\x00" + purpose + "\x00" + binding)
}

func encodeEntrySecret(e hubsecrets.Envelope) string {
	encode := base64.RawURLEncoding.EncodeToString
	return strings.Join([]string{"hk1", strconv.Itoa(e.Version), encode(e.Nonce), encode(e.WrappedKey), encode(e.Ciphertext)}, ".")
}

func decodeEntrySecret(value string) (hubsecrets.Envelope, error) {
	var e hubsecrets.Envelope
	parts := strings.Split(value, ".")
	if len(parts) != 5 || parts[0] != "hk1" {
		return e, hubsecrets.ErrInvalid
	}
	version, err := strconv.Atoi(parts[1])
	if err != nil || version < 1 || strconv.Itoa(version) != parts[1] {
		return e, hubsecrets.ErrInvalid
	}
	e.Version = version
	for i, target := range []*[]byte{&e.Nonce, &e.WrappedKey, &e.Ciphertext} {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(parts[i+2])
		if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != parts[i+2] {
			return hubsecrets.Envelope{}, hubsecrets.ErrInvalid
		}
		*target = decoded
	}
	return e, nil
}

func (a *authStore) resealToken(purpose, binding, value string) (string, error) {
	if value == "" {
		return value, nil
	}
	if strings.HasPrefix(value, "hk1.") {
		e, err := decodeEntrySecret(value)
		if err != nil {
			return "", err
		}
		if e.Version == a.keys.Version() {
			return value, nil
		}
		e, err = a.keys.Rewrap(e, entrySecretAAD(purpose, binding))
		if err != nil {
			return "", err
		}
		return encodeEntrySecret(e), nil
	}
	plain, err := a.openToken(purpose, binding, value)
	if err != nil {
		return "", err
	}
	return a.sealToken(purpose, binding, plain)
}

func (s *Service) startAuthReseal(parent context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.stopAuthReseal = cancel
	s.authResealDone = make(chan struct{})
	go func() {
		defer close(s.authResealDone)
		if err := s.resealAuthorizations(ctx); err != nil && ctx.Err() == nil {
			s.config.Logger.ErrorContext(ctx, "reseal authorizations failed")
		}
	}()
}

func (s *Service) resealAuthorizations(ctx context.Context) error {
	if s.auth.keys == nil {
		return nil
	}
	var after string
	for {
		var item authorization
		err := s.auth.store.reader.QueryRowContext(ctx, `SELECT binding,access_token,refresh_token FROM authorizations WHERE binding>? AND revoked_at IS NULL AND (access_token<>'' OR refresh_token<>'') ORDER BY binding LIMIT 1`, after).Scan(&item.Binding, &item.sealedAccess, &item.sealedRefresh)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		after = item.Binding
		if err := s.auth.resealAuthorization(ctx, item); err != nil && ctx.Err() == nil {
			hash := sha256.Sum256([]byte(item.Binding))
			s.config.Logger.ErrorContext(ctx, "reseal authorization failed", "error", errNoSession, "binding_hash", hex.EncodeToString(hash[:6]))
		}
	}
}

func (a *authStore) resealAuthorization(ctx context.Context, item authorization) error {
	access, err := a.resealToken("provider-access", item.Binding, item.sealedAccess)
	if err != nil {
		return err
	}
	refresh, err := a.resealToken("provider-refresh", item.Binding, item.sealedRefresh)
	if err != nil {
		return err
	}
	if access == item.sealedAccess && refresh == item.sealedRefresh {
		return nil
	}
	_, err = a.store.db.ExecContext(ctx, `UPDATE authorizations SET access_token=?,refresh_token=? WHERE binding=? AND access_token=? AND refresh_token=? AND revoked_at IS NULL`, access, refresh, item.Binding, item.sealedAccess, item.sealedRefresh)
	return err
}
