package sprites

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const spritesSecretSentinel = "detent-test/org-id/token-id/SPRITES-VALUE-SENTINEL"

type spritesTestTransport func(*http.Request) (*http.Response, error)

func (f spritesTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func spritesTestResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestSpritesTokenValidation(t *testing.T) {
	for _, test := range []struct {
		name, token, body string
		status            int
		transportError    bool
		valid             bool
	}{
		{name: "current response", body: `{"sprites":[{"organization":"detent-test"}]}`, status: 200, valid: true},
		{name: "legacy response", body: `{"sprites":[{"org_slug":"detent-test"}]}`, status: 200, valid: true},
		{name: "empty organization", body: `{"sprites":[]}`, status: 200, valid: true},
		{name: "invalid token", body: spritesSecretSentinel, status: 401},
		{name: "organization billing required", body: spritesSecretSentinel, status: 402},
		{name: "provider unavailable", body: spritesSecretSentinel, status: 503},
		{name: "provider error contains value", transportError: true},
		{name: "bad response contains value", body: spritesSecretSentinel, status: 200},
		{name: "missing sprites", body: `{}`, status: 200},
		{name: "missing organization", body: `{"sprites":[{}]}`, status: 200},
		{name: "different organization", body: `{"sprites":[{"organization":"other"}]}`, status: 200},
		{name: "conflicting fields", body: `{"sprites":[{"org_slug":"other","organization":"detent-test"}]}`, status: 200},
		{name: "trailing json", body: `{"sprites":[]} {}`, status: 200},
		{name: "header injection", token: "detent-test/org/id/value\nInjected: value", status: 200},
		{name: "invalid format", token: "SPRITES-VALUE-SENTINEL", status: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			token := test.token
			if token == "" {
				token = spritesSecretSentinel
			}
			client := &http.Client{Transport: spritesTestTransport(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.String() != "https://api.sprites.dev/v1/sprites" || request.Header.Get("Authorization") != "Bearer "+token {
					t.Fatal("incorrect validation request")
				}
				if test.transportError {
					return nil, errors.New(spritesSecretSentinel)
				}
				return spritesTestResponse(test.body, test.status), nil
			})}
			slug, err := ValidateToken(t.Context(), client, []byte(token))
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t error=%v", test.valid, err)
			}
			if test.valid && slug != "detent-test" {
				t.Fatal("incorrect organization binding")
			}
			if err != nil && (strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "SPRITES-VALUE-SENTINEL")) {
				t.Fatal("error exposes token")
			}
		})
	}
	t.Run("redirect refused", func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			r := spritesTestResponse("", 302)
			r.Header.Set("Location", "https://api.sprites.dev/leak")
			return r, nil
		})}
		if _, err := ValidateToken(t.Context(), client, []byte(spritesSecretSentinel)); err == nil || calls != 1 {
			t.Fatal("validation followed a redirect")
		}
	})
}
