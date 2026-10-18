package auth_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/auth"
)

func TestWorkOSConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider string
		modify   func(*auth.WorkOSConfig)
		wantErr  bool
	}{
		{name: "default endpoints", provider: "workos"},
		{name: "unsupported", provider: "other", wantErr: true},
		{name: "insecure API", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.APIURL = "http://api.example.com" }, wantErr: true},
		{name: "API credentials", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.APIURL = "https://secret@example.com" }, wantErr: true},
		{name: "API path", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.APIURL = "https://example.com/path" }, wantErr: true},
		{name: "insecure issuer", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.IssuerURL = "http://issuer.example.com" }, wantErr: true},
		{name: "issuer fragment", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.IssuerURL = "https://issuer.example.com/#fragment" }, wantErr: true},
		{name: "relative redirect", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.RedirectURL = "/callback" }, wantErr: true},
		{name: "insecure redirect", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.RedirectURL = "http://app.example.com/callback" }, wantErr: true},
		{name: "missing key", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.APIKey = "" }, wantErr: true},
		{name: "key newline", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.APIKey = "key\nsecret" }, wantErr: true},
		{name: "missing client", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.ClientID = "" }, wantErr: true},
		{name: "client path", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.ClientID = "../secret" }, wantErr: true},
		{name: "loopback IPv6", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.APIURL = "http://[::1]:9000" }},
		{name: "loopback hostname", provider: "workos", modify: func(c *auth.WorkOSConfig) { c.APIURL = "http://localhost:9000" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := auth.WorkOSConfig{ClientID: "client_detent", APIKey: "fixture-secret", RedirectURL: "https://app.example.com/auth/callback"}
			if tt.modify != nil {
				tt.modify(&cfg)
			}
			_, err := auth.NewHostedProvider(tt.provider, cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewHostedProvider() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration error exposed credentials")
			}
		})
	}
}

func TestWorkOSAuthorizationAndExchange(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	provider := f.provider(t)
	authorization, err := url.Parse(provider.AuthorizationURL("state", "nonce", "verifier"))
	if err != nil {
		t.Fatal(err)
	}
	wantQuery := map[string]string{
		"provider": "authkit", "client_id": "client_detent", "response_type": "code", "state": "state",
		"redirect_uri": "https://app.example.com/auth/callback", "code_challenge_method": "S256",
		"code_challenge": oauth2.S256ChallengeFromVerifier("verifier"),
	}
	if authorization.Path != "/user_management/authorize" || authorization.Query().Has("nonce") || authorization.Query().Has("client_secret") {
		t.Fatal("authorization URL contains incorrect path or private fields")
	}
	for key, want := range wantQuery {
		if got := authorization.Query().Get(key); got != want {
			t.Errorf("authorization %s = %q, want %q", key, got, want)
		}
	}
	tests := []struct {
		name    string
		noPKCE  bool
		wantErr bool
	}{
		{name: "valid"}, {name: "issuer-slash"}, {name: "audience"}, {name: "audience-array"}, {name: "pagination"},
		{name: "support", noPKCE: true}, {name: "support-email", noPKCE: true},
		{name: "support-short-expiry", noPKCE: true},
		{name: "ordinary-no-pkce", noPKCE: true, wantErr: true},
		{name: "wrong-issuer", wantErr: true}, {name: "legacy-issuer", wantErr: true}, {name: "wrong-client", wantErr: true}, {name: "missing-client", wantErr: true},
		{name: "wrong-audience", wantErr: true}, {name: "invalid-signature", wantErr: true}, {name: "missing-token", wantErr: true},
		{name: "empty-audience", wantErr: true}, {name: "null-audience", wantErr: true},
		{name: "expired-token", wantErr: true}, {name: "missing-iat", wantErr: true}, {name: "future-iat", wantErr: true},
		{name: "missing-sid", wantErr: true}, {name: "wrong-subject", wantErr: true}, {name: "wrong-org-response", wantErr: true},
		{name: "unverified-email", wantErr: true}, {name: "missing-email", wantErr: true},
		{name: "support-missing-act", noPKCE: true, wantErr: true}, {name: "support-missing-response", noPKCE: true, wantErr: true},
		{name: "support-wrong-act", noPKCE: true, wantErr: true}, {name: "support-conflicting-act", noPKCE: true, wantErr: true},
		{name: "support-unknown-reason", noPKCE: true, wantErr: true}, {name: "support-no-org", noPKCE: true, wantErr: true},
		{name: "support-wrong-session-actor", noPKCE: true, wantErr: true}, {name: "support-wrong-session-reason", noPKCE: true, wantErr: true},
		{name: "support-expired-hour", noPKCE: true, wantErr: true},
		{name: "session-revoked", wantErr: true}, {name: "session-ended", wantErr: true}, {name: "session-expired", wantErr: true},
		{name: "session-wrong-user", wantErr: true}, {name: "session-listed-other-org"},
		{name: "session-missing", wantErr: true}, {name: "session-page-cycle", wantErr: true},
		{name: "http-error", wantErr: true}, {name: "malformed", wantErr: true}, {name: "oversized", wantErr: true}, {name: "trailing-json", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.mode.Store(tt.name)
			verifier := "verifier"
			if tt.noPKCE {
				verifier = ""
			}
			identity, err := provider.Exchange(t.Context(), tt.name, verifier, "nonce")
			if tt.wantErr {
				if !errors.Is(err, auth.ErrHostedIdentity) {
					t.Fatalf("Exchange() error = %v, want sanitized rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if identity.Subject != "user_customer" || identity.Email != "customer@example.com" || !identity.EmailVerified || identity.Hosted == nil || identity.Hosted.OrganizationID != "org_customer" || identity.Hosted.SessionID != "session_customer" {
				t.Fatalf("Exchange() identity = %#v", identity)
			}
			if strings.HasPrefix(tt.name, "support") {
				if identity.Hosted.SupportActor != "support@example.com" || identity.Hosted.SupportReason != "troubleshooting" || identity.Hosted.ExpiresAt.After(identity.Hosted.CreatedAt.Add(time.Hour)) {
					t.Fatalf("Exchange() support = %#v", identity.Hosted)
				}
				if tt.name == "support-short-expiry" && !identity.Hosted.ExpiresAt.Equal(f.now.Add(5*time.Minute)) {
					t.Fatal("provider expiry was extended")
				}
			}
		})
	}
	if _, err := provider.Exchange(t.Context(), "", "verifier", "nonce"); !errors.Is(err, auth.ErrHostedIdentity) {
		t.Fatalf("empty exchange error = %v", err)
	}
	f.mode.Store("valid")
	if _, err := provider.Exchange(t.Context(), "valid", "verifier", "nonce"); !errors.Is(err, auth.ErrHostedIdentity) {
		t.Fatalf("replayed code error = %v", err)
	}
}

func TestWorkOSCurrentSessionRevocation(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	provider := f.provider(t)
	base, err := provider.Exchange(t.Context(), "valid", "verifier", "")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		modify  func(*auth.HostedIdentity)
		wantErr bool
	}{
		{name: "valid"},
		{name: "session-extended"},
		{name: "session-shortened"},
		{name: "session-revoked", wantErr: true},
		{name: "session-listed-other-org"},
		{name: "session-wrong-user", wantErr: true},
		{name: "session-ended", wantErr: true},
		{name: "session-missing", wantErr: true},
		{name: "session-created-changed", wantErr: true},
		{name: "expired-local", modify: func(i *auth.HostedIdentity) { i.ExpiresAt = f.now.Add(-time.Hour) }, wantErr: true},
		{name: "missing-start", modify: func(i *auth.HostedIdentity) { i.CreatedAt = time.Time{} }, wantErr: true},
		{name: "invalid-subject", modify: func(i *auth.HostedIdentity) { i.Subject = "../users" }, wantErr: true},
		{name: "injected-support", modify: func(i *auth.HostedIdentity) { i.SupportActor = "support@example.com" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.mode.Store(tt.name)
			identity := *base.Hosted
			if tt.modify != nil {
				tt.modify(&identity)
			}
			current, err := provider.CurrentSession(t.Context(), identity)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CurrentSession() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && current.ExpiresAt.After(base.Hosted.ExpiresAt) {
				t.Fatal("revalidation extended original session lifetime")
			}
			if tt.name == "session-shortened" && !current.ExpiresAt.Equal(f.now.Add(time.Minute)) {
				t.Fatal("revalidation ignored shortened provider expiry")
			}
		})
	}
	f.mode.Store("support")
	support, err := provider.Exchange(t.Context(), "support", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "support-wrong-session-actor", "support-wrong-session-reason"} {
		t.Run(mode+"-support-revalidation", func(t *testing.T) {
			f.mode.Store(mode)
			if _, err := provider.CurrentSession(t.Context(), *support.Hosted); !errors.Is(err, auth.ErrHostedIdentity) {
				t.Fatalf("CurrentSession() error = %v, want support mismatch rejection", err)
			}
		})
	}
}

