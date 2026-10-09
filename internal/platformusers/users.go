package platformusers

import (
	"errors"
	"net/mail"
	"strings"
	"unicode/utf8"
)

const Limit = 50

type Query struct {
	Text  string `json:"q"`
	Exact bool   `json:"-"`
}

func Parse(text string) (Query, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if utf8.RuneCountInString(text) < 3 {
		return Query{}, errors.New("enter at least three characters")
	}
	address, err := mail.ParseAddress(text)
	return Query{Text: text, Exact: err == nil && address != nil && address.Address == text}, nil
}

type Organization struct {
	ID   string `json:"organization_id"`
	Name string `json:"organization_name"`
}

type Membership struct {
	Organization
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

type Invitation struct {
	Organization
	ExpiresAt string `json:"expires_at"`
}

type User struct {
	Email        string       `json:"email"`
	Subject      string       `json:"subject"`
	PlatformRole string       `json:"platform_role"`
	LastSignInAt string       `json:"last_sign_in_at"`
	Memberships  []Membership `json:"memberships"`
	Invitations  []Invitation `json:"invitations"`
}

type Result struct {
	Users      []User         `json:"users"`
	Unsearched []Organization `json:"unsearched"`
}

type TenantMember struct {
	Email    string `json:"email"`
	Subject  string `json:"subject"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

type TenantInvitation struct {
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at"`
}

type TenantResult struct {
	Members     []TenantMember     `json:"members"`
	Invitations []TenantInvitation `json:"invitations"`
}
