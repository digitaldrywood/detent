package auth_test

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
)

var refreshRequests atomic.Int64

func (f *workosFixture) refresh(w http.ResponseWriter, r *http.Request, request map[string]string, mode string) {
	refreshRequests.Add(1)
	if r.Method != http.MethodPost || request["client_id"] != "client_detent" || request["client_secret"] != "fixture-secret" {
		f.t.Error("incorrect refresh request")
	}
	if mode == "refresh-revoked" || request["refresh_token"] != "refresh_customer" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	claims := map[string]any{
		"iss": f.apiURL + "/user_management/client_detent", "sub": "user_customer", "client_id": "client_detent", "sid": "session_customer",
		"org_id": "org_customer", "role": "member", "iat": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(),
	}
	response := map[string]any{"user": map[string]any{"id": "user_customer", "email": "customer@example.com", "email_verified": true}, "organization_id": "org_customer", "refresh_token": "refresh_rotated"}
	switch mode {
	case "refresh-no-org":
		delete(claims, "org_id")
		delete(claims, "role")
		delete(response, "organization_id")
	case "refresh-missing-rotation":
		delete(response, "refresh_token")
	case "refresh-wrong-user":
		claims["sub"] = "user_other"
	}
	response["access_token"] = signTestJWT(f.t, f.key, claims)
	f.writeJSON(w, response)
}

func TestWorkOSVerifyAccessIsLocal(t *testing.T) {
	f := newWorkOSFixture(t)
	provider := f.provider(t)
	base := func() map[string]any {
		return map[string]any{
			"iss": f.apiURL + "/user_management/client_detent", "sub": "user_customer", "client_id": "client_detent", "sid": "session_customer",
			"org_id": "org_customer", "role": "admin", "iat": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(),
		}
	}
	tests := []struct {
		name    string
		edit    func(map[string]any)
		wrong   bool
		want    auth.HostedAccess
		wantErr error
		reason  string
	}{
		{name: "valid", edit: func(map[string]any) {}, want: auth.HostedAccess{Subject: "user_customer", OrganizationID: "org_customer", SessionID: "session_customer", Role: "admin", ExpiresAt: f.now.Add(5 * time.Minute)}},
		{name: "support actor", edit: func(c map[string]any) { c["act"] = map[string]string{"email": "Support@Example.com"} }, want: auth.HostedAccess{Subject: "user_customer", OrganizationID: "org_customer", SessionID: "session_customer", Role: "admin", SupportActor: "support@example.com", ExpiresAt: f.now.Add(5 * time.Minute)}},
		{name: "unscoped", edit: func(c map[string]any) { delete(c, "org_id"); delete(c, "role") }, want: auth.HostedAccess{Subject: "user_customer", SessionID: "session_customer", ExpiresAt: f.now.Add(5 * time.Minute)}},
		{name: "expired", edit: func(c map[string]any) {
			c["iat"] = f.now.Add(-10 * time.Minute).Unix()
			c["exp"] = f.now.Add(-time.Minute).Unix()
		}, wantErr: auth.ErrAccessExpired},
		{name: "wrong key", edit: func(map[string]any) {}, wrong: true, reason: auth.HostedReasonTokenInvalid},
		{name: "wrong client", edit: func(c map[string]any) { c["client_id"] = "client_other" }, reason: auth.HostedReasonClientMismatch},
		{name: "missing session", edit: func(c map[string]any) { delete(c, "sid") }, reason: auth.HostedReasonTokenInvalid},
		{name: "conflicting actor", edit: func(c map[string]any) {
			c["act"] = map[string]string{"email": "support@example.com", "sub": "other@example.com"}
		}, reason: auth.HostedReasonSupportActorInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := base()
			test.edit(claims)
			key := f.key
			if test.wrong {
				key = f.wrongKey
			}
			before := refreshRequests.Load()
			access, err := provider.VerifyAccess(t.Context(), signTestJWT(t, key, claims))
			if refreshRequests.Load() != before {
				t.Fatal("verification called the provider API")
			}
			switch {
			case test.wantErr != nil:
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("err = %v, want %v", err, test.wantErr)
				}
			case test.reason != "":
				if auth.HostedIdentityReason(err) != test.reason {
					t.Fatalf("reason = %q (%v), want %q", auth.HostedIdentityReason(err), err, test.reason)
				}
			default:
				if err != nil || !access.ExpiresAt.Equal(test.want.ExpiresAt) {
					t.Fatalf("access = %+v, err = %v", access, err)
				}
				access.ExpiresAt = test.want.ExpiresAt
				if access != test.want {
					t.Fatalf("access = %+v, want %+v", access, test.want)
				}
			}
		})
	}
	if _, err := provider.VerifyAccess(t.Context(), ""); auth.HostedIdentityReason(err) != auth.HostedReasonTokenInvalid {
		t.Fatalf("empty token err = %v", err)
	}
}