func TestWorkOSOrganizationScopedReauthenticationReusesSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mode    string
		verify  string
		noPKCE  bool
		wantErr bool
	}{
		{name: "same session keeps original empty organization", mode: "valid", verify: "valid"},
		{name: "same session listed with another organization", mode: "valid", verify: "session-listed-other-org"},
		{name: "session of another user", mode: "session-wrong-user", verify: "session-wrong-user", wantErr: true},
		{name: "revoked session", mode: "session-revoked", verify: "session-revoked", wantErr: true},
		{name: "ended session", mode: "session-ended", verify: "session-ended", wantErr: true},
		{name: "expired session", mode: "session-expired", verify: "session-expired", wantErr: true},
		{name: "support session", mode: "support", verify: "support", noPKCE: true},
		{name: "support wrong session actor", mode: "support-wrong-session-actor", verify: "support-wrong-session-actor", noPKCE: true, wantErr: true},
		{name: "support wrong session reason", mode: "support-wrong-session-reason", verify: "support-wrong-session-reason", noPKCE: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newWorkOSFixture(t)
			provider := f.provider(t)
			f.mode.Store("unscoped")
			first, err := provider.Exchange(t.Context(), "first-sign-in", "verifier", "")
			if err != nil {
				t.Fatalf("unscoped Exchange() error = %v", err)
			}
			if first.Hosted.OrganizationID != "" || first.Hosted.SessionID != "session_customer" {
				t.Fatalf("unscoped identity = %#v", first.Hosted)
			}
			verifier := "verifier"
			if tt.noPKCE {
				verifier = ""
			}
			f.mode.Store(tt.mode)
			scoped, err := provider.Exchange(t.Context(), "organization-sign-in", verifier, "")
			if tt.wantErr {
				if !errors.Is(err, auth.ErrHostedIdentity) {
					t.Fatalf("scoped Exchange() error = %v, want rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("scoped Exchange() error = %v", err)
			}
			if scoped.Hosted.OrganizationID != "org_customer" || scoped.Hosted.SessionID != first.Hosted.SessionID {
				t.Fatalf("scoped identity = %#v", scoped.Hosted)
			}
			f.mode.Store(tt.verify)
			if _, err := provider.CurrentSession(t.Context(), *scoped.Hosted); err != nil {
				t.Fatalf("CurrentSession() after organization switch error = %v", err)
			}
			if tt.noPKCE {
				return
			}
			if _, err := provider.CurrentSession(t.Context(), *first.Hosted); err != nil {
				t.Fatalf("CurrentSession() for original identity error = %v", err)
			}
		})
	}
}

func TestWorkOSConfiguredIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name    string
		issuer  string
		wantErr bool
	}{
		{name: "exact configured issuer", issuer: "https://auth.example.com/user_management/client_default"},
		{name: "configured trailing slash", issuer: "https://auth.example.com/user_management/client_default/"},
		{name: "different application issuer", issuer: "https://auth.example.com/user_management/client_other", wantErr: true},
		{name: "different auth domain", issuer: "https://other.example.com/user_management/client_default", wantErr: true},
		{name: "default issuer", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newWorkOSFixture(t)
			fixture.mode.Store("custom-issuer")
			provider, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
				APIURL: fixture.apiURL, IssuerURL: tt.issuer, ClientID: "client_detent", APIKey: "fixture-secret",
				RedirectURL: "https://app.example.com/auth/callback", HTTPClient: fixture.client,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Exchange(t.Context(), "custom-issuer", "verifier", "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("configured issuer exchange error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestWorkOSIssuerDefault(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		apiSuffix  string
		issuer     string
		tokenIss   func(api string) string
		wantReason string
	}{
		{name: "default issuer", tokenIss: func(api string) string { return api + "/user_management/client_detent" }},
		{name: "default issuer token trailing slash", tokenIss: func(api string) string { return api + "/user_management/client_detent/" }},
		{name: "API URL trailing slash", apiSuffix: "/", tokenIss: func(api string) string { return api + "/user_management/client_detent" }},
		{name: "explicit override", issuer: "https://auth.example.com", tokenIss: func(string) string { return "https://auth.example.com" }},
		{name: "override trailing slash", issuer: "https://auth.example.com/", tokenIss: func(string) string { return "https://auth.example.com" }},
		{name: "legacy bare API issuer rejected", tokenIss: func(api string) string { return api }, wantReason: auth.HostedReasonIssuerMismatch},
		{name: "other client issuer rejected", tokenIss: func(api string) string { return api + "/user_management/client_other" }, wantReason: auth.HostedReasonIssuerMismatch},
		{name: "override rejects default", issuer: "https://auth.example.com", tokenIss: func(api string) string { return api + "/user_management/client_detent" }, wantReason: auth.HostedReasonIssuerMismatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newWorkOSFixture(t)
			fixture.mode.Store("fixed-issuer")
			fixture.issuer.Store(tt.tokenIss(fixture.apiURL))
			provider, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
				APIURL: fixture.apiURL + tt.apiSuffix, IssuerURL: tt.issuer, ClientID: "client_detent", APIKey: "fixture-secret",
				RedirectURL: "https://app.example.com/auth/callback", HTTPClient: fixture.client,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Exchange(t.Context(), "fixed-issuer", "verifier", "")
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("Exchange() error = %v", err)
				}
				return
			}
			if !errors.Is(err, auth.ErrHostedIdentity) || auth.HostedIdentityReason(err) != tt.wantReason {
				t.Fatalf("Exchange() error = %v, reason = %q, want %q", err, auth.HostedIdentityReason(err), tt.wantReason)
			}
			if _, issuer := auth.HostedIdentityDetails(err); issuer != tt.tokenIss(fixture.apiURL) {
				t.Fatalf("token issuer detail = %q", issuer)
			}
		})
	}
	if got := auth.DefaultWorkOSIssuer("https://api.workos.com/", "client_example"); got != "https://api.workos.com/user_management/client_example" {
		t.Fatalf("DefaultWorkOSIssuer() = %q", got)
	}
}

