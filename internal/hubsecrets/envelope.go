// Package hubsecrets seals provider credentials for use inside the Hub process.
package hubsecrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
)

var ErrUnavailable = errors.New("hub secret master key version is unavailable")
var ErrInvalid = errors.New("hub secret envelope authentication failed")

// Keyring contains only operator-supplied keys, never database or file configuration.
// Its fields are private so serializing or logging configuration cannot expose keys.
type Keyring struct {
	keys    map[int][]byte
	version int
}

func (k *Keyring) String() string { return "[redacted Hub secret keys]" }

// FromEnvironment reads base64-encoded 32-byte keys indexed by positive version
// from DETENT_HUB_SECRET_KEYS and the active DETENT_HUB_SECRET_KEY_VERSION.
func FromEnvironment(lookup func(string) string) (*Keyring, error) {
	raw, active := lookup("DETENT_HUB_SECRET_KEYS"), lookup("DETENT_HUB_SECRET_KEY_VERSION")
	if raw == "" && active == "" {
		return nil, nil //nolint:nilnil // An absent configuration intentionally disables secret storage.
	}
	version, err := strconv.Atoi(active)
	if err != nil || version < 1 {
		return nil, errors.New("DETENT_HUB_SECRET_KEY_VERSION must be a positive integer")
	}
	var encoded map[string]string
	if json.Unmarshal([]byte(raw), &encoded) != nil {
		return nil, errors.New("DETENT_HUB_SECRET_KEYS must be a JSON object of versioned base64 keys")
	}
	k := &Keyring{keys: make(map[int][]byte), version: version}
	for name, value := range encoded {
		v, err := strconv.Atoi(name)
		if err != nil || v < 1 || strconv.Itoa(v) != name {
			return nil, errors.New("hub secret key versions must be canonical positive integers")
		}
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != 32 {
			return nil, errors.New("hub secret master keys must be base64-encoded 32-byte values")
		}
		k.keys[v] = key
	}
	if len(k.keys[version]) != 32 {
		return nil, ErrUnavailable
	}
	return k, nil
}

func (k *Keyring) Version() int {
	if k == nil {
		return 0
	}
	return k.version
}

// Envelope contains no plaintext. WrappedKey includes its own GCM nonce prefix.
type Envelope struct {
	Ciphertext []byte
	Nonce      []byte
	WrappedKey []byte
	Version    int
}

func gcm(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalid
	}
	return cipher.NewGCM(block)
}

func wrapAAD(aad []byte, version int) []byte {
	return append(append([]byte("detent:wrapped-key:"+strconv.Itoa(version)+":"), aad...), byte(0))
}

func (k *Keyring) wrap(key, aad []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrUnavailable
	}
	aead, err := gcm(k.keys[k.version])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("generate Hub secret wrapping nonce")
	}
	return aead.Seal(nonce, nonce, key, wrapAAD(aad, k.version)), nil
}

func (k *Keyring) unwrap(e Envelope, aad []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrUnavailable
	}
	aead, err := gcm(k.keys[e.Version])
	if err != nil {
		return nil, err
	}
	n := aead.NonceSize()
	if len(e.WrappedKey) != n+32+aead.Overhead() {
		return nil, ErrInvalid
	}
	key, err := aead.Open(nil, e.WrappedKey[:n], e.WrappedKey[n:], wrapAAD(aad, e.Version))
	if err != nil {
		return nil, ErrInvalid
	}
	return key, nil
}

func (k *Keyring) Seal(value, aad []byte) (Envelope, error) {
	if k == nil {
		return Envelope{}, ErrUnavailable
	}
	key := make([]byte, 32)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return Envelope{}, errors.New("generate Hub secret data key")
	}
	wrapped, err := k.wrap(key, aad)
	if err != nil {
		return Envelope{}, err
	}
	aead, err := gcm(key)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, errors.New("generate Hub secret nonce")
	}
	return Envelope{Ciphertext: aead.Seal(nil, nonce, value, aad), Nonce: nonce, WrappedKey: wrapped, Version: k.version}, nil
}

func (k *Keyring) Open(e Envelope, aad []byte) ([]byte, error) {
	key, err := k.unwrap(e, aad)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	aead, err := gcm(key)
	if err != nil || len(e.Nonce) != aead.NonceSize() {
		return nil, ErrInvalid
	}
	value, err := aead.Open(nil, e.Nonce, e.Ciphertext, aad)
	if err != nil {
		return nil, ErrInvalid
	}
	return value, nil
}

// Rewrap changes only the master-key wrapping, never decrypting the value.
func (k *Keyring) Rewrap(e Envelope, aad []byte) (Envelope, error) {
	key, err := k.unwrap(e, aad)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(key)
	wrapped, err := k.wrap(key, aad)
	if err != nil {
		return Envelope{}, err
	}
	e.WrappedKey, e.Version = wrapped, k.version
	return e, nil
}

// Check verifies that the operator supplied the correct wrapping key at startup.
func (k *Keyring) Check(e Envelope, aad []byte) error {
	key, err := k.unwrap(e, aad)
	clear(key)
	return err
}
