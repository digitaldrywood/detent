package hubsecrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func testKeys(t *testing.T, version string, versions ...string) *Keyring {
	t.Helper()
	encoded := map[string]string{}
	for _, v := range versions {
		encoded[v] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte(v), 32))
	}
	raw, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	k, err := FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return string(raw)
		}
		return version
	})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEnvelope(t *testing.T) {
	keys := testKeys(t, "1", "1", "2")
	aad, token := []byte(`["org","project","fly_sprites_token"]`), []byte("sentinel-secret-value")
	for _, test := range []struct {
		name   string
		change func(*Envelope)
		aad    []byte
		want   error
	}{
		{name: "round trip"},
		{name: "unknown version", change: func(e *Envelope) { e.Version = 3 }, want: ErrUnavailable},
		{name: "wrong existing version", change: func(e *Envelope) { e.Version = 2 }, want: ErrInvalid},
		{name: "tampered ciphertext", change: func(e *Envelope) { e.Ciphertext[0] ^= 1 }, want: ErrInvalid},
		{name: "tampered nonce", change: func(e *Envelope) { e.Nonce[0] ^= 1 }, want: ErrInvalid},
		{name: "short nonce", change: func(e *Envelope) { e.Nonce = nil }, want: ErrInvalid},
		{name: "tampered wrapped key", change: func(e *Envelope) { e.WrappedKey[15] ^= 1 }, want: ErrInvalid},
		{name: "short wrapped key", change: func(e *Envelope) { e.WrappedKey = nil }, want: ErrInvalid},
		{name: "row substitution", aad: []byte(`["org","other","fly_sprites_token"]`), want: ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, err := keys.Seal(token, aad)
			if err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(&envelope)
			}
			binding := aad
			if test.aad != nil {
				binding = test.aad
			}
			value, err := keys.Open(envelope, binding)
			if !errors.Is(err, test.want) {
				t.Fatalf("open error=%v want=%v", err, test.want)
			}
			if err == nil && !bytes.Equal(value, token) {
				t.Fatal("round trip differs")
			}
			if err != nil && (value != nil || strings.Contains(err.Error(), string(token))) {
				t.Fatal("failed open exposed value")
			}
		})
	}
	first, err := keys.Seal(token, aad)
	if err != nil {
		t.Fatal(err)
	}
	second, err := keys.Seal(token, aad)
	if err != nil {
		t.Fatal(err)
	}
	k1, err := keys.unwrap(first, aad)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := keys.unwrap(second, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("secrets share a data key")
	}
	clear(k1)
	clear(k2)
	rotated := testKeys(t, "2", "1", "2")
	rewrapped, err := rotated.Rewrap(first, aad)
	if err != nil {
		t.Fatal(err)
	}
	if rewrapped.Version != 2 || !bytes.Equal(first.Ciphertext, rewrapped.Ciphertext) || !bytes.Equal(first.Nonce, rewrapped.Nonce) || bytes.Equal(first.WrappedKey, rewrapped.WrappedKey) {
		t.Fatal("rotation changed value or retained wrapping")
	}
	newOnly := testKeys(t, "2", "2")
	value, err := newOnly.Open(rewrapped, aad)
	if err != nil || !bytes.Equal(value, token) {
		t.Fatalf("new-only open: %v", err)
	}
	if _, err := keys.Open(rewrapped, aad); err != nil {
		t.Fatal(err)
	}
	oldOnly := testKeys(t, "1", "1")
	if _, err := oldOnly.Open(rewrapped, aad); !errors.Is(err, ErrUnavailable) {
		t.Fatal("old-only key opened rotated envelope")
	}
}

func TestEnvironmentKeys(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))
	for _, test := range []struct {
		name, raw, version string
		valid              bool
	}{
		{name: "disabled", valid: true},
		{name: "valid", raw: `{"1":"` + key + `"}`, version: "1", valid: true},
		{name: "short key", raw: `{"1":"c2VjcmV0"}`, version: "1"},
		{name: "bad base64", raw: `{"1":"SECRET-SENTINEL"}`, version: "1"},
		{name: "invalid json", raw: "SECRET-SENTINEL", version: "1"},
		{name: "missing active", raw: `{"1":"` + key + `"}`},
		{name: "missing version", raw: `{"1":"` + key + `"}`, version: "2"},
		{name: "negative version", raw: `{"-1":"` + key + `"}`, version: "1"},
		{name: "noncanonical version", raw: `{"01":"` + key + `"}`, version: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			keys, err := FromEnvironment(func(name string) string {
				if name == "DETENT_HUB_SECRET_KEYS" {
					return test.raw
				}
				return test.version
			})
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t error=%v", test.valid, err)
			}
			if err != nil && (strings.Contains(err.Error(), "SECRET-SENTINEL") || strings.Contains(err.Error(), key)) {
				t.Fatal("error leaked keys")
			}
			if keys != nil && strings.Contains(fmt.Sprint(keys), key) {
				t.Fatal("keyring exposed keys")
			}
		})
	}
}