func TestWorkOSDenialReasons(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	provider := f.provider(t)
	for _, tt := range []struct {
		mode     string
		verifier string
		want     string
	}{
		{mode: "legacy-issuer", verifier: "verifier", want: auth.HostedReasonIssuerMismatch},
		{mode: "wrong-issuer", verifier: "verifier", want: auth.HostedReasonIssuerMismatch},
		{mode: "wrong-client", verifier: "verifier", want: auth.HostedReasonClientMismatch},
		{mode: "wrong-audience", verifier: "verifier", want: auth.HostedReasonAudienceMismatch},
		{mode: "invalid-signature", verifier: "verifier", want: auth.HostedReasonTokenInvalid},
		{mode: "expired-token", verifier: "verifier", want: auth.HostedReasonTokenInvalid},
		{mode: "wrong-subject", verifier: "verifier", want: auth.HostedReasonSubjectMismatch},
		{mode: "wrong-org-response", verifier: "verifier", want: auth.HostedReasonOrganizationMismatch},
		{mode: "ordinary-no-pkce", want: auth.HostedReasonPKCEMissing},
		{mode: "unverified-email", verifier: "verifier", want: auth.HostedReasonEmailUnverified},
		{mode: "missing-email", verifier: "verifier", want: auth.HostedReasonEmailInvalid},
		{mode: "support-wrong-act", want: auth.HostedReasonSupportActorInvalid},
		{mode: "session-missing", verifier: "verifier", want: auth.HostedReasonSessionNotFound},
		{mode: "session-revoked", verifier: "verifier", want: auth.HostedReasonSessionInvalid},
		{mode: "http-error", verifier: "verifier", want: auth.HostedReasonExchangeFailed},
		{mode: "malformed", verifier: "verifier", want: auth.HostedReasonExchangeFailed},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			f.mode.Store(tt.mode)
			_, err := provider.Exchange(t.Context(), tt.mode+"-reason", tt.verifier, "")
			if !errors.Is(err, auth.ErrHostedIdentity) || auth.HostedIdentityReason(err) != tt.want {
				t.Fatalf("Exchange() error = %v, reason = %q, want %q", err, auth.HostedIdentityReason(err), tt.want)
			}
			if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "customer@example.com") {
				t.Fatalf("error exposed private values: %v", err)
			}
		})
	}
	if _, err := provider.Exchange(t.Context(), "", "verifier", ""); auth.HostedIdentityReason(err) != auth.HostedReasonCodeMissing {
		t.Fatalf("empty code reason = %q", auth.HostedIdentityReason(err))
	}
	f.mode.Store("http-error")
	if _, err := provider.Exchange(t.Context(), "status-reason", "verifier", ""); err != nil {
		if status, _ := auth.HostedIdentityDetails(err); status != http.StatusBadGateway {
			t.Fatalf("exchange status detail = %d", status)
		}
	}
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", want: ""},
		{name: "bare sentinel", err: auth.ErrHostedIdentity, want: auth.HostedReasonUnknown},
		{name: "wrapped", err: fmt.Errorf("outer: %w", &auth.HostedIdentityError{Reason: auth.HostedReasonSessionExpired}), want: auth.HostedReasonSessionExpired},
		{name: "unrelated", err: errors.New("other"), want: auth.HostedReasonUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := auth.HostedIdentityReason(tt.err); got != tt.want {
				t.Fatalf("HostedIdentityReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkOSRequestDebugLogging(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	var output bytes.Buffer
	provider, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
		APIURL: f.apiURL, ClientID: "client_detent", APIKey: "fixture-secret",
		RedirectURL: "https://app.example.com/auth/callback", HTTPClient: f.client,
		Logger: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Exchange(t.Context(), "valid-logging-code", "verifier", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Invitation(t.Context(), "invitation_token"); err != nil {
		t.Fatal(err)
	}
	logged := output.String()
	for _, want := range []string{`"path":"/user_management/authenticate"`, `"status":200`, `"method":"POST"`, `"path":"/user_management/invitations/by_token/redacted"`, `"path":"/user_management/users/user_customer/sessions"`} {
		if !strings.Contains(logged, want) {
			t.Errorf("debug log missing %s:\n%s", want, logged)
		}
	}
	for _, secret := range []string{"fixture-secret", "valid-logging-code", "invitation_token", "limit=", "customer@example.com", "access_token"} {
		if strings.Contains(logged, secret) {
			t.Errorf("debug log exposed %q:\n%s", secret, logged)
		}
	}
}

func TestWorkOSOrganizationAndMembershipOperations(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	p := f.provider(t)
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "organization", run: func() error { _, err := p.Organization(t.Context(), "org_customer"); return err }},
		{name: "user identity", run: func() error {
			user, err := auth.LookupHostedUser(t.Context(), p, "user_customer")
			if err == nil && (user.ID != "user_customer" || user.Email != "customer@example.com" || user.Name != "Customer Person") {
				t.Fatalf("user identity = %#v", user)
			}
			return err
		}},
		{name: "create-organization", run: func() error { _, err := p.CreateOrganization(t.Context(), "detent_customer", "Customer"); return err }},
		{name: "memberships", run: func() error { _, err := p.Memberships(t.Context(), "user_customer", "org_customer"); return err }},
		{name: "create-membership", run: func() error {
			_, err := p.CreateMembership(t.Context(), "user_customer", "org_customer", "owner")
			return err
		}},
		{name: "set-role", run: func() error { return p.SetMembershipRole(t.Context(), "membership_customer", "owner") }},
		{name: "revoke-membership", run: func() error { return p.RevokeMembership(t.Context(), "membership_customer") }},
		{name: "revoke-session", run: func() error { return p.RevokeSession(t.Context(), "session_customer") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.mode.Store("valid")
			if err := tt.run(); err != nil {
				t.Fatal(err)
			}
			f.mode.Store("http-error")
			if err := tt.run(); !errors.Is(err, auth.ErrHostedIdentity) {
				t.Fatalf("provider failure error = %v", err)
			}
		})
	}
	for _, mode := range []string{"membership-wrong-user", "membership-wrong-org", "membership-inactive", "membership-unknown-role", "membership-page-cycle"} {
		t.Run(mode, func(t *testing.T) {
			f.mode.Store(mode)
			if _, err := p.Memberships(t.Context(), "user_customer", "org_customer"); !errors.Is(err, auth.ErrHostedIdentity) {
				t.Fatalf("Memberships() error = %v", err)
			}
		})
	}
	f.mode.Store("membership-pagination")
	memberships, err := p.Memberships(t.Context(), "user_customer", "")
	if err != nil || len(memberships) != 2 {
		t.Fatalf("paginated memberships count = %d, error = %v", len(memberships), err)
	}
}

func TestWorkOSCreationRecoversCommittedProviderWrites(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	p := f.provider(t)
	for _, tt := range []struct {
		name    string
		member  bool
		wantErr bool
	}{
		{name: "recover-organization"},
		{name: "recover-organization-wrong-external", wantErr: true},
		{name: "recover-organization-invalid-id", wantErr: true},
		{name: "recover-organization-unavailable", wantErr: true},
		{name: "recover-membership", member: true},
		{name: "recover-membership-wrong-user", member: true, wantErr: true},
		{name: "recover-membership-wrong-org", member: true, wantErr: true},
		{name: "recover-membership-wrong-role", member: true, wantErr: true},
		{name: "recover-membership-inactive", member: true, wantErr: true},
		{name: "recover-membership-missing", member: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.mode.Store(tt.name)
			var err error
			if tt.member {
				var membership auth.Membership
				membership, err = p.CreateMembership(t.Context(), "user_customer", "org_customer", "owner")
				if !tt.wantErr && (membership.UserID != "user_customer" || membership.OrganizationID != "org_customer" || membership.Role.Slug != "owner") {
					t.Fatalf("recovered membership = %#v", membership)
				}
			} else {
				var organization auth.Organization
				organization, err = p.CreateOrganization(t.Context(), "detent_customer", "Customer")
				if !tt.wantErr && organization.ExternalID != "detent_customer" {
					t.Fatalf("recovered organization = %#v", organization)
				}
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("recover creation error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestWorkOSInvitationAccountLookup(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	t.Parallel()
	f := newWorkOSFixture(t)
	p := f.provider(t)
	for _, tt := range []struct {
		name            string
		exists, wantErr bool
	}{
		{name: "existing", exists: true}, {name: "new"}, {name: "wrong email", wantErr: true}, {name: "invalid id", wantErr: true}, {name: "unavailable", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.mode.Store("user-lookup-" + tt.name)
			exists, err := p.HasUser(t.Context(), "Customer@Example.com")
			if exists != tt.exists || (err != nil) != tt.wantErr {
				t.Fatalf("HasUser = %v,%v", exists, err)
			}
		})
	}
}

func TestWorkOSInvitationSecurity(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	p := f.provider(t)
	if _, err := p.Invite(t.Context(), "org_customer", "Customer@Example.com", "viewer", "user_owner"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "valid"}, {name: "invitation-reused", wantErr: true}, {name: "invitation-expired", wantErr: true},
		{name: "invitation-revoked", wantErr: true}, {name: "invitation-no-org", wantErr: true},
		{name: "invitation-wrong-email", wantErr: true}, {name: "invitation-unverified-user", wantErr: true},
		{name: "invitation-wrong-user", wantErr: true}, {name: "invitation-accepted-wrong-user", wantErr: true},
		{name: "invitation-accepted-wrong-org", wantErr: true}, {name: "invitation-accepted-wrong-id", wantErr: true},
	}
	for _, byID := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/byID=%t", tt.name, byID), func(t *testing.T) {
				f.mode.Store(tt.name)
				before := f.accepted.Load()
				var err error
				if byID {
					err = auth.AcceptInvitationID(t.Context(), p, "invitation_customer", "user_customer")
				} else {
					err = p.AcceptInvitation(t.Context(), "invitation_token", "user_customer")
				}
				if (err != nil) != tt.wantErr {
					t.Fatalf("AcceptInvitation() error = %v, wantErr %v", err, tt.wantErr)
				}
				if tt.wantErr && !strings.HasPrefix(tt.name, "invitation-accepted-") && f.accepted.Load() != before {
					t.Fatal("invalid invitation reached provider acceptance")
				}
			})
		}
	}
}

