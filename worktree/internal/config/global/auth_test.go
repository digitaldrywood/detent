package global

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMagicLinkAuth(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	raw := `apiVersion: detent/v1
kind: GlobalConfig
auth:
  mode: magic_link
  public_url: https://detent.example.com
  allowed_emails:
    - operator@example.com
  link_ttl: 10m
  session_ttl: 168h
  smtp:
    host: smtp.example.com
    port: 587
    username: operator
    password: secret
    from: detent@example.com
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    weight: 1
    priority: 0
`
	cfg, err := Parse([]byte(raw), filepath.Join(paths.root, "global.yaml"), WithHome(paths.home))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Auth.MagicLinkEnabled() || cfg.Auth.SMTP.NormalizedPort() != 587 {
		t.Fatalf("Auth = %#v, want enabled magic link", cfg.Auth)
	}
	if got, err := cfg.Auth.LinkTTLDuration(); err != nil || got != 10*time.Minute {
		t.Fatalf("LinkTTLDuration() = %s, %v", got, err)
	}
	if got, err := cfg.Auth.SessionTTLDuration(); err != nil || got != 168*time.Hour {
		t.Fatalf("SessionTTLDuration() = %s, %v", got, err)
	}
}

func TestParseOIDCAuth(t *testing.T) {
	t.Parallel()

	paths := createProjectFiles(t)
	raw := `apiVersion: detent/v1
kind: GlobalConfig
auth:
  mode: oidc
  public_url: https://detent.example.com
  allowed_emails:
    - operator@example.com
  allowed_domains:
    - example.org
  session_ttl: 8h
  oidc:
    issuer_url: https://issuer.example.com
    client_id: detent
    client_secret: secret
    scopes:
      - profile
      - groups
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects:
  - id: detent
    workflow: ` + paths.workflow + `
    workdir: ` + paths.workdir + `
    weight: 1
    priority: 0
`
	cfg, err := Parse([]byte(raw), filepath.Join(paths.root, "global.yaml"), WithHome(paths.home))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !cfg.Auth.OIDCEnabled() || cfg.Auth.MagicLinkEnabled() {
		t.Fatalf("Auth = %#v, want enabled oidc", cfg.Auth)
	}
	if cfg.Auth.OIDC.IssuerURL != "https://issuer.example.com" || cfg.Auth.OIDC.ClientID != "detent" || cfg.Auth.OIDC.ClientSecret != "secret" {
		t.Fatalf("Auth.OIDC = %#v", cfg.Auth.OIDC)
	}
	if got, err := cfg.Auth.SessionTTLDuration(); err != nil || got != 8*time.Hour {
		t.Fatalf("SessionTTLDuration() = %s, %v", got, err)
	}
}

