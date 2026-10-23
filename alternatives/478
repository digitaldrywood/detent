package auth

import (
	"errors"
	"net/mail"
	"strings"
)

type Allowlist struct {
	emails  map[string]struct{}
	domains map[string]struct{}
}

func NewAllowlist(emails []string, domains []string) (*Allowlist, error) {
	allowlist := &Allowlist{
		emails:  make(map[string]struct{}, len(emails)),
		domains: make(map[string]struct{}, len(domains)),
	}
	for _, email := range emails {
		email = normalizeEmail(email)
		if _, ok := verifiedEmailDomain(email); !ok {
			return nil, errors.New("allowlist email is invalid")
		}
		allowlist.emails[email] = struct{}{}
	}
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if !validDomain(domain) {
			return nil, errors.New("allowlist domain is invalid")
		}
		allowlist.domains[domain] = struct{}{}
	}
	if len(allowlist.emails) == 0 && len(allowlist.domains) == 0 {
		return nil, errors.New("at least one allowed email or domain is required")
	}
	return allowlist, nil
}

func (a *Allowlist) Allows(email string, verified bool) bool {
	if a == nil || !verified {
		return false
	}
	email = normalizeEmail(email)
	domain, ok := verifiedEmailDomain(email)
	if !ok {
		return false
	}
	if _, ok := a.emails[email]; ok {
		return true
	}
	_, ok = a.domains[domain]
	return ok
}

func verifiedEmailDomain(email string) (string, bool) {
	address, err := mail.ParseAddress(email)
	if err != nil || address == nil || !strings.EqualFold(address.Address, email) {
		return "", false
	}
	separator := strings.LastIndexByte(email, '@')
	if separator <= 0 || separator == len(email)-1 {
		return "", false
	}
	domain := strings.ToLower(email[separator+1:])
	return domain, validDomain(domain)
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
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