func TestWorkOSInvitationLookupSupportsAcceptanceRecovery(t *testing.T) {
	t.Parallel()
	f := newWorkOSFixture(t)
	p := f.provider(t)
	for _, tt := range []struct {
		name    string
		state   string
		wantErr bool
	}{
		{name: "valid", state: "pending"},
		{name: "invitation-reused", state: "accepted"},
		{name: "invitation-accepted-expired", state: "accepted"},
		{name: "invitation-accepted-missing-user", wantErr: true},
		{name: "invitation-accepted-invalid-user", wantErr: true},
		{name: "invitation-revoked", state: "revoked"},
		{name: "invitation-expired", state: "pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.mode.Store(tt.name)
			invitation, err := p.Invitation(t.Context(), "invitation_token")
			if (err != nil) != tt.wantErr || (err == nil && invitation.State != tt.state) {
				t.Fatalf("Invitation() state = %q, error = %v", invitation.State, err)
			}
			if tt.state == "accepted" && invitation.AcceptedUserID != "user_customer" {
				t.Fatal("accepted invitation omitted authoritative recipient identity")
			}
		})
	}
}

func TestWorkOSInvitationDelivery(t *testing.T) {
	t.Parallel()
	f := &workosFixture{t: t, now: time.Now().UTC()}
	p, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
		ClientID: "client_detent", APIKey: "fixture-secret", RedirectURL: "https://app.example.com/auth/callback",
		HTTPClient: &http.Client{Transport: workosFixtureTransport{handler: http.HandlerFunc(f.serveHTTP)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"revoke", "resend"} {
		for _, test := range []struct {
			mode    string
			wantErr bool
		}{
			{mode: "valid"},
			{mode: "invitation-delivery-unavailable", wantErr: true},
			{mode: "invitation-delivery-wrong-state", wantErr: true},
			{mode: "invitation-delivery-wrong-id", wantErr: true},
		} {
			t.Run(action+"/"+test.mode, func(t *testing.T) {
				f.mode.Store(test.mode)
				var err error
				if action == "revoke" {
					err = auth.RevokeInvitationID(t.Context(), p, "invitation_customer")
				} else {
					err = auth.ResendInvitationID(t.Context(), p, "invitation_customer")
				}
				if (err != nil) != test.wantErr {
					t.Fatalf("%s error = %v, wantErr %t", action, err, test.wantErr)
				}
			})
		}
	}
}

type workosFixtureTransport struct {
	handler http.Handler
}

func (t workosFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

func TestWorkOSInvalidArgumentsAndRedirects(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	f := newWorkOSFixture(t)
	p := f.provider(t)
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "organization", run: func() error { _, err := p.Organization(t.Context(), "../other"); return err }},
		{name: "create organization", run: func() error { _, err := p.CreateOrganization(t.Context(), "", "Customer"); return err }},
		{name: "memberships", run: func() error { _, err := p.Memberships(t.Context(), "", ""); return err }},
		{name: "create membership", run: func() error {
			_, err := p.CreateMembership(t.Context(), "user_customer", "org_customer", "superuser")
			return err
		}},
		{name: "set role", run: func() error { return p.SetMembershipRole(t.Context(), "membership_customer", "superuser") }},
		{name: "revoke membership", run: func() error { return p.RevokeMembership(t.Context(), "../other") }},
		{name: "invite", run: func() error {
			_, err := p.Invite(t.Context(), "org_customer", "bad email", "viewer", "user_owner")
			return err
		}},
		{name: "invitation", run: func() error { _, err := p.Invitation(t.Context(), "../other"); return err }},
		{name: "accept invitation", run: func() error { return p.AcceptInvitation(t.Context(), "invitation_token", "../other") }},
		{name: "revoke session", run: func() error { return p.RevokeSession(t.Context(), "../other") }},
		{name: "revoke invitation", run: func() error { return auth.RevokeInvitationID(t.Context(), p, "../other") }},
		{name: "resend invitation", run: func() error { return auth.ResendInvitationID(t.Context(), p, "../other") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); !errors.Is(err, auth.ErrHostedIdentity) {
				t.Fatalf("invalid input error = %v", err)
			}
		})
	}
	var redirected atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	t.Cleanup(destination.Close)
	f.redirect.Store(destination.URL)
	f.mode.Store("redirect")
	if _, err := p.Organization(t.Context(), "org_customer"); !errors.Is(err, auth.ErrHostedIdentity) || redirected.Load() {
		t.Fatalf("redirect error = %v, followed = %v", err, redirected.Load())
	}
}