func TestMagicLinkAuthValidation(t *testing.T) {
	t.Parallel()

	valid := Auth{
		Mode:          AuthModeMagicLink,
		AllowedEmails: []string{"operator@example.com"},
		SMTP:          SMTP{Host: "smtp.example.com", From: "detent@example.com"},
	}
	tests := []struct {
		name string
		edit func(*Auth)
		want string
	}{
		{name: "missing mode", edit: func(auth *Auth) { auth.Mode = "" }, want: "auth.mode: is required"},
		{name: "unsupported mode", edit: func(auth *Auth) { auth.Mode = "password" }, want: "auth.mode: must equal magic_link"},
		{name: "missing allowlist", edit: func(auth *Auth) { auth.AllowedEmails = nil }, want: "auth.allowed_emails"},
		{name: "invalid email", edit: func(auth *Auth) { auth.AllowedEmails = []string{"invalid"} }, want: "must be a valid email"},
		{name: "duplicate email", edit: func(auth *Auth) { auth.AllowedEmails = []string{"operator@example.com", "OPERATOR@example.com"} }, want: "duplicates"},
		{name: "domain allowlist", edit: func(auth *Auth) { auth.AllowedDomains = []string{"example.com"} }, want: "allowed_domains: is only valid"},
		{name: "invalid public url", edit: func(auth *Auth) { auth.PublicURL = "/relative" }, want: "absolute http or https"},
		{name: "invalid link ttl", edit: func(auth *Auth) { auth.LinkTTL = "0s" }, want: "auth.link_ttl"},
		{name: "invalid session ttl", edit: func(auth *Auth) { auth.SessionTTL = "forever" }, want: "auth.session_ttl"},
		{name: "missing smtp host", edit: func(auth *Auth) { auth.SMTP.Host = "" }, want: "auth.smtp.host"},
		{name: "missing smtp from", edit: func(auth *Auth) { auth.SMTP.From = "" }, want: "auth.smtp.from"},
		{name: "partial credentials", edit: func(auth *Auth) { auth.SMTP.Username = "user" }, want: "must be set together"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			cfg.AllowedEmails = append([]string(nil), valid.AllowedEmails...)
			tt.edit(&cfg)
			if got := strings.Join(cfg.validate("auth"), "; "); !strings.Contains(got, tt.want) {
				t.Fatalf("validate() = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestAuthDefaultsAndDisabledState(t *testing.T) {
	t.Parallel()

	if (Auth{}).Configured() || (Auth{}).MagicLinkEnabled() || (Auth{}).OIDCEnabled() {
		t.Fatal("zero Auth is configured, want disabled")
	}
	linkTTL, err := (Auth{}).LinkTTLDuration()
	if err != nil || linkTTL != defaultMagicLinkTTL {
		t.Fatalf("LinkTTLDuration() = %s, %v", linkTTL, err)
	}
	sessionTTL, err := (Auth{}).SessionTTLDuration()
	if err != nil || sessionTTL != defaultAuthSessionTTL {
		t.Fatalf("SessionTTLDuration() = %s, %v", sessionTTL, err)
	}
	if got := (SMTP{}).NormalizedPort(); got != defaultAuthSMTPPort {
		t.Fatalf("NormalizedPort() = %d, want %d", got, defaultAuthSMTPPort)
	}
}

func TestOIDCAuthValidation(t *testing.T) {
	t.Parallel()

	valid := Auth{
		Mode:           AuthModeOIDC,
		AllowedDomains: []string{"example.com"},
		OIDC: OIDC{
			IssuerURL:    "https://issuer.example.com",
			ClientID:     "detent",
			ClientSecret: "secret",
			Scopes:       []string{"profile"},
		},
	}
	tests := []struct {
		name string
		edit func(*Auth)
		want string
	}{
		{name: "missing allowlist", edit: func(auth *Auth) { auth.AllowedDomains = nil }, want: "allowed_emails or auth.allowed_domains"},
		{name: "invalid domain", edit: func(auth *Auth) { auth.AllowedDomains = []string{"@example.com"} }, want: "valid domain without @"},
		{name: "duplicate domain", edit: func(auth *Auth) { auth.AllowedDomains = []string{"example.com", "EXAMPLE.com"} }, want: "duplicates"},
		{name: "insecure issuer", edit: func(auth *Auth) { auth.OIDC.IssuerURL = "http://issuer.example.com" }, want: "absolute https URL"},
		{name: "insecure public url", edit: func(auth *Auth) { auth.PublicURL = "http://detent.example.com" }, want: "public_url: must use https"},
		{name: "missing client id", edit: func(auth *Auth) { auth.OIDC.ClientID = "" }, want: "client_id: is required"},
		{name: "missing client secret", edit: func(auth *Auth) { auth.OIDC.ClientSecret = "" }, want: "client_secret: is required"},
		{name: "invalid scope", edit: func(auth *Auth) { auth.OIDC.Scopes = []string{"read groups"} }, want: "single non-blank scope"},
		{name: "duplicate scope", edit: func(auth *Auth) { auth.OIDC.Scopes = []string{"profile", "profile"} }, want: "duplicates"},
		{name: "magic link ttl", edit: func(auth *Auth) { auth.LinkTTL = "5m" }, want: "only valid when auth.mode is magic_link"},
		{name: "smtp settings", edit: func(auth *Auth) { auth.SMTP = SMTP{Host: "smtp.example.com", From: "detent@example.com"} }, want: "auth.smtp: is only valid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			cfg.AllowedDomains = append([]string(nil), valid.AllowedDomains...)
			cfg.OIDC.Scopes = append([]string(nil), valid.OIDC.Scopes...)
			tt.edit(&cfg)
			if got := strings.Join(cfg.validate("auth"), "; "); !strings.Contains(got, tt.want) {
				t.Fatalf("validate() = %q, want substring %q", got, tt.want)
			}
		})
	}
}
