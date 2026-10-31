package global

import (
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	AuthModeMagicLink     = "magic_link"
	AuthModeOIDC          = "oidc"
	defaultMagicLinkTTL   = 15 * time.Minute
	defaultAuthSessionTTL = 30 * 24 * time.Hour
	defaultAuthSMTPPort   = 587
)

type Auth struct {
	Mode           string   `yaml:"mode,omitempty"`
	PublicURL      string   `yaml:"public_url,omitempty"`
	AllowedEmails  []string `yaml:"allowed_emails,omitempty"`
	AllowedDomains []string `yaml:"allowed_domains,omitempty"`
	LinkTTL        string   `yaml:"link_ttl,omitempty"`
	SessionTTL     string   `yaml:"session_ttl,omitempty"`
	SMTP           SMTP     `yaml:"smtp,omitempty"`
	OIDC           OIDC     `yaml:"oidc,omitempty"`
}

type SMTP struct {
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	From     string `yaml:"from,omitempty"`
}

type OIDC struct {
	IssuerURL    string   `yaml:"issuer_url,omitempty"`
	ClientID     string   `yaml:"client_id,omitempty"`
	ClientSecret string   `yaml:"client_secret,omitempty"`
	Scopes       []string `yaml:"scopes,omitempty"`
}

func (a Auth) IsZero() bool {
	return !a.Configured()
}

func (a Auth) Configured() bool {
	return strings.TrimSpace(a.Mode) != "" ||
		strings.TrimSpace(a.PublicURL) != "" ||
		len(a.AllowedEmails) > 0 ||
		len(a.AllowedDomains) > 0 ||
		strings.TrimSpace(a.LinkTTL) != "" ||
		strings.TrimSpace(a.SessionTTL) != "" ||
		!a.SMTP.IsZero() ||
		!a.OIDC.IsZero()
}

func (a Auth) MagicLinkEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(a.Mode), AuthModeMagicLink)
}

func (a Auth) OIDCEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(a.Mode), AuthModeOIDC)
}

func (a Auth) LinkTTLDuration() (time.Duration, error) {
	return authDuration(a.LinkTTL, defaultMagicLinkTTL, "auth.link_ttl")
}

func (a Auth) SessionTTLDuration() (time.Duration, error) {
	return authDuration(a.SessionTTL, defaultAuthSessionTTL, "auth.session_ttl")
}

func (s SMTP) IsZero() bool {
	return strings.TrimSpace(s.Host) == "" && s.Port == 0 && strings.TrimSpace(s.Username) == "" && s.Password == "" && strings.TrimSpace(s.From) == ""
}

func (o OIDC) IsZero() bool {
	return strings.TrimSpace(o.IssuerURL) == "" && strings.TrimSpace(o.ClientID) == "" && o.ClientSecret == "" && len(o.Scopes) == 0
}

func (s SMTP) NormalizedPort() int {
	if s.Port > 0 {
		return s.Port
	}
	return defaultAuthSMTPPort
}

func (a Auth) validate(prefix string) []string {
	if !a.Configured() {
		return nil
	}
	if prefix == "" {
		prefix = "auth"
	}

	var problems []string
	mode := strings.TrimSpace(a.Mode)
	if mode == "" {
		problems = append(problems, prefix+".mode: is required")
	} else if mode != AuthModeMagicLink && mode != AuthModeOIDC {
		problems = append(problems, prefix+".mode: must equal "+AuthModeMagicLink+" or "+AuthModeOIDC)
	}
	if mode == AuthModeMagicLink && len(a.AllowedEmails) == 0 {
		problems = append(problems, prefix+".allowed_emails: must contain at least one email address")
	}
	if mode == AuthModeOIDC && len(a.AllowedEmails) == 0 && len(a.AllowedDomains) == 0 {
		problems = append(problems, prefix+".allowed_emails or "+prefix+".allowed_domains: must contain at least one entry")
	}
	seen := make(map[string]struct{}, len(a.AllowedEmails))
	for index, email := range a.AllowedEmails {
		field := prefix + ".allowed_emails[" + strconv.Itoa(index) + "]"
		normalized := strings.ToLower(strings.TrimSpace(email))
		if !validAuthEmail(normalized) {
			problems = append(problems, field+": must be a valid email address")
			continue
		}
		if _, ok := seen[normalized]; ok {
			problems = append(problems, field+": duplicates an earlier email address")
			continue
		}
		seen[normalized] = struct{}{}
	}
	seenDomains := make(map[string]struct{}, len(a.AllowedDomains))
	for index, domain := range a.AllowedDomains {
		field := prefix + ".allowed_domains[" + strconv.Itoa(index) + "]"
		normalized := strings.ToLower(strings.TrimSpace(domain))
		if !validAuthDomain(normalized) {
			problems = append(problems, field+": must be a valid domain without @")
			continue
		}
		if _, ok := seenDomains[normalized]; ok {
			problems = append(problems, field+": duplicates an earlier domain")
			continue
		}
		seenDomains[normalized] = struct{}{}
	}
	if _, err := a.SessionTTLDuration(); err != nil {
		problems = append(problems, err.Error())
	}
	if publicURL := strings.TrimSpace(a.PublicURL); publicURL != "" {
		parsed, err := url.Parse(publicURL)
		if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			problems = append(problems, prefix+".public_url: must be an absolute http or https URL without query or fragment")
		}
	}
	switch mode {
	case AuthModeMagicLink:
		if len(a.AllowedDomains) > 0 {
			problems = append(problems, prefix+".allowed_domains: is only valid when "+prefix+".mode is "+AuthModeOIDC)
		}
		if _, err := a.LinkTTLDuration(); err != nil {
			problems = append(problems, err.Error())
		}
		problems = append(problems, a.SMTP.validate(prefix+".smtp")...)
		if !a.OIDC.IsZero() {
			problems = append(problems, prefix+".oidc: is only valid when "+prefix+".mode is "+AuthModeOIDC)
		}
	case AuthModeOIDC:
		if publicURL := strings.TrimSpace(a.PublicURL); publicURL != "" {
			parsed, err := url.Parse(publicURL)
			if err == nil && parsed != nil && parsed.Host != "" && !secureOIDCIssuer(parsed) {
				problems = append(problems, prefix+".public_url: must use https; loopback http is allowed for testing")
			}
		}
		if strings.TrimSpace(a.LinkTTL) != "" {
			problems = append(problems, prefix+".link_ttl: is only valid when "+prefix+".mode is "+AuthModeMagicLink)
		}
		if !a.SMTP.IsZero() {
			problems = append(problems, prefix+".smtp: is only valid when "+prefix+".mode is "+AuthModeMagicLink)
		}
		problems = append(problems, a.OIDC.validate(prefix+".oidc")...)
	}
	return problems
}

