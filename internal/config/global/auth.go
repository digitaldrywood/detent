package global

import (
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	AuthModeMagicLink     = "magic_link"
	defaultMagicLinkTTL   = 15 * time.Minute
	defaultAuthSessionTTL = 30 * 24 * time.Hour
	defaultAuthSMTPPort   = 587
)

type Auth struct {
	Mode          string   `yaml:"mode,omitempty"`
	PublicURL     string   `yaml:"public_url,omitempty"`
	AllowedEmails []string `yaml:"allowed_emails,omitempty"`
	LinkTTL       string   `yaml:"link_ttl,omitempty"`
	SessionTTL    string   `yaml:"session_ttl,omitempty"`
	SMTP          SMTP     `yaml:"smtp,omitempty"`
}

type SMTP struct {
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	From     string `yaml:"from,omitempty"`
}

func (a Auth) IsZero() bool {
	return !a.Configured()
}

func (a Auth) Configured() bool {
	return strings.TrimSpace(a.Mode) != "" ||
		strings.TrimSpace(a.PublicURL) != "" ||
		len(a.AllowedEmails) > 0 ||
		strings.TrimSpace(a.LinkTTL) != "" ||
		strings.TrimSpace(a.SessionTTL) != "" ||
		!a.SMTP.IsZero()
}

func (a Auth) MagicLinkEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(a.Mode), AuthModeMagicLink)
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
	} else if mode != AuthModeMagicLink {
		problems = append(problems, prefix+".mode: must equal "+AuthModeMagicLink)
	}
	if len(a.AllowedEmails) == 0 {
		problems = append(problems, prefix+".allowed_emails: must contain at least one email address")
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
	if _, err := a.LinkTTLDuration(); err != nil {
		problems = append(problems, err.Error())
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
	problems = append(problems, a.SMTP.validate(prefix+".smtp")...)
	return problems
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