type workosFixture struct {
	t          *testing.T
	apiURL     string
	client     *http.Client
	key        *rsa.PrivateKey
	wrongKey   *rsa.PrivateKey
	now        time.Time
	mode       atomic.Value
	redirect   atomic.Value
	issuer     atomic.Value
	accepted   atomic.Int64
	mu         sync.Mutex
	codes      map[string]bool
	sessionOrg *string
}

var fixtureSigningKeys = sync.OnceValues(func() ([2]*rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return [2]*rsa.PrivateKey{}, err
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	return [2]*rsa.PrivateKey{key, wrongKey}, err
})

func newWorkOSFixture(t *testing.T) *workosFixture {
	t.Helper()
	keys, err := fixtureSigningKeys()
	if err != nil {
		t.Fatal(err)
	}
	f := &workosFixture{t: t, key: keys[0], wrongKey: keys[1], now: time.Now().UTC().Truncate(time.Second), codes: make(map[string]bool)}
	f.mode.Store("valid")
	f.redirect.Store("")
	f.issuer.Store("")
	f.apiURL = "https://workos.test"
	f.client = &http.Client{Transport: workosFixtureTransport{handler: http.HandlerFunc(f.serveHTTP)}}
	return f
}

func (f *workosFixture) provider(t *testing.T) auth.HostedProvider {
	t.Helper()
	provider, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
		APIURL: f.apiURL, ClientID: "client_detent", APIKey: "fixture-secret",
		RedirectURL: "https://app.example.com/auth/callback", HTTPClient: f.client,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func (f *workosFixture) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		f.t.Errorf("encode fixture: %v", err)
	}
}

