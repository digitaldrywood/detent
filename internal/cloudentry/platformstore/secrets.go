package platformstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

type secretCipher struct {
	legacy     cipher.AEAD
	keys       *hubsecrets.Keyring
	signingKey ed25519.PublicKey
}

func newSecretCipher(key ed25519.PrivateKey, keys *hubsecrets.Keyring) (*secretCipher, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, ErrInvalid
	}
	mac := hmac.New(sha256.New, key.Seed())
	mac.Write([]byte("detent-entry-provider-token-seal"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	legacy, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	publicKey, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, ErrInvalid
	}
	return &secretCipher{legacy: legacy, keys: keys, signingKey: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

func secretAAD(purpose, binding string) []byte {
	return []byte("detent-cloud-entry\x00" + purpose + "\x00" + binding)
}

func (s *secretCipher) seal(purpose, binding, plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if s.keys != nil {
		envelope, err := s.keys.Seal([]byte(plain), secretAAD(purpose, binding))
		if err != nil {
			return "", err
		}
		encode := base64.RawURLEncoding.EncodeToString
		return strings.Join([]string{"hk1", strconv.Itoa(envelope.Version), encode(envelope.Nonce), encode(envelope.WrappedKey), encode(envelope.Ciphertext)}, "."), nil
	}
	nonce := make([]byte, s.legacy.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(s.legacy.Seal(nonce, nonce, []byte(plain), []byte(binding))), nil
}

func (s *secretCipher) open(purpose, binding, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if strings.HasPrefix(sealed, "hk1.") {
		parts := strings.Split(sealed, ".")
		if len(parts) != 5 {
			return "", ErrNoSession
		}
		version, err := strconv.Atoi(parts[1])
		if err != nil || version < 1 || strconv.Itoa(version) != parts[1] {
			return "", ErrNoSession
		}
		envelope := hubsecrets.Envelope{Version: version}
		for i, target := range []*[]byte{&envelope.Nonce, &envelope.WrappedKey, &envelope.Ciphertext} {
			value, err := base64.RawURLEncoding.Strict().DecodeString(parts[i+2])
			if err != nil || base64.RawURLEncoding.EncodeToString(value) != parts[i+2] {
				return "", ErrNoSession
			}
			*target = value
		}
		plain, err := s.keys.Open(envelope, secretAAD(purpose, binding))
		if err != nil {
			return "", ErrNoSession
		}
		return string(plain), nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	n := s.legacy.NonceSize()
	if err != nil || len(raw) <= n {
		return "", ErrNoSession
	}
	plain, err := s.legacy.Open(nil, raw[:n], raw[n:], []byte(binding))
	if err != nil {
		return "", ErrNoSession
	}
	return string(plain), nil
}