func (o OIDC) validate(prefix string) []string {
	var problems []string
	issuerURL := strings.TrimSpace(o.IssuerURL)
	parsed, err := url.Parse(issuerURL)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || !secureOIDCIssuer(parsed) {
		problems = append(problems, prefix+".issuer_url: must be an absolute https URL without query or fragment; loopback http is allowed for testing")
	}
	if strings.TrimSpace(o.ClientID) == "" {
		problems = append(problems, prefix+".client_id: is required")
	}
	if o.ClientSecret == "" {
		problems = append(problems, prefix+".client_secret: is required")
	}
	seen := make(map[string]struct{}, len(o.Scopes))
	for index, scope := range o.Scopes {
		field := prefix + ".scopes[" + strconv.Itoa(index) + "]"
		if strings.TrimSpace(scope) == "" || strings.ContainsAny(scope, " \t\r\n") {
			problems = append(problems, field+": must be a single non-blank scope")
			continue
		}
		if _, ok := seen[scope]; ok {
			problems = append(problems, field+": duplicates an earlier scope")
			continue
		}
		seen[scope] = struct{}{}
	}
	return problems
}

func secureOIDCIssuer(issuer *url.URL) bool {
	if issuer == nil {
		return false
	}
	if issuer.Scheme == "https" {
		return true
	}
	if issuer.Scheme != "http" {
		return false
	}
	host := strings.Trim(issuer.Hostname(), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func (s SMTP) validate(prefix string) []string {
	var problems []string
	if strings.TrimSpace(s.Host) == "" {
		problems = append(problems, prefix+".host: is required")
	} else if strings.ContainsAny(s.Host, "\r\n") {
		problems = append(problems, prefix+".host: must be a single line")
	}
	if s.Port < 0 || s.Port > 65535 {
		problems = append(problems, prefix+".port: must be between 1 and 65535 when set")
	}
	if !validAuthEmail(strings.TrimSpace(s.From)) {
		problems = append(problems, prefix+".from: must be a valid email address")
	}
	if strings.ContainsAny(s.Username, "\r\n") {
		problems = append(problems, prefix+".username: must be a single line")
	}
	if strings.ContainsAny(s.Password, "\r\n") {
		problems = append(problems, prefix+".password: must be a single line")
	}
	if (strings.TrimSpace(s.Username) == "") != (s.Password == "") {
		problems = append(problems, prefix+".username and "+prefix+".password: must be set together")
	}
	return problems
}

func authDuration(value string, fallback time.Duration, field string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s: must be a positive duration", field)
	}
	return duration, nil
}

func validAuthEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address != nil && strings.EqualFold(address.Address, value)
}

func validAuthDomain(value string) bool {
	if value == "" || len(value) > 253 || strings.ContainsAny(value, "@\r\n") || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func authErrors(value any) []string {
	if value == nil {
		return nil
	}
	if _, ok := value.(map[string]any); !ok {
		return []string{"auth: must be a mapping"}
	}
	var auth Auth
	if err := decodeYAMLValue(value, &auth); err != nil {
		return []string{"auth: " + err.Error()}
	}
	return auth.validate("auth")
}

func buildAuth(value any) (Auth, error) {
	if value == nil {
		return Auth{}, nil
	}
	if _, err := mapValue(value, "auth"); err != nil {
		return Auth{}, err
	}
	var auth Auth
	if err := decodeYAMLValue(value, &auth); err != nil {
		return Auth{}, fmt.Errorf("auth: %w", err)
	}
	return auth, nil
}