func (f *workosFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	mode := f.mode.Load().(string)
	if r.URL.Path == "/sso/jwks/client_detent" {
		if mode == "jwks-down" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.writeJSON(w, map[string]any{"keys": []any{rsaJWK(&f.key.PublicKey, "primary")}})
		return
	}
	if r.Header.Get("Authorization") != "Bearer fixture-secret" {
		f.t.Error("provider request missing API authentication")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if strings.HasPrefix(mode, "recover-") && r.Method == http.MethodPost {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch mode {
	case "http-error":
		http.Error(w, "private customer content fixture-secret", http.StatusBadGateway)
		return
	case "malformed":
		f.writeJSON(w, "private customer content fixture-secret")
		return
	case "oversized":
		f.writeJSON(w, map[string]string{"padding": strings.Repeat("x", 1<<20)})
		return
	case "trailing-json":
		f.writeJSON(w, map[string]string{})
		f.writeJSON(w, map[string]string{"private": "content"})
		return
	case "redirect":
		http.Redirect(w, r, f.redirect.Load().(string), http.StatusTemporaryRedirect)
		return
	}
	switch r.URL.Path {
	case "/user_management/authenticate":
		f.exchange(w, r, mode)
	case "/user_management/users/user_customer/sessions":
		f.sessions(w, r, mode)
	case "/user_management/organization_memberships", "/user_management/organization_memberships/membership_customer":
		f.memberships(w, r, mode)
	case "/organizations", "/organizations/org_customer", "/organizations/external_id/detent_customer":
		if r.Method == http.MethodPost {
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["external_id"] != "detent_customer" || request["name"] != "Customer" {
				f.t.Error("incorrect organization creation payload")
			}
		}
		organization := auth.Organization{ID: "org_customer", ExternalID: "detent_customer", Name: "Customer"}
		switch mode {
		case "recover-organization-wrong-external":
			organization.ExternalID = "detent_other"
		case "recover-organization-invalid-id":
			organization.ID = "../other"
		case "recover-organization-unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.writeJSON(w, organization)
	case "/user_management/invitations", "/user_management/invitations/by_token/invitation_token", "/user_management/invitations/invitation_customer", "/user_management/invitations/invitation_customer/accept", "/user_management/invitations/invitation_customer/revoke", "/user_management/invitations/invitation_customer/resend":
		f.invitation(w, r, mode)
	case "/user_management/users":
		if r.Method != http.MethodGet || r.URL.Query().Get("email") != "customer@example.com" || r.URL.Query().Get("limit") != "1" {
			f.t.Error("incorrect account lookup filter")
		}
		if mode == "user-lookup-unavailable" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		users := []map[string]string{}
		if mode != "user-lookup-new" {
			user := map[string]string{"id": "user_customer", "email": "customer@example.com"}
			if mode == "user-lookup-wrong email" {
				user["email"] = "other@example.com"
			}
			if mode == "user-lookup-invalid id" {
				user["id"] = "../other"
			}
			users = append(users, user)
		}
		f.writeJSON(w, map[string]any{"data": users})
	case "/user_management/users/user_customer":
		user := map[string]any{"id": "user_customer", "email": "customer@example.com", "email_verified": true, "first_name": "Customer", "last_name": "Person"}
		switch mode {
		case "invitation-wrong-email":
			user["email"] = "different@example.com"
		case "invitation-unverified-user":
			user["email_verified"] = false
		case "invitation-wrong-user":
			user["id"] = "user_other"
		}
		f.writeJSON(w, user)
	case "/user_management/organization_memberships/membership_customer/deactivate":
		if r.Method != http.MethodPut {
			f.t.Error("incorrect membership deactivation method")
		}
		w.WriteHeader(http.StatusNoContent)
	case "/user_management/sessions/revoke":
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["session_id"] != "session_customer" || r.Method != http.MethodPost {
			f.t.Error("incorrect session revocation request")
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected provider path %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *workosFixture) exchange(w http.ResponseWriter, r *http.Request, mode string) {
	var request map[string]string
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Error(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if request["grant_type"] == "refresh_token" {
		f.refresh(w, r, request, mode)
		return
	}
	if r.Method != http.MethodPost || request["grant_type"] != "authorization_code" || request["client_id"] != "client_detent" || request["client_secret"] != "fixture-secret" {
		f.t.Error("incorrect code exchange request")
	}
	if !strings.HasPrefix(mode, "support") && mode != "ordinary-no-pkce" && request["code_verifier"] != "verifier" {
		f.t.Error("code exchange omitted PKCE")
	}
	f.mu.Lock()
	used := f.codes[request["code"]]
	f.codes[request["code"]] = true
	f.mu.Unlock()
	if used {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	claims := map[string]any{
		"iss": f.apiURL + "/user_management/client_detent", "sub": "user_customer", "client_id": "client_detent", "sid": "session_customer",
		"org_id": "org_customer", "iat": f.now.Unix(), "exp": f.now.Add(10 * time.Minute).Unix(),
	}
	user := map[string]any{"id": "user_customer", "email": "Customer@Example.com", "email_verified": true}
	response := map[string]any{"user": user, "organization_id": "org_customer"}
	key := f.key
	if strings.HasPrefix(mode, "support") {
		claims["act"] = map[string]string{"sub": "support@example.com"}
		response["impersonator"] = map[string]string{"email": "support@example.com", "reason": "troubleshooting"}
	}
	switch mode {
	case "issuer-slash":
		claims["iss"] = f.apiURL + "/user_management/client_detent/"
	case "legacy-issuer":
		claims["iss"] = f.apiURL
	case "fixed-issuer":
		claims["iss"] = f.issuer.Load().(string)
	case "custom-issuer":
		claims["iss"] = "https://auth.example.com/user_management/client_default"
	case "audience":
		claims["aud"] = "client_detent"
	case "audience-array":
		claims["aud"] = []string{"client_detent", "additional"}
	case "wrong-issuer":
		claims["iss"] = "https://other.example.com"
	case "wrong-client":
		claims["client_id"] = "client_other"
	case "missing-client":
		delete(claims, "client_id")
	case "wrong-audience":
		claims["aud"] = "client_other"
	case "empty-audience":
		claims["aud"] = []string{}
	case "null-audience":
		claims["aud"] = nil
	case "invalid-signature":
		key = f.wrongKey
	case "expired-token":
		claims["exp"] = f.now.Add(-time.Minute).Unix()
	case "missing-iat":
		delete(claims, "iat")
	case "future-iat":
		claims["iat"] = f.now.Add(time.Hour).Unix()
	case "missing-sid":
		delete(claims, "sid")
	case "wrong-subject":
		claims["sub"] = "user_other"
	case "wrong-org-response":
		response["organization_id"] = "org_other"
	case "unverified-email":
		user["email_verified"] = false
	case "missing-email":
		delete(user, "email")
	case "support-email":
		claims["act"] = map[string]string{"email": "support@example.com"}
	case "support-missing-act":
		delete(claims, "act")
	case "support-missing-response":
		delete(response, "impersonator")
	case "support-wrong-act":
		claims["act"] = map[string]string{"sub": "other@example.com"}
	case "support-conflicting-act":
		claims["act"] = map[string]string{"sub": "other@example.com", "email": "support@example.com"}
	case "support-unknown-reason":
		response["impersonator"] = map[string]string{"email": "support@example.com", "reason": "customer content"}
	case "support-no-org", "unscoped":
		delete(claims, "org_id")
		delete(response, "organization_id")
	}
	f.mu.Lock()
	if f.sessionOrg == nil {
		organization, _ := claims["org_id"].(string)
		f.sessionOrg = &organization
	}
	f.mu.Unlock()
	if mode != "missing-token" {
		response["access_token"] = signTestJWT(f.t, key, claims)
		response["refresh_token"] = "refresh_customer"
	}
	f.writeJSON(w, response)
}

func (f *workosFixture) sessions(w http.ResponseWriter, r *http.Request, mode string) {
	session := map[string]any{
		"id": "session_customer", "user_id": "user_customer", "organization_id": f.originalSessionOrganization(), "status": "active",
		"created_at": f.now.Add(-10 * time.Minute), "expires_at": f.now.Add(50 * time.Minute), "ended_at": nil,
	}
	if strings.HasPrefix(mode, "support") {
		session["impersonator"] = map[string]string{"email": "support@example.com", "reason": "troubleshooting"}
		session["expires_at"] = f.now.Add(2 * time.Hour)
	}
	switch mode {
	case "session-revoked":
		session["status"] = "revoked"
	case "session-ended":
		session["ended_at"] = f.now.Add(-time.Minute)
	case "session-expired":
		session["expires_at"] = f.now.Add(-time.Minute)
	case "session-wrong-user":
		session["user_id"] = "user_other"
	case "session-listed-other-org":
		session["organization_id"] = "org_other"
	case "session-created-changed":
		session["created_at"] = f.now.Add(-time.Minute)
	case "session-shortened":
		session["expires_at"] = f.now.Add(time.Minute)
	case "session-extended":
		session["expires_at"] = f.now.Add(2 * time.Hour)
	case "support-wrong-session-actor":
		session["impersonator"] = map[string]string{"email": "other@example.com", "reason": "troubleshooting"}
	case "support-wrong-session-reason":
		session["impersonator"] = map[string]string{"email": "support@example.com", "reason": "account-recovery"}
	case "support-short-expiry":
		session["expires_at"] = f.now.Add(5 * time.Minute)
	case "support-expired-hour":
		session["created_at"] = f.now.Add(-2 * time.Hour)
	}
	data := []any{session}
	after := ""
	if mode == "session-missing" || mode == "session-page-cycle" || (mode == "pagination" && r.URL.Query().Get("after") == "") {
		data = nil
		if mode != "session-missing" {
			after = "next_page"
		}
	}
	f.writeJSON(w, map[string]any{"data": data, "list_metadata": map[string]string{"after": after}})
}

func (f *workosFixture) originalSessionOrganization() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessionOrg == nil || *f.sessionOrg == "" {
		return nil
	}
	return *f.sessionOrg
}

func (f *workosFixture) memberships(w http.ResponseWriter, r *http.Request, mode string) {
	membership := auth.Membership{ID: "membership_customer", UserID: "user_customer", OrganizationID: "org_customer", Status: "active"}
	membership.Role.Slug = "owner"
	if r.Method != http.MethodGet {
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["role_slug"] != "owner" {
			f.t.Error("membership request missing explicit role")
		}
		if r.Method == http.MethodPost && (request["user_id"] != "user_customer" || request["organization_id"] != "org_customer") {
			f.t.Error("membership creation missing identity scope")
		}
		f.writeJSON(w, membership)
		return
	}
	if r.URL.Query().Get("statuses") != "active" || r.URL.Query().Get("user_id") != "user_customer" {
		f.t.Error("membership query missing active user filter")
	}
	switch mode {
	case "membership-wrong-user", "recover-membership-wrong-user":
		membership.UserID = "user_other"
	case "membership-wrong-org", "recover-membership-wrong-org":
		membership.OrganizationID = "org_other"
	case "membership-inactive", "recover-membership-inactive":
		membership.Status = "inactive"
	case "membership-unknown-role":
		membership.Role.Slug = "superuser"
	case "recover-membership-wrong-role":
		membership.Role.Slug = "member"
	}
	after := ""
	if mode == "membership-page-cycle" || (mode == "membership-pagination" && r.URL.Query().Get("after") == "") {
		after = "next_page"
	}
	data := []auth.Membership{membership}
	if mode == "recover-membership-missing" {
		data = nil
	}
	f.writeJSON(w, map[string]any{"data": data, "list_metadata": map[string]string{"after": after}})
}

func (f *workosFixture) invitation(w http.ResponseWriter, r *http.Request, mode string) {
	invitation := auth.Invitation{ID: "invitation_customer", Email: "customer@example.com", OrganizationID: "org_customer", State: "pending", ExpiresAt: f.now.Add(time.Hour)}
	if r.Method == http.MethodPost && r.URL.Path == "/user_management/invitations" {
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["email"] != invitation.Email || request["organization_id"] != invitation.OrganizationID || request["role_slug"] != "viewer" || request["inviter_user_id"] != "user_owner" {
			f.t.Error("incorrect invitation request")
		}
	}
	if strings.HasSuffix(r.URL.Path, "/accept") {
		f.accepted.Add(1)
		invitation.State, invitation.AcceptedUserID = "accepted", "user_customer"
	}
	if strings.HasSuffix(r.URL.Path, "/revoke") || strings.HasSuffix(r.URL.Path, "/resend") {
		if r.Method != http.MethodPost {
			f.t.Errorf("invitation delivery method = %s", r.Method)
		}
		if mode == "invitation-delivery-unavailable" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/revoke") {
			invitation.State = "revoked"
		}
		if mode == "invitation-delivery-wrong-state" {
			invitation.State, invitation.AcceptedUserID = "accepted", "user_customer"
		}
		if mode == "invitation-delivery-wrong-id" {
			invitation.ID = "invitation_other"
		}
	}
	switch mode {
	case "invitation-reused":
		invitation.State, invitation.AcceptedUserID = "accepted", "user_customer"
	case "invitation-accepted-expired":
		invitation.State, invitation.AcceptedUserID = "accepted", "user_customer"
		invitation.ExpiresAt = f.now.Add(-time.Hour)
	case "invitation-accepted-missing-user":
		invitation.State = "accepted"
	case "invitation-accepted-invalid-user":
		invitation.State, invitation.AcceptedUserID = "accepted", "../other"
	case "invitation-expired":
		invitation.ExpiresAt = f.now.Add(-time.Hour)
	case "invitation-revoked":
		invitation.State = "revoked"
	case "invitation-no-org":
		invitation.OrganizationID = ""
	case "invitation-accepted-wrong-user":
		if invitation.State == "accepted" {
			invitation.AcceptedUserID = "user_other"
		}
	case "invitation-accepted-wrong-org":
		if invitation.State == "accepted" {
			invitation.OrganizationID = "org_other"
		}
	case "invitation-accepted-wrong-id":
		if invitation.State == "accepted" {
			invitation.ID = "invitation_other"
		}
	}
	f.writeJSON(w, invitation)
}