func TestWorkOSVerifyAccessKeySetUnavailable(t *testing.T) {
	f := newWorkOSFixture(t)
	f.mode.Store("jwks-down")
	claims := map[string]any{
		"iss": f.apiURL + "/user_management/client_detent", "sub": "user_customer", "client_id": "client_detent", "sid": "session_customer",
		"org_id": "org_customer", "role": "admin", "iat": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(),
	}
	_, err := f.provider(t).VerifyAccess(t.Context(), signTestJWT(t, f.key, claims))
	if auth.HostedIdentityReason(err) != auth.HostedReasonProviderUnavailable {
		t.Fatalf("reason = %q (%v), want %q", auth.HostedIdentityReason(err), err, auth.HostedReasonProviderUnavailable)
	}
}

func TestWorkOSRefreshAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	tests := []struct {
		name     string
		mode     string
		token    string
		wantRole string
		reason   string
	}{
		{name: "rotates", mode: "valid", token: "refresh_customer", wantRole: "member"},
		{name: "member removed", mode: "refresh-no-org", token: "refresh_customer"},
		{name: "revoked session", mode: "refresh-revoked", token: "refresh_customer", reason: auth.HostedReasonProviderRejected},
		{name: "unknown token", mode: "valid", token: "refresh_other", reason: auth.HostedReasonProviderRejected},
		{name: "missing rotation", mode: "refresh-missing-rotation", token: "refresh_customer", reason: auth.HostedReasonProviderInvalid},
		{name: "subject mismatch", mode: "refresh-wrong-user", token: "refresh_customer", reason: auth.HostedReasonProviderInvalid},
		{name: "empty token", mode: "valid", token: "", reason: auth.HostedReasonSessionInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newWorkOSFixture(t)
			f.mode.Store(test.mode)
			access, tokens, err := f.provider(t).RefreshAccess(t.Context(), test.token)
			if test.reason != "" {
				if auth.HostedIdentityReason(err) != test.reason {
					t.Fatalf("reason = %q (%v), want %q", auth.HostedIdentityReason(err), err, test.reason)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tokens.RefreshToken != "refresh_rotated" || tokens.AccessToken == "" || access.Subject != "user_customer" || access.SessionID != "session_customer" || access.Role != test.wantRole {
				t.Fatalf("access = %+v tokens = %+v", access, tokens)
			}
			if test.mode == "refresh-no-org" && access.OrganizationID != "" {
				t.Fatalf("organization = %q, want none", access.OrganizationID)
			}
		})
	}
}

func TestWorkOSExchangeReturnsTokens(t *testing.T) {
	f := newWorkOSFixture(t)
	identity, err := f.provider(t).Exchange(t.Context(), "code_customer", "verifier", "state")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Tokens.AccessToken == "" || identity.Tokens.RefreshToken != "refresh_customer" {
		t.Fatalf("tokens = %+v", identity.Tokens)
	}
}
